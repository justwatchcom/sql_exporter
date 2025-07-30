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
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/compute/exec"
	"github.com/apache/arrow-go/v18/arrow/compute/internal/kernels"
	"github.com/apache/arrow-go/v18/arrow/scalar"
)

type compareFunction struct {
	ScalarFunction
}

func (fn *compareFunction) Execute(ctx context.Context, opt FunctionOptions, args ...Datum) (Datum, error) {
	return execInternal(ctx, fn, opt, -1, args...)
}

func (fn *compareFunction) DispatchBest(vals ...arrow.DataType) (exec.Kernel, error) {
	if err := fn.checkArity(len(vals)); err != nil {
		return nil, err
	}

	if hasDecimal(vals...) {
		if err := castBinaryDecimalArgs(decPromoteAdd, vals...); err != nil {
			return nil, err
		}
	}

	if kn, err := fn.DispatchExact(vals...); err == nil {
		return kn, nil
	}

	ensureDictionaryDecoded(vals...)
	replaceNullWithOtherType(vals...)

	if dt := commonNumeric(vals...); dt != nil {
		replaceTypes(dt, vals...)
	} else if dt := commonTemporal(vals...); dt != nil {
		replaceTypes(dt, vals...)
	} else if dt := commonBinary(vals...); dt != nil {
		replaceTypes(dt, vals...)
	}

	return fn.DispatchExact(vals...)
}

type flippedData struct {
	*kernels.CompareData

	unflippedExec exec.ArrayKernelExec
}

func flippedCompare(ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
	kn := ctx.Kernel.(*exec.ScalarKernel)
	knData := kn.Data.(*flippedData)

	flippedBatch := exec.ExecSpan{
		Len:    batch.Len,
		Values: []exec.ExecValue{batch.Values[1], batch.Values[0]},
	}
	return knData.unflippedExec(ctx, &flippedBatch, out)
}

func makeFlippedCompare(name string, fn *compareFunction, doc FunctionDoc) *compareFunction {
	flipped := &compareFunction{*NewScalarFunction(name, Binary(), doc)}
	for _, k := range fn.kernels {
		flippedKernel := k
		if k.Data != nil {
			cmpData := k.Data.(*kernels.CompareData)
			flippedKernel.Data = &flippedData{CompareData: cmpData,
				unflippedExec: k.ExecFn}
		} else {
			flippedKernel.Data = &flippedData{unflippedExec: k.ExecFn}
		}
		flippedKernel.ExecFn = flippedCompare
		flipped.AddKernel(flippedKernel)
	}
	return flipped
}

func RegisterScalarComparisons(reg FunctionRegistry) {
	eqFn := &compareFunction{*NewScalarFunction("equal", Binary(), EmptyFuncDoc)}
	for _, k := range kernels.CompareKernels(kernels.CmpEQ) {
		if err := eqFn.AddKernel(k); err != nil {
			panic(err)
		}
	}
	reg.AddFunction(eqFn, false)

	neqFn := &compareFunction{*NewScalarFunction("not_equal", Binary(), EmptyFuncDoc)}
	for _, k := range kernels.CompareKernels(kernels.CmpNE) {
		if err := neqFn.AddKernel(k); err != nil {
			panic(err)
		}
	}
	reg.AddFunction(neqFn, false)

	gtFn := &compareFunction{*NewScalarFunction("greater", Binary(), EmptyFuncDoc)}
	for _, k := range kernels.CompareKernels(kernels.CmpGT) {
		if err := gtFn.AddKernel(k); err != nil {
			panic(err)
		}
	}
	reg.AddFunction(gtFn, false)

	gteFn := &compareFunction{*NewScalarFunction("greater_equal", Binary(), EmptyFuncDoc)}
	for _, k := range kernels.CompareKernels(kernels.CmpGE) {
		if err := gteFn.AddKernel(k); err != nil {
			panic(err)
		}
	}
	reg.AddFunction(gteFn, false)

	ltFn := makeFlippedCompare("less", gtFn, EmptyFuncDoc)
	reg.AddFunction(ltFn, false)
	lteFn := makeFlippedCompare("less_equal", gteFn, EmptyFuncDoc)
	reg.AddFunction(lteFn, false)

	isOrNotNullKns := kernels.IsNullNotNullKernels()
	isNullFn := &compareFunction{*NewScalarFunction("is_null", Unary(), EmptyFuncDoc)}
	if err := isNullFn.AddKernel(isOrNotNullKns[0]); err != nil {
		panic(err)
	}

	isNotNullFn := &compareFunction{*NewScalarFunction("is_not_null", Unary(), EmptyFuncDoc)}
	if err := isNotNullFn.AddKernel(isOrNotNullKns[1]); err != nil {
		panic(err)
	}

	reg.AddFunction(isNullFn, false)
	reg.AddFunction(isNotNullFn, false)

	reg.AddFunction(NewMetaFunction("is_nan", Unary(), EmptyFuncDoc,
		func(ctx context.Context, opts FunctionOptions, args ...Datum) (Datum, error) {
			type hasType interface {
				Type() arrow.DataType
			}

			// only Scalar, Array and ChunkedArray have a Type method
			arg, ok := args[0].(hasType)
			if !ok {
				// don't support Table/Record/None kinds
				return nil, fmt.Errorf("%w: unsupported type for is_nan %s",
					arrow.ErrNotImplemented, args[0])
			}

			switch arg.Type() {
			case arrow.PrimitiveTypes.Float32, arrow.PrimitiveTypes.Float64:
				return CallFunction(ctx, "not_equal", nil, args[0], args[0])
			default:
				if arg, ok := args[0].(ArrayLikeDatum); ok {
					result, err := scalar.MakeArrayFromScalar(scalar.NewBooleanScalar(false),
						int(arg.Len()), GetAllocator(ctx))
					if err != nil {
						return nil, err
					}
					return NewDatumWithoutOwning(result), nil
				}

				return NewDatum(false), nil
			}
		}), false)
}
