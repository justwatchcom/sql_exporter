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
	"context"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/compute/exec"
	"github.com/apache/arrow-go/v18/arrow/compute/internal/kernels"
)

type (
	// ScalarAggregateOptions controls the general behaviour of the scalar
	// aggregate functions: whether nulls are skipped and how many values
	// have to be seen for a result to be produced rather than a null.
	//
	// The zero value means SkipNulls false and MinCount zero, which is not
	// the default behaviour of the functions; see
	// DefaultScalarAggregateOptions.
	ScalarAggregateOptions = kernels.ScalarAggregateOptions
	// CountOptions controls which values the count function counts.
	CountOptions = kernels.CountOptions
	// CountMode is the enum of the values CountOptions.Mode can take.
	CountMode = kernels.CountMode
)

const (
	// CountOnlyValid counts only non-null values.
	CountOnlyValid = kernels.CountOnlyValid
	// CountOnlyNull counts only null values.
	CountOnlyNull = kernels.CountOnlyNull
	// CountAllRows counts both non-null and null values.
	CountAllRows = kernels.CountAllRows
)

// DefaultScalarAggregateOptions returns the options which the scalar
// aggregate functions use when they are called without options: nulls are
// skipped and one non-null value is enough to produce a result.
func DefaultScalarAggregateOptions() *ScalarAggregateOptions {
	return kernels.DefaultScalarAggregateOptions()
}

// DefaultCountOptions returns the options which the count function uses when
// it is called without options, which is to count only non-null values.
func DefaultCountOptions() *CountOptions { return kernels.DefaultCountOptions() }

var (
	countDoc = FunctionDoc{
		Summary: "Count the number of values",
		Description: "By default, only non-null values are counted.\n" +
			"This can be changed through the mode option.",
		ArgNames:    []string{"array"},
		OptionsType: "CountOptions",
	}

	sumDoc = FunctionDoc{
		Summary: "Compute the sum of a numeric array",
		Description: "Null values are ignored by default. If the skip_nulls\n" +
			"option is set to false, then a null is emitted as soon as one of\n" +
			"the values is null. A null is also emitted when fewer than\n" +
			"min_count non-null values are seen, which by default means that\n" +
			"an empty or an all-null input produces a null.",
		ArgNames:    []string{"array"},
		OptionsType: "ScalarAggregateOptions",
	}
)

// sumKernelTypes lists the input types the sum function accepts along with
// the type it accumulates them into, following the C++ implementation: the
// signed integers widen to int64, the unsigned ones and booleans to uint64,
// and the floats to float64. Null input sums as int64 so that a sum over a
// column of unknown type still has a numeric type.
var sumKernelTypes = []struct {
	in  arrow.DataType
	out arrow.DataType
}{
	{arrow.PrimitiveTypes.Int8, arrow.PrimitiveTypes.Int64},
	{arrow.PrimitiveTypes.Int16, arrow.PrimitiveTypes.Int64},
	{arrow.PrimitiveTypes.Int32, arrow.PrimitiveTypes.Int64},
	{arrow.PrimitiveTypes.Int64, arrow.PrimitiveTypes.Int64},
	{arrow.PrimitiveTypes.Uint8, arrow.PrimitiveTypes.Uint64},
	{arrow.PrimitiveTypes.Uint16, arrow.PrimitiveTypes.Uint64},
	{arrow.PrimitiveTypes.Uint32, arrow.PrimitiveTypes.Uint64},
	{arrow.PrimitiveTypes.Uint64, arrow.PrimitiveTypes.Uint64},
	{arrow.PrimitiveTypes.Float32, arrow.PrimitiveTypes.Float64},
	{arrow.PrimitiveTypes.Float64, arrow.PrimitiveTypes.Float64},
	{arrow.FixedWidthTypes.Boolean, arrow.PrimitiveTypes.Uint64},
	{arrow.Null, arrow.PrimitiveTypes.Int64},
}

// RegisterScalarAggregates registers the scalar aggregate functions, which
// compute a single summary value from array input.
func RegisterScalarAggregates(reg FunctionRegistry) {
	countFn := NewScalarAggregateFunction("count", Unary(), countDoc)
	countFn.SetDefaultOptions(DefaultCountOptions())
	// count is registered for any input type: it only ever looks at the
	// validity of the values, never at the values themselves. CountInit
	// rejects the modes that need a logical null count for the types whose
	// validity bitmap does not carry it.
	if err := countFn.AddNewKernel([]exec.InputType{{}}, exec.NewOutputType(arrow.PrimitiveTypes.Int64),
		kernels.CountInit, kernels.AggregateConsume, kernels.AggregateMerge, kernels.AggregateFinalize); err != nil {
		panic(err)
	}
	reg.AddFunction(countFn, false)

	sumFn := NewScalarAggregateFunction("sum", Unary(), sumDoc)
	sumFn.SetDefaultOptions(DefaultScalarAggregateOptions())
	for _, kt := range sumKernelTypes {
		if err := sumFn.AddNewKernel([]exec.InputType{exec.NewExactInput(kt.in)}, exec.NewOutputType(kt.out),
			kernels.SumInit, kernels.AggregateConsume, kernels.AggregateMerge, kernels.AggregateFinalize); err != nil {
			panic(err)
		}
	}
	reg.AddFunction(sumFn, false)
}

// Count returns the number of values in the input, counting either the
// non-null values, the null values or every row depending on the mode in the
// provided options.
func Count(ctx context.Context, opts CountOptions, value Datum) (Datum, error) {
	return CallFunction(ctx, "count", &opts, value)
}

// Sum returns the sum of the values in the input, as an int64 for signed
// integer and null input, a uint64 for unsigned integer and boolean input and
// a float64 for floating point input. The sum of an integer input which does
// not fit its output type wraps around.
func Sum(ctx context.Context, opts ScalarAggregateOptions, value Datum) (Datum, error) {
	return CallFunction(ctx, "sum", &opts, value)
}
