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
	"math"
	"math/big"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/compute/exec"
	"github.com/apache/arrow-go/v18/arrow/decimal128"
	"github.com/apache/arrow-go/v18/arrow/decimal256"
	"github.com/apache/arrow-go/v18/arrow/scalar"
	"golang.org/x/exp/constraints"
)

//go:generate go tool stringer -type=RoundMode

type RoundMode int8

const (
	// Round to nearest integer less than or equal in magnitude (aka "floor")
	RoundDown RoundMode = iota
	// Round to nearest integer greater than or equal in magnitude (aka "ceil")
	RoundUp
	// Get integral part without fractional digits (aka "trunc")
	TowardsZero
	// Round negative values with DOWN and positive values with UP
	AwayFromZero
	// Round ties with DOWN (aka "round half towards negative infinity")
	HalfDown
	// Round ties with UP (aka "round half towards positive infinity")
	HalfUp
	// Round ties with TowardsZero (aka "round half away from infinity")
	HalfTowardsZero
	// Round ties with AwayFromZero (aka "round half towards infinity")
	HalfAwayFromZero
	// Round ties to nearest even integer
	HalfToEven
	// Round ties to nearest odd integer
	HalfToOdd
)

type RoundOptions struct {
	NDigits int64
	Mode    RoundMode
}

func (RoundOptions) TypeName() string { return "RoundOptions" }

type RoundState struct {
	RoundOptions
	Pow10 float64
}

func InitRoundState(_ *exec.KernelCtx, args exec.KernelInitArgs) (exec.KernelState, error) {
	var rs RoundState

	opts, ok := args.Options.(*RoundOptions)
	if ok {
		rs.RoundOptions = *opts
	} else {
		if rs.RoundOptions, ok = args.Options.(RoundOptions); !ok {
			return nil, fmt.Errorf("%w: attempted to initialize kernel state from invalid function options",
				arrow.ErrInvalid)
		}
	}

	// Only positive exponents for powers of 10 are used because combining
	// multiply and division operations produced more stable rounding than
	// using multiply-only.  Refer to NumPy's round implementation:
	// https://github.com/numpy/numpy/blob/7b2f20b406d27364c812f7a81a9c901afbd3600c/numpy/core/src/multiarray/calculation.c#L589
	rs.Pow10 = math.Pow10(int(math.Abs(float64(rs.NDigits))))
	return rs, nil
}

type RoundToMultipleOptions struct {
	// Multiple is the multiple to round to.
	//
	// Should be a positive numeric scalar of a type compatible
	// with the argument to be rounded. The cast kernel is used
	// to convert the rounding multiple to match the result type.
	Multiple scalar.Scalar
	// Mode is the rounding and tie-breaking mode
	Mode RoundMode
}

func (RoundToMultipleOptions) TypeName() string { return "RoundToMultipleOptions" }

type RoundToMultipleState = RoundToMultipleOptions

func isPositive(s scalar.Scalar) bool {
	switch s := s.(type) {
	case *scalar.Decimal128:
		return s.Value.Greater(decimal128.Num{})
	case *scalar.Decimal256:
		return s.Value.Greater(decimal256.Num{})
	case *scalar.Int8:
		return s.Value > 0
	case *scalar.Uint8, *scalar.Uint16, *scalar.Uint32, *scalar.Uint64:
		return true
	case *scalar.Int16:
		return s.Value > 0
	case *scalar.Int32:
		return s.Value > 0
	case *scalar.Int64:
		return s.Value > 0
	case *scalar.Float32:
		return s.Value > 0
	case *scalar.Float64:
		return s.Value > 0
	default:
		return false
	}
}

func InitRoundToMultipleState(_ *exec.KernelCtx, args exec.KernelInitArgs) (exec.KernelState, error) {
	var rs RoundToMultipleState

	opts, ok := args.Options.(*RoundToMultipleOptions)
	if ok {
		rs = *opts
	} else {
		if rs, ok = args.Options.(RoundToMultipleOptions); !ok {
			return nil, fmt.Errorf("%w: attempted to initialize kernel state from invalid function options",
				arrow.ErrInvalid)
		}
	}

	mult := rs.Multiple
	if mult == nil || !mult.IsValid() {
		return nil, fmt.Errorf("%w: rounding multiple must be non-null and valid",
			arrow.ErrInvalid)
	}

	if !isPositive(mult) {
		return nil, fmt.Errorf("%w: rounding multiple must be positive", arrow.ErrInvalid)
	}

	// ensure the rounding multiple option matches the kernel's output type.
	// the output type is not available here, so we use the following rule:
	// if "multiple" is neither a floating-point nor decimal type,
	// then cast to float64, else cast to the kernel's input type.
	var toType arrow.DataType
	if !arrow.IsFloating(mult.DataType().ID()) && !arrow.IsDecimal(mult.DataType().ID()) {
		toType = arrow.PrimitiveTypes.Float64
	} else {
		toType = args.Inputs[0]
	}

	if !arrow.TypeEqual(mult.DataType(), toType) {
		castedMultiple, err := mult.CastTo(toType)
		if err != nil {
			return nil, err
		}

		rs.Multiple = castedMultiple
	}

	return rs, nil
}

func getFloatRoundImpl[T constraints.Float](mode RoundMode) func(T) T {
	switch mode {
	case RoundDown:
		return func(t T) T { return T(math.Floor(float64(t))) }
	case RoundUp:
		return func(t T) T { return T(math.Ceil(float64(t))) }
	case TowardsZero: // truncate
		return func(t T) T { return T(math.Trunc(float64(t))) }
	case AwayFromZero:
		return func(t T) T {
			v := float64(t)
			if math.Signbit(v) {
				return T(math.Floor(v))
			}
			return T(math.Ceil(v))
		}
	// the Half variants are only called when the fractional portion
	// was 0.5
	case HalfDown:
		return func(t T) T { return T(math.Floor(float64(t))) }
	case HalfUp:
		return func(t T) T { return T(math.Ceil(float64(t))) }
	case HalfTowardsZero:
		return func(t T) T { return T(math.Trunc(float64(t))) }
	case HalfAwayFromZero:
		return func(t T) T {
			v := float64(t)
			if math.Signbit(v) {
				return T(math.Floor(v))
			}
			return T(math.Ceil(v))
		}
	case HalfToEven:
		return func(t T) T { return T(math.RoundToEven(float64(t))) }
	case HalfToOdd:
		return func(t T) T {
			v := float64(t)
			return T(math.Floor(v*0.5) + math.Ceil(v*0.5))
		}
	}
	panic("invalid rounding mode")
}

func getDecRounding[T decimal128.Num | decimal256.Num](mode RoundMode, opsImpl *roundDecImpl[T]) func(val, remainder T, pow10 T, scale int32) T {
	var (
		z   T
		one = opsImpl.fromI64(1)
		neg = opsImpl.fromI64(-1)
	)

	switch mode {
	case RoundDown:
		return func(val, remainder, pow10 T, _ int32) T {
			val = opsImpl.Sub(val, remainder)
			if opsImpl.Sign(val) < 0 {
				val = opsImpl.Sub(val, pow10)
			}
			return val
		}
	case RoundUp:
		return func(val, remainder, pow10 T, _ int32) T {
			val = opsImpl.Sub(val, remainder)
			if opsImpl.Sign(val) > 0 && remainder != z {
				val = opsImpl.Add(val, pow10)
			}
			return val
		}
	case TowardsZero:
		return func(val, remainder, _ T, _ int32) T {
			return opsImpl.Sub(val, remainder)
		}
	case AwayFromZero:
		return func(val, remainder, pow10 T, _ int32) T {
			val = opsImpl.Sub(val, remainder)
			if opsImpl.Sign(remainder) < 0 {
				val = opsImpl.Sub(val, pow10)
			} else if opsImpl.Sign(remainder) > 0 && remainder != z {
				val = opsImpl.Add(val, pow10)
			}
			return val
		}
	// variants for Half_* modes are only invoked when the fractional part
	// is equal to 0.5
	case HalfDown:
		return func(val, remainder, pow10 T, _ int32) T {
			val = opsImpl.Sub(val, remainder)
			if opsImpl.Sign(val) < 0 {
				val = opsImpl.Sub(val, pow10)
			}
			return val
		}
	case HalfUp:
		return func(val, remainder, pow10 T, _ int32) T {
			val = opsImpl.Sub(val, remainder)
			if opsImpl.Sign(val) > 0 && remainder != z {
				val = opsImpl.Add(val, pow10)
			}
			return val
		}
	case HalfTowardsZero:
		return func(val, remainder, _ T, _ int32) T {
			return opsImpl.Sub(val, remainder)
		}
	case HalfAwayFromZero:
		return func(val, remainder, pow10 T, _ int32) T {
			val = opsImpl.Sub(val, remainder)
			if opsImpl.Sign(remainder) < 0 {
				val = opsImpl.Sub(val, pow10)
			} else if opsImpl.Sign(remainder) > 0 && remainder != z {
				val = opsImpl.Add(val, pow10)
			}
			return val
		}
	case HalfToEven:
		return func(val, remainder, _ T, scale int32) T {
			scaled := opsImpl.reduceScale(val, scale, false)
			if opsImpl.lowBits(scaled)%2 != 0 {
				if opsImpl.Sign(remainder) >= 0 {
					scaled = opsImpl.Add(scaled, one)
				} else {
					scaled = opsImpl.Add(scaled, neg)
				}
			}
			return opsImpl.increaseScale(scaled, scale)
		}
	case HalfToOdd:
		return func(val, remainder, _ T, scale int32) T {
			scaled := opsImpl.reduceScale(val, scale, false)
			if opsImpl.lowBits(scaled)%2 == 0 {
				if opsImpl.Sign(remainder) > 0 {
					scaled = opsImpl.Add(scaled, one)
				} else {
					scaled = opsImpl.Add(scaled, neg)
				}
			}
			return opsImpl.increaseScale(scaled, scale)
		}
	}
	panic("invalid rounding mode")
}

type round[T constraints.Float] struct {
	pow10   T
	ndigits int64
	mode    RoundMode

	fn func(T) T
}

func (rnd *round[T]) call(_ *exec.KernelCtx, arg T, e *error) T {
	val := float64(arg)
	// do not process INF or NaN because they will trigger overflow errors
	// at the end of this
	if math.IsInf(val, 0) || math.IsNaN(val) {
		return arg
	}

	var roundVal T
	if rnd.ndigits >= 0 {
		roundVal = arg * rnd.pow10
	} else {
		roundVal = arg / rnd.pow10
	}

	frac := roundVal - T(math.Floor(float64(roundVal)))
	if frac == 0 {
		// scaled value has no fractional component
		// no rounding is needed.
		return arg
	}

	if rnd.mode >= HalfDown && frac != 0.5 {
		roundVal = T(math.Round(float64(roundVal)))
	} else {
		roundVal = rnd.fn(roundVal)
	}

	// equality check is omitted so that the common case of 10^0
	// (integer rounding) uses multiply-only
	if rnd.ndigits > 0 {
		roundVal /= rnd.pow10
	} else {
		roundVal *= rnd.pow10
	}
	if math.IsInf(float64(roundVal), 0) || math.IsNaN(float64(roundVal)) {
		*e = errOverflow
		return arg
	}

	return roundVal
}

