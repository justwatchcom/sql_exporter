package vertigo

// Copyright (c) 2019-2024 Open Text.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

import (
	"fmt"
	"strings"

	"github.com/vertica/vertica-sql-go/logger"
)

// UDSFStatementType represents the type of UDSF-related SQL statement.
type UDSFStatementType int

const (
	UDSFStatementTypeUnknown UDSFStatementType = iota
	UDSFStatementTypeCreateFunction
	UDSFStatementTypeAlterFunction
	UDSFStatementTypeDropFunction
	UDSFStatementTypeGrantUsage
	UDSFStatementTypeGrantExecute
	UDSFStatementTypeRevokeUsage
	UDSFStatementTypeRevokeExecute
)

var udsfLogger = logger.New("udsf")

// String returns the string representation of the UDSF statement type.
func (t UDSFStatementType) String() string {
	switch t {
	case UDSFStatementTypeCreateFunction:
		return "CREATE FUNCTION"
	case UDSFStatementTypeAlterFunction:
		return "ALTER FUNCTION"
	case UDSFStatementTypeDropFunction:
		return "DROP FUNCTION"
	case UDSFStatementTypeGrantUsage:
		return "GRANT USAGE"
	case UDSFStatementTypeGrantExecute:
		return "GRANT EXECUTE"
	case UDSFStatementTypeRevokeUsage:
		return "REVOKE USAGE"
	case UDSFStatementTypeRevokeExecute:
		return "REVOKE EXECUTE"
	default:
		return "UNKNOWN"
	}
}

// UDSFAnalyzer provides analysis and detection of UDSF-related SQL statements.
type UDSFAnalyzer struct{}

// NewUDSFAnalyzer creates a new UDSF analyzer instance.
func NewUDSFAnalyzer() *UDSFAnalyzer {
	return &UDSFAnalyzer{}
}

// IsUDSFStatement determines whether the given SQL statement is a function DDL statement
// related to UDSFs (CREATE/ALTER/DROP FUNCTION).
func (ua *UDSFAnalyzer) IsUDSFStatement(sql string) bool {
	stmtType, err := ua.GetStatementType(sql)
	if err != nil {
		udsfLogger.Debug("is_udsf=false reason=get_statement_type_error sql_len=%d err=%v", len(sql), err)
		return false
	}
	if !ua.hasSingleTopLevelStatement(sql) {
		udsfLogger.Debug("is_udsf=false reason=not_single_top_level_statement sql_len=%d type=%s", len(sql), stmtType)
		return false
	}

	switch stmtType {
	case UDSFStatementTypeCreateFunction,
		UDSFStatementTypeAlterFunction,
		UDSFStatementTypeDropFunction:
		udsfLogger.Debug("is_udsf=true sql_len=%d type=%s", len(sql), stmtType)
		return true
	default:
		udsfLogger.Debug("is_udsf=false reason=non_function_ddl sql_len=%d type=%s", len(sql), stmtType)
		return false
	}
}

// IsAtomicPrivilegeStatement determines whether the statement is a function privilege
// statement that should be treated as atomic when handling statement splitting.
//
// Only GRANT/REVOKE EXECUTE ON FUNCTION statements are considered atomic. Schema-level
// privilege statements are intentionally excluded.
func (ua *UDSFAnalyzer) IsAtomicPrivilegeStatement(sql string) bool {
	if !ua.hasSingleTopLevelStatement(sql) {
		udsfLogger.Debug("is_atomic_privilege=false reason=not_single_top_level_statement sql_len=%d", len(sql))
		return false
	}

	tokens := ua.tokenizeTopLevel(sql)
	if len(tokens) < 4 {
		udsfLogger.Debug("is_atomic_privilege=false reason=insufficient_tokens sql_len=%d token_count=%d", len(sql), len(tokens))
		return false
	}

	firstToken := strings.ToUpper(tokens[0])
	secondToken := strings.ToUpper(tokens[1])
	if (firstToken != "GRANT" && firstToken != "REVOKE") || secondToken != "EXECUTE" {
		udsfLogger.Debug("is_atomic_privilege=false reason=verb_mismatch sql_len=%d first=%s second=%s", len(sql), firstToken, secondToken)
		return false
	}

	for i := 2; i+1 < len(tokens); i++ {
		if strings.ToUpper(tokens[i]) == "ON" && strings.ToUpper(tokens[i+1]) == "FUNCTION" {
			udsfLogger.Debug("is_atomic_privilege=true sql_len=%d", len(sql))
			return true
		}
	}

	udsfLogger.Debug("is_atomic_privilege=false reason=not_on_function sql_len=%d", len(sql))
	return false
}

