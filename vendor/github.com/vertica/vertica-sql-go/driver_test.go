package vertigo

// Copyright (c) 2019-2020 Micro Focus or one of its affiliates.
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
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vertica/vertica-sql-go/logger"
)

var (
	testLogger         = logger.New("test")
	myDBConnectString  string
	otherConnectString string
	badConnectString   string
	ctx                context.Context
)

func assertTrue(t *testing.T, v bool) {
	t.Helper()

	if v {
		return
	}

	t.Fatal("value was not true")
}

func assertEqual(t *testing.T, a interface{}, b interface{}) {
	t.Helper()

	if a == b {
		return
	}

	t.Fatal(fmt.Sprintf("%v != %v", a, b))
}

func assertNoErr(t *testing.T, e error) {
	t.Helper()

	if e == nil {
		return
	}

	t.Fatal(e.Error())
}

func assertExecSQL(t *testing.T, connDB *sql.DB, script ...interface{}) {
	t.Helper()

	contents, err := ioutil.ReadFile(fmt.Sprintf("resources/tests/driver_test/%v.sql", script[0]))
	strContents := fmt.Sprintf(string(contents), script[1:]...)

	assertNoErr(t, err)

	for _, cmd := range strings.Split(strContents, ";") {
		trimmedCmd := strings.TrimSpace(cmd)

		if len(trimmedCmd) > 0 {
			testLogger.Debug("sending command: %s", trimmedCmd)
			_, err = connDB.ExecContext(ctx, trimmedCmd)
			assertNoErr(t, err)
		}
	}
}

func assertErr(t *testing.T, err error, errorSubstring string) {
	t.Helper()

	if err == nil {
		t.Fatalf("expected error containing '%s', but there was no error at all", errorSubstring)
	}

	errStr := err.Error()

	if strings.Contains(errStr, errorSubstring) {
		return
	}

	t.Fatalf("expected an error containing '%s', but found '%s'", errorSubstring, errStr)
}

func assertNext(t *testing.T, rows *sql.Rows) {
	t.Helper()

	if !rows.Next() {
		t.Fatal("another row was expected to be available, but wasn't")
	}
}

func assertNoNext(t *testing.T, rows *sql.Rows) {
	t.Helper()

	if rows.Next() {
		t.Fatal("no more rows expected available, but were")
	}
}

func openConnection(t *testing.T, setupScript ...interface{}) *sql.DB {
	connDB, err := sql.Open("vertica", myDBConnectString)
	assertNoErr(t, err)

	err = connDB.PingContext(ctx)
	assertNoErr(t, err)

	if len(setupScript) > 0 {
		assertExecSQL(t, connDB, setupScript...)
	}

	return connDB
}

func closeConnection(t *testing.T, connDB *sql.DB, teardownScript ...interface{}) {
	if len(teardownScript) > 0 {
		assertExecSQL(t, connDB, teardownScript...)
	}
	assertNoErr(t, connDB.Close())
}

func TestBasicQuery(t *testing.T) {
	connDB := openConnection(t)
	defer closeConnection(t, connDB)

	rows, err := connDB.QueryContext(ctx, "SELECT * FROM v_monitor.cpu_usage LIMIT 5")
	assertNoErr(t, err)

	defer rows.Close()

	columnNames, _ := rows.Columns()
	for _, columnName := range columnNames {
		testLogger.Debug("%s", columnName)
	}

	for rows.Next() {
		var nodeName string
		var startTime string
		var endTime string
		var avgCPU float64

		assertNoErr(t, rows.Scan(&nodeName, &startTime, &endTime, &avgCPU))

		testLogger.Debug("%s\t%s\t%s\t%f", nodeName, startTime, endTime, avgCPU)
	}

	rows2, err := connDB.QueryContext(ctx, "SELECT DISTINCT(keyword) FROM v_catalog.standard_keywords WHERE reserved='R' LIMIT 10")
	assertNoErr(t, err)

	defer rows2.Close()

	for rows2.Next() {
		var keyword string
		assertNoErr(t, rows2.Scan(&keyword))

		testLogger.Debug("\"%s\": true,", keyword)
	}

}