func roundKernelFloating[T constraints.Float](ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
	opts := ctx.State.(RoundState)
	rnd := round[T]{
		pow10:   T(opts.Pow10),
		ndigits: opts.NDigits,
		mode:    opts.Mode,
		fn:      getFloatRoundImpl[T](opts.Mode),
	}

	return ScalarUnaryNotNull(rnd.call)(ctx, batch, out)
}

func roundToMultipleFloating[T constraints.Float](ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
	opts := ctx.State.(RoundToMultipleState)
	rnd := roundToMultiple[T]{
		mode:     opts.Mode,
		multiple: UnboxScalar[T](opts.Multiple.(scalar.PrimitiveScalar)),
		fn:       getFloatRoundImpl[T](opts.Mode),
	}

	return ScalarUnaryNotNull(rnd.call)(ctx, batch, out)
}

type roundDecImpl[T decimal128.Num | decimal256.Num] struct {
	*decOps[T]
	scaleMultiplier     func(int) T
	halfScaleMultiplier func(int) T
	divide              func(a, b T) (res, rem T)
	fitsInPrec          func(T, int32) bool
	less                func(a, b T) bool
	reduceScale         func(T, int32, bool) T
	increaseScale       func(T, int32) T
	lowBits             func(T) uint64
	fromI64             func(int64) T
	str                 func(T, int32) string
}

var (
	roundDec128 = roundDecImpl[decimal128.Num]{
		decOps:              &dec128Ops,
		scaleMultiplier:     decimal128.GetScaleMultiplier,
		halfScaleMultiplier: decimal128.GetHalfScaleMultiplier,
		divide:              func(a, b decimal128.Num) (res, rem decimal128.Num) { return a.Div(b) },
		fitsInPrec:          func(a decimal128.Num, prec int32) bool { return a.FitsInPrecision(prec) },
		less:                func(a, b decimal128.Num) bool { return a.Less(b) },
		reduceScale:         func(a decimal128.Num, scale int32, round bool) decimal128.Num { return a.ReduceScaleBy(scale, round) },
		increaseScale:       func(a decimal128.Num, scale int32) decimal128.Num { return a.IncreaseScaleBy(scale) },
		lowBits:             func(a decimal128.Num) uint64 { return a.LowBits() },
		fromI64:             func(v int64) decimal128.Num { return decimal128.FromI64(v) },
		str:                 func(a decimal128.Num, scale int32) string { return a.ToString(scale) },
	}
	roundDec256 = roundDecImpl[decimal256.Num]{
		decOps:              &dec256Ops,
		scaleMultiplier:     decimal256.GetScaleMultiplier,
		halfScaleMultiplier: decimal256.GetHalfScaleMultiplier,
		divide:              func(a, b decimal256.Num) (res, rem decimal256.Num) { return a.Div(b) },
		fitsInPrec:          func(a decimal256.Num, prec int32) bool { return a.FitsInPrecision(prec) },
		less:                func(a, b decimal256.Num) bool { return a.Less(b) },
		reduceScale:         func(a decimal256.Num, scale int32, round bool) decimal256.Num { return a.ReduceScaleBy(scale, round) },
		increaseScale:       func(a decimal256.Num, scale int32) decimal256.Num { return a.IncreaseScaleBy(scale) },
		lowBits:             func(a decimal256.Num) uint64 { return a.LowBits() },
		fromI64:             func(v int64) decimal256.Num { return decimal256.FromI64(v) },
		str:                 func(a decimal256.Num, scale int32) string { return a.ToString(scale) },
	}
)

type roundDec[T decimal128.Num | decimal256.Num] struct {
	ty      arrow.DecimalType
	mode    RoundMode
	ndigits int64
	pow     int32
	// pow10 is "1" for the given decimal scale. Similarly halfPow10 is "0.5"
	pow10, halfPow10, negHalfPow10 T

	opsImpl *roundDecImpl[T]
	fn      func(T, T, T, int32) T
}

func (rnd *roundDec[T]) call(_ *exec.KernelCtx, arg T, e *error) T {
	var def T
	if rnd.pow >= rnd.ty.GetPrecision() {
		*e = fmt.Errorf("%w: rounding to %d digits will not fit in precision of %s",
			arrow.ErrInvalid, rnd.ndigits, rnd.ty)
		return def
	} else if rnd.pow < 0 {
		// no-op copy output to input
		return arg
	}

	_, remainder := rnd.opsImpl.divide(arg, rnd.pow10)
	// the remainder is effectively the scaled fractional part after division
	if remainder == def {
		return arg
	}

	if rnd.mode >= HalfDown {
		if remainder == rnd.halfPow10 || remainder == rnd.negHalfPow10 {
			// on the halfway point, use tiebreaker
			arg = rnd.fn(arg, remainder, rnd.pow10, rnd.pow)
		} else if rnd.opsImpl.Sign(remainder) >= 0 {
			// positive, round up/down
			arg = rnd.opsImpl.Sub(arg, remainder)
			if rnd.opsImpl.less(rnd.halfPow10, remainder) {
				arg = rnd.opsImpl.Add(arg, rnd.pow10)
			}
		} else {
			// negative, round up/down
			arg = rnd.opsImpl.Sub(arg, remainder)
			if rnd.opsImpl.less(remainder, rnd.negHalfPow10) {
				arg = rnd.opsImpl.Sub(arg, rnd.pow10)
			}
		}
	} else {
		arg = rnd.fn(arg, remainder, rnd.pow10, rnd.pow)
	}

	if !rnd.opsImpl.fitsInPrec(arg, rnd.ty.GetPrecision()) {
		*e = fmt.Errorf("%w: rounded value %s does not fit in precision of %s",
			arrow.ErrInvalid, rnd.opsImpl.str(arg, rnd.ty.GetScale()), rnd.ty)
		return def
	}
	return arg
}

func getRoundKernelDecimal[T decimal128.Num | decimal256.Num]() exec.ArrayKernelExec {
	var def T
	switch any(def).(type) {
	case decimal128.Num:
		return func(ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
			return roundKernelDecimal(&roundDec128, ctx, batch, out)
		}
	case decimal256.Num:
		return func(ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
			return roundKernelDecimal(&roundDec256, ctx, batch, out)
		}
	}
	panic("should never get here")
}

func roundKernelDecimal[T decimal128.Num | decimal256.Num](opsImpl *roundDecImpl[T], ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
	opts := ctx.State.(RoundState)
	rnd := roundDec[T]{
		ty:      out.Type.(arrow.DecimalType),
		ndigits: opts.NDigits,
		mode:    opts.Mode,
		opsImpl: opsImpl,
		fn:      getDecRounding(opts.Mode, opsImpl),
	}

	rnd.pow = rnd.ty.GetScale() - int32(rnd.ndigits)
	if rnd.pow < rnd.ty.GetPrecision() && rnd.pow >= 0 {
		rnd.pow10 = opsImpl.scaleMultiplier(int(rnd.pow))
		rnd.halfPow10 = opsImpl.halfScaleMultiplier(int(rnd.pow))
		rnd.negHalfPow10 = opsImpl.Neg(rnd.halfPow10)
	}

	return ScalarUnaryNotNull(rnd.call)(ctx, batch, out)
}

func getRoundToMultipleKernelDecimal[T decimal128.Num | decimal256.Num]() exec.ArrayKernelExec {
	var def T
	switch any(def).(type) {
	case decimal128.Num:
		return func(ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
			return roundToMultipleDecimal(&roundDec128, ctx, batch, out)
		}
	case decimal256.Num:
		return func(ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
			return roundToMultipleDecimal(&roundDec256, ctx, batch, out)
		}
	}
	panic("should never get here")
}

func roundToMultipleDecimal[T decimal128.Num | decimal256.Num](opsImpl *roundDecImpl[T], ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
	opts := ctx.State.(RoundToMultipleState)
	rnd := roundToMultipleDec[T]{
		ty:      out.Type.(arrow.DecimalType),
		mode:    opts.Mode,
		opsImpl: opsImpl,
		fn:      getDecRounding(opts.Mode, opsImpl),
		mult:    UnboxScalar[T](opts.Multiple.(scalar.PrimitiveScalar)),
	}

	rnd.halfMult = opsImpl.Div(rnd.mult, opsImpl.fromI64(2))
	rnd.negHalfMult = opsImpl.Neg(rnd.halfMult)
	rnd.hasHalfwayPoint = opsImpl.lowBits(rnd.mult)%2 == 0

	return ScalarUnaryNotNull(rnd.call)(ctx, batch, out)
}

type roundToMultiple[T constraints.Float] struct {
	multiple T
	mode     RoundMode

	fn func(T) T
}

func (rnd *roundToMultiple[T]) call(_ *exec.KernelCtx, arg T, e *error) T {
	val := float64(arg)
	// do not process Inf or NaN because they will trigger the overflow error
	// at the end of this.
	if math.IsInf(val, 0) || math.IsNaN(val) {
		return arg
	}

	roundVal := arg / rnd.multiple
	frac := roundVal - T(math.Floor(float64(roundVal)))
	if frac == 0 {
		// scaled value is an integer, no rounding needed
		return arg
	}

	if rnd.mode >= HalfDown && frac != 0.5 {
		roundVal = T(math.Round(float64(roundVal)))
	} else {
		roundVal = rnd.fn(roundVal)
	}
	roundVal *= rnd.multiple

	if math.IsInf(float64(roundVal), 0) || math.IsNaN(float64(roundVal)) {
		*e = errOverflow
		return arg
	}

	return roundVal
}

type roundToMultipleDec[T decimal128.Num | decimal256.Num] struct {
	ty   arrow.DecimalType
	mode RoundMode

	mult, halfMult, negHalfMult T
	hasHalfwayPoint             bool

	opsImpl *roundDecImpl[T]
	fn      func(T, T, T, int32) T
}

