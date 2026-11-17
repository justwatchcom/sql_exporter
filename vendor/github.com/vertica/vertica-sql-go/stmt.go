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
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/vertica/vertica-sql-go/common"
	"github.com/vertica/vertica-sql-go/logger"
	"github.com/vertica/vertica-sql-go/msgs"
	"github.com/vertica/vertica-sql-go/parse"
)

var (
	stmtLogger        = logger.New("stmt")
	emailRegexPattern = regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)
)

type parseState int

const (
	parseStateUnparsed parseState = iota
	parseStateParseError
	parseStateParsed
)

type stmt struct {
	conn         *connection
	command      string
	preparedName string
	parseState   parseState
	isAtomicStmt bool
	namedArgPos  []string
	posArgCnt    int
	paramTypes   []common.ParameterType
	lastRowDesc  *msgs.BERowDescMsg
	// set if Vertica issues an error of ROLLBACK severity
	rolledBack      bool
	multiStatements bool
}

func newStmt(connection *connection, command string) (*stmt, error) {
	s := &stmt{
		conn:         connection,
		command:      command,
		preparedName: fmt.Sprintf("S%d%d%d", os.Getpid(), time.Now().Unix(), rand.Int31()),
		parseState:   parseStateUnparsed,
	}

	// Atomic statements (UDSF function DDL and function EXECUTE privilege statements)
	// must bypass the splitter to preserve semantics.
	analyzer := NewUDSFAnalyzer()
	s.isAtomicStmt = analyzer.ShouldTreatAsAtomicUnit(command)
	if s.isAtomicStmt {
		// Atomic statement: treat as single statement, no splitting
		s.multiStatements = false
		return s, nil
	}

	initialStatements := parse.SplitStatements(command)
	if len(initialStatements) == 0 {
		s.command = ""
		return s, nil
	}
	s.multiStatements = len(initialStatements) > 1

	if len(command) == 0 || emailRegexPattern.MatchString(command) {
		return s, nil
	}

	argCounter := func() string {
		s.posArgCnt++
		return "?"
	}
	s.command = parse.Lex(command, parse.WithNamedCallback(s.pushNamed), parse.WithPositionalSubstitution(argCounter))
	finalStatements := parse.SplitStatements(s.command)
	if len(finalStatements) == 0 {
		s.command = ""
		s.multiStatements = false
	} else {
		s.multiStatements = len(finalStatements) > 1
	}
	return s, nil
}

func (s *stmt) pushNamed(name string) {
	s.namedArgPos = append(s.namedArgPos, name)
}

// Close closes this statement.
func (s *stmt) Close() error {
	if s.parseState != parseStateParsed {
		return nil
	}
	if s.rolledBack {
		s.parseState = parseStateUnparsed
		s.conn.dead = true
		return nil
	}
	closeMsg := &msgs.FECloseMsg{TargetType: msgs.CmdTargetTypeStatement, TargetName: s.preparedName}

	if err := s.conn.sendMessage(closeMsg); err != nil {
		return err
	}

	if err := s.conn.sendMessage(&msgs.FEFlushMsg{}); err != nil {
		return err
	}

	for {
		bMsg, err := s.conn.recvMessage()

		if err != nil {
			return err
		}

		switch bMsg.(type) {
		case *msgs.BECloseCompleteMsg:
			s.parseState = parseStateUnparsed
			return nil
		case *msgs.BECmdDescriptionMsg:
			continue
		default:
			s.conn.defaultMessageHandler(bMsg)
		}
	}
}

// NumInput is used by database/sql to sanity check the number of arguments given
// before calling into the driver's query/exec functions. If named arguments are used
// this will return the number of unique named parameters, otherwise it is the number
// if ? placeholders.
func (s *stmt) NumInput() int {
	if len(s.namedArgPos) > 0 {
		uniqueArgs := make(map[string]bool)
		for _, arg := range s.namedArgPos {
			uniqueArgs[arg] = true
		}
		return len(uniqueArgs)
	}
	return s.posArgCnt
}

