// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build go1.18

package compute

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/scalar"
)

//go:generate go tool stringer -type=DatumKind -linecomment

// DatumKind is an enum used for denoting which kind of type a datum is encapsulating
type DatumKind int

const (
	KindNone    DatumKind = iota // none
	KindScalar                   // scalar
	KindArray                    // array
	KindChunked                  // chunked_array
	KindRecord                   // record_batch
	KindTable                    // table
)

const UnknownLength int64 = -1

// DatumIsValue returns true if the datum passed is a Scalar, Array
// or ChunkedArray type (e.g. it contains a specific value not a
// group of values)
func DatumIsValue(d Datum) bool {
	switch d.Kind() {
	case KindScalar, KindArray, KindChunked:
		return true
	}
	return false
}

// Datum is a variant interface for wrapping the various Arrow data structures
// for now the various Datum types just hold a Value which is the type they
// are wrapping, but it might make sense in the future for those types
// to actually be aliases or embed their types instead. Not sure yet.
type Datum interface {
	fmt.Stringer
	Kind() DatumKind
	Len() int64
	Equals(Datum) bool
	Release()

	data() any
}

// ArrayLikeDatum is an interface for treating a Datum similarly to an Array,
// so that it is easy to differentiate between Record/Table/Collection and Scalar,
// Array/ChunkedArray for ease of use. Chunks will return an empty slice for Scalar,
// a slice with 1 element for Array, and the slice of chunks for a chunked array.
type ArrayLikeDatum interface {
	Datum
	NullN() int64
	Type() arrow.DataType
	Chunks() []arrow.Array
}

// TableLikeDatum is an interface type for specifying either a RecordBatch or a
// Table as both contain a schema as opposed to just a single data type.
type TableLikeDatum interface {
	Datum
	Schema() *arrow.Schema
}

// EmptyDatum is the null case, a Datum with nothing in it.
type EmptyDatum struct{}

func (EmptyDatum) String() string  { return "nullptr" }
func (EmptyDatum) Kind() DatumKind { return KindNone }
func (EmptyDatum) Len() int64      { return UnknownLength }
func (EmptyDatum) Release()        {}
func (EmptyDatum) Equals(other Datum) bool {
	_, ok := other.(EmptyDatum)
	return ok
}
func (EmptyDatum) data() any { return nil }

// ScalarDatum contains a scalar value
type ScalarDatum struct {
	Value    scalar.Scalar
	borrowed bool
}

func (ScalarDatum) Kind() DatumKind         { return KindScalar }
func (ScalarDatum) Len() int64              { return 1 }
func (ScalarDatum) Chunks() []arrow.Array   { return nil }
func (d *ScalarDatum) Type() arrow.DataType { return d.Value.DataType() }
func (d *ScalarDatum) String() string       { return d.Value.String() }
func (d *ScalarDatum) ToScalar() (scalar.Scalar, error) {
	return d.Value, nil
}
func (d *ScalarDatum) data() any { return d.Value }
func (d *ScalarDatum) NullN() int64 {
	if d.Value.IsValid() {
		return 0
	}
	return 1
}

type releasable interface {
	Release()
}

func (d *ScalarDatum) Release() {
	if d.borrowed {
		return
	}
	if v, ok := d.Value.(releasable); ok {
		v.Release()
	}
}

func (d *ScalarDatum) Equals(other Datum) bool {
	if rhs, ok := other.(*ScalarDatum); ok {
		return scalar.Equals(d.Value, rhs.Value)
	}
	return false
}

// ArrayDatum references an array.Data object which can be used to create
// array instances from if needed.
type ArrayDatum struct {
	Value    arrow.ArrayData
	borrowed bool
}

func (ArrayDatum) Kind() DatumKind           { return KindArray }
func (d *ArrayDatum) Type() arrow.DataType   { return d.Value.DataType() }
func (d *ArrayDatum) Len() int64             { return int64(d.Value.Len()) }
func (d *ArrayDatum) NullN() int64           { return int64(d.Value.NullN()) }
func (d *ArrayDatum) String() string         { return fmt.Sprintf("Array:{%s}", d.Value.DataType()) }
func (d *ArrayDatum) MakeArray() arrow.Array { return array.MakeFromData(d.Value) }
func (d *ArrayDatum) Chunks() []arrow.Array  { return []arrow.Array{d.MakeArray()} }
func (d *ArrayDatum) ToScalar() (scalar.Scalar, error) {
	return scalar.NewListScalarData(d.Value), nil
}
func (d *ArrayDatum) Release() {
	if d.borrowed {
		return
	}
	d.Value.Release()
	d.Value = nil
}
func (d *ArrayDatum) data() any { return d.Value }
func (d *ArrayDatum) Equals(other Datum) bool {
	rhs, ok := other.(*ArrayDatum)
	if !ok {
		return false
	}

	left := d.MakeArray()
	defer left.Release()
	right := rhs.MakeArray()
	defer right.Release()

	return array.Equal(left, right)
}

// ChunkedDatum contains a chunked array for use with expressions and compute.
type ChunkedDatum struct {
	Value    *arrow.Chunked
	borrowed bool
}

func (ChunkedDatum) Kind() DatumKind          { return KindChunked }
func (d *ChunkedDatum) Type() arrow.DataType  { return d.Value.DataType() }
func (d *ChunkedDatum) Len() int64            { return int64(d.Value.Len()) }
func (d *ChunkedDatum) NullN() int64          { return int64(d.Value.NullN()) }
func (d *ChunkedDatum) String() string        { return fmt.Sprintf("Array:{%s}", d.Value.DataType()) }
func (d *ChunkedDatum) Chunks() []arrow.Array { return d.Value.Chunks() }
func (d *ChunkedDatum) data() any             { return d.Value }
func (d *ChunkedDatum) Release() {
	if d.borrowed {
		return
	}
	d.Value.Release()
	d.Value = nil
}

