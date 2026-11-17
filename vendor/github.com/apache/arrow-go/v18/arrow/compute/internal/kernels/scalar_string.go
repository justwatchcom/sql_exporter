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

package kernels

import (
	"bytes"
	"fmt"
	"slices"
	"unicode"
	"unicode/utf8"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/bitutil"
	"github.com/apache/arrow-go/v18/arrow/compute/exec"
	"github.com/apache/arrow-go/v18/internal/bitutils"
)

// MatchSubstringOptions controls the substring matching functions such as
// starts_with, which match Pattern literally, not as a regular
// expression. With IgnoreCase, string input matches Unicode simple case
// folds and binary input matches ASCII case folds.
type MatchSubstringOptions struct {
	Pattern    string `compute:"pattern"`
	IgnoreCase bool   `compute:"ignore_case"`
}

func (MatchSubstringOptions) TypeName() string { return "MatchSubstringOptions" }

type matchSubstringState struct {
	pattern []byte
}

type foldUTF8State struct {
	pattern []rune
}

// foldASCIIState holds the pattern with each byte lowered by foldASCII.
type foldASCIIState struct {
	pattern []byte
}

func initMatchSubstring(utf8Input bool) exec.KernelInitFn {
	return func(_ *exec.KernelCtx, args exec.KernelInitArgs) (exec.KernelState, error) {
		opts, ok := args.Options.(MatchSubstringOptions)
		if p, isPtr := args.Options.(*MatchSubstringOptions); isPtr && p != nil {
			opts, ok = *p, true
		}
		if !ok {
			return nil, fmt.Errorf("%w: attempted to initialize kernel state from invalid function options",
				arrow.ErrInvalid)
		}

		switch {
		case !opts.IgnoreCase:
			return matchSubstringState{pattern: []byte(opts.Pattern)}, nil
		case utf8Input:
			if !utf8.ValidString(opts.Pattern) {
				return nil, fmt.Errorf("%w: case-insensitive pattern must be valid UTF-8 for string input",
					arrow.ErrInvalid)
			}
			return foldUTF8State{pattern: []rune(opts.Pattern)}, nil
		default:
			pattern := []byte(opts.Pattern)
			for i, b := range pattern {
				pattern[i] = foldASCII(b)
			}
			return foldASCIIState{pattern: pattern}, nil
		}
	}
}

func startsWith(ctx *exec.KernelCtx, v []byte, _ *error) bool {
	return bytes.HasPrefix(v, ctx.State.(matchSubstringState).pattern)
}

// viewStartsWith decides from the view header alone when it can, and
// reads the data buffer only for the part of pattern past the prefix.
func viewStartsWith(h *arrow.ViewHeader, data, pattern []byte) bool {
	if h.Len() < len(pattern) {
		return false
	}
	if h.IsInline() {
		return bytes.HasPrefix(h.InlineBytes(), pattern)
	}
	prefix := h.Prefix()
	n := min(len(pattern), arrow.ViewPrefixLen)
	if !bytes.Equal(prefix[:n], pattern[:n]) {
		return false
	}
	off := int(h.BufferOffset())
	return bytes.Equal(data[off+n:off+len(pattern)], pattern[n:])
}

func viewBytes(h *arrow.ViewHeader, data []byte) []byte {
	if h.IsInline() {
		return h.InlineBytes()
	}
	off := int(h.BufferOffset())
	return data[off : off+h.Len()]
}

