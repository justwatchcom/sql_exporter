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

package kernels

import (
	"bytes"
	"fmt"
	"sort"
	"unsafe"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/bitutil"
	"github.com/apache/arrow-go/v18/arrow/compute/exec"
	"github.com/apache/arrow-go/v18/arrow/decimal128"
	"github.com/apache/arrow-go/v18/arrow/decimal256"
	"github.com/apache/arrow-go/v18/arrow/float16"
	"github.com/apache/arrow-go/v18/arrow/internal/debug"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/arrow/scalar"
	"github.com/apache/arrow-go/v18/internal/bitutils"
)

type binaryKernel func(left, right, out []byte, offset int)

type (
	cmpFn[LeftT, RightT arrow.FixedWidthType]          func([]LeftT, []RightT, []uint32)
	cmpScalarLeft[LeftT, RightT arrow.FixedWidthType]  func(LeftT, []RightT, []uint32)
	cmpScalarRight[LeftT, RightT arrow.FixedWidthType] func([]LeftT, RightT, []uint32)
)

type cmpOp[T arrow.FixedWidthType] struct {
	arrArr    cmpFn[T, T]
	arrScalar cmpScalarRight[T, T]
	scalarArr cmpScalarLeft[T, T]
}

func comparePrimitiveArrayArray[T arrow.FixedWidthType](op cmpFn[T, T]) binaryKernel {
	return func(leftBytes, rightBytes, out []byte, offset int) {
		const batchSize = 32
		var (
			left      = arrow.GetData[T](leftBytes)
			right     = arrow.GetData[T](rightBytes)
			nvals     = len(left)
			tmpOutput [batchSize]uint32
		)

		tmpOutSlice := tmpOutput[:]
		if prefix := offset % 8; prefix != 0 {
			vals := min(nvals, 8-prefix)
			op(left[:vals], right[:vals], tmpOutSlice[:vals])
			left, right = left[vals:], right[vals:]
			nvals -= vals

			for i, v := range tmpOutSlice[:vals] {
				bitutil.SetBitTo(out, prefix+i, v != 0)
			}
			if nvals == 0 {
				return
			}
			out = out[1:]
		}

		nbatches := nvals / batchSize
		for j := 0; j < nbatches; j++ {
			op(left[:batchSize], right[:batchSize], tmpOutSlice)
			left, right = left[batchSize:], right[batchSize:]
			packBits(tmpOutput, out)
			out = out[batchSize/8:]
		}

		remaining := nvals - (batchSize * nbatches)
		op(left, right, tmpOutput[:remaining])
		for bitIndex, v := range tmpOutput[:remaining] {
			bitutil.SetBitTo(out, bitIndex, v != 0)
		}
	}
}

func comparePrimitiveArrayScalar[T arrow.FixedWidthType](op cmpScalarRight[T, T]) binaryKernel {
	return func(leftBytes, rightBytes, out []byte, offset int) {
		const batchSize = 32
		var (
			left      = arrow.GetData[T](leftBytes)
			rightVal  = *(*T)(unsafe.Pointer(&rightBytes[0]))
			nvals     = len(left)
			tmpOutput [batchSize]uint32
		)

		tmpOutSlice := tmpOutput[:]
		if prefix := offset % 8; prefix != 0 {
			vals := min(nvals, 8-prefix)
			op(left[:vals], rightVal, tmpOutSlice[:vals])
			left = left[vals:]
			nvals -= vals

			for i, v := range tmpOutSlice[:vals] {
				bitutil.SetBitTo(out, prefix+i, v != 0)
			}
			if nvals == 0 {
				return
			}
			out = out[1:]
		}

		nbatches := nvals / batchSize
		for j := 0; j < nbatches; j++ {
			op(left[:batchSize], rightVal, tmpOutSlice)
			left = left[batchSize:]
			packBits(tmpOutput, out)
			out = out[batchSize/8:]
		}

		remaining := nvals - (batchSize * nbatches)
		op(left, rightVal, tmpOutput[:remaining])
		for bitIndex, v := range tmpOutput[:remaining] {
			bitutil.SetBitTo(out, bitIndex, v != 0)
		}
	}
}

