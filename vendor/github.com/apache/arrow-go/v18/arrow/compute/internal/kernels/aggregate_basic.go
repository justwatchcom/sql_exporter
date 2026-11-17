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
	"fmt"
	"math/bits"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/bitutil"
	"github.com/apache/arrow-go/v18/arrow/compute/exec"
	"github.com/apache/arrow-go/v18/arrow/scalar"
	"github.com/apache/arrow-go/v18/internal/bitutils"
)

// ScalarAggregateOptions controls the general behaviour of the scalar
// aggregate kernels. The field names and tags are the ones the C++
// implementation uses so that serialized expressions round-trip.
//
// The zero value is not the default: use DefaultScalarAggregateOptions for
// the behaviour the functions have when they are called without options.
type ScalarAggregateOptions struct {
	// SkipNulls, if true, ignores null values. Otherwise, if any value is
	// null, the aggregation emits null.
	SkipNulls bool `compute:"skip_nulls"`
	// MinCount is the number of non-null values which have to be observed
	// for the aggregation to emit a value rather than null.
	MinCount uint32 `compute:"min_count"`
}

func (ScalarAggregateOptions) TypeName() string { return "ScalarAggregateOptions" }

// DefaultScalarAggregateOptions returns the options the scalar aggregate
// functions use when they are called with nil options: null values are
// skipped and a single non-null value is enough to produce a result.
func DefaultScalarAggregateOptions() *ScalarAggregateOptions {
	return &ScalarAggregateOptions{SkipNulls: true, MinCount: 1}
}

// CountMode is the enum for the values which CountOptions.Mode can take.
type CountMode int8

const (
	// CountOnlyValid counts only non-null values.
	CountOnlyValid CountMode = iota
	// CountOnlyNull counts only null values.
	CountOnlyNull
	// CountAllRows counts both non-null and null values.
	CountAllRows
)

// CountOptions controls the behaviour of the count aggregate kernel.
type CountOptions struct {
	Mode CountMode `compute:"mode"`
}

func (CountOptions) TypeName() string { return "CountOptions" }

// DefaultCountOptions returns the options the count function uses when it is
// called with nil options, which is to count only the non-null values. This
// is also the zero value of CountOptions.
func DefaultCountOptions() *CountOptions { return &CountOptions{Mode: CountOnlyValid} }

// aggState is implemented by every aggregate state in this package, so that
// one set of consume/merge/finalize functions can serve all of the kernels,
// as the C++ ScalarAggregator does.
type aggState interface {
	consume(span *exec.ExecSpan) error
	mergeFrom(src aggState) error
	finalize() (*exec.AggregateResult, error)
}

// AggregateConsume is the exec.ScalarAggConsume shared by the aggregate
// kernels in this package.
func AggregateConsume(ctx *exec.KernelCtx, span *exec.ExecSpan) error {
	st, ok := ctx.State.(aggState)
	if !ok {
		return fmt.Errorf("%w: aggregate kernel state of unexpected type %T", arrow.ErrInvalid, ctx.State)
	}
	return st.consume(span)
}

// AggregateMerge is the exec.ScalarAggMerge shared by the aggregate kernels
// in this package. It folds src into dst, leaving src untouched.
func AggregateMerge(_ *exec.KernelCtx, src, dst exec.KernelState) error {
	dstState, ok := dst.(aggState)
	if !ok {
		return fmt.Errorf("%w: aggregate kernel state of unexpected type %T", arrow.ErrInvalid, dst)
	}
	srcState, ok := src.(aggState)
	if !ok {
		return fmt.Errorf("%w: aggregate kernel state of unexpected type %T", arrow.ErrInvalid, src)
	}
	return dstState.mergeFrom(srcState)
}

// AggregateFinalize is the exec.ScalarAggFinalize shared by the aggregate
// kernels in this package.
func AggregateFinalize(ctx *exec.KernelCtx) (*exec.AggregateResult, error) {
	st, ok := ctx.State.(aggState)
	if !ok {
		return nil, fmt.Errorf("%w: aggregate kernel state of unexpected type %T", arrow.ErrInvalid, ctx.State)
	}
	return st.finalize()
}