// startsWithView reads out-of-line values from Buffers[2], so inputs with
// more than one data buffer must be coalesced before execution.
func startsWithView(ctx *exec.KernelCtx, in *exec.ExecSpan, out *exec.ExecResult) error {
	var (
		arg0       = &in.Values[0].Array
		views      = arrow.ViewHeaderTraits.CastFromBytes(arg0.Buffers[1].Buf)[arg0.Offset:]
		data       = arg0.Buffers[2].Buf
		outData    = out.Buffers[1].Buf
		outPos     = int(out.Offset)
		visitValid func(pos int64)
	)
	visitFold := func(fold func(*exec.KernelCtx, []byte, *error) bool) func(int64) {
		return func(pos int64) {
			bitutil.SetBitTo(outData, outPos, fold(ctx, viewBytes(&views[pos], data), nil))
			outPos++
		}
	}
	switch st := ctx.State.(type) {
	case matchSubstringState:
		pattern := st.pattern
		visitValid = func(pos int64) {
			bitutil.SetBitTo(outData, outPos, viewStartsWith(&views[pos], data, pattern))
			outPos++
		}
	case foldUTF8State:
		visitValid = visitFold(startsWithFoldUTF8)
	case foldASCIIState:
		visitValid = visitFold(startsWithFoldASCII)
	}
	bitutils.VisitBitBlocks(arg0.Buffers[0].Buf, arg0.Offset, arg0.Len, visitValid,
		func() {
			bitutil.ClearBit(outData, outPos)
			outPos++
		})
	return nil
}

func startsWithFoldUTF8(ctx *exec.KernelCtx, v []byte, _ *error) bool {
	for _, p := range ctx.State.(foldUTF8State).pattern {
		r, size := utf8.DecodeRune(v)
		if (r == utf8.RuneError && size <= 1) || !equalSimpleFold(r, p) {
			return false
		}
		v = v[size:]
	}
	return true
}

func equalSimpleFold(r, p rune) bool {
	for f := r; ; {
		if f == p {
			return true
		}
		if f = unicode.SimpleFold(f); f == r {
			return false
		}
	}
}

func startsWithFoldASCII(ctx *exec.KernelCtx, v []byte, _ *error) bool {
	pattern := ctx.State.(foldASCIIState).pattern
	if len(v) < len(pattern) {
		return false
	}
	for i, p := range pattern {
		if foldASCII(v[i]) != p {
			return false
		}
	}
	return true
}

func foldASCII(b byte) byte {
	if 'A' <= b && b <= 'Z' {
		return b + 0x20
	}
	return b
}

func startsWithExec[OffsetT int32 | int64]() exec.ArrayKernelExec {
	caseSensitive := ScalarUnaryNotNullBinaryArgBoolOut[OffsetT](false, startsWith)
	foldUTF8 := ScalarUnaryNotNullBinaryArgBoolOut[OffsetT](false, startsWithFoldUTF8)
	foldASCII := ScalarUnaryNotNullBinaryArgBoolOut[OffsetT](false, startsWithFoldASCII)
	return func(ctx *exec.KernelCtx, in *exec.ExecSpan, out *exec.ExecResult) error {
		switch ctx.State.(type) {
		case foldUTF8State:
			return foldUTF8(ctx, in, out)
		case foldASCIIState:
			return foldASCII(ctx, in, out)
		default:
			return caseSensitive(ctx, in, out)
		}
	}
}

// GetStartsWithKernels returns the kernels for the starts_with function
// over the binary, string and view types.
func GetStartsWithKernels() []exec.ScalarKernel {
	out := exec.NewOutputType(arrow.FixedWidthTypes.Boolean)
	types := append(slices.Clip(baseBinaryTypes), arrow.BinaryTypes.BinaryView, arrow.BinaryTypes.StringView)
	kernels := make([]exec.ScalarKernel, 0, len(types))
	for _, ty := range types {
		var ex exec.ArrayKernelExec
		switch ty.ID() {
		case arrow.BINARY_VIEW, arrow.STRING_VIEW:
			ex = startsWithView
		case arrow.LARGE_BINARY, arrow.LARGE_STRING:
			ex = startsWithExec[int64]()
		default:
			ex = startsWithExec[int32]()
		}
		k := exec.NewScalarKernel([]exec.InputType{exec.NewExactInput(ty)}, out, ex,
			initMatchSubstring(ty.(arrow.BinaryDataType).IsUtf8()))
		k.NullHandling = exec.NullIntersection
		k.MemAlloc = exec.MemPrealloc
		kernels = append(kernels, k)
	}
	return kernels
}
