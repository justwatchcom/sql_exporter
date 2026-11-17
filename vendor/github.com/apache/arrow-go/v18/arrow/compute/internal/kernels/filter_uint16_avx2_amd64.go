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

//go:build go1.18 && amd64 && !noasm && !appengine

package kernels

import (
	"unsafe"

	"golang.org/x/sys/cpu"
)

var filterUint16Avx2Tables = makeFilterShuffleTables(2)

//go:noescape
func _filter_uint16_avx2(values, filter, output, tables unsafe.Pointer, length int64)

func filterUint16Avx2(values []uint16, output []uint16, filterData []byte, filterOffset, length int64) bool {
	if !cpu.X86.HasAVX2 || length > int64(len(values)) || len(output) == 0 {
		return false
	}

	filterBytes, ok := filterVectorInput(filterData, filterOffset, length)
	if !ok {
		return false
	}

	_filter_uint16_avx2(
		unsafe.Pointer(unsafe.SliceData(values)),
		unsafe.Pointer(unsafe.SliceData(filterBytes)),
		unsafe.Pointer(unsafe.SliceData(output)),
		unsafe.Pointer(unsafe.SliceData(filterUint16Avx2Tables[:])),
		length,
	)
	return true
}