func TestBasicNamedArgs(t *testing.T) {
	connDB := openConnection(t)
	defer closeConnection(t, connDB)
	rows, err := connDB.QueryContext(ctx, "SELECT DISTINCT(keyword) FROM v_catalog.standard_keywords WHERE reserved=@type LIMIT 10", sql.Named("type", "R"))
	assertNoErr(t, err)
	defer rows.Close()
	for rows.Next() {
		var keyword string
		assertNoErr(t, rows.Scan(&keyword))
	}
}

func TestPreparedNamedArgs(t *testing.T) {
	connDB := openConnection(t)
	defer closeConnection(t, connDB)
	stmt, err := connDB.PrepareContext(ctx, "SELECT DISTINCT(keyword) FROM v_catalog.standard_keywords WHERE reserved=@type LIMIT 10")
	assertNoErr(t, err)
	rows, err := stmt.QueryContext(ctx, sql.Named("type", "R"))
	assertNoErr(t, err)
	defer rows.Close()
	for rows.Next() {
		var keyword string
		assertNoErr(t, rows.Scan(&keyword))
	}
}

func TestBasicExec(t *testing.T) {
	connDB := openConnection(t, "test_basic_exec_pre")
	defer closeConnection(t, connDB, "test_basic_exec_post")

	res, err := connDB.ExecContext(ctx, "INSERT INTO MyTable VALUES (21, 'Joe Perry')")
	assertNoErr(t, err)

	ct, err := res.RowsAffected()
	assertNoErr(t, err)
	assertEqual(t, ct, int64(1))

	_, err = res.LastInsertId()
	assertNoErr(t, err)
}

func TestBasicArgsQuery(t *testing.T) {
	connDB := openConnection(t, "test_basic_args_query_pre")
	defer closeConnection(t, connDB, "test_basic_args_query_post")

	res, err := connDB.ExecContext(ctx, "INSERT INTO MyTable VALUES (21, 'Joe Perry', true, 123.45, '1950-09-10 13:59:47')")
	assertNoErr(t, err)

	ct, err := res.RowsAffected()
	assertNoErr(t, err)
	assertEqual(t, ct, int64(1))

	//-----------------------------------------------------------------------------------------
	// Make sure we can iterate queries with a string
	//-----------------------------------------------------------------------------------------

	rows, err := connDB.QueryContext(ctx, "SELECT name FROM MyTable WHERE id=?", 21)
	assertNoErr(t, err)
	assertNext(t, rows)

	var nameStr string
	assertNoErr(t, rows.Scan(&nameStr))

	assertEqual(t, nameStr, "Joe Perry")
	assertNoNext(t, rows)

	assertNoErr(t, rows.Close())

	//-----------------------------------------------------------------------------------------
	// Make sure we can run queries with an int, bool and float
	//-----------------------------------------------------------------------------------------

	rows, err = connDB.QueryContext(ctx, "SELECT id, guitarist, height, birthday FROM MyTable WHERE name=?", "Joe Perry")
	assertNoErr(t, err)
	assertNext(t, rows)

	var id int
	var guitarist bool
	var height float64
	var birthday time.Time
	assertNoErr(t, rows.Scan(&id, &guitarist, &height, &birthday))

	assertEqual(t, id, 21)
	assertEqual(t, guitarist, true)
	assertEqual(t, height, 123.45)

	assertEqual(t, birthday.String()[0:19], "1950-09-10 13:59:47") // We gave a timestamp with assumed UTC0, so this is correct.
	assertNoNext(t, rows)

	//-----------------------------------------------------------------------------------------
	// Now see if we can do this are a prepare and a query
	//-----------------------------------------------------------------------------------------

	stmt, err := connDB.PrepareContext(ctx, "SELECT id FROM MyTable WHERE name=?")
	assertNoErr(t, err)

	rows, err = stmt.Query("Joe Perry")
	assertNoErr(t, err)
	assertNext(t, rows)

	assertNoErr(t, rows.Scan(&id))

	assertEqual(t, id, 21)
	assertNoNext(t, rows)

	//-----------------------------------------------------------------------------------------
	// Ensure the 'QueryRowContext()' variant works.
	//-----------------------------------------------------------------------------------------

	err = connDB.QueryRowContext(ctx, "SELECT id FROM MyTable WHERE name=?", "Joe Perry").Scan(&id)
	assertNoErr(t, err)
	assertEqual(t, id, 21)

	assertNoErr(t, rows.Close())
}