func (d *ChunkedDatum) Equals(other Datum) bool {
	if rhs, ok := other.(*ChunkedDatum); ok {
		return array.ChunkedEqual(d.Value, rhs.Value)
	}
	return false
}

// RecordDatum contains an array.Record for passing a full record to an expression
// or to compute.
type RecordDatum struct {
	Value    arrow.RecordBatch
	borrowed bool
}

func (RecordDatum) Kind() DatumKind          { return KindRecord }
func (RecordDatum) String() string           { return "RecordBatch" }
func (r *RecordDatum) Len() int64            { return r.Value.NumRows() }
func (r *RecordDatum) Schema() *arrow.Schema { return r.Value.Schema() }
func (r *RecordDatum) data() any             { return r.Value }
func (r *RecordDatum) Release() {
	if r.borrowed {
		return
	}
	r.Value.Release()
	r.Value = nil
}

func (r *RecordDatum) Equals(other Datum) bool {
	if rhs, ok := other.(*RecordDatum); ok {
		return array.RecordEqual(r.Value, rhs.Value)
	}
	return false
}

// TableDatum contains a table so that multiple record batches can be worked with
// together as a single table for being passed to compute and expression handling.
type TableDatum struct {
	Value    arrow.Table
	borrowed bool
}

func (TableDatum) Kind() DatumKind          { return KindTable }
func (TableDatum) String() string           { return "Table" }
func (d *TableDatum) Len() int64            { return d.Value.NumRows() }
func (d *TableDatum) Schema() *arrow.Schema { return d.Value.Schema() }
func (d *TableDatum) data() any             { return d.Value }
func (d *TableDatum) Release() {
	if d.borrowed {
		return
	}
	d.Value.Release()
	d.Value = nil
}

func (d *TableDatum) Equals(other Datum) bool {
	if rhs, ok := other.(*TableDatum); ok {
		return array.TableEqual(d.Value, rhs.Value)
	}
	return false
}

// NewDatum will construct the appropriate Datum type based on what is passed in
// as the argument.
//
// An arrow.Array gets an ArrayDatum
// An array.Chunked gets a ChunkedDatum
// An array.Record gets a RecordDatum
// An array.Table gets a TableDatum
// A scalar.Scalar gets a ScalarDatum
//
// Anything else is passed to scalar.MakeScalar and receives a scalar
// datum of that appropriate type.
func NewDatum(value interface{}) Datum {
	switch v := value.(type) {
	case Datum:
		return NewDatum(v.data())
	case arrow.ArrayData:
		v.Retain()
		return &ArrayDatum{Value: v}
	case arrow.Array:
		v.Data().Retain()
		return &ArrayDatum{Value: v.Data()}
	case *arrow.Chunked:
		v.Retain()
		return &ChunkedDatum{Value: v}
	case arrow.RecordBatch:
		v.Retain()
		return &RecordDatum{Value: v}
	case arrow.Table:
		v.Retain()
		return &TableDatum{Value: v}
	case scalar.Releasable:
		v.Retain()
		if sc, ok := v.(scalar.Scalar); ok {
			return &ScalarDatum{Value: sc}
		}
		return &ScalarDatum{Value: scalar.MakeScalar(v)}
	case scalar.Scalar:
		return &ScalarDatum{Value: v}
	default:
		return &ScalarDatum{Value: scalar.MakeScalar(value)}
	}
}

// NewDatumWithoutOwning is like NewDatum but does not call Retain on the
// value it is given. The returned Datum borrows the underlying value — the
// caller is responsible for keeping it alive for as long as the Datum is
// in use. Release on a Datum from NewDatumWithoutOwning is a no-op, so use
// NewDatum when the Datum should own the value.
//
// Example:
//
//	builder := array.NewInt32Builder(memory.DefaultAllocator)
//	builder.AppendValues([]int32{1, 2, 3}, nil)
//	arr := builder.NewArray()
//	defer arr.Release()
//	datum := NewDatumWithoutOwning(arr) // datum borrows arr, arr is still alive
func NewDatumWithoutOwning(value interface{}) Datum {
	switch v := value.(type) {
	case arrow.Array:
		return &ArrayDatum{Value: v.Data(), borrowed: true}
	case arrow.ArrayData:
		return &ArrayDatum{Value: v, borrowed: true}
	case *arrow.Chunked:
		return &ChunkedDatum{Value: v, borrowed: true}
	case arrow.RecordBatch:
		return &RecordDatum{Value: v, borrowed: true}
	case arrow.Table:
		return &TableDatum{Value: v, borrowed: true}
	case scalar.Scalar:
		return &ScalarDatum{Value: v, borrowed: true}
	default:
		// When given a plain Go value (not an Arrow type), we construct
		// a new Scalar via scalar.MakeScalar. Since this scalar is created
		// here with no prior owner, the Datum takes ownership of it
		// (borrowed=false) — Datum.Release() will free it.
		return &ScalarDatum{Value: scalar.MakeScalar(value)}
	}
}

var (
	_ ArrayLikeDatum = (*ScalarDatum)(nil)
	_ ArrayLikeDatum = (*ArrayDatum)(nil)
	_ ArrayLikeDatum = (*ChunkedDatum)(nil)
	_ TableLikeDatum = (*RecordDatum)(nil)
	_ TableLikeDatum = (*TableDatum)(nil)
)