// hasSingleTopLevelStatement returns false when a top-level semicolon is followed
// by additional SQL tokens, indicating a multi-statement batch.
//
// This is a heuristic parser for UDSF detection, not a complete SQL parser.
// It tracks common PL/SQL-like nesting keywords so semicolons inside BEGIN/CASE/IF/
// LOOP/WHILE blocks are not treated as statement boundaries.
func (ua *UDSFAnalyzer) hasSingleTopLevelStatement(statement string) bool {
	inSingleQuote := false
	inDoubleQuote := false
	inLineComment := false
	inBlockComment := false
	dollarTag := ""

	depth := 0
	prevToken := ""
	var current strings.Builder

	flushToken := func(nextTokenStart int) {
		if current.Len() == 0 {
			return
		}

		token := strings.ToUpper(current.String())
		nextToken := ""
		if nextTokenStart < len(statement) {
			if lookaheadToken, ok := ua.nextTopLevelToken(statement, nextTokenStart); ok {
				nextToken = lookaheadToken
			}
		}

		switch token {
		case "BEGIN":
			depth++
		case "CASE", "LOOP", "WHILE":
			// Avoid counting the trailing keyword in END CASE/IF/LOOP/WHILE.
			if prevToken != "END" {
				depth++
			}
		case "IF":
			// Treat FUNCTION IF EXISTS / FUNCTION IF NOT EXISTS as DDL modifiers, not block openers.
			isFunctionDDLModifier := prevToken == "FUNCTION" && (nextToken == "EXISTS" || nextToken == "NOT")
			if prevToken != "END" && !isFunctionDDLModifier {
				depth++
			}
		case "END":
			if depth > 0 {
				depth--
			}
		}

		prevToken = token
		current.Reset()
	}

	for i := 0; i < len(statement); i++ {
		ch := statement[i]

		if inLineComment {
			if ch == '\n' || ch == '\r' {
				inLineComment = false
			}
			continue
		}

		if inBlockComment {
			if ch == '*' && i+1 < len(statement) && statement[i+1] == '/' {
				i++
				inBlockComment = false
			}
			continue
		}

		if inSingleQuote {
			if ch == '\'' {
				if i+1 < len(statement) && statement[i+1] == '\'' {
					i++
					continue
				}
				inSingleQuote = false
			}
			continue
		}

		if inDoubleQuote {
			if ch == '"' {
				if i+1 < len(statement) && statement[i+1] == '"' {
					i++
					continue
				}
				inDoubleQuote = false
			}
			continue
		}

		if dollarTag != "" {
			if i+len(dollarTag) <= len(statement) && statement[i:i+len(dollarTag)] == dollarTag {
				i += len(dollarTag) - 1
				dollarTag = ""
			}
			continue
		}

		if ch == '\'' {
			flushToken(i)
			inSingleQuote = true
			continue
		}

		if ch == '"' {
			flushToken(i)
			inDoubleQuote = true
			continue
		}

		if ch == '-' && i+1 < len(statement) && statement[i+1] == '-' {
			flushToken(i)
			i++
			inLineComment = true
			continue
		}

		if ch == '/' && i+1 < len(statement) {
			next := statement[i+1]
			if next == '*' {
				flushToken(i)
				i++
				inBlockComment = true
				continue
			}
		}

		if ch == '$' {
			if tag, length, ok := ua.readDollarTag(statement, i); ok {
				flushToken(i)
				dollarTag = tag
				i += length - 1
				continue
			}
		}

		if ch == ';' {
			flushToken(i)
			if depth == 0 {
				if _, ok := ua.nextTopLevelToken(statement, i+1); ok {
					return false
				}
			}
			continue
		}

		if isTokenChar(ch) {
			current.WriteByte(ch)
			continue
		}

		flushToken(i)
	}

	flushToken(len(statement))
	return true
}