func TestTransaction(t *testing.T) {
	connDB := openConnection(t, "test_transaction_pre")
	defer closeConnection(t, connDB, "test_transaction_post")

	res, err := connDB.ExecContext(ctx, "INSERT INTO MyTable VALUES (21, 'Joe Perry', true, 123.45, '1950-09-10 13:59:47')")
	assertNoErr(t, err)

	ct, err := res.RowsAffected()
	assertNoErr(t, err)
	assertEqual(t, ct, int64(1))

	//-----------------------------------------------------------------------------------------
	// Test Syntaxes for Begin/Commit/Rollback
	//-----------------------------------------------------------------------------------------

	opts := &sql.TxOptions{
		Isolation: sql.LevelDefault,
		ReadOnly:  false,
	}

	tx, err := connDB.BeginTx(ctx, opts)
	assertNoErr(t, err)
	assertNoErr(t, tx.Commit())

	opts.Isolation = sql.LevelReadCommitted
	tx, err = connDB.BeginTx(ctx, opts)
	assertNoErr(t, err)
	assertNoErr(t, tx.Rollback())
}

func TestPWAuthentication(t *testing.T) {
	connDB := openConnection(t, "test_pw_authentication_pre")
	defer closeConnection(t, connDB, "test_pw_authentication_post")

	// Let the user try to login now.
	connDB2, err := sql.Open("vertica", otherConnectString)
	assertNoErr(t, err)

	assertNoErr(t, connDB2.PingContext(ctx))
	assertNoErr(t, connDB2.Close())

	// Try it again with a bad password
	connDB3, err := sql.Open("vertica", badConnectString)
	assertNoErr(t, err)

	err = connDB3.PingContext(ctx)

	assertErr(t, err, "Invalid username or password")

	assertNoErr(t, connDB3.Close())
}

func testAnAuthScheme(t *testing.T, scheme string) {
	connDB := openConnection(t, "test_an_auth_scheme_pre", scheme)
	defer closeConnection(t, connDB, "test_an_auth_scheme_post")

	// Let the user try to login now.
	connDB2, err := sql.Open("vertica", otherConnectString)
	assertNoErr(t, err)

	err = connDB2.PingContext(ctx)
	assertNoErr(t, err)
	assertNoErr(t, connDB2.Close())

	// Try it again with a bad password
	connDB3, err := sql.Open("vertica", badConnectString)
	assertNoErr(t, err)

	err = connDB3.PingContext(ctx)
	if err != nil && err.Error() != "EOF" {
		assertErr(t, err, "Invalid username or password")
	}

	assertNoErr(t, connDB3.Close())
}

func TestMD5Authentication(t *testing.T) {
	testAnAuthScheme(t, "MD5")
}

func TestSHA512Authentication(t *testing.T) {
	testAnAuthScheme(t, "SHA512")
}

