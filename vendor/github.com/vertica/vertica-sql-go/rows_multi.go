package vertigo

// Copyright (c) 2019-2026 Open Text.
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
	"io"
	"reflect"
)

// multiRows stitches together the results of multiple SQL statements so the
// standard database/sql iteration APIs can page through each result set.
type multiRows struct {
	sets    []*rows
	current int
}

func (m *multiRows) currentRows() *rows {
	return m.sets[m.current]
}

func (m *multiRows) Columns() []string {
	return m.currentRows().Columns()
}

func (m *multiRows) Close() error {
	var err error
	for _, set := range m.sets {
		if closeErr := set.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}

func (m *multiRows) Next(dest []driver.Value) error {
	return m.currentRows().Next(dest)
}

func (m *multiRows) HasNextResultSet() bool {
	return m.current+1 < len(m.sets)
}

// NextResultSet advances to the next buffered result set, mirroring how
// database/sql exposes multi-statement results.
func (m *multiRows) NextResultSet() error {
	if !m.HasNextResultSet() {
		return io.EOF
	}
	m.current++
	return nil
}

// The column metadata helpers simply forward to the active rows instance so
// callers always see the schema for the current statement.
func (m *multiRows) ColumnTypeDatabaseTypeName(index int) string {
	return m.currentRows().ColumnTypeDatabaseTypeName(index)
}

func (m *multiRows) ColumnTypeNullable(index int) (nullable, ok bool) {
	return m.currentRows().ColumnTypeNullable(index)
}

func (m *multiRows) ColumnTypePrecisionScale(index int) (precision, scale int64, ok bool) {
	return m.currentRows().ColumnTypePrecisionScale(index)
}

func (m *multiRows) ColumnTypeLength(index int) (length int64, ok bool) {
	return m.currentRows().ColumnTypeLength(index)
}

func (m *multiRows) ColumnTypeScanType(index int) reflect.Type {
	return m.currentRows().ColumnTypeScanType(index)
}
