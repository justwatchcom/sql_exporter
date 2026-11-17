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

// Package compute is a native-go implementation of an Acero-like
// arrow compute engine.
//
// While consumers of Arrow that are able to use CGO could utilize the
// C Data API (using the cdata package) and could link against the
// acero library directly, there are consumers who cannot use CGO. This
// is an attempt to provide for those users, and in general create a
// native-go arrow compute engine.
//
// # What is implemented
//
// The function registry holds scalar functions (element-wise arithmetic,
// comparisons, boolean logic, bit-wise operations, rounding, set lookup with
// is_in, prefix matching with starts_with, list_element and the null
// checks), vector functions (array_filter, array_take, unique,
// dictionary_encode, cumulative_sum and the run-end encode/decode
// functions) and the meta functions cast, filter, take, sort
// and sort_indices that dispatch to them. It also holds the scalar aggregate
// functions count and sum, which fold a whole array into a single value
// through the Init, Consume, Merge and Finalize lifecycle of an
// exec.ScalarAggKernel. The remaining scalar aggregates (mean, min_max, min,
// max, any, all, variance and so on) and the hash aggregate functions (the
// hash_* family used for group-by) are not implemented yet: FuncHashAgg
// exists as a function kind, but no function of that kind is registered, and
// GetFunction returns false for their names.
//
// Aggregate functions are deliberately not available from the exprs package,
// which accepts scalar functions only, matching the C++ implementation where
// aggregations run through Acero rather than through expressions.
package compute

//go:generate go tool stringer -type=FuncKind -linecomment
