package vertigo

// Copyright (c) 2020 Micro Focus or one of its affiliates.
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
	"database/sql/driver"
	"testing"
)

func testStatement(command string) *stmt {
	stmt, _ := newStmt(nil, command)
	return stmt
}

func TestInterpolate(t *testing.T) {
	var testCases = []struct {
		name     string
		command  string
		expected string
		args     []driver.NamedValue
	}{
		{
			name:     "no parameters",
			command:  "select * from something",
			expected: "select * from something",
			args:     []driver.NamedValue{},
		},
		{
			name:     "simple string",
			command:  "select * from something where value = ?",
			expected: "select * from something where value = 'taco'",
			args:     []driver.NamedValue{{Value: "taco"}},
		},
		{
			name:     "multiple values",
			command:  "select * from something where value = ? and otherVal = ?",
			expected: "select * from something where value = 'taco' and otherVal = 15.5",
			args:     []driver.NamedValue{{Value: "taco"}, {Value: 15.5}},
		},
		{
			name:     "strings with quotes",
			command:  "select * from something where value = ?",
			expected: "select * from something where value = 'it''s other''s'",
			args:     []driver.NamedValue{{Value: "it's other's"}},
		},
		{
			name:     "strings with already escaped quotes",
			command:  "select * from something where value = ?",
			expected: "select * from something where value = 'it''s other''s'",
			args:     []driver.NamedValue{{Value: "it''s other''s"}},
		},
		{
			name:     "with a param looking rune in a string",
			command:  "select * from something where value = ? and test = '?bad'",
			expected: "select * from something where value = 'replace' and test = '?bad'",
			args:     []driver.NamedValue{{Value: "replace"}},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			stmt := testStatement(tc.command)
			result, err := stmt.interpolate(tc.args)
			if result != tc.expected {
				t.Errorf("Expected query to be %s but got %s", tc.command, result)
			}
			if err != nil {
				t.Errorf("Received error from interpolate: %v", err)
			}
		})
	}
}

func TestCleanQuotes(t *testing.T) {
	var testCases = []struct {
		name     string
		val      string
		expected string
	}{
		{
			name:     "Already paired",
			val:      "isn''t",
			expected: "isn''t",
		},
		{
			name:     "Unpaired at end",
			val:      "pair it'''",
			expected: "pair it''''",
		},
		{
			name:     "Unpaired at start",
			val:      "'pair it",
			expected: "''pair it",
		},
		{
			name:     "multiple unpaired",
			val:      "isn't wasn't",
			expected: "isn''t wasn''t",
		},
		{
			name:     "simple fix",
			val:      "isn't",
			expected: "isn''t",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			stmt := testStatement("")
			result := stmt.cleanQuotes(tc.val)
			if result != tc.expected {
				t.Errorf("Expected result to be %s got %s", tc.expected, result)
			}
		})
	}
}

func TestInjectNamedArgs(t *testing.T) {
	var testCases = []struct {
		name     string
		query    string
		args     []driver.NamedValue
		expected []driver.NamedValue
	}{
		{
			name:     "no named arguments",
			query:    "select * from table where a=?",
			args:     []driver.NamedValue{{Name: "", Value: "hello"}},
			expected: []driver.NamedValue{{Name: "", Value: "hello"}},
		},
		{
			name:     "multiple names",
			query:    "select * from table where a=@first and b=@second",
			args:     []driver.NamedValue{{Name: "first", Value: "hello"}, {Name: "second", Value: 123}},
			expected: []driver.NamedValue{{Name: "first", Value: "hello"}, {Name: "second", Value: 123}},
		},
		{
			name:     "reusing names",
			query:    "select * from table where a=@id and other=@test and b=@id",
			args:     []driver.NamedValue{{Name: "id", Value: 123}, {Name: "test", Value: 456}},
			expected: []driver.NamedValue{{Name: "id", Value: 123}, {Name: "test", Value: 456}, {Name: "id", Value: 123}},
		},
		{
			name:     "nested NamedValue from Query or Exec",
			query:    "select * from table where a=@id and other=@test and b=@id",
			args:     []driver.NamedValue{{Name: "id", Value: 123}, {Name: "", Value: driver.NamedValue{Name: "test", Value: 456}}},
			expected: []driver.NamedValue{{Name: "id", Value: 123}, {Name: "test", Value: 456}, {Name: "id", Value: 123}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			stmt, _ := newStmt(nil, tc.query)
			result, _ := stmt.injectNamedArgs(tc.args)
			for i, r := range result {
				if r.Value != tc.expected[i].Value {
					t.Errorf("Expected %v at pos %d but got %v", tc.expected[i].Value, i, r)
				}
				if r.Ordinal != i {
					t.Errorf("Expected ordinal to be set as %d but got %d", i, r.Ordinal)
				}
			}
		})
	}
}

func TestNumInput(t *testing.T) {
	var testCases = []struct {
		name     string
		query    string
		expected int
	}{
		{
			name:     "basic positional",
			query:    "select * from table where a = ?",
			expected: 1,
		},
		{
			name:     "basic named",
			query:    "select * from table where a = @name",
			expected: 1,
		},
		{
			name:     "reused named",
			query:    "select * from table where a = @name and b = @name and c = @id",
			expected: 2,
		},
		{
			name: "positional character in comment",
			query: `select * --test?
			from table where a = 1`,
			expected: 0,
		},
		{
			name:     "positional character in string",
			query:    "select * from test where a = 'test?'",
			expected: 0,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			stmt, _ := newStmt(nil, tc.query)
			result := stmt.NumInput()
			if result != tc.expected {
				t.Errorf("Expected %d query inputs, got %d", tc.expected, result)
			}
		})
	}
}