func (rnd *roundToMultipleDec[T]) call(_ *exec.KernelCtx, arg T, e *error) T {
	var def T

	val, remainder := rnd.opsImpl.divide(arg, rnd.mult)
	if remainder == def {
		return arg
	}

	one := rnd.opsImpl.fromI64(1)
	if rnd.mode >= HalfDown {
		if rnd.hasHalfwayPoint && (remainder == rnd.halfMult || remainder == rnd.negHalfMult) {
			// on the halfway point, use tiebreaker
			// manually implement rounding since we aren't actually rounding
			// a decimal value, but rather manipulating the multiple
			switch rnd.mode {
			case HalfDown:
				if rnd.opsImpl.Sign(remainder) < 0 {
					val = rnd.opsImpl.Sub(val, one)
				}
			case HalfUp:
				if rnd.opsImpl.Sign(remainder) >= 0 {
					val = rnd.opsImpl.Add(val, one)
				}
			case HalfTowardsZero:
			case HalfAwayFromZero:
				if rnd.opsImpl.Sign(remainder) >= 0 {
					val = rnd.opsImpl.Add(val, one)
				} else {
					val = rnd.opsImpl.Sub(val, one)
				}
			case HalfToEven:
				if rnd.opsImpl.lowBits(val)%2 != 0 {
					if rnd.opsImpl.Sign(remainder) >= 0 {
						val = rnd.opsImpl.Add(val, one)
					} else {
						val = rnd.opsImpl.Sub(val, one)
					}
				}
			case HalfToOdd:
				if rnd.opsImpl.lowBits(val)%2 == 0 {
					if rnd.opsImpl.Sign(remainder) >= 0 {
						val = rnd.opsImpl.Add(val, one)
					} else {
						val = rnd.opsImpl.Sub(val, one)
					}
				}
			}
		} else if rnd.opsImpl.Sign(remainder) >= 0 {
			// positive, round up/down
			if rnd.opsImpl.less(rnd.halfMult, remainder) {
				val = rnd.opsImpl.Add(val, one)
			}
		} else {
			// negative, round up/down
			if rnd.opsImpl.less(remainder, rnd.negHalfMult) {
				val = rnd.opsImpl.Sub(val, one)
			}
		}
	} else {
		// manually implement rounding since we aren't actually rounding
		// a decimal value, but rather manipulating the multiple
		switch rnd.mode {
		case RoundDown:
			if rnd.opsImpl.Sign(remainder) < 0 {
				val = rnd.opsImpl.Sub(val, one)
			}
		case RoundUp:
			if rnd.opsImpl.Sign(remainder) >= 0 {
				val = rnd.opsImpl.Add(val, one)
			}
		case TowardsZero:
		case AwayFromZero:
			if rnd.opsImpl.Sign(remainder) >= 0 {
				val = rnd.opsImpl.Add(val, one)
			} else {
				val = rnd.opsImpl.Sub(val, one)
			}
		}
	}

	roundVal := rnd.opsImpl.Mul(val, rnd.mult)
	if !rnd.opsImpl.fitsInPrec(roundVal, rnd.ty.GetPrecision()) {
		*e = fmt.Errorf("%w: rounded value %s does not fit in precision of %s",
			arrow.ErrInvalid, rnd.opsImpl.str(roundVal, rnd.ty.GetScale()), rnd.ty)
		return def
	}
	return roundVal
}

func UnaryRoundExec(ty arrow.Type) exec.ArrayKernelExec {
	switch ty {
	case arrow.FLOAT32:
		return roundKernelFloating[float32]
	case arrow.FLOAT64:
		return roundKernelFloating[float64]
	case arrow.DECIMAL128:
		return getRoundKernelDecimal[decimal128.Num]()
	case arrow.DECIMAL256:
		return getRoundKernelDecimal[decimal256.Num]()
	}
	panic("should never get here")
}

func UnaryRoundToMultipleExec(ty arrow.Type) exec.ArrayKernelExec {
	switch ty {
	case arrow.FLOAT32:
		return roundToMultipleFloating[float32]
	case arrow.FLOAT64:
		return roundToMultipleFloating[float64]
	case arrow.DECIMAL128:
		return getRoundToMultipleKernelDecimal[decimal128.Num]()
	case arrow.DECIMAL256:
		return getRoundToMultipleKernelDecimal[decimal256.Num]()
	}
	panic("should never get here")
}

func GetRoundUnaryKernels(init exec.KernelInitFn, knFn func(arrow.Type) exec.ArrayKernelExec) []exec.ScalarKernel {
	kernels := make([]exec.ScalarKernel, 0)
	for _, ty := range []arrow.DataType{arrow.PrimitiveTypes.Float32, arrow.PrimitiveTypes.Float64,
		&arrow.Decimal128Type{Precision: 1}, &arrow.Decimal256Type{Precision: 1}} {
		tyID := ty.ID()

		var out exec.OutputType
		if arrow.IsDecimal(tyID) {
			out = OutputFirstType
		} else {
			out = exec.NewOutputType(ty)
		}

		kernels = append(kernels, exec.NewScalarKernel(
			[]exec.InputType{exec.NewIDInput(tyID)}, out, knFn(tyID), init))
	}

	return append(kernels, NullExecKernel(1))
}

func GetSimpleRoundKernels(mode RoundMode) []exec.ScalarKernel {
	kernels := make([]exec.ScalarKernel, 0)
	for _, ty := range floatingTypes {
		var ex exec.ArrayKernelExec
		switch ty.ID() {
		case arrow.FLOAT32:
			fn := getFloatRoundImpl[float32](mode)
			ex = ScalarUnary(func(_ *exec.KernelCtx, in []float32, out []float32) error {
				for i, v := range in {
					out[i] = fn(v)
				}
				return nil
			})
		case arrow.FLOAT64:
			fn := getFloatRoundImpl[float64](mode)
			ex = ScalarUnary(func(_ *exec.KernelCtx, in []float64, out []float64) error {
				for i, v := range in {
					out[i] = fn(v)
				}
				return nil
			})
		}
		kernels = append(kernels, exec.NewScalarKernel(
			[]exec.InputType{exec.NewExactInput(ty)}, exec.NewOutputType(ty),
			ex, nil))
	}
	return append(kernels, NullExecKernel(1))
}

func fixedRoundDecimalExec[T decimal128.Num | decimal256.Num](opsImpl *roundDecImpl[T], mode RoundMode) exec.ArrayKernelExec {
	return func(ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
		rnd := roundDec[T]{
			ty:      out.Type.(arrow.DecimalType),
			mode:    mode,
			opsImpl: opsImpl,
			fn:      getDecRounding(mode, opsImpl),
		}

		rnd.pow = rnd.ty.GetScale() - int32(rnd.ndigits)
		if rnd.pow < rnd.ty.GetPrecision() && rnd.pow >= 0 {
			rnd.pow10 = opsImpl.scaleMultiplier(int(rnd.pow))
			rnd.halfPow10 = opsImpl.halfScaleMultiplier(int(rnd.pow))
			rnd.negHalfPow10 = opsImpl.Neg(rnd.halfPow10)
		}

		return ScalarUnaryNotNull(rnd.call)(ctx, batch, out)
	}
}

func FixedRoundDecimalExec[T decimal128.Num | decimal256.Num](mode RoundMode) exec.ArrayKernelExec {
	var def T
	switch any(def).(type) {
	case decimal128.Num:
		return func() exec.ArrayKernelExec {
			return fixedRoundDecimalExec(&roundDec128, mode)
		}()
	case decimal256.Num:
		return func() exec.ArrayKernelExec {
			return fixedRoundDecimalExec(&roundDec256, mode)
		}()
	}
	panic("should never get here")
}

// RoundTemporalUnit represents units supported for temporal rounding
type RoundTemporalUnit int8

const (
	RoundTemporalYear RoundTemporalUnit = iota
	RoundTemporalQuarter
	RoundTemporalMonth
	RoundTemporalWeek
	RoundTemporalDay
	RoundTemporalHour
	RoundTemporalMinute
	RoundTemporalSecond
	RoundTemporalMillisecond
	RoundTemporalMicrosecond
	RoundTemporalNanosecond
)

// RoundTemporalOptions provides configuration for temporal rounding operations
type RoundTemporalOptions struct {
	// Multiple is the number of units to round to. Must be positive.
	Multiple int64
	// Unit is the rounding unit (day, hour, etc.)
	Unit RoundTemporalUnit
	// WeekStartsMonday determines the start of the week for week-based rounding
	WeekStartsMonday bool
	// CeilIsStrictlyGreater: if true, ceil returns a value strictly greater than input
	CeilIsStrictlyGreater bool
	// CalendarBasedOrigin: if true, use calendar units as origin (e.g., start of day for hours)
	CalendarBasedOrigin bool
}

func (RoundTemporalOptions) TypeName() string { return "RoundTemporalOptions" }

type roundTemporalState struct {
	RoundTemporalOptions
	mode RoundMode

	// Pre-calculated values to avoid repeated computation
	unitNanos        int64 // Duration of the unit in nanoseconds
	roundingInterval int64 // unitNanos * Multiple
	isSubDay         bool  // true if this is a sub-day unit (can use fast path)
}

func InitRoundTemporalState(_ *exec.KernelCtx, args exec.KernelInitArgs) (exec.KernelState, error) {
	var rs roundTemporalState

	opts, ok := args.Options.(*RoundTemporalOptions)
	if ok {
		rs.RoundTemporalOptions = *opts
	} else {
		if rs.RoundTemporalOptions, ok = args.Options.(RoundTemporalOptions); !ok {
			return nil, fmt.Errorf("%w: attempted to initialize kernel state from invalid function options",
				arrow.ErrInvalid)
		}
	}

	if rs.Multiple <= 0 {
		return nil, fmt.Errorf("%w: rounding multiple must be positive", arrow.ErrInvalid)
	}

	// Pre-calculate constants for this rounding operation
	rs.unitNanos, rs.isSubDay = unitInNanos(rs.Unit)
	if rs.isSubDay {
		var err error
		rs.roundingInterval, err = checkedMulInt64(rs.unitNanos, rs.Multiple)
		if err != nil {
			return nil, err
		}
	}

	return rs, nil
}

// unitInNanos returns (nanoseconds, hasFixedDuration) for a temporal unit.
// Returns false for calendar units with variable durations (year, quarter, month, week).
func unitInNanos(unit RoundTemporalUnit) (int64, bool) {
	switch unit {
	case RoundTemporalNanosecond:
		return 1, true
	case RoundTemporalMicrosecond:
		return 1000, true
	case RoundTemporalMillisecond:
		return 1000000, true
	case RoundTemporalSecond:
		return 1000000000, true
	case RoundTemporalMinute:
		return 60 * 1000000000, true
	case RoundTemporalHour:
		return 3600 * 1000000000, true
	case RoundTemporalDay:
		return 86400 * 1000000000, true
	default:
		return 0, false
	}
}