// convertToNamed takes an argument list of Value that come from the older Exec/Query functions
// and converts them to NamedValue to be forwarded to their Context equivalents.
func (s *stmt) convertToNamed(args []driver.Value) []driver.NamedValue {
	namedArgs := make([]driver.NamedValue, len(args))
	for idx, arg := range args {
		namedArgs[idx] = driver.NamedValue{
			Ordinal: idx,
			Value:   arg,
		}
	}
	return namedArgs
}

// injectNamedArgs takes a list of arguments, builds a symbol table of name => arg and then
// fills a list of positional arguments based on the names from the args parameter
// This will return an error if any of the given args lack a name
func (s *stmt) injectNamedArgs(args []driver.NamedValue) ([]driver.NamedValue, error) {
	if len(s.namedArgPos) == 0 {
		return args, nil
	}
	symbols := make(map[string]driver.NamedValue, len(args))
	for _, arg := range args {
		if len(arg.Name) > 0 {
			symbols[strings.ToUpper(arg.Name)] = arg
			continue
		}
		namedVal, ok := arg.Value.(driver.NamedValue)
		if !ok || len(namedVal.Name) == 0 {
			return nil, errors.New("all parameters must have names when using named parameters")
		}
		symbols[strings.ToUpper(namedVal.Name)] = namedVal
	}
	realArgs := make([]driver.NamedValue, len(s.namedArgPos))
	for pos, name := range s.namedArgPos {
		realArgs[pos] = symbols[name]
		realArgs[pos].Ordinal = pos
	}
	return realArgs, nil
}

// Exec docs
func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), s.convertToNamed(args))
}

// Query docs
func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	stmtLogger.Debug("stmt.Query(): %s\n", s.command)
	return s.QueryContext(context.Background(), s.convertToNamed(args))
}

// ExecContext docs
func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	stmtLogger.Trace("stmt.ExecContext()")

	rows, err := s.QueryContext(ctx, args)

	if err != nil {
		return driver.ResultNoRows, err
	}

	numCols := len(rows.Columns())
	vals := make([]driver.Value, numCols)

	if rows.Next(vals) == io.EOF {
		return driver.ResultNoRows, nil
	}

	rv := reflect.ValueOf(vals[0])

	return &result{lastInsertID: 0, rowsAffected: rv.Int()}, nil
}

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.QueryContextRaw(ctx, args)
}