func comparePrimitiveScalarArray[T arrow.FixedWidthType](op cmpScalarLeft[T, T]) binaryKernel {
	return func(leftBytes, rightBytes, out []byte, offset int) {
		const batchSize = 32
		var (
			leftVal = *(*T)(unsafe.Pointer(&leftBytes[0]))
			right   = arrow.GetData[T](rightBytes)

			nvals     = len(right)
			tmpOutput [batchSize]uint32
		)

		tmpOutSlice := tmpOutput[:]
		if prefix := offset % 8; prefix != 0 {
			vals := min(nvals, 8-prefix)
			op(leftVal, right[:vals], tmpOutSlice[:vals])
			right = right[vals:]
			nvals -= vals

			for i, v := range tmpOutSlice[:vals] {
				bitutil.SetBitTo(out, prefix+i, v != 0)
			}
			if nvals == 0 {
				return
			}
			out = out[1:]
		}

		nbatches := nvals / batchSize
		for j := 0; j < nbatches; j++ {
			op(leftVal, right[:batchSize], tmpOutSlice)
			right = right[batchSize:]
			packBits(tmpOutput, out)
			out = out[batchSize/8:]
		}

		remaining := nvals - (batchSize * nbatches)
		op(leftVal, right, tmpOutput[:remaining])
		for bitIndex, v := range tmpOutput[:remaining] {
			bitutil.SetBitTo(out, bitIndex, v != 0)
		}
	}
}

type CompareData struct {
	funcAA, funcSA, funcAS binaryKernel
}

func (c *CompareData) Funcs() *CompareData { return c }

type CompareFuncData interface {
	Funcs() *CompareData
}

func getOffsetSpanBytes(span *exec.ArraySpan) []byte {
	if len(span.Buffers[1].Buf) == 0 {
		return nil
	}

	buf := span.Buffers[1].Buf
	byteWidth := int64(span.Type.(arrow.FixedWidthDataType).Bytes())
	start := span.Offset * byteWidth
	return buf[start : start+(span.Len*byteWidth)]
}

func compareKernel[T arrow.FixedWidthType](ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
	kn := ctx.Kernel.(*exec.ScalarKernel)
	knData := kn.Data.(CompareFuncData).Funcs()

	outPrefix := int(out.Offset % 8)
	outBuf := out.Buffers[1].Buf[out.Offset/8:]

	if batch.Values[0].IsArray() && batch.Values[1].IsArray() {
		knData.funcAA(getOffsetSpanBytes(&batch.Values[0].Array),
			getOffsetSpanBytes(&batch.Values[1].Array), outBuf, outPrefix)
	} else if batch.Values[1].IsScalar() {
		knData.funcAS(getOffsetSpanBytes(&batch.Values[0].Array),
			batch.Values[1].Scalar.(scalar.PrimitiveScalar).Data(), outBuf, outPrefix)
	} else {
		knData.funcSA(batch.Values[0].Scalar.(scalar.PrimitiveScalar).Data(),
			getOffsetSpanBytes(&batch.Values[1].Array), outBuf, outPrefix)
	}

	return nil
}

func genGoCompareKernel[T arrow.FixedWidthType](op *cmpOp[T]) *CompareData {
	return &CompareData{
		funcAA: comparePrimitiveArrayArray(op.arrArr),
		funcAS: comparePrimitiveArrayScalar(op.arrScalar),
		funcSA: comparePrimitiveScalarArray(op.scalarArr),
	}
}

type decCmp[T decimal128.Num | decimal256.Num] struct {
	Gt func(T, T) bool
	Ge func(T, T) bool
}

var dec128Cmp = decCmp[decimal128.Num]{
	Gt: func(a, b decimal128.Num) bool { return a.Greater(b) },
	Ge: func(a, b decimal128.Num) bool { return a.GreaterEqual(b) },
}

var dec256Cmp = decCmp[decimal256.Num]{
	Gt: func(a, b decimal256.Num) bool { return a.Greater(b) },
	Ge: func(a, b decimal256.Num) bool { return a.GreaterEqual(b) },
}