// roundTimestamp rounds a timestamp value according to the specified options.
// tz specifies the timezone for calendar-aware rounding (nil defaults to UTC).
func roundTimestamp(ts int64, inputUnit arrow.TimeUnit, tz *time.Location, opts roundTemporalState) (int64, error) {
	if tz == nil {
		tz = time.UTC
	}

	// Calendar units with variable duration (year, quarter, month, week) require date arithmetic.
	if !opts.isSubDay {
		return roundTimestampCalendar(ts, inputUnit, tz, opts)
	}

	// A day is a calendar unit when it is associated with a timezone. This also
	// handles CalendarBasedOrigin for day multiples, whose origin is the start
	// of the month rather than the Unix epoch.
	if opts.Unit == RoundTemporalDay {
		return roundTimestampCalendar(ts, inputUnit, tz, opts)
	}

	// Zoned timestamps are rounded in their local wall-clock timeline. This is
	// observable for offsets that are not whole hours (for example, Kathmandu)
	// and around daylight-saving transitions.
	if tz != time.UTC && tz.String() != "UTC" {
		if opts.CalendarBasedOrigin {
			return roundTimestampCalendarOrigin(ts, inputUnit, tz, opts)
		}
		return roundTimestampFixedTimezone(ts, inputUnit, tz, opts)
	}

	// CalendarBasedOrigin rounds fixed-duration units against the preceding
	// calendar boundary (for example, minutes from the start of the hour).
	// Use local wall-clock arithmetic here: time.Time.Sub measures elapsed time
	// and therefore gives the wrong answer across daylight-saving transitions.
	if opts.CalendarBasedOrigin {
		return roundTimestampCalendarOrigin(ts, inputUnit, tz, opts)
	}

	// Sub-day units (hour, minute, second, etc.) use fixed-duration arithmetic.
	// Fast path: round directly in input unit when possible. A finer interval that
	// divides the input unit is already aligned and needs no calculation.
	if canRoundInInputUnit(inputUnit, opts.roundingInterval) {
		inputUnitNanos := int64(inputUnit.Multiplier())
		if opts.roundingInterval < inputUnitNanos {
			// Every input value is already aligned to a finer interval that divides
			// the input unit. Keep the calculation in the input unit so wider
			// timestamp ranges do not overflow during a nanosecond conversion.
			return ts, nil
		}
		intervalInInputUnit := opts.roundingInterval / inputUnitNanos
		return roundToMultipleInt64(ts, intervalInInputUnit, opts.mode, opts.CeilIsStrictlyGreater)
	}

	// Slow path: calculate the floor in nanoseconds, then cast the floor and
	// ceiling back to the input resolution before comparing them. This mirrors
	// the C++ kernel's duration_cast behavior when the requested interval is not
	// representable in the input unit.
	return roundTimestampInInputUnit(ts, inputUnit, opts)
}

func roundTimestampInInputUnit(ts int64, inputUnit arrow.TimeUnit, opts roundTemporalState) (int64, error) {
	// Keep the input in the target unit's arithmetic domain. Converting a wide
	// timestamp to nanoseconds first would reject values that are valid in the
	// requested unit (for example timestamp[s] in year 2300 rounded to 3 ms).
	inputUnitNanos := big.NewInt(int64(inputUnit.Multiplier()))
	valueNanos := new(big.Int).Mul(big.NewInt(ts), inputUnitNanos)
	floorNanos := floorBigIntToMultiple(valueNanos, opts.roundingInterval)
	targetUnit := big.NewInt(opts.unitNanos)
	floorTarget := new(big.Int).Quo(new(big.Int).Set(floorNanos), targetUnit)
	if !floorTarget.IsInt64() {
		return 0, overflowError()
	}
	floorInput := new(big.Int).Quo(floorNanos, inputUnitNanos)
	if !floorInput.IsInt64() {
		return 0, overflowError()
	}
	floor := floorInput.Int64()

	ceil := floor
	increment := opts.roundingInterval / inputUnitNanos.Int64()
	if opts.CeilIsStrictlyGreater || floor < ts {
		var err error
		ceil, err = checkedAddInt64(floor, increment)
		if err != nil {
			return 0, err
		}
	}

	switch opts.mode {
	case RoundDown:
		return floor, nil
	case RoundUp:
		return ceil, nil
	default:
		fromFloor := new(big.Int).Sub(big.NewInt(ts), big.NewInt(floor))
		toCeil := new(big.Int).Sub(big.NewInt(ceil), big.NewInt(ts))
		if fromFloor.Cmp(toCeil) >= 0 {
			return ceil, nil
		}
		return floor, nil
	}
}

// canRoundInInputUnit checks if rounding can be done in the input unit
// without converting to nanoseconds (true when either unit is evenly divisible
// by the other).
func canRoundInInputUnit(inputUnit arrow.TimeUnit, roundingIntervalNanos int64) bool {
	if roundingIntervalNanos <= 0 {
		return false
	}

	inputUnitNanos := int64(inputUnit.Multiplier())
	return roundingIntervalNanos%inputUnitNanos == 0 ||
		(roundingIntervalNanos < inputUnitNanos && inputUnitNanos%roundingIntervalNanos == 0)
}

// roundTimestampCalendarOrigin rounds a fixed-duration unit relative to the
// preceding calendar boundary. Candidate boundaries are calculated in local
// wall time, then compared as system times so daylight-saving transitions are
// handled consistently with the temporal rounding reference implementation.
func roundTimestampCalendarOrigin(ts int64, inputUnit arrow.TimeUnit, tz *time.Location, opts roundTemporalState) (int64, error) {
	t, err := timestampToCalendarTime(ts, inputUnit, tz)
	if err != nil {
		return 0, err
	}
	origin, err := fixedCalendarOrigin(t, opts.Unit)
	if err != nil {
		return 0, err
	}
	return roundTimestampFromWallOrigin(t, inputUnit, tz, opts, origin)
}

func roundTimestampFixedTimezone(ts int64, inputUnit arrow.TimeUnit, tz *time.Location, opts roundTemporalState) (int64, error) {
	t, err := timestampToCalendarTime(ts, inputUnit, tz)
	if err != nil {
		return 0, err
	}
	origin := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	return roundTimestampFromWallOrigin(t, inputUnit, tz, opts, origin)
}

func roundTimestampFromWallOrigin(t time.Time, inputUnit arrow.TimeUnit, tz *time.Location, opts roundTemporalState, origin time.Time) (int64, error) {
	inputTime := t
	wallTime := calendarWallTime(t)

	elapsed, err := wallClockDifference(wallTime, origin)
	var floor time.Time
	if err == nil {
		flooredElapsed, roundErr := roundToMultipleInt64(elapsed, opts.roundingInterval, RoundDown, false)
		if roundErr != nil {
			return 0, roundErr
		}
		floor, err = addWallNanos(origin, flooredElapsed, tz)
	} else {
		// Keep the wide timestamp range available for zoned timestamps. The
		// reference implementation performs this arithmetic in the input
		// duration, while an int64 nanosecond distance only spans about 292
		// years.
		wideElapsed, wideErr := wallClockDifferenceBig(wallTime, origin)
		if wideErr != nil {
			return 0, wideErr
		}
		flooredElapsed := floorBigIntToMultiple(wideElapsed, opts.roundingInterval)
		floor, err = addWallNanosBig(origin, flooredElapsed, tz)
	}
	if err != nil {
		return 0, err
	}
	floorWall := calendarWallTime(floor.In(tz))
	floorWallAtInputResolution, err := truncateWallToInputResolution(floorWall, origin, inputUnit)
	if err != nil {
		return 0, err
	}
	floorAtInputResolution, err := checkedLocalTime(floorWallAtInputResolution, tz)
	if err != nil {
		return 0, err
	}

	var rounded time.Time
	switch opts.mode {
	case RoundDown:
		rounded = floorAtInputResolution
	case RoundUp:
		rounded, err = ceilCalendarOrigin(inputTime, floorAtInputResolution, inputUnit, opts, tz)
		if err != nil {
			return 0, err
		}
	default:
		ceil, ceilErr := ceilCalendarOrigin(inputTime, floorAtInputResolution, inputUnit, opts, tz)
		if ceilErr != nil {
			return 0, ceilErr
		}
		rounded, err = halfRoundPeriod(inputTime, floorAtInputResolution, ceil)
		if err != nil {
			return 0, err
		}
	}

	result, err := timestampFromCalendarTime(rounded, inputUnit)
	if err != nil {
		return 0, err
	}
	return int64(result), nil
}

func truncateWallToInputResolution(value, origin time.Time, inputUnit arrow.TimeUnit) (time.Time, error) {
	elapsed, err := wallClockDifferenceBig(value, origin)
	if err != nil {
		return time.Time{}, err
	}
	unitNanos := big.NewInt(int64(inputUnit.Multiplier()))
	elapsed.Quo(elapsed, unitNanos)
	elapsed.Mul(elapsed, unitNanos)
	return addWallNanosBig(origin, elapsed, time.UTC)
}

func ceilCalendarOrigin(value, floor time.Time, inputUnit arrow.TimeUnit, opts roundTemporalState, tz *time.Location) (time.Time, error) {
	// Convert the floored system time back to local wall time before deciding
	// whether to advance. This matters when the period crosses a timezone
	// transition because the elapsed system duration need not equal the wall
	// clock duration.
	floorWall := calendarWallTime(floor.In(tz))
	cs, err := checkedLocalTime(floorWall, tz)
	if err != nil {
		return time.Time{}, err
	}
	if !opts.CeilIsStrictlyGreater && !cs.Before(value) {
		return cs, nil
	}

	inputUnitNanos := int64(inputUnit.Multiplier())
	increment := (opts.roundingInterval / inputUnitNanos) * inputUnitNanos
	nextWall, err := addWallNanos(floorWall, increment, tz)
	if err != nil {
		return time.Time{}, err
	}
	return nextWall, nil
}

// Keep the origin's civil fields in UTC because its local time may not exist.
// The requested location is applied only after rounding the wall-clock value.
func fixedCalendarOrigin(value time.Time, unit RoundTemporalUnit) (time.Time, error) {
	switch unit {
	case RoundTemporalHour:
		return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC), nil
	case RoundTemporalMinute:
		return time.Date(value.Year(), value.Month(), value.Day(), value.Hour(), 0, 0, 0, time.UTC), nil
	case RoundTemporalSecond:
		return time.Date(value.Year(), value.Month(), value.Day(), value.Hour(), value.Minute(), 0, 0, time.UTC), nil
	case RoundTemporalMillisecond:
		return time.Date(value.Year(), value.Month(), value.Day(), value.Hour(), value.Minute(), value.Second(), 0, time.UTC), nil
	case RoundTemporalMicrosecond:
		return time.Date(value.Year(), value.Month(), value.Day(), value.Hour(), value.Minute(), value.Second(),
			(value.Nanosecond()/1_000_000)*1_000_000, time.UTC), nil
	case RoundTemporalNanosecond:
		return time.Date(value.Year(), value.Month(), value.Day(), value.Hour(), value.Minute(), value.Second(),
			(value.Nanosecond()/1_000)*1_000, time.UTC), nil
	default:
		return time.Time{}, fmt.Errorf("%w: unsupported fixed calendar unit", arrow.ErrNotImplemented)
	}
}