func TestTimestampParsers(t *testing.T) {
	val, err := parseTimestampTZColumn("2018-02-01 21:09:33.1234+00")
	assertNoErr(t, err)
	assertEqual(t, fmt.Sprintf("%s", val)[0:30], "2018-02-01 21:09:33.1234 +0000")

	val, err = parseTimestampTZColumn("2018-02-01 21:09:33.123456-06")
	assertNoErr(t, err)
	assertEqual(t, fmt.Sprintf("%s", val)[0:32], "2018-02-01 21:09:33.123456 -0600")

	val, err = parseTimestampTZColumn("2018-01-27 21:09:44+10")
	assertNoErr(t, err)
	assertEqual(t, fmt.Sprintf("%s", val)[0:25], "2018-01-27 21:09:44 +1000")

	val, err = parseTimestampTZColumn("2018-01-27 21:09:44+00")
	assertNoErr(t, err)
	assertEqual(t, fmt.Sprintf("%s", val)[0:25], "2018-01-27 21:09:44 +0000")
}

func TestEmptyStatementError(t *testing.T) {
	connDB := openConnection(t)
	defer closeConnection(t, connDB)

	// Try as exec.
	_, err := connDB.ExecContext(ctx, "")
	assertErr(t, err, "empty statement")

	// Try as query.
	_, err = connDB.QueryContext(ctx, "")
	assertErr(t, err, "empty statement")
}

func TestValueTypes(t *testing.T) {
	connDB := openConnection(t, "test_value_types_pre")
	defer closeConnection(t, connDB, "test_value_types_post")

	var (
		boolVal        bool
		intVal         int
		floatVal       float64
		charVal        string
		varCharVal     string
		timestampVal   string
		timestampTZVal string
		varBinVal      string
		uuidVal        string
		lVarCharVal    string
		lVarBinaryVal  string
		binaryVal      string
		numericVal     float64
	)

	rows, err := connDB.QueryContext(ctx, "SELECT * FROM full_type_table")
	assertNoErr(t, err)
	assertNext(t, rows)
	assertNoErr(t, rows.Scan(&boolVal, &intVal, &floatVal, &charVal, &varCharVal, &timestampVal, &timestampTZVal,
		&varBinVal, &uuidVal, &lVarCharVal, &lVarBinaryVal, &binaryVal, &numericVal))
	assertEqual(t, boolVal, true)
	assertEqual(t, intVal, 123)
	assertEqual(t, floatVal, 3.141)
	assertEqual(t, charVal, "a")
	assertEqual(t, varCharVal, "test values")
	assertEqual(t, varBinVal, "5c3237365c3335375c3333365c323535")
	assertEqual(t, uuidVal, "372fd680-6a72-4003-96b0-10bbe78cd635")
	assertEqual(t, lVarCharVal, "longer var char")
	assertEqual(t, lVarBinaryVal, "5c3333365c3235355c3237365c333537")
	assertEqual(t, binaryVal, "5c323732")
	assertEqual(t, numericVal, 1.2345)

	assertNext(t, rows)

	var (
		nullBoolVal        sql.NullBool
		nullIntVal         sql.NullInt64
		nullFloatVal       sql.NullFloat64
		nullCharVal        sql.NullString
		nullVarCharVal     sql.NullString
		nullTimestampVal   sql.NullString
		nullTimestampTZVal sql.NullString
		nullVarBinVal      sql.NullString
		nullUuidVal        sql.NullString
		nullLVarCharVal    sql.NullString
		nullLVarBinaryVal  sql.NullString
		nullBinaryVal      sql.NullString
		nullNumericVal     sql.NullFloat64
	)

	assertNoErr(t, rows.Scan(&nullBoolVal, &nullIntVal, &nullFloatVal, &nullCharVal,
		&nullVarCharVal, &nullTimestampVal, &nullTimestampTZVal, &nullVarBinVal, &nullUuidVal,
		&nullLVarCharVal, &nullLVarBinaryVal, &nullBinaryVal, &nullNumericVal))

	assertTrue(t, !nullBoolVal.Valid)
	assertTrue(t, !nullIntVal.Valid)
	assertTrue(t, !nullFloatVal.Valid)
	assertTrue(t, !nullCharVal.Valid)
	assertTrue(t, !nullVarCharVal.Valid)
	assertTrue(t, !nullTimestampVal.Valid)
	assertTrue(t, !nullTimestampTZVal.Valid)
	assertTrue(t, !nullVarBinVal.Valid)
	assertTrue(t, !nullUuidVal.Valid)
	assertTrue(t, !nullLVarCharVal.Valid)
	assertTrue(t, !nullLVarBinaryVal.Valid)
	assertTrue(t, !nullBinaryVal.Valid)
	assertTrue(t, !nullNumericVal.Valid)

	assertNoErr(t, rows.Close())
}