// QueryContext docs
func (s *stmt) QueryContextRaw(ctx context.Context, baseArgs []driver.NamedValue) (driver.Rows, error) {
	stmtLogger.Debug("stmt.QueryContextRaw(): %s", s.command)

	args, err := s.injectNamedArgs(baseArgs)
	if err != nil {
		return newEmptyRows(), err
	}

	if len(strings.TrimSpace(s.command)) == 0 {
		return newEmptyRows(), nil
	}

	doneChan := make(chan bool, 1)
	go func(pid, key uint32) {
		select {
		case <-doneChan:
			return
		case <-ctx.Done():
			stmtLogger.Info("Context cancelled, cancelling %s", s.preparedName)
			cancelMsg := msgs.FECancelMsg{PID: pid, Key: key}
			conn, err := s.conn.establishSocketConnection()
			if err != nil {
				stmtLogger.Warn("unable to establish connection for cancellation")
				return
			}
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			if err := s.conn.sendMessageTo(&cancelMsg, conn); err != nil {
				stmtLogger.Warn("unable to send cancel message: %v", err)
			}
			if err := conn.Close(); err != nil {
				stmtLogger.Warn("error closing cancel connection: %v", err)
			}
			stmtLogger.Info("Cancelled %s", s.preparedName)
		}
	}(s.conn.backendPID, s.conn.cancelKey)

	s.conn.lockSessionMutex()
	defer s.conn.unlockSessionMutex()
	defer func() {
		doneChan <- true
	}()

	// LOCAL COPY must always use the simple query protocol. With the prepared-
	// statement path, bindAndExecute sends FEFlushMsg right after FEExecuteMsg;
	// the server enters GetLocalFileInfo state while processing FEExecuteMsg and
	// then rejects the FEFlushMsg with "Flush is invalid in state GetLocalFileInfo".
	if s.parseState == parseStateParsed && !s.isLocalCopyStatement() {
		if err = s.bindAndExecute("", args); err != nil {
			return newEmptyRows(), err
		}
		return s.collectResults(ctx)
	}

	interpolated, err := s.interpolate(args)
	if err != nil {
		return newEmptyRows(), err
	}

	// Atomic statements (UDSF function DDL and function EXECUTE privilege statements)
	// may include syntax that should not be split before execution.
	if s.isAtomicStmt {
		resultSet, runErr := s.runSimpleStatement(ctx, strings.TrimSpace(interpolated))
		if runErr != nil {
			return newEmptyRows(), runErr
		}
		return resultSet, nil
	}

	statements := parse.SplitStatements(interpolated)
	if len(statements) == 0 {
		return newEmptyRows(), nil
	}

	resultSets := make([]*rows, 0, len(statements))
	for _, statementSQL := range statements {
		execSQL := statementSQL
		execCtx := ctx
		var localFiles []*os.File

		if rewrittenSQL, localPaths, isLocal := rewriteLocalCopyToSTDIN(statementSQL); isLocal {
			execSQL = rewrittenSQL
			var openErr error
			localFiles, openErr = openLocalCopyFiles(localPaths)
			if openErr != nil {
				return newEmptyRows(), openErr
			}

			vCtx := NewVerticaContext(ctx)
			if parentCtx, ok := ctx.(VerticaContext); ok {
				_ = vCtx.SetCopyBlockSizeBytes(parentCtx.GetCopyBlockSizeBytes())
			}
			_ = vCtx.SetCopyInputStream(multiReaderFromFiles(localFiles))
			execCtx = vCtx
		}

		resultSet, runErr := s.runSimpleStatement(execCtx, execSQL)
		closeLocalCopyFiles(localFiles)
		if runErr != nil {
			return newEmptyRows(), runErr
		}
		resultSets = append(resultSets, resultSet)
	}

	return mergeRowSets(resultSets), nil
}

func (s *stmt) copySTDIN(ctx context.Context) {

	var streamToUse io.Reader
	streamToUse = os.Stdin

	var copyBlockSize = stdInDefaultCopyBlockSize

	if vCtx, ok := ctx.(VerticaContext); ok {
		streamToUse = vCtx.GetCopyInputStream()
		copyBlockSize = vCtx.GetCopyBlockSizeBytes()
	}

	block := make([]byte, copyBlockSize)
	for {
		bytesRead, err := streamToUse.Read(block)
		if err == io.EOF {
			s.conn.sendMessage(&msgs.FELoadDoneMsg{})
			break
		}
		if err != nil {
			s.conn.sendMessage(&msgs.FELoadFailMsg{Message: err.Error()})
			break
		}
		s.conn.sendMessage(&msgs.FELoadDataMsg{Data: block, UsedBytes: bytesRead})
	}
	s.conn.sendMessage(&msgs.FEFlushMsg{})
}

func (s *stmt) verifyLocalFiles(msg *msgs.BEVerifyLoadFilesMsg) error {
	fileSizes := make([]uint64, len(msg.FileList))

	for idx, fileName := range msg.FileList {
		fileInfo, err := os.Stat(filepath.Clean(fileName))
		if err != nil {
			return s.conn.sendMessage(&msgs.FELoadFailMsg{Message: err.Error()})
		}
		if fileInfo.IsDir() {
			return s.conn.sendMessage(&msgs.FELoadFailMsg{Message: fmt.Sprintf("%s is a directory", fileName)})
		}
		fileSizes[idx] = uint64(fileInfo.Size())
	}

	return s.conn.sendMessage(&msgs.FEVerifyLoadFiles{
		FileNames: msg.FileList,
		FileSizes: fileSizes,
	})
}