func wallClockNanos(value time.Time) (int64, error) {
	seconds := int64(value.Hour())*3600 + int64(value.Minute())*60 + int64(value.Second())
	nanos, err := checkedMulInt64(seconds, int64(time.Second))
	if err != nil {
		return 0, err
	}
	return checkedAddInt64(nanos, int64(value.Nanosecond()))
}

func wallClockDifference(value, origin time.Time) (int64, error) {
	days, err := calendarDayDifference(value, origin)
	if err != nil {
		return 0, err
	}
	daysNanos, err := checkedMulInt64(days, nanosPerDay)
	if err != nil {
		return 0, err
	}
	valueNanos, err := wallClockNanos(value)
	if err != nil {
		return 0, err
	}
	originNanos, err := wallClockNanos(origin)
	if err != nil {
		return 0, err
	}
	daysNanos, err = checkedAddInt64(daysNanos, valueNanos)
	if err != nil {
		return 0, err
	}
	return checkedSubInt64(daysNanos, originNanos)
}

func wallClockDifferenceBig(value, origin time.Time) (*big.Int, error) {
	days, err := calendarDayDifference(value, origin)
	if err != nil {
		return nil, err
	}
	valueNanos, err := wallClockNanos(value)
	if err != nil {
		return nil, err
	}
	originNanos, err := wallClockNanos(origin)
	if err != nil {
		return nil, err
	}

	result := new(big.Int).Mul(big.NewInt(days), big.NewInt(nanosPerDay))
	result.Add(result, big.NewInt(valueNanos))
	result.Sub(result, big.NewInt(originNanos))
	return result, nil
}

func floorBigIntToMultiple(value *big.Int, multiple int64) *big.Int {
	divisor := big.NewInt(multiple)
	quotient := new(big.Int)
	remainder := new(big.Int)
	quotient.QuoRem(value, divisor, remainder)
	if remainder.Sign() < 0 {
		quotient.Sub(quotient, big.NewInt(1))
	}
	return quotient.Mul(quotient, divisor)
}

func calendarWallTime(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), value.Hour(), value.Minute(), value.Second(), value.Nanosecond(), time.UTC)
}

func checkedLocalTime(value time.Time, tz *time.Location) (time.Time, error) {
	result := time.Date(value.Year(), value.Month(), value.Day(), value.Hour(), value.Minute(), value.Second(), value.Nanosecond(), tz)
	if result.Year() != value.Year() || result.Month() != value.Month() || result.Day() != value.Day() ||
		result.Hour() != value.Hour() || result.Minute() != value.Minute() || result.Second() != value.Second() ||
		result.Nanosecond() != value.Nanosecond() {
		return time.Time{}, fmt.Errorf("%w: local time does not exist", arrow.ErrInvalid)
	}
	if ambiguousLocalTime(value, result, tz) {
		return time.Time{}, fmt.Errorf("%w: local time is ambiguous", arrow.ErrInvalid)
	}
	return result, nil
}

func ambiguousLocalTime(value, result time.Time, tz *time.Location) bool {
	_, selectedOffset := result.Zone()
	wall := calendarWallTime(value)
	for _, delta := range []time.Duration{-2 * time.Hour, 2 * time.Hour, -48 * time.Hour, 48 * time.Hour} {
		_, offset := result.Add(delta).Zone()
		if offset == selectedOffset {
			continue
		}
		candidate := wall.Add(-time.Duration(offset) * time.Second).In(tz)
		if candidate.Year() == value.Year() && candidate.Month() == value.Month() && candidate.Day() == value.Day() &&
			candidate.Hour() == value.Hour() && candidate.Minute() == value.Minute() &&
			candidate.Second() == value.Second() && candidate.Nanosecond() == value.Nanosecond() {
			return true
		}
	}
	return false
}

// addWallNanos adds a duration to a local wall-clock value without using
// time.Time.Add, which applies elapsed-time arithmetic across timezone
// transitions. The result is normalized through calendar dates in UTC and then
// reconstructed in the requested location.
func addWallNanos(origin time.Time, nanos int64, tz *time.Location) (time.Time, error) {
	days := floorDivInt64(nanos, nanosPerDay)
	daysNanos, err := checkedMulInt64(days, nanosPerDay)
	if err != nil {
		return time.Time{}, err
	}
	remainder, err := checkedSubInt64(nanos, daysNanos)
	if err != nil {
		return time.Time{}, err
	}

	originNanos, err := wallClockNanos(origin)
	if err != nil {
		return time.Time{}, err
	}
	dayTimeNanos, err := checkedAddInt64(originNanos, remainder)
	if err != nil {
		return time.Time{}, err
	}
	if dayTimeNanos >= nanosPerDay {
		days, err = checkedAddInt64(days, 1)
		if err != nil {
			return time.Time{}, err
		}
		dayTimeNanos -= nanosPerDay
	}

	date := time.Date(origin.Year(), origin.Month(), origin.Day(), 0, 0, 0, 0, time.UTC)
	date, err = checkedCalendarAddDays(date, days)
	if err != nil {
		return time.Time{}, err
	}

	hour := dayTimeNanos / int64(time.Hour)
	dayTimeNanos -= hour * int64(time.Hour)
	minute := dayTimeNanos / int64(time.Minute)
	dayTimeNanos -= minute * int64(time.Minute)
	second := dayTimeNanos / int64(time.Second)
	nanosecond := dayTimeNanos - second*int64(time.Second)
	return checkedLocalTime(time.Date(date.Year(), date.Month(), date.Day(), int(hour), int(minute), int(second), int(nanosecond), time.UTC), tz)
}

func addWallNanosBig(origin time.Time, nanos *big.Int, tz *time.Location) (time.Time, error) {
	days := new(big.Int)
	remainder := new(big.Int)
	days.QuoRem(nanos, big.NewInt(nanosPerDay), remainder)
	if remainder.Sign() < 0 {
		days.Sub(days, big.NewInt(1))
		remainder.Add(remainder, big.NewInt(nanosPerDay))
	}

	originNanos, err := wallClockNanos(origin)
	if err != nil {
		return time.Time{}, err
	}
	dayTimeNanos := new(big.Int).Add(remainder, big.NewInt(originNanos))
	if dayTimeNanos.Cmp(big.NewInt(nanosPerDay)) >= 0 {
		days.Add(days, big.NewInt(1))
		dayTimeNanos.Sub(dayTimeNanos, big.NewInt(nanosPerDay))
	}
	if !days.IsInt64() {
		return time.Time{}, overflowError()
	}

	date := time.Date(origin.Year(), origin.Month(), origin.Day(), 0, 0, 0, 0, time.UTC)
	date, err = checkedCalendarAddDays(date, days.Int64())
	if err != nil {
		return time.Time{}, err
	}
	dayTime := dayTimeNanos.Int64()
	hour := dayTime / int64(time.Hour)
	dayTime -= hour * int64(time.Hour)
	minute := dayTime / int64(time.Minute)
	dayTime -= minute * int64(time.Minute)
	second := dayTime / int64(time.Second)
	nanosecond := dayTime - second*int64(time.Second)
	return checkedLocalTime(time.Date(date.Year(), date.Month(), date.Day(), int(hour), int(minute), int(second), int(nanosecond), time.UTC), tz)
}

func overflowError() error {
	return fmt.Errorf("%w: temporal rounding overflow", arrow.ErrInvalid)
}

// time.Time's calendar representation wraps for Unix seconds before this
// boundary. time.Unix(math.MaxInt64, 0) also overflows its internal signed
// seconds field. Keep both cases out of calendar arithmetic before constructing
// a time.Time. The public timestamp conversion intentionally remains bijective
// over the full int64 range, so this check belongs in the calendar kernel.
const minCalendarTimestampSecond = -9223372028741760000

func timestampToCalendarTime(ts int64, inputUnit arrow.TimeUnit, tz *time.Location) (time.Time, error) {
	if inputUnit == arrow.Second && (ts < minCalendarTimestampSecond || ts == math.MaxInt64) {
		return time.Time{}, overflowError()
	}
	return arrow.Timestamp(ts).ToTime(inputUnit).In(tz), nil
}

func checkedAddInt64(left, right int64) (int64, error) {
	if (right > 0 && left > math.MaxInt64-right) || (right < 0 && left < math.MinInt64-right) {
		return 0, overflowError()
	}
	return left + right, nil
}

func checkedSubInt64(left, right int64) (int64, error) {
	if (right > 0 && left < math.MinInt64+right) || (right < 0 && left > math.MaxInt64+right) {
		return 0, overflowError()
	}
	return left - right, nil
}

func checkedMulInt64(left, right int64) (int64, error) {
	if left == 0 || right == 0 {
		return 0, nil
	}
	if (left == math.MinInt64 && right == -1) || (right == math.MinInt64 && left == -1) {
		return 0, overflowError()
	}

	result := left * right
	if result/right != left {
		return 0, overflowError()
	}
	return result, nil
}

func checkedInt64ToInt(value int64) (int, error) {
	result := int(value)
	if int64(result) != value {
		return 0, overflowError()
	}
	return result, nil
}

// floorDivInt64 divides by a positive divisor and rounds towards negative
// infinity. Go's integer division rounds towards zero, which would place
// negative calendar values in the wrong period.
func floorDivInt64(dividend, divisor int64) int64 {
	quotient := dividend / divisor
	if dividend%divisor < 0 {
		quotient--
	}
	return quotient
}

func checkedCalendarDate(year, month int64, tz *time.Location) (time.Time, error) {
	dateYear, err := checkedInt64ToInt(year)
	if err != nil {
		return time.Time{}, err
	}
	dateMonth, err := checkedInt64ToInt(month)
	if err != nil {
		return time.Time{}, err
	}
	wall := time.Date(dateYear, time.Month(dateMonth), 1, 0, 0, 0, 0, time.UTC)
	if int64(wall.Year()) != year {
		return time.Time{}, overflowError()
	}
	return checkedLocalTime(wall, tz)
}

func checkedCalendarMidnight(year int, month time.Month, day int, tz *time.Location) (time.Time, error) {
	return checkedLocalTime(time.Date(year, month, day, 0, 0, 0, 0, time.UTC), tz)
}