// Issue 17 : Reusing prepared statements throws runtime errors
// https://github.com/vertica/vertica-sql-go/issues/17
func TestStmtReuseBug(t *testing.T) {
	connDB := openConnection(t)
	defer closeConnection(t, connDB)

	var res bool

	stmt, err := connDB.PrepareContext(ctx, "SELECT true AS res")
	assertNoErr(t, err)

	// first call
	rows, err := stmt.QueryContext(ctx)
	assertNoErr(t, err)

	defer rows.Close()

	assertNext(t, rows)
	assertNoErr(t, rows.Scan(&res))
	assertEqual(t, res, true)
	assertNoNext(t, rows)

	// second call
	rows, err = stmt.QueryContext(ctx)
	assertNoErr(t, err)

	defer rows.Close()

	assertNext(t, rows)
	assertNoErr(t, rows.Scan(&res))
	assertEqual(t, res, true)
	assertNoNext(t, rows)
}

// Issue 20 : No columns returned when query returns no rows
// https://github.com/vertica/vertica-sql-go/issues/20
func TestColumnsWithNoRows(t *testing.T) {
	connDB := openConnection(t)
	defer closeConnection(t, connDB)

	stmt, err := connDB.PrepareContext(ctx, "SELECT true AS res WHERE false")
	assertNoErr(t, err)

	defer stmt.Close()

	rows, err := stmt.QueryContext(ctx)
	assertNoErr(t, err)

	columns, err := rows.Columns()
	assertNoErr(t, err)

	defer rows.Close()

	assertEqual(t, len(columns), 1)

	assertNoNext(t, rows)
}

type threadedQuery struct {
	query         string
	resultColumns []string
}