func (s *stmt) copyLocalFile(ctx context.Context, fileName string) error {
	copyBlockSize := stdInDefaultCopyBlockSize
	if vCtx, ok := ctx.(VerticaContext); ok {
		copyBlockSize = vCtx.GetCopyBlockSizeBytes()
	}

	fileHandle, err := os.Open(filepath.Clean(fileName))
	if err != nil {
		if sendErr := s.conn.sendMessage(&msgs.FELoadFailMsg{Message: err.Error()}); sendErr != nil {
			return sendErr
		}
		return nil
	}
	defer fileHandle.Close()

	block := make([]byte, copyBlockSize)
	for {
		bytesRead, readErr := fileHandle.Read(block)
		if readErr == io.EOF {
			// Signal end-of-file to the server. Do NOT send FEFlushMsg ('H') here:
			// after FELoadDoneMsg the server may still be in GetLocalFileInfo state
			// (e.g. waiting for additional files), and 'H' is invalid in that state.
			return s.conn.sendMessage(&msgs.FELoadDoneMsg{})
		}
		if readErr != nil {
			if err = s.conn.sendMessage(&msgs.FELoadFailMsg{Message: readErr.Error()}); err != nil {
				return err
			}
			return nil
		}
		if err = s.conn.sendMessage(&msgs.FELoadDataMsg{Data: block, UsedBytes: bytesRead}); err != nil {
			return err
		}
	}
}

