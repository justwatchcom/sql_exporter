package parse

import (
	"strings"
	"unicode"
)

// Copyright (c) 2020-2026 Open Text.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//    http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// SplitStatements breaks a SQL string into individual statements separated by semicolons
// that are not contained within literals or comments.
func SplitStatements(query string) []string {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil
	}

	var statements []string
	var current strings.Builder
	// Track our current lexical state so we can ignore semicolons that live
	// inside literals or comments.
	inSingleQuote := false
	inDoubleQuote := false
	inLineComment := false
	inBlockComment := false
	var dollarTag string
	depth := 0
	prevToken := ""
	statementHasContent := false
	var currentToken strings.Builder

	markNonWhitespace := func(b byte) {
		if !unicode.IsSpace(rune(b)) {
			statementHasContent = true
		}
	}
	flushToken := func(nextTokenStart int) {
		if currentToken.Len() == 0 {
			return
		}

		token := strings.ToUpper(currentToken.String())
		nextToken := ""
		if nextTokenStart < len(query) {
			if lookaheadToken, ok := nextTopLevelToken(query, nextTokenStart); ok {
				nextToken = lookaheadToken
			}
		}

		switch token {
		case "BEGIN":
			depth++
		case "CASE", "LOOP", "WHILE":
			if prevToken != "END" {
				depth++
			}
		case "IF":
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
		currentToken.Reset()
	}
	flush := func() {
		statement := strings.TrimSpace(current.String())
		current.Reset()
		currentToken.Reset()
		depth = 0
		prevToken = ""
		if statement != "" && statementHasContent {
			statements = append(statements, statement)
		}
		statementHasContent = false
	}

	i := 0
	for i < len(query) {
		ch := query[i]

		if inLineComment {
			// Swallow comment text but keep the newline terminator so tokens remain separated.
			if ch == '\n' || ch == '\r' {
				current.WriteByte(ch)
				inLineComment = false
			}
			i++
			continue
		}

		if inBlockComment {
			// Traditional /* ... */ comments block statement splitting
			// until the closing marker is found.
			current.WriteByte(ch)
			if ch == '*' && i+1 < len(query) && query[i+1] == '/' {
				current.WriteByte('/')
				i += 2
				inBlockComment = false
				continue
			}
			i++
			continue
		}

		if inSingleQuote {
			// Stay inside the literal, handling doubled single quotes.
			current.WriteByte(ch)
			markNonWhitespace(ch)
			if ch == '\'' {
				if i+1 < len(query) && query[i+1] == '\'' {
					current.WriteByte('\'')
					markNonWhitespace('\'')
					i += 2
					continue
				}
				inSingleQuote = false
			}
			i++
			continue
		}

		if inDoubleQuote {
			// Identifiers can be quoted with double quotes; treat them like
			// strings for splitter purposes.
			current.WriteByte(ch)
			markNonWhitespace(ch)
			if ch == '"' {
				if i+1 < len(query) && query[i+1] == '"' {
					current.WriteByte('"')
					markNonWhitespace('"')
					i += 2
					continue
				}
				inDoubleQuote = false
			}
			i++
			continue
		}

		if dollarTag != "" {
			statementHasContent = true
			// Inside a dollar-quoted literal; exit only when the exact tag is
			// observed again.
			if i+len(dollarTag) <= len(query) && query[i:i+len(dollarTag)] == dollarTag {
				current.WriteString(dollarTag)
				markNonWhitespace(dollarTag[0])
				i += len(dollarTag)
				dollarTag = ""
				continue
			}
			current.WriteByte(ch)
			i++
			continue
		}

		if isStatementTokenChar(ch) {
			current.WriteByte(ch)
			currentToken.WriteByte(ch)
			markNonWhitespace(ch)
			i++
			continue
		}

		flushToken(i)

		if ch == '\'' {
			inSingleQuote = true
			current.WriteByte(ch)
			markNonWhitespace(ch)
			i++
			continue
		}

		if ch == '"' {
			inDoubleQuote = true
			current.WriteByte(ch)
			markNonWhitespace(ch)
			i++
			continue
		}

		if ch == '-' && i+1 < len(query) && query[i+1] == '-' {
			i += 2
			inLineComment = true
			continue
		}

		if ch == '/' && i+1 < len(query) {
			next := query[i+1]
			if next == '*' {
				current.WriteByte('/')
				current.WriteByte('*')
				i += 2
				inBlockComment = true
				continue
			}
		}

		if ch == '$' {
			if tag, length, ok := readDollarTag(query, i); ok {
				dollarTag = tag
				current.WriteString(tag)
				markNonWhitespace(tag[0])
				i += length
				continue
			}
		}

		if ch == ';' {
			if depth == 0 {
				flush()
			} else {
				current.WriteByte(ch)
				markNonWhitespace(ch)
			}
			i++
			continue
		}

		current.WriteByte(ch)
		markNonWhitespace(ch)
		i++
	}

	flush()
	return statements
}

func nextTopLevelToken(query string, start int) (string, bool) {
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

	for i := start; i < len(query); i++ {
		ch := query[i]

		if inLineComment {
			if ch == '\n' || ch == '\r' {
				inLineComment = false
			}
			continue
		}

		if inBlockComment {
			if ch == '*' && i+1 < len(query) && query[i+1] == '/' {
				i++
				inBlockComment = false
			}
			continue
		}

		if inSingleQuote {
			if ch == '\'' {
				if i+1 < len(query) && query[i+1] == '\'' {
					i++
					continue
				}
				inSingleQuote = false
			}
			continue
		}

		if inDoubleQuote {
			if ch == '"' {
				if i+1 < len(query) && query[i+1] == '"' {
					i++
					continue
				}
				inDoubleQuote = false
			}
			continue
		}

		if dollarTag != "" {
			if i+len(dollarTag) <= len(query) && query[i:i+len(dollarTag)] == dollarTag {
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

		if ch == '-' && i+1 < len(query) && query[i+1] == '-' {
			if token, ok := flushCurrent(); ok {
				return token, true
			}
			i++
			inLineComment = true
			continue
		}

		if ch == '/' && i+1 < len(query) && query[i+1] == '*' {
			if token, ok := flushCurrent(); ok {
				return token, true
			}
			i++
			inBlockComment = true
			continue
		}

		if ch == '$' {
			if tag, length, ok := readDollarTag(query, i); ok {
				if token, ok := flushCurrent(); ok {
					return token, true
				}
				dollarTag = tag
				i += length - 1
				continue
			}
		}

		if isStatementTokenChar(ch) {
			current.WriteByte(ch)
			continue
		}

		if token, ok := flushCurrent(); ok {
			return token, true
		}
	}

	return flushCurrent()
}

func isStatementTokenChar(ch byte) bool {
	return (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') ||
		(ch >= '0' && ch <= '9') || ch == '_'
}

func readDollarTag(query string, start int) (string, int, bool) {
	if query[start] != '$' {
		return "", 0, false
	}

	end := start + 1
	for end < len(query) {
		r := rune(query[end])
		if query[end] == '$' {
			return query[start : end+1], end + 1 - start, true
		}
		if !isDollarTagRune(r) {
			break
		}
		end++
	}

	return "", 0, false
}

func isDollarTagRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}