// spanNullCount returns the number of nulls in the span without writing to
// it. ArraySpan.UpdateNullCount caches its answer in the span, and the
// executor re-slices one span for every chunk of the input, so a kernel which
// updated the count of the span it was handed would leave a stale count
// behind for the following chunk.
func spanNullCount(a *exec.ArraySpan) int64 {
	if a.Type.ID() == arrow.NULL {
		return a.Len
	}
	if nulls := a.Nulls; nulls != array.UnknownNullCount {
		return nulls
	}
	if len(a.Buffers[0].Buf) == 0 {
		return 0
	}
	return a.Len - int64(bitutil.CountSetBits(a.Buffers[0].Buf, int(a.Offset), int(a.Len)))
}

func resolveAggOptions(opts any) (ScalarAggregateOptions, error) {
	switch o := opts.(type) {
	case nil:
		return *DefaultScalarAggregateOptions(), nil
	case *ScalarAggregateOptions:
		if o == nil {
			return *DefaultScalarAggregateOptions(), nil
		}
		return *o, nil
	case ScalarAggregateOptions:
		return o, nil
	}
	return ScalarAggregateOptions{}, fmt.Errorf("%w: attempted to initialize a scalar aggregate kernel with options of type %T",
		arrow.ErrInvalid, opts)
}

func resolveCountOptions(opts any) (CountOptions, error) {
	var out CountOptions
	switch o := opts.(type) {
	case nil:
		out = *DefaultCountOptions()
	case *CountOptions:
		if o == nil {
			out = *DefaultCountOptions()
		} else {
			out = *o
		}
	case CountOptions:
		out = o
	default:
		return out, fmt.Errorf("%w: attempted to initialize the count kernel with options of type %T",
			arrow.ErrInvalid, opts)
	}

	switch out.Mode {
	case CountOnlyValid, CountOnlyNull, CountAllRows:
	default:
		return out, fmt.Errorf("%w: invalid count mode %d", arrow.ErrInvalid, out.Mode)
	}
	return out, nil
}

// ----------------------------------------------------------------------
// count

type countImpl struct {
	opts     CountOptions
	nulls    int64
	nonNulls int64
}

func (c *countImpl) consume(span *exec.ExecSpan) error {
	v := &span.Values[0]
	switch {
	case c.opts.Mode == CountAllRows:
		// ALL never has to look at the validity bitmap.
		c.nonNulls += span.Len
	case v.IsArray():
		nulls := spanNullCount(&v.Array)
		c.nulls += nulls
		c.nonNulls += v.Array.Len - nulls
	case v.Scalar.IsValid():
		c.nonNulls += span.Len
	default:
		c.nulls += span.Len
	}
	return nil
}

func (c *countImpl) mergeFrom(src aggState) error {
	other, ok := src.(*countImpl)
	if !ok {
		return fmt.Errorf("%w: cannot merge %T into a count state", arrow.ErrInvalid, src)
	}
	c.nulls += other.nulls
	c.nonNulls += other.nonNulls
	return nil
}

func (c *countImpl) finalize() (*exec.AggregateResult, error) {
	// ALL is equivalent to ONLY_VALID here because consume counted every
	// row as non-null rather than computing a null count it would not use.
	if c.opts.Mode == CountOnlyNull {
		return exec.NewScalarResult(scalar.NewInt64Scalar(c.nulls)), nil
	}
	return exec.NewScalarResult(scalar.NewInt64Scalar(c.nonNulls)), nil
}

// CountInit is the exec.KernelInitFn for the count kernel.
func CountInit(_ *exec.KernelCtx, args exec.KernelInitArgs) (exec.KernelState, error) {
	opts, err := resolveCountOptions(args.Options)
	if err != nil {
		return nil, err
	}

	// The logical null count of these types is not the number of unset bits
	// in the validity bitmap of the top-level array, so counting them needs
	// machinery this kernel does not have yet.
	switch args.Inputs[0].ID() {
	case arrow.RUN_END_ENCODED, arrow.DICTIONARY, arrow.SPARSE_UNION, arrow.DENSE_UNION:
		if opts.Mode != CountAllRows {
			return nil, fmt.Errorf("%w: count of %s requires a logical null count",
				arrow.ErrNotImplemented, args.Inputs[0])
		}
	}

	return &countImpl{opts: opts}, nil
}

// ----------------------------------------------------------------------
// sum

// sumBase holds the parts of a sum state which do not depend on the type
// being summed: how many values were seen, whether any null was seen, and the
// options which turn the two into a null or a value at finalize time.
type sumBase struct {
	opts          ScalarAggregateOptions
	count         uint64
	nullsObserved bool
}