func (s *stmt) runSimpleStatement(ctx context.Context, sql string) (*rows, error) {
	statement := strings.TrimSpace(sql)
	if len(statement) == 0 {
		return newEmptyRows(), nil
	}

	result := newEmptyRows()

	if err := s.conn.sendMessage(&msgs.FEQueryMsg{Query: statement}); err != nil {
		return result, err
	}

	for {
		bMsg, err := s.conn.recvMessage()
		if err != nil {
			return newEmptyRows(), err
		}

		switch msg := bMsg.(type) {
		case *msgs.BEDataRowMsg:
			if err = result.addRow(msg); err != nil {
				return result, err
			}
		case *msgs.BERowDescMsg:
			result = newRows(ctx, msg, s.conn.serverTZOffset)
		case *msgs.BECmdDescriptionMsg:
			continue
		case *msgs.BECmdCompleteMsg, *msgs.BEParseCompleteMsg:
			continue
		case *msgs.BEErrorMsg:
			if drainErr := s.conn.drainUntilReady(); drainErr != nil {
				return newEmptyRows(), drainErr
			}
			return newEmptyRows(), s.evaluateErrorMsg(msg)
		case *msgs.BEEmptyQueryResponseMsg:
			return newEmptyRows(), nil
		case *msgs.BEReadyForQueryMsg, *msgs.BEPortalSuspendedMsg:
			if err = result.finalize(); err != nil {
				return result, err
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			return result, nil
		case *msgs.BEInitSTDINLoadMsg:
			s.copySTDIN(ctx)
		case *msgs.BEVerifyLoadFilesMsg:
			if err = s.verifyLocalFiles(msg); err != nil {
				return newEmptyRows(), err
			}
		case *msgs.BELoadNewFileMsg:
			if err = s.copyLocalFile(ctx, msg.FileName); err != nil {
				return newEmptyRows(), err
			}
		default:
			s.conn.defaultMessageHandler(bMsg)
		}
	}
}

func mergeRowSets(sets []*rows) driver.Rows {
	filtered := make([]*rows, 0, len(sets))
	for _, set := range sets {
		if set != nil {
			filtered = append(filtered, set)
		}
	}

	switch len(filtered) {
	case 0:
		return newEmptyRows()
	case 1:
		return filtered[0]
	default:
		return &multiRows{sets: filtered}
	}
}

func (s *stmt) cleanQuotes(val string) string {
	re := regexp.MustCompile(`'+`)
	pairs := re.FindAllStringIndex(val, -1)
	if pairs == nil {
		return val
	}
	cleaned := strings.Builder{}
	cleaned.Grow(len(val))
	cleanedTo := 0
	for _, matchPair := range pairs {
		if (matchPair[1]-matchPair[0])%2 != 0 {
			cleaned.WriteString(val[cleanedTo:matchPair[1]])
			cleaned.WriteRune('\'')
			cleanedTo = matchPair[1]
		}
	}
	cleaned.WriteString(val[cleanedTo:])
	return cleaned.String()
}

func (s *stmt) formatArg(arg driver.NamedValue) string {
	var replaceStr string
	switch v := arg.Value.(type) {
	case nil:
		replaceStr = "NULL"
	case int64, float64:
		replaceStr = fmt.Sprintf("%v", v)
	case string:
		replaceStr = fmt.Sprintf("'%s'", s.cleanQuotes(v))
	case bool:
		if v {
			replaceStr = "true"
		} else {
			replaceStr = "false"
		}
	case time.Time:
		replaceStr = fmt.Sprintf("'%02d-%02d-%02d %02d:%02d:%02d.%09d'",
			v.Year(),
			v.Month(),
			v.Day(),
			v.Hour(),
			v.Minute(),
			v.Second(),
			v.Nanosecond())
	default:
		replaceStr = "?unknown_type?"
	}
	return replaceStr
}

func (s *stmt) interpolate(args []driver.NamedValue) (string, error) {

	numArgs := s.NumInput()

	if numArgs == 0 {
		return s.command, nil
	}

	curArg := 0
	argSwapper := func() string {
		arg := s.formatArg(args[curArg])
		curArg++
		return arg
	}

	result := parse.Lex(s.command, parse.WithPositionalSubstitution(argSwapper))
	return result, nil
}

func (s *stmt) evaluateErrorMsg(msg *msgs.BEErrorMsg) error {
	if msg.Severity == "ROLLBACK" {
		s.rolledBack = true
	}
	return errorMsgToVError(msg)
}

// isLocalCopyStatement reports whether the statement is a COPY ... FROM LOCAL ...
// command or an atomic statement. Such statements must use the simple query protocol because
// the server enters special states (GetLocalFileInfo for COPY LOCAL, or function parsing for UDSF)
// immediately after FEExecuteMsg is processed, making the trailing FEFlushMsg sent by
// bindAndExecute invalid in those states.
func (s *stmt) isLocalCopyStatement() bool {
	if s.isAtomicStmt {
		return true
	}

	statements := parse.SplitStatements(s.command)
	if len(statements) != 1 {
		return false
	}

	_, ok := analyzeLocalCopyStatement(statements[0])
	return ok
}

func openLocalCopyFiles(paths []string) ([]*os.File, error) {
	files := make([]*os.File, 0, len(paths))
	for _, localPath := range paths {
		localFile, openErr := os.Open(filepath.Clean(localPath))
		if openErr != nil {
			closeLocalCopyFiles(files)
			return nil, openErr
		}
		files = append(files, localFile)
	}
	return files, nil
}

func closeLocalCopyFiles(files []*os.File) {
	for _, localFile := range files {
		_ = localFile.Close()
	}
}

func multiReaderFromFiles(files []*os.File) io.Reader {
	if len(files) == 1 {
		return files[0]
	}
	// Rewriting LOCAL file lists to a single STDIN stream loses the server's
	// native file boundary handling. Reinsert a line break only when a source
	// file does not already end with one so adjacent text records do not merge.
	readers := make([]io.Reader, 0, len(files)*2)
	for idx, localFile := range files {
		readers = append(readers, localFile)
		if idx < len(files)-1 && fileNeedsTrailingNewline(localFile) {
			readers = append(readers, bytes.NewReader([]byte{'\n'}))
		}
	}
	return io.MultiReader(readers...)
}

func fileNeedsTrailingNewline(fileHandle *os.File) bool {
	fileInfo, err := fileHandle.Stat()
	if err != nil || fileInfo.Size() == 0 {
		return false
	}

	lastByte := []byte{0}
	if _, err = fileHandle.ReadAt(lastByte, fileInfo.Size()-1); err != nil {
		return false
	}

	return lastByte[0] != '\n' && lastByte[0] != '\r'
}

type sqlToken struct {
	text  string
	start int
	end   int
}

type localCopyAnalysis struct {
	fromStart   int
	suffixStart int
	paths       []string
}

func topLevelSQLTokens(statement string) []sqlToken {
	tokens := make([]sqlToken, 0, 16)
	var current strings.Builder
	tokenStart := -1
	// Tokenize only top-level SQL words and ignore quoted/commented regions so
	// keywords inside literals/comments do not affect LOCAL COPY detection.
	flushCurrent := func() {
		if current.Len() == 0 {
			return
		}
		tokens = append(tokens, sqlToken{text: strings.ToUpper(current.String()), start: tokenStart, end: tokenStart + current.Len()})
		current.Reset()
		tokenStart = -1
	}

	inSingleQuote := false
	inDoubleQuote := false
	inLineComment := false
	inBlockComment := false
	dollarTag := ""

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
			flushCurrent()
			inSingleQuote = true
			continue
		}

		if ch == '"' {
			flushCurrent()
			inDoubleQuote = true
			continue
		}

		if ch == '-' && i+1 < len(statement) && statement[i+1] == '-' {
			flushCurrent()
			i++
			inLineComment = true
			continue
		}

		if ch == '/' && i+1 < len(statement) {
			next := statement[i+1]
			if next == '*' {
				flushCurrent()
				i++
				inBlockComment = true
				continue
			}
		}

		if ch == '$' {
			if tag, length, ok := readDollarTag(statement, i); ok {
				flushCurrent()
				dollarTag = tag
				i += length - 1
				continue
			}
		}

		if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') ||
			(ch >= '0' && ch <= '9') || ch == '_' {
			if tokenStart < 0 {
				tokenStart = i
			}
			current.WriteByte(ch)
			continue
		}

		flushCurrent()
	}

	flushCurrent()
	return tokens
}