// Issue 22 : Possible issue with wrong rows returned from current stmt results
// https://github.com/vertica/vertica-sql-go/issues/22
func TestStmtOrderingInThreads(t *testing.T) {
	connDB := openConnection(t, "test_stmt_ordering_threads_pre")
	connDB.SetMaxOpenConns(1)
	defer connDB.SetMaxOpenConns(0)
	defer closeConnection(t, connDB, "test_stmt_ordering_threads_post")

	connDB.SetMaxOpenConns(1)

	queries := []threadedQuery{
		{query: "SELECT a FROM stmt_thread_test", resultColumns: []string{"a"}},
		{query: "SELECT a, b FROM stmt_thread_test", resultColumns: []string{"a", "b"}},
		{query: "SELECT a, b, c FROM stmt_thread_test", resultColumns: []string{"a", "b", "c"}},
		{query: "SELECT a, b, c, d FROM stmt_thread_test", resultColumns: []string{"a", "b", "c", "d"}},
		{query: "SELECT a, b, c, d, e FROM stmt_thread_test", resultColumns: []string{"a", "b", "c", "d", "e"}},
		{query: "SELECT a FROM stmt_thread_test", resultColumns: []string{"a"}},
		{query: "SELECT a, b FROM stmt_thread_test", resultColumns: []string{"a", "b"}},
		{query: "SELECT a, b, c FROM stmt_thread_test", resultColumns: []string{"a", "b", "c"}},
		{query: "SELECT a, b, c, d FROM stmt_thread_test", resultColumns: []string{"a", "b", "c", "d"}},
		{query: "SELECT a, b, c, d, e FROM stmt_thread_test", resultColumns: []string{"a", "b", "c", "d", "e"}},
		{query: "SELECT a FROM stmt_thread_test", resultColumns: []string{"a"}},
		{query: "SELECT a, b FROM stmt_thread_test", resultColumns: []string{"a", "b"}},
		{query: "SELECT a, b, c FROM stmt_thread_test", resultColumns: []string{"a", "b", "c"}},
		{query: "SELECT a, b, c, d FROM stmt_thread_test", resultColumns: []string{"a", "b", "c", "d"}},
		{query: "SELECT a, b, c, d, e FROM stmt_thread_test", resultColumns: []string{"a", "b", "c", "d", "e"}},
		{query: "SELECT a FROM stmt_thread_test", resultColumns: []string{"a"}},
		{query: "SELECT a, b FROM stmt_thread_test", resultColumns: []string{"a", "b"}},
		{query: "SELECT a, b, c FROM stmt_thread_test", resultColumns: []string{"a", "b", "c"}},
		{query: "SELECT a, b, c, d FROM stmt_thread_test", resultColumns: []string{"a", "b", "c", "d"}},
		{query: "SELECT a, b, c, d, e FROM stmt_thread_test", resultColumns: []string{"a", "b", "c", "d", "e"}},
		{query: "SELECT a FROM stmt_thread_test", resultColumns: []string{"a"}},
		{query: "SELECT a, b FROM stmt_thread_test", resultColumns: []string{"a", "b"}},
		{query: "SELECT a, b, c FROM stmt_thread_test", resultColumns: []string{"a", "b", "c"}},
		{query: "SELECT a, b, c, d FROM stmt_thread_test", resultColumns: []string{"a", "b", "c", "d"}},
		{query: "SELECT a, b, c, d, e FROM stmt_thread_test", resultColumns: []string{"a", "b", "c", "d", "e"}},
	}

	var wg sync.WaitGroup
	wg.Add(len(queries))

	for ct, query := range queries {
		go func(idx int, q threadedQuery) {
			defer wg.Done()
			stmt, err := connDB.PrepareContext(ctx, q.query)
			assertNoErr(t, err)
			defer stmt.Close()
			rows, err := stmt.QueryContext(ctx)
			assertNoErr(t, err)
			defer rows.Close()
			assertNext(t, rows)

			columns, err := rows.Columns()
			assertNoErr(t, err)
			assertEqual(t, len(columns), len(q.resultColumns))
		}(ct, query)
	}

	wg.Wait()

}

// Issue 9 : Does it support COPY FROM / COPY TO ?
// https://github.com/vertica/vertica-sql-go/issues/9
func TestSTDINCopy(t *testing.T) {
	connDB := openConnection(t, "test_stdin_copy_pre")
	defer closeConnection(t, connDB, "test_stdin_copy_post")

	// Do some trickery with os.Stdin, but make sure to put it back when we're done.
	oldStdIn := os.Stdin

	fp, err := os.OpenFile("./resources/csv/sample_data.csv", os.O_RDONLY, 0600)
	assertNoErr(t, err)

	os.Stdin = fp
	defer func() { os.Stdin.Close(); os.Stdin = oldStdIn }()

	_, err = connDB.ExecContext(ctx, "COPY stdin_data FROM STDIN DELIMITER ','")
	assertNoErr(t, err)

	rows, err := connDB.QueryContext(ctx, "SELECT name,id FROM stdin_data as t(name,id) order by name")
	assertNoErr(t, err)

	defer rows.Close()

	columns, _ := rows.Columns()
	assertEqual(t, len(columns), 2)

	names := []string{"john", "roger", "siting", "tom", "yang"}
	ids := []int{555, 123, 456, 789, 333}
	matched := 0
	var name string
	var id int

	for rows.Next() {
		assertNoErr(t, rows.Scan(&name, &id))
		assertEqual(t, name, names[matched])
		assertEqual(t, id, ids[matched])
		matched++
	}

	assertEqual(t, matched, 5)
	assertNoNext(t, rows)
}