func checkedCalendarAddDays(value time.Time, days int64) (time.Time, error) {
	dayOffset, err := checkedInt64ToInt(days)
	if err != nil {
		return time.Time{}, err
	}
	result := value.AddDate(0, 0, dayOffset)
	if (days > 0 && result.Before(value)) || (days < 0 && result.After(value)) {
		return time.Time{}, overflowError()
	}
	return result, nil
}

func roundToMultipleInt64(value, multiple int64, mode RoundMode, strictCeil bool) (int64, error) {
	if multiple == 0 || value%multiple == 0 {
		if strictCeil && mode == RoundUp {
			return checkedAddInt64(value, multiple)
		}
		return value, nil
	}

	quotient := value / multiple
	remainder := value % multiple

	switch mode {
	case RoundDown:
		if remainder < 0 {
			quotient, err := checkedSubInt64(quotient, 1)
			if err != nil {
				return 0, err
			}
			return checkedMulInt64(quotient, multiple)
		}
		return checkedMulInt64(quotient, multiple)
	case RoundUp:
		if remainder > 0 || (strictCeil && remainder == 0) {
			quotient, err := checkedAddInt64(quotient, 1)
			if err != nil {
				return 0, err
			}
			return checkedMulInt64(quotient, multiple)
		}
		if remainder < 0 {
			return checkedMulInt64(quotient, multiple)
		}
		quotient, err := checkedAddInt64(quotient, 1)
		if err != nil {
			return 0, err
		}
		return checkedMulInt64(quotient, multiple)
	case HalfUp, HalfDown, HalfToEven:
		half := multiple / 2
		absRemainder := remainder
		if absRemainder < 0 {
			absRemainder = -absRemainder
		}

		// Odd multiples do not have an exact halfway point. For example,
		// a remainder of 1 when rounding to multiples of 3 is closer to 0
		// than to 3, so it must not be treated as a tie.
		if absRemainder < half || (multiple%2 != 0 && absRemainder == half) {
			return checkedMulInt64(quotient, multiple)
		} else if absRemainder > half {
			if remainder > 0 {
				quotient, err := checkedAddInt64(quotient, 1)
				if err != nil {
					return 0, err
				}
				return checkedMulInt64(quotient, multiple)
			}
			quotient, err := checkedSubInt64(quotient, 1)
			if err != nil {
				return 0, err
			}
			return checkedMulInt64(quotient, multiple)
		} else {
			// Exactly on the halfway point
			switch mode {
			case HalfDown:
				if remainder > 0 {
					return checkedMulInt64(quotient, multiple)
				}
				quotient, err := checkedSubInt64(quotient, 1)
				if err != nil {
					return 0, err
				}
				return checkedMulInt64(quotient, multiple)
			case HalfUp:
				if remainder > 0 {
					quotient, err := checkedAddInt64(quotient, 1)
					if err != nil {
						return 0, err
					}
					return checkedMulInt64(quotient, multiple)
				}
				return checkedMulInt64(quotient, multiple)
			case HalfToEven:
				if quotient%2 == 0 {
					return checkedMulInt64(quotient, multiple)
				}
				if remainder > 0 {
					quotient, err := checkedAddInt64(quotient, 1)
					if err != nil {
						return 0, err
					}
					return checkedMulInt64(quotient, multiple)
				}
				quotient, err := checkedSubInt64(quotient, 1)
				if err != nil {
					return 0, err
				}
				return checkedMulInt64(quotient, multiple)
			}
		}
	}
	return checkedMulInt64(quotient, multiple)
}

// halfRoundPeriod performs half-rounding by finding the midpoint between period start and end.
// It does not use time.Time.Sub or time.Time.Unix because those methods cannot represent
// sufficiently large calendar periods and timestamps.
func halfRoundPeriod(t, periodStart, periodEnd time.Time) (time.Time, error) {
	startDays, startNanos, err := calendarTimeParts(periodStart)
	if err != nil {
		return time.Time{}, err
	}
	endDays, endNanos, err := calendarTimeParts(periodEnd)
	if err != nil {
		return time.Time{}, err
	}
	if endDays < startDays || (endDays == startDays && endNanos < startNanos) {
		return time.Time{}, overflowError()
	}
	deltaDays, err := checkedSubInt64(endDays, startDays)
	if err != nil {
		return time.Time{}, err
	}
	deltaNanos, err := checkedSubInt64(endNanos, startNanos)
	if err != nil {
		return time.Time{}, err
	}
	if deltaNanos < 0 {
		deltaDays, err = checkedSubInt64(deltaDays, 1)
		if err != nil {
			return time.Time{}, err
		}
		deltaNanos += nanosPerDay
	}

	tDays, tNanos, err := calendarTimeParts(t)
	if err != nil {
		return time.Time{}, err
	}
	relativeDays, err := checkedSubInt64(tDays, startDays)
	if err != nil {
		return time.Time{}, err
	}
	relativeNanos, err := checkedSubInt64(tNanos, startNanos)
	if err != nil {
		return time.Time{}, err
	}
	if relativeNanos < 0 {
		relativeDays, err = checkedSubInt64(relativeDays, 1)
		if err != nil {
			return time.Time{}, err
		}
		relativeNanos += nanosPerDay
	}

	// Compare 2*(t-periodStart) with periodEnd-periodStart without ever
	// converting the whole interval to a time.Duration.
	doubledDays, err := checkedMulInt64(relativeDays, 2)
	if err != nil {
		return time.Time{}, err
	}
	doubledNanos, err := checkedMulInt64(relativeNanos, 2)
	if err != nil {
		return time.Time{}, err
	}
	if doubledNanos >= nanosPerDay {
		doubledDays, err = checkedAddInt64(doubledDays, 1)
		if err != nil {
			return time.Time{}, err
		}
		doubledNanos -= nanosPerDay
	}

	if doubledDays < deltaDays || (doubledDays == deltaDays && doubledNanos < deltaNanos) {
		return periodStart, nil
	}
	return periodEnd, nil
}

const nanosPerDay = int64(24 * time.Hour)

// calendarTimeParts returns a time instant as a day number and nanoseconds into
// the corresponding UTC day. Unlike Unix timestamps, this representation
// remains valid for all time.Time values supported by the time package.
func calendarTimeParts(value time.Time) (int64, int64, error) {
	days, err := calendarDays(value)
	if err != nil {
		return 0, 0, err
	}

	seconds := int64(value.Hour())*3600 + int64(value.Minute())*60 + int64(value.Second())
	nanos, err := checkedMulInt64(seconds, int64(time.Second))
	if err != nil {
		return 0, 0, err
	}
	nanos, err = checkedAddInt64(nanos, int64(value.Nanosecond()))
	if err != nil {
		return 0, 0, err
	}
	_, offset := value.Zone()
	offsetNanos, err := checkedMulInt64(int64(offset), int64(time.Second))
	if err != nil {
		return 0, 0, err
	}
	nanos, err = checkedSubInt64(nanos, offsetNanos)
	if err != nil {
		return 0, 0, err
	}

	if nanos < 0 {
		days, err = checkedSubInt64(days, 1)
		if err != nil {
			return 0, 0, err
		}
		nanos += nanosPerDay
	} else if nanos >= nanosPerDay {
		days, err = checkedAddInt64(days, 1)
		if err != nil {
			return 0, 0, err
		}
		nanos -= nanosPerDay
	}

	return days, nanos, nil
}

// calendarDays returns the proleptic Gregorian day number for a local date.
// The calculation is based on civil calendar fields, so it does not saturate
// at time.Duration or int64 Unix-second boundaries.
func calendarDays(value time.Time) (int64, error) {
	year := int64(value.Year())
	month := int64(value.Month())
	day := int64(value.Day())
	if month <= 2 {
		var err error
		year, err = checkedSubInt64(year, 1)
		if err != nil {
			return 0, err
		}
	}

	era := year / 400
	if year < 0 && year%400 != 0 {
		era--
	}
	eraYear, err := checkedMulInt64(era, 400)
	if err != nil {
		return 0, err
	}
	yearOfEra, err := checkedSubInt64(year, eraYear)
	if err != nil {
		return 0, err
	}

	monthOfMarch := month
	if month > 2 {
		monthOfMarch -= 3
	} else {
		monthOfMarch += 9
	}
	daysOfYear := (153*monthOfMarch+2)/5 + day - 1
	daysOfEra := yearOfEra*365 + yearOfEra/4 - yearOfEra/100 + daysOfYear
	days, err := checkedMulInt64(era, 146097)
	if err != nil {
		return 0, err
	}
	days, err = checkedAddInt64(days, daysOfEra)
	if err != nil {
		return 0, err
	}
	return checkedSubInt64(days, 719468)
}

func calendarDayDifference(left, right time.Time) (int64, error) {
	leftDays, err := calendarDays(left)
	if err != nil {
		return 0, err
	}
	rightDays, err := calendarDays(right)
	if err != nil {
		return 0, err
	}
	return checkedSubInt64(leftDays, rightDays)
}

// floorCalendarIndex returns the start of the current calendar period as an
// index in either months or quarters. Without a calendar origin, periods are
// anchored at 1970-01-01. With one, multiple periods start at the beginning of
// the current year.
func floorCalendarIndex(year, offset, periodsPerYear, multiple int64, calendarOrigin bool) (int64, error) {
	yearIndex, err := checkedMulInt64(year, periodsPerYear)
	if err != nil {
		return 0, err
	}
	currentIndex, err := checkedAddInt64(yearIndex, offset)
	if err != nil {
		return 0, err
	}

	if calendarOrigin && multiple != 1 {
		roundedOffset, err := checkedMulInt64(floorDivInt64(offset, multiple), multiple)
		if err != nil {
			return 0, err
		}
		return checkedAddInt64(yearIndex, roundedOffset)
	}

	epochIndex, err := checkedMulInt64(1970, periodsPerYear)
	if err != nil {
		return 0, err
	}
	relative, err := checkedSubInt64(currentIndex, epochIndex)
	if err != nil {
		return 0, err
	}
	roundedRelative, err := checkedMulInt64(floorDivInt64(relative, multiple), multiple)
	if err != nil {
		return 0, err
	}
	return checkedAddInt64(epochIndex, roundedRelative)
}

func calendarDateFromIndex(index, periodsPerYear, monthsPerPeriod int64, tz *time.Location) (time.Time, error) {
	year := floorDivInt64(index, periodsPerYear)
	yearIndex, err := checkedMulInt64(year, periodsPerYear)
	if err != nil {
		return time.Time{}, err
	}
	offset, err := checkedSubInt64(index, yearIndex)
	if err != nil {
		return time.Time{}, err
	}
	monthOffset, err := checkedMulInt64(offset, monthsPerPeriod)
	if err != nil {
		return time.Time{}, err
	}
	month, err := checkedAddInt64(monthOffset, 1)
	if err != nil {
		return time.Time{}, err
	}
	return checkedCalendarDate(year, month, tz)
}