func skipSQLTrivia(statement string, pos int) int {
	for pos < len(statement) {
		ch := statement[pos]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '\f' {
			pos++
			continue
		}

		if ch == '-' && pos+1 < len(statement) && statement[pos+1] == '-' {
			pos += 2
			for pos < len(statement) && statement[pos] != '\n' && statement[pos] != '\r' {
				pos++
			}
			continue
		}

		if ch == '/' && pos+1 < len(statement) && statement[pos+1] == '*' {
			pos += 2
			for pos+1 < len(statement) {
				if statement[pos] == '*' && statement[pos+1] == '/' {
					pos += 2
					break
				}
				pos++
			}
			continue
		}

		break
	}

	return pos
}

func parseQuotedLocalPath(statement string, pos int) (string, int, bool) {
	if pos >= len(statement) || statement[pos] != '\'' {
		return "", pos, false
	}

	pos++
	var raw strings.Builder
	for pos < len(statement) {
		ch := statement[pos]
		if ch == '\'' {
			if pos+1 < len(statement) && statement[pos+1] == '\'' {
				raw.WriteByte('\'')
				pos += 2
				continue
			}
			pos++
			return raw.String(), pos, true
		}
		raw.WriteByte(ch)
		pos++
	}

	return "", pos, false
}

func analyzeLocalCopyStatement(statement string) (localCopyAnalysis, bool) {
	tokens := topLevelSQLTokens(statement)
	if len(tokens) == 0 || tokens[0].text != "COPY" {
		return localCopyAnalysis{}, false
	}

	fromLocalIdx := -1
	for i := 1; i+1 < len(tokens); i++ {
		if tokens[i].text == "FROM" && tokens[i+1].text == "LOCAL" {
			fromLocalIdx = i
			break
		}
	}
	if fromLocalIdx < 0 {
		return localCopyAnalysis{}, false
	}

	analysis := localCopyAnalysis{fromStart: tokens[fromLocalIdx].start, paths: make([]string, 0, 1)}
	pos := skipSQLTrivia(statement, tokens[fromLocalIdx+1].end)

	for {
		// Vertica LOCAL paths are SQL single-quoted literals. We parse one path,
		// then continue parsing comma-separated path literals if present.
		path, nextPos, ok := parseQuotedLocalPath(statement, pos)
		if !ok {
			return localCopyAnalysis{}, false
		}
		analysis.paths = append(analysis.paths, path)
		analysis.suffixStart = nextPos

		pos = skipSQLTrivia(statement, nextPos)
		if pos < len(statement) && statement[pos] == ',' {
			pos++
			pos = skipSQLTrivia(statement, pos)
			continue
		}
		break
	}

	if len(analysis.paths) == 0 {
		return localCopyAnalysis{}, false
	}

	return analysis, true
}

