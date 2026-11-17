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

package exec

import (
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/scalar"
)

// AggregateResult is the single value produced by a scalar aggregate
// kernel's finalize function.
//
// It exists because this package cannot import the compute package and
// therefore cannot name a Datum, while an aggregate result is not always a
// scalar: most aggregations produce one (sum, count, min_max as a struct
// scalar), but some, such as tdigest, produce an array. AggregateResult can
// hold either one and the executor in the compute package boxes it into the
// corresponding Datum.
//
// # Ownership
//
// An AggregateResult owns exactly one reference to the value it holds. A
// finalize function returns a result that it owns and hands over to its
// caller, which must eventually either Release it or Take the value out of
// it and become responsible for that reference. Cleaning up the aggregate
// state after finalize must not invalidate the returned result: a kernel
// whose state owns buffers that the result refers to has to retain them for
// the result (or copy them) before returning it.
type AggregateResult struct {
	sc   scalar.Scalar
	data arrow.ArrayData
}

// NewScalarResult constructs an aggregate result which owns the reference to
// the provided scalar.
func NewScalarResult(sc scalar.Scalar) *AggregateResult {
	return &AggregateResult{sc: sc}
}

// NewArrayResult constructs an aggregate result which owns the reference to
// the provided array data.
func NewArrayResult(data arrow.ArrayData) *AggregateResult {
	return &AggregateResult{data: data}
}

// IsScalar reports whether this result holds a scalar value.
func (r *AggregateResult) IsScalar() bool { return r.sc != nil }

// IsArray reports whether this result holds array data.
func (r *AggregateResult) IsArray() bool { return r.data != nil }

// Scalar returns the scalar this result holds, or nil if it holds array
// data. The reference remains owned by the result.
func (r *AggregateResult) Scalar() scalar.Scalar { return r.sc }

// ArrayData returns the array data this result holds, or nil if it holds a
// scalar. The reference remains owned by the result.
func (r *AggregateResult) ArrayData() arrow.ArrayData { return r.data }

// Type returns the data type of the value this result holds, or nil if it is
// empty.
func (r *AggregateResult) Type() arrow.DataType {
	switch {
	case r.sc != nil:
		return r.sc.DataType()
	case r.data != nil:
		return r.data.DataType()
	default:
		return nil
	}
}

// Take hands the owned value over to the caller and empties this result. The
// caller takes over the reference and is responsible for releasing it. The
// returned value is a scalar.Scalar, an arrow.ArrayData, or nil if the
// result was already empty.
func (r *AggregateResult) Take() any {
	switch {
	case r.sc != nil:
		sc := r.sc
		r.sc = nil
		return sc
	case r.data != nil:
		data := r.data
		r.data = nil
		return data
	default:
		return nil
	}
}

// Release releases the reference this result owns, if any, and empties it.
// It is safe to call more than once.
func (r *AggregateResult) Release() {
	if rel, ok := r.sc.(scalar.Releasable); ok {
		rel.Release()
	}
	if r.data != nil {
		r.data.Release()
	}
	r.sc, r.data = nil, nil
}

// ScalarAggConsume folds one span of input into the aggregate state found in
// ctx.State. Implementations must not modify the span: the executor reuses
// one ExecSpan for every chunk of the input.
type ScalarAggConsume = func(ctx *KernelCtx, span *ExecSpan) error

// ScalarAggMerge folds the src state into the dst state.
//
// dst is mutated in place and must hold the combined aggregation afterwards.
// src and dst must not alias, and both must have come from the same kernel's
// init function. src is left untouched by the merge and remains owned by the
// caller, which is free to reuse or clean it up; an implementation that wants
// to keep anything from src past the call has to take its own reference. When
// the kernel is Ordered, the caller must merge the partitions in the logical
// order of the input, that is, dst must cover a prefix of the input and src
// the partition that directly follows it.
type ScalarAggMerge = func(ctx *KernelCtx, src, dst KernelState) error

// ScalarAggFinalize produces the result of the aggregation from the state in
// ctx.State. It returns one owned result, see AggregateResult for what the
// ownership means for the state that produced it.
type ScalarAggFinalize = func(ctx *KernelCtx) (*AggregateResult, error)

// ScalarAggCleanup releases any resources held by an aggregate state. It is
// called exactly once for every state that the init function produced,
// whether the aggregation succeeded, saw no input, was cancelled, or failed in
// init, consume, merge or finalize. It must not invalidate a result that
// finalize already returned.
type ScalarAggCleanup = func(ctx *KernelCtx, state KernelState) error

// AggKernel builds on the base Kernel interface for aggregate execution
// kernels, which fold input into a state rather than producing an output
// value per span.
type AggKernel interface {
	Kernel
	Consume(*KernelCtx, *ExecSpan) error
	Merge(ctx *KernelCtx, src, dst KernelState) error
	Finalize(*KernelCtx) (*AggregateResult, error)
	Cleanup(ctx *KernelCtx, state KernelState) error
	IsOrdered() bool
}

