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

//go:build go1.18 && arm64 && !noasm && !appengine

#include "textflag.h"

// Each filter byte selects up to eight uint16 values. TBL compacts one
// sixteen-byte input vector, and stores write exactly the selected bytes.
// func _filter_uint16_neon(values, filter, output, tables unsafe.Pointer, length int64)
TEXT ·_filter_uint16_neon(SB), NOSPLIT|NOFRAME, $0-40
	MOVD values+0(FP), R0
	MOVD filter+8(FP), R1
	MOVD output+16(FP), R2
	MOVD tables+24(FP), R3
	MOVD length+32(FP), R4

	LSR $3, R4, R4
	ADD $4096, R3, R8

filter_uint16_loop:
	MOVBU (R1), R5
	ADD $1, R1
	CBZ R5, filter_uint16_next
	CMPW $255, R5
	BEQ filter_uint16_all

	MOVBU (R8)(R5<<0), R6
	CBZ R6, filter_uint16_next

	LSL $4, R5, R7
	ADD R3, R7, R7
	VLD1 (R0), [V0.B16]
	VLD1 (R7), [V1.B16]
	VTBL V1.B16, [V0.B16], V2.B16

	CMPW $4, R6
	BLT filter_uint16_store_small
	VST1 [V2.H4], (R2)
	ADD $8, R2
	SUB $4, R6
	CBZ R6, filter_uint16_next
	CMPW $1, R6
	BEQ filter_uint16_store_high1
	CMPW $2, R6
	BEQ filter_uint16_store_high2
	VMOV V2.H[4], R9
	MOVH R9, (R2)
	VMOV V2.H[5], R9
	MOVH R9, 2(R2)
	VMOV V2.H[6], R9
	MOVH R9, 4(R2)
	ADD $6, R2
	B filter_uint16_next

filter_uint16_store_high1:
	VMOV V2.H[4], R9
	MOVH R9, (R2)
	ADD $2, R2
	B filter_uint16_next

filter_uint16_store_high2:
	VMOV V2.H[4], R9
	MOVH R9, (R2)
	VMOV V2.H[5], R9
	MOVH R9, 2(R2)
	ADD $4, R2
	B filter_uint16_next

filter_uint16_store_small:
	CMPW $1, R6
	BEQ filter_uint16_store1
	CMPW $2, R6
	BEQ filter_uint16_store2
	VMOV V2.H[0], R9
	MOVH R9, (R2)
	VMOV V2.H[1], R9
	MOVH R9, 2(R2)
	VMOV V2.H[2], R9
	MOVH R9, 4(R2)
	ADD $6, R2
	B filter_uint16_next

filter_uint16_store1:
	VMOV V2.H[0], R9
	MOVH R9, (R2)
	ADD $2, R2
	B filter_uint16_next

filter_uint16_store2:
	VMOV V2.H[0], R9
	MOVH R9, (R2)
	VMOV V2.H[1], R9
	MOVH R9, 2(R2)
	ADD $4, R2
	B filter_uint16_next

filter_uint16_all:
	VLD1 (R0), [V0.B16]
	VST1 [V0.B16], (R2)
	ADD $16, R2

filter_uint16_next:
	ADD $16, R0
	SUBS $1, R4, R4
	BNE filter_uint16_loop
	RET
