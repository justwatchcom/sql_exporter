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

package compute

import (
	"context"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/compute/exec"
	"github.com/apache/arrow-go/v18/arrow/compute/internal/kernels"
)

// MatchSubstringOptions controls the substring matching functions such as
// starts_with, which match Pattern literally, not as a regular
// expression. With IgnoreCase, string and string_view input match Unicode
// simple case folds and require a valid UTF-8 pattern, while binary and
// binary_view input match ASCII case folds only. String case folding uses
// the tables of the Go toolchain's unicode package, so results can differ
// between Go versions.
type MatchSubstringOptions = kernels.MatchSubstringOptions

var startsWithDoc = FunctionDoc{
	Summary: "Check if strings start with a literal pattern",
	Description: "For each string in \"strings\", emit true iff it starts with a\n" +
		"given pattern. The pattern must be given in MatchSubstringOptions.\n" +
		"Null inputs emit null. Dictionary inputs are decoded to their value\n" +
		"type, and extension inputs use their storage type.",
	ArgNames:        []string{"strings"},
	OptionsType:     "MatchSubstringOptions",
	OptionsRequired: true,
}

type matchSubstringFunc struct {
	ScalarFunction
}

func (fn *matchSubstringFunc) Execute(ctx context.Context, opts FunctionOptions, args ...Datum) (Datum, error) {
	if len(args) == 1 {
		coalesced, err := coalesceMultiBufferViewDatum(ctx, args[0])
		if err != nil {
			return nil, err
		}
		if coalesced != nil {
			defer coalesced.Release()
			args = []Datum{coalesced}
		}
	}
	return execInternal(ctx, fn, opts, -1, args...)
}

func (fn *matchSubstringFunc) DispatchBest(vals ...arrow.DataType) (exec.Kernel, error) {
	ensureDictionaryDecoded(vals...)
	ensureNoExtensionType(vals...)
	return fn.DispatchExact(vals...)
}

// RegisterScalarString registers the starts_with function.
func RegisterScalarString(reg FunctionRegistry) {
	fn := &matchSubstringFunc{*NewScalarFunction("starts_with", Unary(), startsWithDoc)}
	for _, k := range kernels.GetStartsWithKernels() {
		if err := fn.AddKernel(k); err != nil {
			panic(err)
		}
	}
	reg.AddFunction(fn, false)
}

// StartsWith returns, for each value in input, whether it starts with
// opts.Pattern. Null values produce null. Dictionary inputs are decoded
// to their value type, and extension inputs use their storage type.
func StartsWith(ctx context.Context, opts MatchSubstringOptions, input Datum) (Datum, error) {
	return CallFunction(ctx, "starts_with", &opts, input)
}