// observe folds the value and null counts of one span into the state. It
// returns the number of non-null values in the span and whether their values
// still have to be accumulated: once a null has been seen with SkipNulls
// false, the result is null whatever the values are.
func (s *sumBase) observe(span *exec.ExecSpan) (valid int64, accumulate bool) {
	v := &span.Values[0]
	if v.IsArray() {
		nulls := spanNullCount(&v.Array)
		valid = v.Array.Len - nulls
		s.nullsObserved = s.nullsObserved || nulls > 0
	} else if v.Scalar.IsValid() {
		valid = span.Len
	} else {
		s.nullsObserved = true
	}
	s.count += uint64(valid)
	return valid, valid > 0 && (s.opts.SkipNulls || !s.nullsObserved)
}

func (s *sumBase) mergeBase(other *sumBase) {
	s.count += other.count
	s.nullsObserved = s.nullsObserved || other.nullsObserved
}

// emitNull reports whether finalize has to produce a null rather than the
// accumulated value.
func (s *sumBase) emitNull() bool {
	return (!s.opts.SkipNulls && s.nullsObserved) || s.count < uint64(s.opts.MinCount)
}

// intSumImpl sums the signed integer types into an int64. Go defines the
// wrap-around, so an overflowing sum wraps rather than being undefined as it
// is in C++, but the value produced is the same one C++ produces in practice.
type intSumImpl[T int8 | int16 | int32 | int64] struct {
	sumBase
	sum int64
}

func (s *intSumImpl[T]) consume(span *exec.ExecSpan) error {
	if _, accumulate := s.observe(span); !accumulate {
		return nil
	}
	v := &span.Values[0]
	if v.IsArray() {
		visitValues(&v.Array, func(vals []T, pos, length int64) {
			for i := int64(0); i < length; i++ {
				s.sum += int64(vals[pos+i])
			}
		})
	} else {
		s.sum += int64(UnboxScalar[T](v.Scalar.(scalar.PrimitiveScalar))) * span.Len
	}
	return nil
}

func (s *intSumImpl[T]) mergeFrom(src aggState) error {
	other, ok := src.(*intSumImpl[T])
	if !ok {
		return fmt.Errorf("%w: cannot merge %T into a sum state", arrow.ErrInvalid, src)
	}
	s.mergeBase(&other.sumBase)
	s.sum += other.sum
	return nil
}

func (s *intSumImpl[T]) finalize() (*exec.AggregateResult, error) {
	if s.emitNull() {
		return exec.NewScalarResult(scalar.MakeNullScalar(arrow.PrimitiveTypes.Int64)), nil
	}
	return exec.NewScalarResult(scalar.NewInt64Scalar(s.sum)), nil
}

// uintSumImpl sums the unsigned integer types into a uint64.
type uintSumImpl[T uint8 | uint16 | uint32 | uint64] struct {
	sumBase
	sum uint64
}

func (s *uintSumImpl[T]) consume(span *exec.ExecSpan) error {
	if _, accumulate := s.observe(span); !accumulate {
		return nil
	}
	v := &span.Values[0]
	if v.IsArray() {
		visitValues(&v.Array, func(vals []T, pos, length int64) {
			for i := int64(0); i < length; i++ {
				s.sum += uint64(vals[pos+i])
			}
		})
	} else {
		s.sum += uint64(UnboxScalar[T](v.Scalar.(scalar.PrimitiveScalar))) * uint64(span.Len)
	}
	return nil
}

func (s *uintSumImpl[T]) mergeFrom(src aggState) error {
	other, ok := src.(*uintSumImpl[T])
	if !ok {
		return fmt.Errorf("%w: cannot merge %T into a sum state", arrow.ErrInvalid, src)
	}
	s.mergeBase(&other.sumBase)
	s.sum += other.sum
	return nil
}

func (s *uintSumImpl[T]) finalize() (*exec.AggregateResult, error) {
	if s.emitNull() {
		return exec.NewScalarResult(scalar.MakeNullScalar(arrow.PrimitiveTypes.Uint64)), nil
	}
	return exec.NewScalarResult(scalar.NewUint64Scalar(s.sum)), nil
}

// floatSumImpl sums the floating point types into a float64 using the same
// pairwise summation the C++ implementation uses, so that the result is the
// same one down to the last bit.
type floatSumImpl[T float32 | float64] struct {
	sumBase
	sum float64
}