// nextTopLevelToken returns the first top-level token at or after start while
// respecting quoted strings, comments, and dollar-quoted literals.
func (ua *UDSFAnalyzer) nextTopLevelToken(statement string, start int) (string, bool) {
	inSingleQuote := false
	inDoubleQuote := false
	inLineComment := false
	inBlockComment := false
	dollarTag := ""

	var current strings.Builder

	flushCurrent := func() (string, bool) {
		if current.Len() == 0 {
			return "", false
		}
		token := strings.ToUpper(current.String())
		current.Reset()
		return token, true
	}

	for i := start; i < len(statement); i++ {
		ch := statement[i]

		if inLineComment {
			if ch == '\n' || ch == '\r' {
				inLineComment = false
			}
			continue
		}

		if inBlockComment {
			if ch == '*' && i+1 < len(statement) && statement[i+1] == '/' {
				i++
				inBlockComment = false
			}
			continue
		}

		if inSingleQuote {
			if ch == '\'' {
				if i+1 < len(statement) && statement[i+1] == '\'' {
					i++
					continue
				}
				inSingleQuote = false
			}
			continue
		}

		if inDoubleQuote {
			if ch == '"' {
				if i+1 < len(statement) && statement[i+1] == '"' {
					i++
					continue
				}
				inDoubleQuote = false
			}
			continue
		}

		if dollarTag != "" {
			if i+len(dollarTag) <= len(statement) && statement[i:i+len(dollarTag)] == dollarTag {
				i += len(dollarTag) - 1
				dollarTag = ""
			}
			continue
		}

		if ch == '\'' {
			if token, ok := flushCurrent(); ok {
				return token, true
			}
			inSingleQuote = true
			continue
		}

		if ch == '"' {
			if token, ok := flushCurrent(); ok {
				return token, true
			}
			inDoubleQuote = true
			continue
		}

		if ch == '-' && i+1 < len(statement) && statement[i+1] == '-' {
			if token, ok := flushCurrent(); ok {
				return token, true
			}
			i++
			inLineComment = true
			continue
		}

		if ch == '/' && i+1 < len(statement) {
			next := statement[i+1]
			if next == '*' {
				if token, ok := flushCurrent(); ok {
					return token, true
				}
				i++
				inBlockComment = true
				continue
			}
		}

		if ch == '$' {
			if tag, length, ok := ua.readDollarTag(statement, i); ok {
				if token, ok := flushCurrent(); ok {
					return token, true
				}
				dollarTag = tag
				i += length - 1
				continue
			}
		}

		if isTokenChar(ch) {
			current.WriteByte(ch)
			continue
		}

		if token, ok := flushCurrent(); ok {
			return token, true
		}
	}

	if token, ok := flushCurrent(); ok {
		return token, true
	}

	return "", false
}

// GetStatementType determines the type of the given SQL statement.
// Returns UDSFStatementTypeUnknown if the statement is not a recognized UDSF statement.
func (ua *UDSFAnalyzer) GetStatementType(sql string) (UDSFStatementType, error) {
	tokens := ua.tokenizeTopLevel(sql)
	if len(tokens) == 0 {
		return UDSFStatementTypeUnknown, fmt.Errorf("empty statement")
	}

	firstToken := strings.ToUpper(tokens[0])
	secondToken := ""
	if len(tokens) > 1 {
		secondToken = strings.ToUpper(tokens[1])
	}

	// Detect CREATE FUNCTION
	if firstToken == "CREATE" && secondToken == "FUNCTION" {
		return UDSFStatementTypeCreateFunction, nil
	}

	// Detect CREATE OR REPLACE FUNCTION
	if firstToken == "CREATE" && len(tokens) > 3 {
		thirdToken := strings.ToUpper(tokens[2])
		fourthToken := strings.ToUpper(tokens[3])
		if secondToken == "OR" && thirdToken == "REPLACE" && fourthToken == "FUNCTION" {
			return UDSFStatementTypeCreateFunction, nil
		}
	}

	// Detect ALTER FUNCTION
	if firstToken == "ALTER" && secondToken == "FUNCTION" {
		return UDSFStatementTypeAlterFunction, nil
	}

	// Detect DROP FUNCTION
	if firstToken == "DROP" && secondToken == "FUNCTION" {
		return UDSFStatementTypeDropFunction, nil
	}

	// Detect GRANT/REVOKE with USAGE or EXECUTE
	if firstToken == "GRANT" && (secondToken == "USAGE" || secondToken == "EXECUTE") {
		if secondToken == "USAGE" {
			return UDSFStatementTypeGrantUsage, nil
		}
		return UDSFStatementTypeGrantExecute, nil
	}

	if firstToken == "REVOKE" && (secondToken == "USAGE" || secondToken == "EXECUTE") {
		if secondToken == "USAGE" {
			return UDSFStatementTypeRevokeUsage, nil
		}
		return UDSFStatementTypeRevokeExecute, nil
	}

	return UDSFStatementTypeUnknown, nil
}