func getCmpDec[T decimal128.Num | decimal256.Num](op CompareOperator, fns decCmp[T]) *cmpOp[T] {
	switch op {
	case CmpEQ:
		return &cmpOp[T]{
			arrArr: func(lt, rt []T, u []uint32) {
				for i := range lt {
					if lt[i] == rt[i] {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			arrScalar: func(lt []T, rt T, u []uint32) {
				for i := range lt {
					if lt[i] == rt {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			scalarArr: func(lt T, rt []T, u []uint32) {
				for i := range rt {
					if lt == rt[i] {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
		}
	case CmpNE:
		return &cmpOp[T]{
			arrArr: func(lt, rt []T, u []uint32) {
				for i := range lt {
					if lt[i] != rt[i] {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			arrScalar: func(lt []T, rt T, u []uint32) {
				for i := range lt {
					if lt[i] != rt {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			scalarArr: func(lt T, rt []T, u []uint32) {
				for i := range rt {
					if lt != rt[i] {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
		}
	case CmpGT:
		return &cmpOp[T]{
			arrArr: func(lt, rt []T, u []uint32) {
				for i := range lt {
					if fns.Gt(lt[i], rt[i]) {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			arrScalar: func(lt []T, rt T, u []uint32) {
				for i := range lt {
					if fns.Gt(lt[i], rt) {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			scalarArr: func(lt T, rt []T, u []uint32) {
				for i := range rt {
					if fns.Gt(lt, rt[i]) {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
		}
	case CmpGE:
		return &cmpOp[T]{
			arrArr: func(lt, rt []T, u []uint32) {
				for i := range lt {
					if fns.Ge(lt[i], rt[i]) {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			arrScalar: func(lt []T, rt T, u []uint32) {
				for i := range lt {
					if fns.Ge(lt[i], rt) {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			scalarArr: func(lt T, rt []T, u []uint32) {
				for i := range rt {
					if fns.Ge(lt, rt[i]) {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
		}
	}
	debug.Assert(false, "")
	return nil
}

func genDecimalCompareKernel[T decimal128.Num | decimal256.Num](op CompareOperator) (ex exec.ArrayKernelExec, data exec.KernelState) {
	ex = compareKernel[T]

	var def T
	switch any(def).(type) {
	case decimal128.Num:
		cmp := getCmpDec(op, dec128Cmp)
		data = &CompareData{
			funcAA: comparePrimitiveArrayArray(cmp.arrArr),
			funcAS: comparePrimitiveArrayScalar(cmp.arrScalar),
			funcSA: comparePrimitiveScalarArray(cmp.scalarArr),
		}
	case decimal256.Num:
		cmp := getCmpDec(op, dec256Cmp)
		data = &CompareData{
			funcAA: comparePrimitiveArrayArray(cmp.arrArr),
			funcAS: comparePrimitiveArrayScalar(cmp.arrScalar),
			funcSA: comparePrimitiveScalarArray(cmp.scalarArr),
		}
	}

	return
}

func getCmpOp[T arrow.NumericType](op CompareOperator) *cmpOp[T] {
	switch op {
	case CmpEQ:
		return &cmpOp[T]{
			arrArr: func(lt, rt []T, u []uint32) {
				for i := range u {
					if lt[i] == rt[i] {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			arrScalar: func(lt []T, rt T, u []uint32) {
				for i := range u {
					if lt[i] == rt {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			scalarArr: func(lt T, rt []T, u []uint32) {
				for i := range u {
					if lt == rt[i] {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
		}
	case CmpNE:
		return &cmpOp[T]{
			arrArr: func(lt, rt []T, u []uint32) {
				for i := range u {
					if lt[i] != rt[i] {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			arrScalar: func(lt []T, rt T, u []uint32) {
				for i := range u {
					if lt[i] != rt {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			scalarArr: func(lt T, rt []T, u []uint32) {
				for i := range u {
					if lt != rt[i] {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
		}
	case CmpGT:
		return &cmpOp[T]{
			arrArr: func(lt, rt []T, u []uint32) {
				for i := range u {
					if lt[i] > rt[i] {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			arrScalar: func(lt []T, rt T, u []uint32) {
				for i := range u {
					if lt[i] > rt {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			scalarArr: func(lt T, rt []T, u []uint32) {
				for i := range u {
					if lt > rt[i] {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
		}
	case CmpGE:
		return &cmpOp[T]{
			arrArr: func(lt, rt []T, u []uint32) {
				for i := range u {
					if lt[i] >= rt[i] {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			arrScalar: func(lt []T, rt T, u []uint32) {
				for i := range u {
					if lt[i] >= rt {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
			scalarArr: func(lt T, rt []T, u []uint32) {
				for i := range u {
					if lt >= rt[i] {
						u[i] = 1
					} else {
						u[i] = 0
					}
				}
			},
		}
	}
	return nil
}

func getBinaryCmp(op CompareOperator) binaryBinOp[bool] {
	switch op {
	case CmpEQ:
		return func(_ *exec.KernelCtx, arg0, arg1 []byte) bool {
			return bytes.Equal(arg0, arg1)
		}
	case CmpNE:
		return func(_ *exec.KernelCtx, arg0, arg1 []byte) bool {
			return !bytes.Equal(arg0, arg1)
		}
	case CmpGT:
		return func(_ *exec.KernelCtx, arg0, arg1 []byte) bool {
			return bytes.Compare(arg0, arg1) == 1
		}
	case CmpGE:
		return func(_ *exec.KernelCtx, arg0, arg1 []byte) bool {
			return bytes.Compare(arg0, arg1) != -1
		}
	}
	return nil
}

func numericCompareKernel[T arrow.NumericType](ty exec.InputType, op CompareOperator) (kn exec.ScalarKernel) {
	ex := compareKernel[T]
	kn = exec.NewScalarKernelWithSig(&exec.KernelSignature{
		InputTypes: []exec.InputType{ty, ty},
		OutType:    exec.NewOutputType(arrow.FixedWidthTypes.Boolean),
	}, ex, nil)
	kn.Data = genCompareKernel[T](op)
	return
}

func decimalCompareKernel[T decimal128.Num | decimal256.Num](ty exec.InputType, op CompareOperator) (kn exec.ScalarKernel) {
	ex, data := genDecimalCompareKernel[T](op)
	kn = exec.NewScalarKernelWithSig(&exec.KernelSignature{
		InputTypes: []exec.InputType{ty, ty},
		OutType:    exec.NewOutputType(arrow.FixedWidthTypes.Boolean),
	}, ex, nil)
	kn.Data = data
	return
}

func GetCompareKernel(ty exec.InputType, cmpType arrow.Type, op CompareOperator) exec.ScalarKernel {
	switch cmpType {
	case arrow.INT8:
		return numericCompareKernel[int8](ty, op)
	case arrow.INT16:
		return numericCompareKernel[int16](ty, op)
	case arrow.INT32, arrow.DATE32, arrow.TIME32:
		return numericCompareKernel[int32](ty, op)
	case arrow.INT64, arrow.DATE64, arrow.TIMESTAMP, arrow.TIME64, arrow.DURATION:
		return numericCompareKernel[int64](ty, op)
	case arrow.UINT8:
		return numericCompareKernel[uint8](ty, op)
	case arrow.UINT16:
		return numericCompareKernel[uint16](ty, op)
	case arrow.UINT32:
		return numericCompareKernel[uint32](ty, op)
	case arrow.UINT64:
		return numericCompareKernel[uint64](ty, op)
	case arrow.FLOAT32:
		return numericCompareKernel[float32](ty, op)
	case arrow.FLOAT64:
		return numericCompareKernel[float64](ty, op)
	}
	debug.Assert(false, "")
	return exec.ScalarKernel{}
}

func compareTimestampKernel(ty exec.InputType, op CompareOperator) exec.ScalarKernel {
	kn := GetCompareKernel(ty, arrow.TIMESTAMP, op)
	ex := kn.ExecFn
	kn.ExecFn = func(ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
		lhs, rhs := batch.Values[0].Type().(*arrow.TimestampType), batch.Values[1].Type().(*arrow.TimestampType)
		if (len(lhs.TimeZone) == 0) != (len(rhs.TimeZone) == 0) {
			return fmt.Errorf("%w: cannot compare timestamp with timezone to timestamp without timezone, got: %s and %s",
				arrow.ErrInvalid, lhs, rhs)
		}
		return ex(ctx, batch, out)
	}
	return kn
}

var (
	boolEQ = binaryBoolOps{
		arrArr: func(_ *exec.KernelCtx, lhs, rhs, out bitutil.Bitmap) error {
			bitutil.BitmapXnor(lhs.Data, rhs.Data, lhs.Offset, rhs.Offset, out.Data, out.Offset, out.Len)
			return nil
		},
		arrScalar: func(_ *exec.KernelCtx, lhs bitutil.Bitmap, rhs bool, out bitutil.Bitmap) error {
			rdr := bitutil.NewBitmapReader(lhs.Data, int(lhs.Offset), int(lhs.Len))
			bitutils.GenerateBitsUnrolled(out.Data, out.Offset, out.Len, func() (out bool) {
				out = rdr.Set() == rhs
				rdr.Next()
				return
			})
			return nil
		},
		scalarArr: func(_ *exec.KernelCtx, lhs bool, rhs, out bitutil.Bitmap) error {
			rdr := bitutil.NewBitmapReader(rhs.Data, int(rhs.Offset), int(rhs.Len))
			bitutils.GenerateBitsUnrolled(out.Data, out.Offset, out.Len, func() (out bool) {
				out = lhs == rdr.Set()
				rdr.Next()
				return
			})
			return nil
		},
	}
	boolNE = binaryBoolOps{
		arrArr: func(_ *exec.KernelCtx, lhs, rhs, out bitutil.Bitmap) error {
			bitutil.BitmapXor(lhs.Data, rhs.Data, lhs.Offset, rhs.Offset, out.Data, out.Offset, out.Len)
			return nil
		},
		arrScalar: func(_ *exec.KernelCtx, lhs bitutil.Bitmap, rhs bool, out bitutil.Bitmap) error {
			rdr := bitutil.NewBitmapReader(lhs.Data, int(lhs.Offset), int(lhs.Len))
			bitutils.GenerateBitsUnrolled(out.Data, out.Offset, out.Len, func() (out bool) {
				out = rdr.Set() != rhs
				rdr.Next()
				return
			})
			return nil
		},
		scalarArr: func(_ *exec.KernelCtx, lhs bool, rhs, out bitutil.Bitmap) error {
			rdr := bitutil.NewBitmapReader(rhs.Data, int(rhs.Offset), int(rhs.Len))
			bitutils.GenerateBitsUnrolled(out.Data, out.Offset, out.Len, func() (out bool) {
				out = lhs != rdr.Set()
				rdr.Next()
				return
			})
			return nil
		},
	}
)

func CompareKernels(op CompareOperator) []exec.ScalarKernel {
	kns := make([]exec.ScalarKernel, 0)

	outType := exec.NewOutputType(arrow.FixedWidthTypes.Boolean)
	switch op {
	case CmpEQ:
		in := exec.NewExactInput(arrow.FixedWidthTypes.Boolean)
		kns = append(kns, exec.NewScalarKernel([]exec.InputType{in, in}, outType,
			ScalarBinaryBools(&boolEQ), nil))
	case CmpNE:
		in := exec.NewExactInput(arrow.FixedWidthTypes.Boolean)
		kns = append(kns, exec.NewScalarKernel([]exec.InputType{in, in}, outType,
			ScalarBinaryBools(&boolNE), nil))
	}

	for _, ty := range numericTypes {
		in := exec.NewExactInput(ty)
		kns = append(kns, GetCompareKernel(in, ty.ID(), op))
	}
	kns = append(kns,
		GetCompareKernel(exec.NewExactInput(arrow.FixedWidthTypes.Date32), arrow.DATE32, op),
		GetCompareKernel(exec.NewExactInput(arrow.FixedWidthTypes.Date64), arrow.DATE64, op))

	for _, unit := range arrow.TimeUnitValues {
		in := exec.NewMatchedInput(exec.TimestampTypeUnit(unit))
		kns = append(kns, compareTimestampKernel(in, op))

		in = exec.NewMatchedInput(exec.DurationTypeUnit(unit))
		kns = append(kns, GetCompareKernel(in, arrow.INT64, op))
	}

	for _, unit := range []arrow.TimeUnit{arrow.Second, arrow.Millisecond} {
		in := exec.NewMatchedInput(exec.Time32TypeUnit(unit))
		kns = append(kns, GetCompareKernel(in, arrow.INT32, op))
	}
	for _, unit := range []arrow.TimeUnit{arrow.Microsecond, arrow.Nanosecond} {
		in := exec.NewMatchedInput(exec.Time64TypeUnit(unit))
		kns = append(kns, GetCompareKernel(in, arrow.INT64, op))
	}

	for _, ty := range baseBinaryTypes {
		var ex exec.ArrayKernelExec
		switch ty.Layout().Buffers[1].ByteWidth {
		case 4:
			ex = ScalarBinaryBinaryArgsBoolOut(exec.NewVarBinaryIter[int32], getBinaryCmp(op))
		default:
			ex = ScalarBinaryBinaryArgsBoolOut(exec.NewVarBinaryIter[int64], getBinaryCmp(op))
		}
		in := exec.NewExactInput(ty)
		kns = append(kns, exec.NewScalarKernel([]exec.InputType{in, in},
			outType, ex, nil))
	}

	in128, in256 := exec.NewIDInput(arrow.DECIMAL128), exec.NewIDInput(arrow.DECIMAL256)
	kns = append(kns, decimalCompareKernel[decimal128.Num](in128, op),
		decimalCompareKernel[decimal256.Num](in256, op))

	inFSB := exec.NewIDInput(arrow.FIXED_SIZE_BINARY)
	kns = append(kns, exec.NewScalarKernel([]exec.InputType{inFSB, inFSB}, outType,
		ScalarBinaryBinaryArgsBoolOut(exec.NewFSBIter, getBinaryCmp(op)), nil))

	return kns
}

func dictHasNullValues(input *exec.ArraySpan) bool {
	if input.Type.ID() != arrow.DICTIONARY {
		return false
	}
	dict := input.Dictionary()
	return dict.MayHaveNulls() || dict.Type.ID() == arrow.NULL
}

// dictValidity writes a bitmap to out, starting at bit 0, with a bit set for
// each slot that is logically null when wantNull is true, or logically valid
// otherwise. A slot is null if its index is null or refers to a null
// dictionary value, or to a NaN floating-point value when nanIsNull is true.
func dictValidity(ctx *exec.KernelCtx, input *exec.ArraySpan, out *exec.ExecResult, wantNull, nanIsNull bool) error {
	var set func(*exec.ArraySpan, []byte, int64, []byte, bool)
	switch input.Type.(*arrow.DictionaryType).IndexType.ID() {
	case arrow.INT8, arrow.UINT8:
		set = setDictBits[uint8]
	case arrow.INT16, arrow.UINT16:
		set = setDictBits[uint16]
	case arrow.INT32, arrow.UINT32:
		set = setDictBits[uint32]
	case arrow.INT64, arrow.UINT64:
		set = setDictBits[uint64]
	default:
		return fmt.Errorf("%w: unsupported dictionary index type", arrow.ErrInvalid)
	}

	dict := input.Dictionary()
	valueValidity, valueOffset := dict.Buffers[0].Buf, dict.Offset
	if nanIsNull && arrow.IsFloating(dict.Type.ID()) {
		buf := ctx.AllocateBitmap(dict.Len)
		defer buf.Release()
		valueValidity, valueOffset = buf.Bytes(), 0
		if dict.Buffers[0].Buf != nil {
			bitutil.CopyBitmap(dict.Buffers[0].Buf, int(dict.Offset), int(dict.Len), valueValidity, 0)
		} else {
			bitutil.SetBitsTo(valueValidity, 0, dict.Len, true)
		}
		setNaNBits(dict, valueValidity, false)
	}

	out.Buffers[1].WrapBuffer(ctx.AllocateBitmap(input.Len))
	set(input, valueValidity, valueOffset, out.Buffers[1].Buf, wantNull)
	return nil
}

func setDictBits[T arrow.UintType](input *exec.ArraySpan, valueValidity []byte, valueOffset int64, out []byte, wantNull bool) {
	indexValidity := input.Buffers[0].Buf
	for i, idx := range exec.GetSpanValues[T](input, 1) {
		// Signed indices are read as unsigned. Only a valid slot's index must
		// be in range, so check index validity first. Null-type values have no
		// validity bitmap and are all null.
		valid := (indexValidity == nil || bitutil.BitIsSet(indexValidity, int(input.Offset)+i)) &&
			valueValidity != nil && bitutil.BitIsSet(valueValidity, int(valueOffset)+int(idx))
		bitutil.SetBitTo(out, i, valid != wantNull)
	}
}

// hasLogicalNulls reports whether input's nulls are not described by a
// validity bitmap, as for null, union and run-end encoded types.
func hasLogicalNulls(input *exec.ArraySpan) bool {
	// a run-end encoded scalar promoted to an array has a validity bitmap and
	// no children
	if input.Buffers[0].Buf != nil {
		return false
	}
	switch input.Type.ID() {
	case arrow.NULL, arrow.SPARSE_UNION, arrow.DENSE_UNION, arrow.RUN_END_ENCODED:
		return true
	}
	return false
}

// logicalValidity writes a bitmap to out like dictValidity, for the types
// accepted by hasLogicalNulls. It follows Arrow C++'s SetLogicalNullBits: a
// union slot takes the validity of its child slot, and a run-end encoded slot
// takes the validity bitmap of its value.
func logicalValidity(ctx *exec.KernelCtx, input *exec.ArraySpan, out *exec.ExecResult, wantNull bool) error {
	out.Buffers[1].WrapBuffer(ctx.AllocateBitmap(input.Len))
	bits := out.Buffers[1].Buf
	switch input.Type.ID() {
	case arrow.NULL:
		bitutil.SetBitsTo(bits, 0, input.Len, wantNull)
	case arrow.RUN_END_ENCODED:
		values := &input.Children[1]
		run := findRun(input, 0)
		for pos := int64(0); pos < input.Len; run++ {
			end := min(runEnd(input, run)-input.Offset, input.Len)
			valid := values.Buffers[0].Buf == nil ||
				bitutil.BitIsSet(values.Buffers[0].Buf, int(values.Offset)+run)
			bitutil.SetBitsTo(bits, pos, end-pos, valid != wantNull)
			pos = end
		}
	default:
		for i := int64(0); i < input.Len; i++ {
			bitutil.SetBitTo(bits, int(i), isValidAt(input, i) != wantNull)
		}
	}
	return nil
}

// findRun returns the index of the run that holds slot i of a run-end encoded
// span.
func findRun(span *exec.ArraySpan, i int64) int {
	return sort.Search(int(span.Children[0].Len), func(j int) bool { return runEnd(span, j) > span.Offset+i })
}

// runEnd returns the logical end of run j of a run-end encoded span.
func runEnd(span *exec.ArraySpan, j int) int64 {
	runEnds := &span.Children[0]
	switch runEnds.Type.ID() {
	case arrow.INT16:
		return int64(exec.GetSpanValues[int16](runEnds, 1)[j])
	case arrow.INT32:
		return int64(exec.GetSpanValues[int32](runEnds, 1)[j])
	default:
		return int64(exec.GetSpanValues[int64](runEnds, 1)[j])
	}
}

// isValidAt reports whether slot i of span is valid, following Arrow C++'s
// ArraySpan::IsValid. Unlike the top-level kernels, it ignores null dictionary
// values, and it checks the value of a run-end encoded array with isValidAt
// rather than the validity bitmap of the values.
func isValidAt(span *exec.ArraySpan, i int64) bool {
	if span.Buffers[0].Buf != nil {
		return bitutil.BitIsSet(span.Buffers[0].Buf, int(span.Offset+i))
	}
	switch span.Type.ID() {
	case arrow.NULL:
		return false
	case arrow.SPARSE_UNION, arrow.DENSE_UNION:
		code := exec.GetSpanValues[arrow.UnionTypeCode](span, 1)[i]
		child := &span.Children[span.Type.(arrow.UnionType).ChildIDs()[code]]
		j := span.Offset + i
		if span.Type.ID() == arrow.DENSE_UNION {
			j = int64(exec.GetSpanValues[int32](span, 2)[i])
		}
		return isValidAt(child, j)
	case arrow.RUN_END_ENCODED:
		return isValidAt(&span.Children[1], int64(findRun(span, i)))
	}
	return true
}

// setNaNBits sets bit i of out to v for each NaN slot i of a float16, float32
// or float64 span, and does nothing for other types.
func setNaNBits(input *exec.ArraySpan, out []byte, v bool) {
	switch input.Type.ID() {
	case arrow.FLOAT16:
		for i, x := range exec.GetSpanValues[float16.Num](input, 1) {
			if x.IsNaN() {
				bitutil.SetBitTo(out, i, v)
			}
		}
	case arrow.FLOAT32:
		for i, x := range exec.GetSpanValues[float32](input, 1) {
			if x != x {
				bitutil.SetBitTo(out, i, v)
			}
		}
	case arrow.FLOAT64:
		for i, x := range exec.GetSpanValues[float64](input, 1) {
			if x != x {
				bitutil.SetBitTo(out, i, v)
			}
		}
	}
}

type NullOptions struct {
	NanIsNull bool `compute:"nan_is_null"`
}

func (NullOptions) TypeName() string { return "NullOptions" }

func initIsNull(_ *exec.KernelCtx, args exec.KernelInitArgs) (exec.KernelState, error) {
	switch opts := args.Options.(type) {
	// exprs initializes kernels without applying the default options
	case nil:
		return NullOptions{}, nil
	case NullOptions:
		return opts, nil
	case *NullOptions:
		if opts == nil {
			return NullOptions{}, nil
		}
		return *opts, nil
	}
	return nil, fmt.Errorf("%w: attempted to initialize kernel state from invalid function options",
		arrow.ErrInvalid)
}

func isNullExec(ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
	out.Release()
	input := batch.Values[0].Array
	opts, _ := ctx.State.(NullOptions)

	floatDict := input.Type.ID() == arrow.DICTIONARY && arrow.IsFloating(input.Dictionary().Type.ID())
	if dictHasNullValues(&input) || (opts.NanIsNull && floatDict) {
		return dictValidity(ctx, &input, out, true, opts.NanIsNull)
	}

	if hasLogicalNulls(&input) {
		return logicalValidity(ctx, &input, out, true)
	}

	validityBuf := input.GetBuffer(0)
	out.Buffers[1].WrapBuffer(ctx.AllocateBitmap(input.Len))
	if validityBuf != nil {
		bitutil.InvertBitmap(validityBuf.Bytes(), int(input.Offset), int(input.Len),
			out.Buffers[1].Buf, 0)
	}
	if opts.NanIsNull {
		setNaNBits(&input, out.Buffers[1].Buf, true)
	}

	return nil
}

func isNotNullExec(ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
	out.Release()
	input := batch.Values[0].Array

	if dictHasNullValues(&input) {
		return dictValidity(ctx, &input, out, false, false)
	}

	if hasLogicalNulls(&input) {
		return logicalValidity(ctx, &input, out, false)
	}

	validityBuf := input.GetBuffer(0)
	switch {
	case validityBuf == nil:
		out.Buffers[1].WrapBuffer(ctx.AllocateBitmap(input.Len))
		memory.Set(out.Buffers[1].Buf, 0xFF)
	case input.Offset == 0:
		out.Buffers[1].SetBuffer(validityBuf)
	default:
		out.Buffers[1].WrapBuffer(ctx.AllocateBitmap(input.Len))
		bitutil.CopyBitmap(validityBuf.Bytes(), int(input.Offset), int(input.Len),
			out.Buffers[1].Buf, 0)
	}

	return nil
}

func IsNullNotNullKernels() []exec.ScalarKernel {
	in := exec.InputType{Kind: exec.InputAny}
	out := exec.NewOutputType(arrow.FixedWidthTypes.Boolean)

	results := make([]exec.ScalarKernel, 2)
	results[0] = exec.NewScalarKernel([]exec.InputType{in}, out, isNullExec, initIsNull)
	results[0].NullHandling = exec.NullComputedNoPrealloc
	results[0].MemAlloc = exec.MemNoPrealloc

	results[1] = exec.NewScalarKernel([]exec.InputType{in}, out, isNotNullExec, nil)
	results[1].NullHandling = exec.NullComputedNoPrealloc
	results[1].MemAlloc = exec.MemNoPrealloc

	return results
}

func ConstBoolExec(val bool) func(*exec.KernelCtx, *exec.ExecSpan, *exec.ExecResult) error {
	return func(ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
		bitutil.SetBitsTo(out.Buffers[1].Buf, out.Offset, batch.Len, val)
		return nil
	}
}

func isNanKernelExec[T float32 | float64](ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
	kn := ctx.Kernel.(*exec.ScalarKernel)
	knData := kn.Data.(CompareFuncData).Funcs()

	outPrefix := int(out.Offset % 8)
	outBuf := out.Buffers[1].Buf[out.Offset/8:]

	inputBytes := getOffsetSpanBytes(&batch.Values[0].Array)
	knData.funcAA(inputBytes, inputBytes, outBuf, outPrefix)
	return nil
}

func IsNaNKernels() []exec.ScalarKernel {
	outputType := exec.NewOutputType(arrow.FixedWidthTypes.Boolean)

	knFloat32 := exec.NewScalarKernel([]exec.InputType{exec.NewExactInput(arrow.PrimitiveTypes.Float32)},
		outputType, isNanKernelExec[float32], nil)
	knFloat32.Data = genCompareKernel[float32](CmpNE)
	knFloat32.NullHandling = exec.NullNoOutput
	knFloat64 := exec.NewScalarKernel([]exec.InputType{exec.NewExactInput(arrow.PrimitiveTypes.Float64)},
		outputType, isNanKernelExec[float64], nil)
	knFloat64.Data = genCompareKernel[float64](CmpNE)
	knFloat64.NullHandling = exec.NullNoOutput

	kernels := []exec.ScalarKernel{knFloat32, knFloat64}

	for _, dt := range intTypes {
		kn := exec.NewScalarKernel(
			[]exec.InputType{exec.NewExactInput(dt)},
			outputType, ConstBoolExec(false), nil)
		kn.NullHandling = exec.NullNoOutput
		kernels = append(kernels, kn)
	}

	for _, id := range []arrow.Type{arrow.NULL, arrow.DURATION, arrow.DECIMAL32, arrow.DECIMAL64, arrow.DECIMAL128, arrow.DECIMAL256} {
		kn := exec.NewScalarKernel(
			[]exec.InputType{exec.NewIDInput(id)},
			outputType, ConstBoolExec(false), nil)
		kn.NullHandling = exec.NullNoOutput
		kernels = append(kernels, kn)
	}

	return kernels
}