func (s *floatSumImpl[T]) consume(span *exec.ExecSpan) error {
	valid, accumulate := s.observe(span)
	if !accumulate {
		return nil
	}
	v := &span.Values[0]
	if v.IsArray() {
		s.sum += pairwiseSum[T](&v.Array, valid)
	} else {
		// C++ multiplies the unboxed value by the span length in the value's
		// own type before widening it to the accumulator.
		val := UnboxScalar[T](v.Scalar.(scalar.PrimitiveScalar))
		s.sum += float64(val * T(span.Len))
	}
	return nil
}

func (s *floatSumImpl[T]) mergeFrom(src aggState) error {
	other, ok := src.(*floatSumImpl[T])
	if !ok {
		return fmt.Errorf("%w: cannot merge %T into a sum state", arrow.ErrInvalid, src)
	}
	s.mergeBase(&other.sumBase)
	s.sum += other.sum
	return nil
}

func (s *floatSumImpl[T]) finalize() (*exec.AggregateResult, error) {
	if s.emitNull() {
		return exec.NewScalarResult(scalar.MakeNullScalar(arrow.PrimitiveTypes.Float64)), nil
	}
	return exec.NewScalarResult(scalar.NewFloat64Scalar(s.sum)), nil
}

// boolSumImpl sums boolean input as the number of true values, into a uint64.
type boolSumImpl struct {
	sumBase
	sum uint64
}

func (s *boolSumImpl) consume(span *exec.ExecSpan) error {
	if _, accumulate := s.observe(span); !accumulate {
		return nil
	}
	v := &span.Values[0]
	if v.IsArray() {
		s.sum += uint64(trueCount(&v.Array))
	} else if v.Scalar.(*scalar.Boolean).Value {
		s.sum += uint64(span.Len)
	}
	return nil
}

func (s *boolSumImpl) mergeFrom(src aggState) error {
	other, ok := src.(*boolSumImpl)
	if !ok {
		return fmt.Errorf("%w: cannot merge %T into a sum state", arrow.ErrInvalid, src)
	}
	s.mergeBase(&other.sumBase)
	s.sum += other.sum
	return nil
}

func (s *boolSumImpl) finalize() (*exec.AggregateResult, error) {
	if s.emitNull() {
		return exec.NewScalarResult(scalar.MakeNullScalar(arrow.PrimitiveTypes.Uint64)), nil
	}
	return exec.NewScalarResult(scalar.NewUint64Scalar(s.sum)), nil
}

// nullSumImpl sums null-typed input, whose every value is null, into an
// int64. It reproduces the C++ NullSumImpl: the only thing it has to track is
// whether anything at all was consumed.
type nullSumImpl struct {
	opts    ScalarAggregateOptions
	isEmpty bool
}

func (s *nullSumImpl) consume(span *exec.ExecSpan) error {
	v := &span.Values[0]
	if v.IsScalar() || spanNullCount(&v.Array) > 0 {
		s.isEmpty = false
	}
	return nil
}

func (s *nullSumImpl) mergeFrom(src aggState) error {
	other, ok := src.(*nullSumImpl)
	if !ok {
		return fmt.Errorf("%w: cannot merge %T into a sum state", arrow.ErrInvalid, src)
	}
	s.isEmpty = s.isEmpty && other.isEmpty
	return nil
}

func (s *nullSumImpl) finalize() (*exec.AggregateResult, error) {
	if (s.opts.SkipNulls || s.isEmpty) && s.opts.MinCount == 0 {
		return exec.NewScalarResult(scalar.NewInt64Scalar(0)), nil
	}
	return exec.NewScalarResult(scalar.MakeNullScalar(arrow.PrimitiveTypes.Int64)), nil
}