// tokenizeTopLevel extracts top-level SQL tokens while respecting quoted strings,
// comments, and dollar-quoted literals. This is a non-regex implementation that
// does not interpret or modify the SQL content.
func (ua *UDSFAnalyzer) tokenizeTopLevel(statement string) []string {
	tokens := make([]string, 0, 16)
	var current strings.Builder
	tokenStart := -1

	// Tokenize only top-level SQL words and ignore quoted/commented regions
	flushCurrent := func() {
		if current.Len() == 0 {
			return
		}
		tokens = append(tokens, current.String())
		current.Reset()
		tokenStart = -1
	}

	inSingleQuote := false
	inDoubleQuote := false
	inLineComment := false
	inBlockComment := false
	var dollarTag string

	for i := 0; i < len(statement); i++ {
		ch := statement[i]

		// Handle line comments: skip until newline
		if inLineComment {
			if ch == '\n' || ch == '\r' {
				inLineComment = false
			}
			continue
		}

		// Handle block comments: skip until */
		if inBlockComment {
			if ch == '*' && i+1 < len(statement) && statement[i+1] == '/' {
				i++
				inBlockComment = false
			}
			continue
		}

		// Handle single-quoted strings: doubled quotes are escaped
		if inSingleQuote {
			if ch == '\'' {
				if i+1 < len(statement) && statement[i+1] == '\'' {
					i++
					continue
				}
				inSingleQuote = false
			}
			continue
		}

		// Handle double-quoted identifiers: doubled quotes are escaped
		if inDoubleQuote {
			if ch == '"' {
				if i+1 < len(statement) && statement[i+1] == '"' {
					i++
					continue
				}
				inDoubleQuote = false
			}
			continue
		}

		// Handle dollar-quoted strings ($tag$...$tag$)
		if dollarTag != "" {
			if i+len(dollarTag) <= len(statement) && statement[i:i+len(dollarTag)] == dollarTag {
				i += len(dollarTag) - 1
				dollarTag = ""
			}
			continue
		}

		// Detect start of single-quoted string
		if ch == '\'' {
			flushCurrent()
			inSingleQuote = true
			continue
		}

		// Detect start of double-quoted identifier
		if ch == '"' {
			flushCurrent()
			inDoubleQuote = true
			continue
		}

		// Detect line comment: --
		if ch == '-' && i+1 < len(statement) && statement[i+1] == '-' {
			flushCurrent()
			i++
			inLineComment = true
			continue
		}

		// Detect block comment: /* */
		if ch == '/' && i+1 < len(statement) {
			next := statement[i+1]
			if next == '*' {
				flushCurrent()
				i++
				inBlockComment = true
				continue
			}
		}

		// Detect dollar-quoted string: $tag$
		if ch == '$' {
			if tag, length, ok := ua.readDollarTag(statement, i); ok {
				flushCurrent()
				dollarTag = tag
				i += length - 1
				continue
			}
		}

		// Accumulate alphanumeric characters and underscores as token characters
		if isTokenChar(ch) {
			if tokenStart < 0 {
				tokenStart = i
			}
			current.WriteByte(ch)
			continue
		}

		// Non-token character: flush current token
		flushCurrent()
	}

	flushCurrent()
	return tokens
}

// readDollarTag attempts to read a dollar-quoted string tag starting at position i.
// Returns the tag (including $), its length in characters, and a boolean indicating success.
func (ua *UDSFAnalyzer) readDollarTag(statement string, start int) (string, int, bool) {
	if start >= len(statement) || statement[start] != '$' {
		return "", 0, false
	}

	end := start + 1
	for end < len(statement) {
		if statement[end] == '$' {
			return statement[start : end+1], end + 1 - start, true
		}
		r := rune(statement[end])
		// Check if character is valid in dollar-quote tag
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') || r == '_') {
			break
		}
		end++
	}

	return "", 0, false
}

// isTokenChar reports whether a character is valid in a SQL token (letter, digit, or underscore).
func isTokenChar(ch byte) bool {
	return (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') ||
		(ch >= '0' && ch <= '9') || ch == '_'
}

// ShouldTreatAsAtomicUnit determines whether a statement should not be split
// by the statement splitter.
//
// Atomic statements currently include:
//   - UDSF function DDL statements (CREATE/ALTER/DROP FUNCTION)
//   - Function EXECUTE privilege statements (GRANT/REVOKE EXECUTE ON FUNCTION)
//
// The split is intentional: schema-level USAGE grants/revokes are not treated
// as atomic and therefore remain on the normal split/merge path.
func (ua *UDSFAnalyzer) ShouldTreatAsAtomicUnit(sql string) bool {
	isUDSF := ua.IsUDSFStatement(sql)
	isAtomicPrivilege := ua.IsAtomicPrivilegeStatement(sql)
	result := isUDSF || isAtomicPrivilege
	udsfLogger.Debug("should_treat_atomic=%t sql_len=%d is_udsf=%t is_atomic_privilege=%t", result, len(sql), isUDSF, isAtomicPrivilege)
	return result
}