// Issue 9 : Does it support COPY FROM / COPY TO ?
// https://github.com/vertica/vertica-sql-go/issues/9
func TestSTDINCopyWithStream(t *testing.T) {
	connDB := openConnection(t, "test_stdin_copy_pre")
	defer closeConnection(t, connDB, "test_stdin_copy_post")

	fp, err := os.OpenFile("./resources/csv/sample_data.csv", os.O_RDONLY, 0600)
	assertNoErr(t, err)

	defer fp.Close()

	vCtx := NewVerticaContext(ctx)
	_ = vCtx.SetCopyInputStream(fp)
	_ = vCtx.SetCopyBlockSizeBytes(32768)

	_, err = connDB.ExecContext(vCtx, "COPY stdin_data FROM STDIN DELIMITER ','")
	assertNoErr(t, err)

	rows, err := connDB.QueryContext(ctx, "SELECT name,id FROM stdin_data AS t(name,id) ORDER BY name")
	assertNoErr(t, err)

	defer rows.Close()

	columns, _ := rows.Columns()
	assertEqual(t, len(columns), 2)

	names := []string{"john", "roger", "siting", "tom", "yang"}
	ids := []int{555, 123, 456, 789, 333}
	matched := 0
	var name string
	var id int

	for rows.Next() {
		assertNoErr(t, rows.Scan(&name, &id))
		assertEqual(t, name, names[matched])
		assertEqual(t, id, ids[matched])
		matched++
	}

	assertEqual(t, matched, 5)
	assertNoNext(t, rows)
}

// Issue 44 : error during parsing of prepared statement causes perpetual error state
// https://github.com/vertica/vertica-sql-go/issues/44
func TestHangAfterError(t *testing.T) {
	connDB := openConnection(t)
	defer closeConnection(t, connDB)

	rows, err := connDB.QueryContext(ctx, "SELECT 1")
	defer rows.Close()

	assertNoErr(t, err)
	assertNext(t, rows)
	assertNoNext(t, rows)

	rows, err = connDB.QueryContext(ctx, "SELECT 1+'abcd'")

	assertErr(t, err, "Invalid")

	rows, err = connDB.QueryContext(ctx, "SELECT 2")
	defer rows.Close()

	assertNoErr(t, err)
	assertNext(t, rows)
	assertNoNext(t, rows)
}

func testEnableResultCachePageSized(t *testing.T, connDB *sql.DB, ctx VerticaContext, pageSize int) {
	assertNoErr(t, ctx.SetInMemoryResultRowLimit(pageSize))

	rows, _ := connDB.QueryContext(ctx, "SELECT a, b, c, d, e FROM result_cache_test ORDER BY a")
	defer rows.Close()

	var a int
	var b string
	var c bool
	var d float64
	var e int
	var count int

	for rows.Next() {
		count++
		assertNoErr(t, rows.Scan(&a, &b, &c, &d, &e))
		assertEqual(t, a, count)
		assertEqual(t, b, "dog")
		assertEqual(t, c, true)
		assertEqual(t, d, 3.14159)
		assertEqual(t, e, 456)
	}

	assertNoNext(t, rows)
	assertEqual(t, count, 42)
}

// Issue 43 : response batching / cursor / lazy queries
// https://github.com/vertica/vertica-sql-go/issues/43
func TestEnableResultCache(t *testing.T) {
	connDB := openConnection(t, "test_enable_result_cache_pre")
	defer closeConnection(t, connDB, "test_enable_result_cache_post")

	vCtx := NewVerticaContext(context.Background())

	testEnableResultCachePageSized(t, connDB, vCtx, 1)
	testEnableResultCachePageSized(t, connDB, vCtx, 5)
	testEnableResultCachePageSized(t, connDB, vCtx, 49)
	testEnableResultCachePageSized(t, connDB, vCtx, 0)
}