func floorCalendarDay(value time.Time, multiple int64, calendarOrigin bool, tz *time.Location) (time.Time, error) {
	valueDay := time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
	if multiple == 1 {
		return checkedCalendarMidnight(value.Year(), value.Month(), value.Day(), tz)
	}

	origin := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	if calendarOrigin {
		origin = time.Date(value.Year(), value.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	daysSinceOrigin, err := calendarDayDifference(valueDay, origin)
	if err != nil {
		return time.Time{}, err
	}
	roundedDays, err := checkedMulInt64(floorDivInt64(daysSinceOrigin, multiple), multiple)
	if err != nil {
		return time.Time{}, err
	}
	result, err := checkedCalendarAddDays(origin, roundedDays)
	if err != nil {
		return time.Time{}, err
	}
	return checkedCalendarMidnight(result.Year(), result.Month(), result.Day(), tz)
}

func floorCalendarWeek(value time.Time, multiple int64, weekStartsMonday bool, tz *time.Location) (time.Time, error) {
	valueDay := time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
	weekday := int(valueDay.Weekday())
	targetWeekday := time.Wednesday
	if weekStartsMonday {
		weekday = (weekday + 6) % 7
		targetWeekday = time.Thursday
	}
	weekStart, err := checkedCalendarAddDays(valueDay, -int64(weekday))
	if err != nil {
		return time.Time{}, err
	}
	// Use a fixed representative day from the input's week to identify the
	// calendar week-year. Adding a constant to the input can cross into the
	// following week for late-week values.
	weekYearDate, err := checkedCalendarAddDays(weekStart, 3)
	if err != nil {
		return time.Time{}, err
	}

	previousYear, err := checkedSubInt64(int64(weekYearDate.Year()), 1)
	if err != nil {
		return time.Time{}, err
	}
	december := time.Date(int(previousYear), time.December, 31, 0, 0, 0, 0, time.UTC)
	daysBack := (int(december.Weekday()) - int(targetWeekday) + 7) % 7
	lastWeekday, err := checkedCalendarAddDays(december, -int64(daysBack))
	if err != nil {
		return time.Time{}, err
	}
	origin, err := checkedCalendarAddDays(lastWeekday, 4)
	if err != nil {
		return time.Time{}, err
	}

	daysSinceOrigin, err := calendarDayDifference(valueDay, origin)
	if err != nil {
		return time.Time{}, err
	}
	weeksSinceOrigin := floorDivInt64(daysSinceOrigin, 7)
	roundedWeeks, err := checkedMulInt64(floorDivInt64(weeksSinceOrigin, multiple), multiple)
	if err != nil {
		return time.Time{}, err
	}
	roundedDays, err := checkedMulInt64(roundedWeeks, 7)
	if err != nil {
		return time.Time{}, err
	}
	result, err := checkedCalendarAddDays(origin, roundedDays)
	if err != nil {
		return time.Time{}, err
	}
	return checkedLocalTime(result, tz)
}

func floorFixedCalendarWeek(value time.Time, multiple int64, weekStartsMonday bool, tz *time.Location) (time.Time, error) {
	weekdayOffset := int64(4)
	if weekStartsMonday {
		weekdayOffset = 3
	}

	// The reference algorithm shifts the local wall-clock timestamp by the
	// weekday offset, floors fixed seven-day periods from the Unix epoch, and
	// only then converts the candidate back to system time. Keep the wall-clock
	// date in UTC while doing the arithmetic so a missing local midnight cannot
	// move the period into a different civil date.
	shifted, err := checkedCalendarAddDays(calendarWallTime(value), weekdayOffset)
	if err != nil {
		return time.Time{}, err
	}
	shiftedDay := time.Date(shifted.Year(), shifted.Month(), shifted.Day(), 0, 0, 0, 0, time.UTC)
	dayIndex, err := calendarDays(shiftedDay)
	if err != nil {
		return time.Time{}, err
	}
	weekIndex := floorDivInt64(dayIndex, 7)
	roundedWeeks, err := checkedMulInt64(floorDivInt64(weekIndex, multiple), multiple)
	if err != nil {
		return time.Time{}, err
	}
	roundedDays, err := checkedMulInt64(roundedWeeks, 7)
	if err != nil {
		return time.Time{}, err
	}
	boundaryWall, err := checkedCalendarAddDays(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), roundedDays)
	if err != nil {
		return time.Time{}, err
	}
	weekStartWall, err := checkedCalendarAddDays(boundaryWall, -weekdayOffset)
	if err != nil {
		return time.Time{}, err
	}
	return time.Date(weekStartWall.Year(), weekStartWall.Month(), weekStartWall.Day(), 0, 0, 0, 0, tz), nil
}

func ceilFixedCalendarWeek(value, floor time.Time, multiple int64, strict bool, tz *time.Location) (time.Time, error) {
	floorWall := calendarWallTime(floor.In(tz))
	cs, err := checkedLocalTime(floorWall, tz)
	if err != nil {
		return time.Time{}, err
	}
	if !strict && !cs.Before(value) {
		return cs, nil
	}

	weekDays, err := checkedMulInt64(multiple, 7)
	if err != nil {
		return time.Time{}, err
	}
	nextWall, err := checkedCalendarAddDays(floorWall, weekDays)
	if err != nil {
		return time.Time{}, err
	}
	return checkedLocalTime(nextWall, tz)
}

func roundCalendarOriginWeek(value time.Time, opts roundTemporalState, tz *time.Location) (time.Time, error) {
	floor, err := floorCalendarWeek(value, opts.Multiple, opts.WeekStartsMonday, tz)
	if err != nil {
		return time.Time{}, err
	}
	if opts.mode == RoundDown {
		return floor, nil
	}

	var next time.Time
	makeNext := func() (time.Time, error) {
		if next.IsZero() {
			weekDays, err := checkedMulInt64(opts.Multiple, 7)
			if err != nil {
				return time.Time{}, err
			}
			nextWall, err := checkedCalendarAddDays(calendarWallTime(floor.In(tz)), weekDays)
			if err != nil {
				return time.Time{}, err
			}
			next, err = checkedLocalTime(nextWall, tz)
			if err != nil {
				return time.Time{}, err
			}
		}
		return next, nil
	}

	cs := floor

	switch opts.mode {
	case RoundUp:
		if opts.CeilIsStrictlyGreater || cs.Before(value) {
			return makeNext()
		}
		return cs, nil
	default:
		if cs.After(value) {
			return cs, nil
		}
		next, err := makeNext()
		if err != nil {
			return time.Time{}, err
		}
		return halfRoundPeriod(value, floor, next)
	}
}

// roundTimestampCalendar handles calendar-based rounding (year, quarter, month, week, day).
// Requires date arithmetic for variable-length periods and timezone-aware boundaries.
func roundTimestampCalendar(ts int64, inputUnit arrow.TimeUnit, tz *time.Location, opts roundTemporalState) (int64, error) {
	// Convert to time.Time for calendar operations in the specified timezone
	t, err := timestampToCalendarTime(ts, inputUnit, tz)
	if err != nil {
		return 0, err
	}

	var rounded time.Time
	multiple := opts.Multiple

	switch opts.Unit {
	case RoundTemporalYear:
		year := int64(t.Year())
		roundedYear, err := checkedMulInt64(floorDivInt64(year, multiple), multiple)
		if err != nil {
			return 0, err
		}
		switch opts.mode {
		case RoundDown:
			var dateErr error
			rounded, dateErr = checkedCalendarDate(roundedYear, 1, tz)
			if dateErr != nil {
				return 0, dateErr
			}
		case RoundUp:
			roundedYear, err = checkedAddInt64(roundedYear, multiple)
			if err != nil {
				return 0, err
			}
			rounded, err = checkedCalendarDate(roundedYear, 1, tz)
			if err != nil {
				return 0, err
			}
		default:
			yearStart, dateErr := checkedCalendarDate(roundedYear, 1, tz)
			if dateErr != nil {
				return 0, dateErr
			}
			nextYear, dateErr := checkedAddInt64(roundedYear, multiple)
			if dateErr != nil {
				return 0, dateErr
			}
			yearEnd, dateErr := checkedCalendarDate(nextYear, 1, tz)
			if dateErr != nil {
				return 0, dateErr
			}
			rounded, err = halfRoundPeriod(t, yearStart, yearEnd)
			if err != nil {
				return 0, err
			}
		}

	case RoundTemporalQuarter:
		// Q1=Jan-Mar, Q2=Apr-Jun, Q3=Jul-Sep, Q4=Oct-Dec
		month := int64(t.Month())
		year := int64(t.Year())
		roundedQuarters, err := floorCalendarIndex(
			year, (month-1)/3, 4, multiple, opts.CalendarBasedOrigin,
		)
		if err != nil {
			return 0, err
		}

		switch opts.mode {
		case RoundDown:
			rounded, err = calendarDateFromIndex(roundedQuarters, 4, 3, tz)
			if err != nil {
				return 0, err
			}
		case RoundUp:
			nextQuarters, dateErr := checkedAddInt64(roundedQuarters, multiple)
			if dateErr != nil {
				return 0, dateErr
			}
			rounded, dateErr = calendarDateFromIndex(nextQuarters, 4, 3, tz)
			if dateErr != nil {
				return 0, dateErr
			}
		default:
			quarterStart, dateErr := calendarDateFromIndex(roundedQuarters, 4, 3, tz)
			if dateErr != nil {
				return 0, dateErr
			}
			nextQuarterNum, dateErr := checkedAddInt64(roundedQuarters, multiple)
			if dateErr != nil {
				return 0, dateErr
			}
			quarterEnd, dateErr := calendarDateFromIndex(nextQuarterNum, 4, 3, tz)
			if dateErr != nil {
				return 0, dateErr
			}
			rounded, err = halfRoundPeriod(t, quarterStart, quarterEnd)
			if err != nil {
				return 0, err
			}
		}

	case RoundTemporalMonth:
		month := int64(t.Month())
		year := int64(t.Year())
		roundedMonths, err := floorCalendarIndex(
			year, month-1, 12, multiple, opts.CalendarBasedOrigin,
		)
		if err != nil {
			return 0, err
		}

		switch opts.mode {
		case RoundDown:
			rounded, err = calendarDateFromIndex(roundedMonths, 12, 1, tz)
			if err != nil {
				return 0, err
			}
		case RoundUp:
			nextMonths, dateErr := checkedAddInt64(roundedMonths, multiple)
			if dateErr != nil {
				return 0, dateErr
			}
			rounded, dateErr = calendarDateFromIndex(nextMonths, 12, 1, tz)
			if dateErr != nil {
				return 0, dateErr
			}
		default:
			monthStart, dateErr := calendarDateFromIndex(roundedMonths, 12, 1, tz)
			if dateErr != nil {
				return 0, dateErr
			}
			nextMonthNum, dateErr := checkedAddInt64(roundedMonths, multiple)
			if dateErr != nil {
				return 0, dateErr
			}
			monthEnd, dateErr := calendarDateFromIndex(nextMonthNum, 12, 1, tz)
			if dateErr != nil {
				return 0, dateErr
			}
			rounded, err = halfRoundPeriod(t, monthStart, monthEnd)
			if err != nil {
				return 0, err
			}
		}

	case RoundTemporalWeek:
		if opts.CalendarBasedOrigin && multiple != 1 {
			rounded, err = roundCalendarOriginWeek(t, opts, tz)
			if err != nil {
				return 0, err
			}
			break
		}

		floor, dateErr := floorFixedCalendarWeek(t, multiple, opts.WeekStartsMonday, tz)
		if dateErr != nil {
			return 0, dateErr
		}
		ceil, dateErr := ceilFixedCalendarWeek(t, floor, multiple, opts.CeilIsStrictlyGreater, tz)
		if dateErr != nil {
			return 0, dateErr
		}

		switch opts.mode {
		case RoundDown:
			rounded = floor
		case RoundUp:
			rounded = ceil
		default:
			rounded, err = halfRoundPeriod(t, floor, ceil)
			if err != nil {
				return 0, err
			}
		}

	case RoundTemporalDay:
		startOfDay, dateErr := floorCalendarDay(t, multiple, opts.CalendarBasedOrigin, tz)
		if dateErr != nil {
			return 0, dateErr
		}
		makeNextDay := func() (time.Time, error) {
			nextWall, err := checkedCalendarAddDays(calendarWallTime(startOfDay), multiple)
			if err != nil {
				return time.Time{}, err
			}
			return checkedLocalTime(nextWall, tz)
		}

		switch opts.mode {
		case RoundDown:
			rounded = startOfDay
		case RoundUp:
			if opts.CeilIsStrictlyGreater || !t.Equal(startOfDay) {
				rounded, dateErr = makeNextDay()
				if dateErr != nil {
					return 0, dateErr
				}
			} else {
				rounded = startOfDay
			}
		default:
			nextDay, nextErr := makeNextDay()
			if nextErr != nil {
				return 0, nextErr
			}
			rounded, err = halfRoundPeriod(t, startOfDay, nextDay)
			if err != nil {
				return 0, err
			}
		}

	default:
		return 0, fmt.Errorf("%w: unsupported calendar unit", arrow.ErrNotImplemented)
	}
	if err != nil {
		return 0, err
	}

	// Convert back to the input unit, validating only the selected result.
	roundedTimestamp, err := timestampFromCalendarTime(rounded, inputUnit)
	if err != nil {
		return 0, err
	}
	return int64(roundedTimestamp), nil
}

func timestampFromCalendarTime(value time.Time, unit arrow.TimeUnit) (arrow.Timestamp, error) {
	if unit != arrow.Second {
		return arrow.TimestampFromTime(value, unit)
	}

	days, nanos, err := calendarTimeParts(value)
	if err != nil {
		return 0, err
	}
	seconds, err := checkedMulInt64(days, int64(24*time.Hour/time.Second))
	if err != nil {
		return 0, err
	}
	seconds, err = checkedAddInt64(seconds, nanos/int64(time.Second))
	if err != nil {
		return 0, err
	}
	return arrow.Timestamp(seconds), nil
}

func timeToNanos(value time.Time) (int64, error) {
	timestamp, err := arrow.TimestampFromTime(value, arrow.Nanosecond)
	if err != nil {
		return 0, err
	}
	return int64(timestamp), nil
}

// Kernel execution functions for temporal rounding
func FloorTemporalKernel(ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
	state := ctx.State.(roundTemporalState)
	state.mode = RoundDown
	return roundTemporalExec(ctx, batch, out, state)
}

func CeilTemporalKernel(ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
	state := ctx.State.(roundTemporalState)
	state.mode = RoundUp
	return roundTemporalExec(ctx, batch, out, state)
}

func RoundTemporalKernel(ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult) error {
	state := ctx.State.(roundTemporalState)
	state.mode = HalfUp
	return roundTemporalExec(ctx, batch, out, state)
}

func roundTemporalExec(ctx *exec.KernelCtx, batch *exec.ExecSpan, out *exec.ExecResult, state roundTemporalState) error {
	input := &batch.Values[0].Array

	// Handle date types by converting to timestamp equivalents
	switch input.Type.ID() {
	case arrow.DATE32:
		// Date32 stores days since epoch as int32, treat as timestamp[s] at midnight
		fn := func(_ *exec.KernelCtx, days int32, e *error) int32 {
			// Convert days to seconds (timestamp at midnight UTC)
			tsSeconds := int64(days) * 86400
			result, err := roundTimestamp(tsSeconds, arrow.Second, nil, state)
			if err != nil {
				*e = err
				return 0
			}
			// Convert back to days
			daysResult := result / 86400
			if daysResult < math.MinInt32 || daysResult > math.MaxInt32 {
				*e = overflowError()
				return 0
			}
			return int32(daysResult)
		}
		return ScalarUnaryNotNull(fn)(ctx, batch, out)

	case arrow.DATE64:
		// Date64 stores milliseconds since epoch, treat as timestamp[ms]
		fn := func(_ *exec.KernelCtx, ms int64, e *error) int64 {
			result, err := roundTimestamp(ms, arrow.Millisecond, nil, state)
			if err != nil {
				*e = err
				return 0
			}
			return result
		}
		return ScalarUnaryNotNull(fn)(ctx, batch, out)

	case arrow.TIME32:
		// Time32 stores time-of-day in seconds or milliseconds
		// Rounding wraps at day boundaries (modulo 24 hours)
		timeType := input.Type.(*arrow.Time32Type)
		fn := func(_ *exec.KernelCtx, time int32, e *error) int32 {
			// Convert to int64 for rounding
			result, err := roundTimestamp(int64(time), timeType.Unit, nil, state)
			if err != nil {
				*e = err
				return 0
			}
			// Wrap at day boundary
			var dayInUnit int64
			if timeType.Unit == arrow.Second {
				dayInUnit = 86400 // 24 hours in seconds
			} else {
				dayInUnit = 86400000 // 24 hours in milliseconds
			}
			wrapped := result % dayInUnit
			if wrapped < 0 {
				wrapped += dayInUnit
			}
			return int32(wrapped)
		}
		return ScalarUnaryNotNull(fn)(ctx, batch, out)

	case arrow.TIME64:
		// Time64 stores time-of-day in microseconds or nanoseconds
		// Rounding wraps at day boundaries (modulo 24 hours)
		timeType := input.Type.(*arrow.Time64Type)
		fn := func(_ *exec.KernelCtx, time int64, e *error) int64 {
			result, err := roundTimestamp(time, timeType.Unit, nil, state)
			if err != nil {
				*e = err
				return 0
			}
			// Wrap at day boundary
			var dayInUnit int64
			if timeType.Unit == arrow.Microsecond {
				dayInUnit = 86400000000 // 24 hours in microseconds
			} else {
				dayInUnit = 86400000000000 // 24 hours in nanoseconds
			}
			wrapped := result % dayInUnit
			if wrapped < 0 {
				wrapped += dayInUnit
			}
			return wrapped
		}
		return ScalarUnaryNotNull(fn)(ctx, batch, out)
	}

	// Handle timestamp types
	inputType := input.Type.(arrow.TemporalWithUnit)

	// Extract timezone if present (for timestamp types)
	var tz *time.Location
	if tsType, ok := input.Type.(*arrow.TimestampType); ok && tsType.TimeZone != "" {
		var err error
		tz, err = tsType.GetZone()
		if err != nil {
			return fmt.Errorf("%w: invalid timezone %q: %v", arrow.ErrInvalid, tsType.TimeZone, err)
		}
	}

	fn := func(_ *exec.KernelCtx, ts int64, e *error) int64 {
		result, err := roundTimestamp(ts, inputType.TimeUnit(), tz, state)
		if err != nil {
			*e = err
			return 0
		}
		return result
	}

	switch inputType.TimeUnit() {
	case arrow.Second, arrow.Millisecond, arrow.Microsecond, arrow.Nanosecond:
		return ScalarUnaryNotNull(fn)(ctx, batch, out)
	default:
		return fmt.Errorf("%w: unsupported time unit", arrow.ErrNotImplemented)
	}
}

func GetTemporalRoundingKernels(init exec.KernelInitFn, execFn exec.ArrayKernelExec) []exec.ScalarKernel {
	kernels := make([]exec.ScalarKernel, 0)

	// Timestamp kernels
	for _, unit := range arrow.TimeUnitValues {
		kernels = append(kernels, exec.NewScalarKernel(
			[]exec.InputType{exec.NewMatchedInput(exec.TimestampTypeUnit(unit))},
			OutputFirstType,
			execFn,
			init,
		))
	}

	// Date32 kernel
	kernels = append(kernels, exec.NewScalarKernel(
		[]exec.InputType{exec.NewMatchedInput(exec.Date32Type())},
		OutputFirstType,
		execFn,
		init,
	))

	// Date64 kernel
	kernels = append(kernels, exec.NewScalarKernel(
		[]exec.InputType{exec.NewMatchedInput(exec.Date64Type())},
		OutputFirstType,
		execFn,
		init,
	))

	// Time32 kernels (seconds and milliseconds)
	kernels = append(kernels, exec.NewScalarKernel(
		[]exec.InputType{exec.NewMatchedInput(exec.Time32TypeUnit(arrow.Second))},
		OutputFirstType,
		execFn,
		init,
	))
	kernels = append(kernels, exec.NewScalarKernel(
		[]exec.InputType{exec.NewMatchedInput(exec.Time32TypeUnit(arrow.Millisecond))},
		OutputFirstType,
		execFn,
		init,
	))

	// Time64 kernels (microseconds and nanoseconds)
	kernels = append(kernels, exec.NewScalarKernel(
		[]exec.InputType{exec.NewMatchedInput(exec.Time64TypeUnit(arrow.Microsecond))},
		OutputFirstType,
		execFn,
		init,
	))
	kernels = append(kernels, exec.NewScalarKernel(
		[]exec.InputType{exec.NewMatchedInput(exec.Time64TypeUnit(arrow.Nanosecond))},
		OutputFirstType,
		execFn,
		init,
	))

	return append(kernels, NullExecKernel(1))
}
