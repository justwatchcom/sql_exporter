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
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/binary"
	"testing"

	"github.com/vertica/vertica-sql-go/msgs"
)

func makeColumnDef() *msgs.BERowDescMsg {
	cols := make([]*msgs.BERowDescColumnDef, 4)
	cols[0] = &msgs.BERowDescColumnDef{FieldName: "a", AttribNum: 1, DataTypeOID: 6, DataTypeName: "integer", Length: 8}
	cols[1] = &msgs.BERowDescColumnDef{FieldName: "b", AttribNum: 2, DataTypeOID: 9, DataTypeName: "varchar", Length: -1}
	cols[2] = &msgs.BERowDescColumnDef{FieldName: "c", AttribNum: 3, DataTypeOID: 5, DataTypeName: "boolean", Length: 1}
	cols[3] = &msgs.BERowDescColumnDef{FieldName: "d", AttribNum: 4, DataTypeOID: 6, DataTypeName: "integer", Length: 8}
	return &msgs.BERowDescMsg{Columns: cols}
}

func mockRow() []byte {
	buf := bytes.NewBuffer(make([]byte, 0, 30))
	binary.Write(buf, binary.BigEndian, int16(4))
	binary.Write(buf, binary.BigEndian, int32(3))
	binary.Write(buf, binary.BigEndian, []byte("123"))
	binary.Write(buf, binary.BigEndian, int32(5))
	binary.Write(buf, binary.BigEndian, []byte("hello"))
	binary.Write(buf, binary.BigEndian, int32(1))
	binary.Write(buf, binary.BigEndian, false)
	binary.Write(buf, binary.BigEndian, int32(3))
	binary.Write(buf, binary.BigEndian, []byte("456"))
	return buf.Bytes()
}

//Simulate loading a bunch of rows from messages and then extracting them with Next()
func BenchmarkRows(b *testing.B) {
	const rowCount = 10000
	var msgType msgs.BEDataRowMsg
	var mockData [rowCount][]byte
	for i := 0; i < rowCount; i++ {
		mockData[i] = mockRow()
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		rows := newRows(context.Background(), makeColumnDef(), "")
		for i := 0; i < rowCount; i++ {
			rowI, _ := msgType.CreateFromMsgBody(msgs.NewMsgBufferFromBytes(mockData[i]))
			rows.addRow(rowI.(*msgs.BEDataRowMsg))
		}
		result := make([]driver.Value, 4)
		for i := 0; i < rowCount; i++ {
			rows.Next(result)
		}
	}
}

func BenchmarkRowsWithLimit(b *testing.B) {
	const rowCount = 10000
	vCtx := NewVerticaContext(context.Background())
	vCtx.SetInMemoryResultRowLimit(1000)
	var msgType msgs.BEDataRowMsg
	var mockData [rowCount][]byte
	for i := 0; i < rowCount; i++ {
		mockData[i] = mockRow()
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		rows := newRows(vCtx, makeColumnDef(), "")
		for i := 0; i < rowCount; i++ {
			rowI, _ := msgType.CreateFromMsgBody(msgs.NewMsgBufferFromBytes(mockData[i]))
			rows.addRow(rowI.(*msgs.BEDataRowMsg))
		}
		result := make([]driver.Value, 4)
		for i := 0; i < rowCount; i++ {
			rows.Next(result)
		}
	}
}