// isBinaryOrCompressedFormat checks if the COPY statement uses a format
// that cannot safely concatenate multiple files with newline delimiters.
// Binary and compressed formats (Avro, Parquet, ORC, GZIP, BZIP2) require
// native file boundary handling and cannot be safely merged as text streams.
func isBinaryOrCompressedFormat(statement string) bool {
	lowerStmt := strings.ToUpper(statement)
	binaryFormats := []string{
		"PARQUET", "ORC", "AVRO",
	}
	for _, format := range binaryFormats {
		if strings.Contains(lowerStmt, format) {
			return true
		}
	}
	// Check for compression hints
	compressors := []string{"GZIP", "BZIP2", "BZIP", "ZSTD"}
	for _, comp := range compressors {
		if strings.Contains(lowerStmt, comp) {
			return true
		}
	}
	return false
}

func rewriteLocalCopyToSTDIN(statement string) (string, []string, bool) {
	analysis, ok := analyzeLocalCopyStatement(statement)
	if !ok {
		return statement, nil, false
	}
	// Multi-file LOCAL COPY is only safe for text-based formats (CSV, JSON, TSV, etc.)
	// Binary and compressed formats require native file boundary handling.
	if len(analysis.paths) > 1 && isBinaryOrCompressedFormat(statement) {
		return statement, nil, false
	}
	// Keep the original suffix exactly as-is (including spacing/comments) to
	// avoid collapsing tokens such as STDINPARSER or STDINDELIMITER.
	rewritten := statement[:analysis.fromStart] + "FROM STDIN" + statement[analysis.suffixStart:]
	return rewritten, analysis.paths, true
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

func (s *stmt) prepareAndDescribe() error {
	if len(strings.TrimSpace(s.command)) == 0 {
		s.parseState = parseStateUnparsed
		return nil
	}

	parseMsg := &msgs.FEParseMsg{
		PreparedName: s.preparedName,
		Command:      s.command,
		NumArgs:      0,
	}

	// If we've already been parsed, no reason to do it again.
	if s.parseState == parseStateParsed {
		return nil
	}

	s.parseState = parseStateParseError

	s.conn.lockSessionMutex()
	defer s.conn.unlockSessionMutex()

	if err := s.conn.sendMessage(parseMsg); err != nil {
		return err
	}

	describeMsg := &msgs.FEDescribeMsg{TargetType: msgs.CmdTargetTypeStatement, TargetName: s.preparedName}

	if err := s.conn.sendMessage(describeMsg); err != nil {
		return err
	}

	if err := s.conn.sendMessage(&msgs.FEFlushMsg{}); err != nil {
		return err
	}

	for {
		bMsg, err := s.conn.recvMessage()

		if err != nil {
			return err
		}

		switch msg := bMsg.(type) {
		case *msgs.BEErrorMsg:
			s.conn.sync()
			return errorMsgToVError(msg)
		case *msgs.BEParseCompleteMsg:
			s.parseState = parseStateParsed
		case *msgs.BERowDescMsg:
			s.lastRowDesc = msg
			return nil
		case *msgs.BENoDataMsg:
			s.lastRowDesc = nil
			return nil
		case *msgs.BEParameterDescMsg:
			s.paramTypes = msg.ParameterTypes
		case *msgs.BECmdDescriptionMsg:
			continue
		default:
			s.conn.defaultMessageHandler(msg)
		}
	}
}

func (s *stmt) bindAndExecute(portalName string, args []driver.NamedValue) error {

	// We only need to send the OID types
	paramOIDs := make([]int32, len(s.paramTypes))
	for i, p := range s.paramTypes {
		paramOIDs[i] = int32(p.TypeOID)
	}

	if err := s.conn.sendMessage(&msgs.FEBindMsg{Portal: portalName, Statement: s.preparedName, NamedArgs: args, OIDTypes: paramOIDs}); err != nil {
		return err
	}

	if err := s.conn.sendMessage(&msgs.FEExecuteMsg{Portal: portalName}); err != nil {
		return err
	}

	if err := s.conn.sendMessage(&msgs.FEFlushMsg{}); err != nil {
		return err
	}

	return nil
}

func (s *stmt) collectResults(ctx context.Context) (*rows, error) {
	rows := newEmptyRows()

	if s.lastRowDesc != nil {
		rows = newRows(ctx, s.lastRowDesc, s.conn.serverTZOffset)
	}

	for {
		bMsg, err := s.conn.recvMessage()

		if err != nil {
			return newEmptyRows(), err
		}

		switch msg := bMsg.(type) {
		case *msgs.BEDataRowMsg:
			// Vertica's Describe phase under-reports the column count for CALL
			// statements that emit RAISE NOTICE: a DataRow at execution time may
			// contain more fields than columnDefs describes. Expand columnDefs
			// whenever a wider DataRow is seen so that Columns() always returns
			// the correct width.
			//
			// This is safe because collectResults() is synchronous and fully
			// buffers all rows before returning *rows to the caller. database/sql
			// calls Columns() only after receiving that object, so all expansions
			// are complete by the time the column list is observed.
			if uint16(len(rows.columnDefs.Columns)) < msg.Columns().NumCols {
				rows.expandColumnDefs(msg.Columns().NumCols)
			}
			err = rows.addRow(msg)
			if err != nil {
				return rows, err
			}
		case *msgs.BERowDescMsg:
			// An execution-time RowDescription may arrive before any DataRows
			// and can carry a more complete schema than the Describe-phase one.
			// Only adopt it if it has at least as many columns, to prevent a
			// truncated description from silently replacing a wider one.
			if rows.resultData.Peek() == nil && len(msg.Columns) >= len(rows.columnDefs.Columns) {
				s.lastRowDesc = msg
				rows = newRows(ctx, s.lastRowDesc, s.conn.serverTZOffset)
			}
		case *msgs.BEErrorMsg:
			s.conn.sync()
			return newEmptyRows(), s.evaluateErrorMsg(msg)
		case *msgs.BEEmptyQueryResponseMsg:
			return newEmptyRows(), nil
		case *msgs.BEBindCompleteMsg, *msgs.BECmdDescriptionMsg:
			continue
		case *msgs.BEReadyForQueryMsg, *msgs.BEPortalSuspendedMsg, *msgs.BECmdCompleteMsg:
			err = rows.finalize()
			if err != nil {
				return rows, err
			}
			return rows, ctx.Err()
		case *msgs.BEInitSTDINLoadMsg:
			s.copySTDIN(ctx)
		case *msgs.BEVerifyLoadFilesMsg:
			if err = s.verifyLocalFiles(msg); err != nil {
				return newEmptyRows(), err
			}
		case *msgs.BELoadNewFileMsg:
			if err = s.copyLocalFile(ctx, msg.FileName); err != nil {
				return newEmptyRows(), err
			}
		default:
			_, _ = s.conn.defaultMessageHandler(msg)
		}
	}
}