//func TestConnectionClosure(t *testing.T) {
//	adminDB := openConnection(t, "test_connection_closed_pre")
//	defer closeConnection(t, adminDB, "test_connection_closed_post")
//	const userQuery = "select 1 as test"
//
//	userDB, _ := sql.Open("vertica", otherConnectString)
//	defer userDB.Close()
//	rows, err := userDB.Query(userQuery)
//	assertNoErr(t, err)
//	rows.Close()
//	adminDB.Query("select close_user_sessions('TestGuy')")
//	rows, err = userDB.Query(userQuery)
//	// Depending on Go version this second query may or may not error
//	if err == nil {
//		rows.Close()
//	}
//	rows, err = userDB.Query(userQuery)
//	assertNoErr(t, err) // Should definitely have a working connection again here
//	rows.Close()
//}

func TestConcurrentStatementQuery(t *testing.T) {
	connDB := openConnection(t, "test_stmt_ordering_threads_pre")
	defer closeConnection(t, connDB, "test_stmt_ordering_threads_post")
	stmt, err := connDB.PrepareContext(ctx, "SELECT a FROM stmt_thread_test")
	assertNoErr(t, err)
	wg := new(sync.WaitGroup)
	wg.Add(3)
	for i := 0; i < 3; i++ {
		go func() {
			defer wg.Done()
			_, err := stmt.QueryContext(ctx)
			assertNoErr(t, err)
		}()
	}
	wg.Wait()
}

func TestInvalidDDLStatement(t *testing.T) {
	connDB := openConnection(t)
	defer closeConnection(t, connDB)
	_, err := connDB.Exec("DROP VIEW DOESNOTEXISTVIEW")
	assertErr(t, err, "does not exist")
}

func TestLockOnError(t *testing.T) {
	connDB := openConnection(t)
	defer closeConnection(t, connDB)

	_, err := connDB.Query("select throw_error('whatever')")
	assertErr(t, err, "ERROR: whatever")

	_, err = connDB.QueryContext(ctx, "select 1")
	assertNoErr(t, err)
}

var verticaUserName = flag.String("user", "dbadmin", "the user name to connect to Vertica")
var verticaPassword = flag.String("password", os.Getenv("VERTICA_TEST_PASSWORD"), "Vertica password for this user")
var verticaHostPort = flag.String("locator", "localhost:5433", "Vertica's host and port")
var tlsMode = flag.String("tlsmode", "none", "SSL/TLS mode (none, server, server-strict)")
var usePreparedStmts = flag.Bool("use_prepared_statements", true, "whether to use prepared statements for all queries/executes")

func init() {
	// One or both lines below are necessary depending on your go version
	// testing.Init()
	// flag.Parse()

	testLogger.Info("user name: %s", *verticaUserName)
	testLogger.Info("password : **********")
	testLogger.Info("locator  : %s", *verticaHostPort)
	testLogger.Info("tlsmode  : %s", *tlsMode)

	usePreparedStmtsString := "use_prepared_statements="

	if *usePreparedStmts {
		usePreparedStmtsString += "1"
	} else {
		usePreparedStmtsString += "0"
	}

	myDBConnectString = "vertica://" + *verticaUserName + ":" + *verticaPassword + "@" + *verticaHostPort + "/" + *verticaUserName + "?" + usePreparedStmtsString + "&tlsMode=" + *tlsMode
	otherConnectString = "vertica://TestGuy:TestGuyPass@" + *verticaHostPort + "/TestGuy?tlsmode=" + *tlsMode
	badConnectString = "vertica://TestGuy:TestGuyBadPass@" + *verticaHostPort + "/TestGuy?tlsmode=" + *tlsMode

	ctx = context.Background()
}
