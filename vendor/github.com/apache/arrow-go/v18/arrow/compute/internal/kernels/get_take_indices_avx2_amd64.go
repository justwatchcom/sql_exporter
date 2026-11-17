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
	"encoding/binary"
	"unsafe"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/bitutil"
	"github.com/apache/arrow-go/v18/arrow/compute/exec"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"golang.org/x/sys/cpu"
)

//go:noescape
func _get_take_indices_uint32_avx2(filter, output, tables unsafe.Pointer, nbytes, tailMask int64)

var getTakeIndicesUint32AVX2Tables = makeGetTakeIndicesUint32AVX2Tables()

func makeGetTakeIndicesUint32AVX2Tables() (tables [400]byte) {
	copy(tables[:], filterUint32Tables[:])
	for lane := 0; lane < 8; lane++ {
		binary.LittleEndian.PutUint32(tables[352+lane*4:], uint32(lane))
	}
	for lane := 0; lane < 4; lane++ {
		binary.LittleEndian.PutUint32(tables[384+lane*4:], 8)
	}
	return tables
}

func getTakeIndicesUint32AVX2(mem memory.Allocator, filter *exec.ArraySpan) (arrow.ArrayData, bool) {
	if !cpu.X86.HasAVX2 || filter.MayHaveNulls() || filter.Offset%8 != 0 || filter.Len < 64 {
		return nil, false
	}

	filterData := filter.Buffers[1].Buf
	filterByteOffset := filter.Offset / 8
	nbytes := (filter.Len + 7) / 8
	if filterByteOffset < 0 || filterByteOffset+nbytes > int64(len(filterData)) {
		return nil, false
	}
	filterBytes := filterData[filterByteOffset : filterByteOffset+nbytes]

	// VisitSetBitRuns is especially effective for long runs, so only use the
	// compactor when a short sample shows enough fragmented bytes to amortize
	// its setup cost.
	const (
		sampleBytes = 256
		minMixed    = 4
	)
	mixed := 0
	for i := int64(0); i < nbytes && i < sampleBytes; i++ {
		mask := filterBytes[i]
		if mask != 0 && mask != 0xff {
			mixed++
			if mixed == minMixed {
				break
			}
		}
	}
	if mixed < minMixed {
		return nil, false
	}

	length := int64(bitutil.CountSetBits(filterBytes, 0, int(filter.Len)))
	if length == 0 {
		return array.NewData(arrow.PrimitiveTypes.Uint32, 0, []*memory.Buffer{nil, memory.NewBufferBytes(nil)}, nil, 0, 0), true
	}

	outputBuf := memory.NewBufferWithAllocator(mem.Allocate(int(length)*4), mem)
	defer outputBuf.Release()
	output := arrow.GetData[uint32](outputBuf.Bytes())
	tailMask := int64(0xff)
	if tailBits := filter.Len & 7; tailBits != 0 {
		tailMask = int64((uint64(1) << uint(tailBits)) - 1)
	}
	_get_take_indices_uint32_avx2(
		unsafe.Pointer(unsafe.SliceData(filterBytes)),
		unsafe.Pointer(unsafe.SliceData(output)),
		unsafe.Pointer(unsafe.SliceData(getTakeIndicesUint32AVX2Tables[:])),
		nbytes,
		tailMask,
	)
	return array.NewData(arrow.PrimitiveTypes.Uint32, int(length), []*memory.Buffer{nil, outputBuf}, nil, 0, 0), true
}