// SumInit is the exec.KernelInitFn for every kernel of the sum function; it
// picks the accumulator from the input type the way the C++ SumLikeInit
// visitor does.
func SumInit(_ *exec.KernelCtx, args exec.KernelInitArgs) (exec.KernelState, error) {
	opts, err := resolveAggOptions(args.Options)
	if err != nil {
		return nil, err
	}

	base := sumBase{opts: opts}
	switch args.Inputs[0].ID() {
	case arrow.INT8:
		return &intSumImpl[int8]{sumBase: base}, nil
	case arrow.INT16:
		return &intSumImpl[int16]{sumBase: base}, nil
	case arrow.INT32:
		return &intSumImpl[int32]{sumBase: base}, nil
	case arrow.INT64:
		return &intSumImpl[int64]{sumBase: base}, nil
	case arrow.UINT8:
		return &uintSumImpl[uint8]{sumBase: base}, nil
	case arrow.UINT16:
		return &uintSumImpl[uint16]{sumBase: base}, nil
	case arrow.UINT32:
		return &uintSumImpl[uint32]{sumBase: base}, nil
	case arrow.UINT64:
		return &uintSumImpl[uint64]{sumBase: base}, nil
	case arrow.FLOAT32:
		return &floatSumImpl[float32]{sumBase: base}, nil
	case arrow.FLOAT64:
		return &floatSumImpl[float64]{sumBase: base}, nil
	case arrow.BOOL:
		return &boolSumImpl{sumBase: base}, nil
	case arrow.NULL:
		return &nullSumImpl{opts: opts, isEmpty: true}, nil
	}
	return nil, fmt.Errorf("%w: no sum implemented for %s", arrow.ErrNotImplemented, args.Inputs[0])
}

// ----------------------------------------------------------------------
// value iteration helpers

// visitValues calls fn once for every run of valid values in the span,
// passing the typed values of the span along with the position and length of
// the run relative to the start of the span.
func visitValues[T arrow.FixedWidthType](a *exec.ArraySpan, fn func(vals []T, pos, length int64)) {
	vals := exec.GetSpanValues[T](a, 1)
	bitutils.VisitSetBitRunsNoErr(a.Buffers[0].Buf, a.Offset, a.Len, func(pos, length int64) {
		fn(vals, pos, length)
	})
}

// trueCount returns the number of values in the boolean span which are both
// valid and true.
func trueCount(a *exec.ArraySpan) int64 {
	values := a.Buffers[1].Buf
	if len(values) == 0 {
		return 0
	}
	if len(a.Buffers[0].Buf) == 0 {
		return int64(bitutil.CountSetBits(values, int(a.Offset), int(a.Len)))
	}

	var count int64
	bitutils.VisitSetBitRunsNoErr(a.Buffers[0].Buf, a.Offset, a.Len, func(pos, length int64) {
		count += int64(bitutil.CountSetBits(values, int(a.Offset+pos), int(length)))
	})
	return count
}

// pairwiseSum is a port of the non-recursive pairwise summation the C++
// implementation uses for floating point input, so that the two produce
// bit-for-bit identical results.
//
// https://en.wikipedia.org/wiki/Pairwise_summation
func pairwiseSum[T float32 | float64](a *exec.ArraySpan, dataSize int64) float64 {
	if dataSize == 0 {
		return 0
	}

	// number of inputs to accumulate before merging with another block
	const blockSize = 16 // same as numpy
	// levels (tree depth) = ceil(log2(len)) + 1, a bit larger than necessary
	levels := bits.Len64(uint64(dataSize-1)) + 1
	// temporary summation per level
	sum := make([]float64, levels+1)
	// whether two summations are ready and should be reduced to the level
	// above; one bit per level, bit 0 is level 0, and so on
	var mask uint64
	// level of the root node holding the final summation
	var rootLevel int

	// reduce the summation of one block (which may be smaller than blockSize)
	// from a leaf node, continuing to the level above whenever two summations
	// are ready for a non-leaf node
	reduce := func(blockSum float64) {
		curLevel, curLevelMask := 0, uint64(1)
		sum[curLevel] += blockSum
		mask ^= curLevelMask
		for mask&curLevelMask == 0 {
			blockSum = sum[curLevel]
			sum[curLevel] = 0
			curLevel++
			curLevelMask <<= 1
			sum[curLevel] += blockSum
			mask ^= curLevelMask
		}
		if curLevel > rootLevel {
			rootLevel = curLevel
		}
	}

	visitValues[T](a, func(vals []T, pos, length int64) {
		v := vals[pos:]
		blocks, remains := length/blockSize, length%blockSize
		for i := int64(0); i < blocks; i++ {
			var blockSum float64
			for j := 0; j < blockSize; j++ {
				blockSum += float64(v[j])
			}
			reduce(blockSum)
			v = v[blockSize:]
		}
		if remains > 0 {
			var blockSum float64
			for i := int64(0); i < remains; i++ {
				blockSum += float64(v[i])
			}
			reduce(blockSum)
		}
	})

	// reduce the intermediate summations from all of the non-leaf nodes
	for i := 1; i <= rootLevel; i++ {
		sum[i] += sum[i-1]
	}
	return sum[rootLevel]
}