// A ScalarAggKernel is the kernel implementation for a scalar aggregate
// function, which computes a single summary value from array input. The four
// necessary components of an aggregation kernel are the init, consume, merge
// and finalize functions:
//
//   - init (the embedded Kernel's init function): creates a new state.
//   - consume: folds one ExecSpan into the state in the KernelCtx.
//   - merge: combines one state with another.
//   - finalize: produces the result of the aggregation from the state in the
//     KernelCtx.
//
// A fifth, optional, cleanup function releases the resources of a state.
type ScalarAggKernel struct {
	kernel

	ConsumeFn  ScalarAggConsume
	MergeFn    ScalarAggMerge
	FinalizeFn ScalarAggFinalize
	CleanupFn  ScalarAggCleanup

	// Ordered indicates that this kernel requires its input in a defined
	// order, as an aggregation like "first" does. It is the caller of the
	// kernel which is responsible for passing the data in that order, and
	// for merging partitions in the logical order of the input. Kernels
	// whose result does not depend on the order of the input leave this
	// false.
	//
	// The executor in the compute package consumes the whole input into a single
	// state, in order, so the flag does not change what it does; it is a
	// contract for MergeAll and for callers that partition the input
	// themselves.
	Ordered bool
}

// NewScalarAggKernel constructs a new kernel for scalar aggregation,
// constructing a KernelSignature from the provided input and output types.
//
// The init function is required: the executor cannot consume without a state.
// The cleanup function and the Ordered flag are left at their zero values and
// can be set on the returned kernel.
func NewScalarAggKernel(in []InputType, out OutputType, init KernelInitFn,
	consume ScalarAggConsume, merge ScalarAggMerge, finalize ScalarAggFinalize) ScalarAggKernel {
	return NewScalarAggKernelWithSig(&KernelSignature{
		InputTypes: in,
		OutType:    out,
	}, init, consume, merge, finalize)
}

// NewScalarAggKernelWithSig is a convenience for when you already have a
// signature to use for constructing a kernel. It is equivalent to passing the
// components of the signature (input and output types) to NewScalarAggKernel.
func NewScalarAggKernelWithSig(sig *KernelSignature, init KernelInitFn,
	consume ScalarAggConsume, merge ScalarAggMerge, finalize ScalarAggFinalize) ScalarAggKernel {
	return ScalarAggKernel{
		kernel:     kernel{Signature: sig, Init: init},
		ConsumeFn:  consume,
		MergeFn:    merge,
		FinalizeFn: finalize,
	}
}

func (s *ScalarAggKernel) Consume(ctx *KernelCtx, span *ExecSpan) error {
	return s.ConsumeFn(ctx, span)
}

func (s *ScalarAggKernel) Merge(ctx *KernelCtx, src, dst KernelState) error {
	return s.MergeFn(ctx, src, dst)
}

func (s *ScalarAggKernel) Finalize(ctx *KernelCtx) (*AggregateResult, error) {
	return s.FinalizeFn(ctx)
}

// Cleanup calls this kernel's cleanup function for the provided state, or
// does nothing if the kernel does not have one.
func (s *ScalarAggKernel) Cleanup(ctx *KernelCtx, state KernelState) error {
	if s.CleanupFn == nil {
		return nil
	}
	return s.CleanupFn(ctx, state)
}

func (s *ScalarAggKernel) IsOrdered() bool { return s.Ordered }

var _ AggKernel = (*ScalarAggKernel)(nil)

// MergeAll folds every state in states into the first one and returns it,
// which is the state the caller then finalizes. It is the entry point for a
// caller which aggregated partitions of the input separately, and mirrors the
// C++ ScalarAggregateKernel::MergeAll.
//
// The states must all have come from this kernel's init function, and, when
// the kernel is Ordered, must be in the logical order of the input. Every
// state but the first is cleaned up once it has been merged in, whether or
// not the merge succeeded; on an error the first state is cleaned up as well
// and the returned state is nil, so that the caller never has to clean up a
// state MergeAll was given.
//
// With no states there is nothing to merge or to finalize, and MergeAll
// returns (nil, nil). A caller that ended up with zero partitions aggregates
// the empty input the way the executor does: create one state with the init
// function and finalize it, then clean it up.
func MergeAll(kernel AggKernel, ctx *KernelCtx, states []KernelState) (KernelState, error) {
	if len(states) == 0 {
		return nil, nil
	}

	dst := states[0]
	var err error
	for _, src := range states[1:] {
		if err == nil {
			err = kernel.Merge(ctx, src, dst)
		}
		if cleanupErr := kernel.Cleanup(ctx, src); cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}

	if err != nil {
		// best effort: the merge error is the one worth reporting
		_ = kernel.Cleanup(ctx, dst)
		return nil, err
	}
	return dst, nil
}
