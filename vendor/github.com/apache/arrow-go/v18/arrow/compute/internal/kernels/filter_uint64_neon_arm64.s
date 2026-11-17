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

// Each filter byte selects up to four uint64 values from each of two
// four-value halves. The table-driven VTBL compacts one half into two
// vectors. The stores write exactly the selected number of bytes.

// func _filter_uint64_neon(values, filter, output, tables unsafe.Pointer, length int64)
TEXT ·_filter_uint64_neon(SB), NOSPLIT|NOFRAME, $0-40
	MOVD values+0(FP), R0
	MOVD filter+8(FP), R1
	MOVD output+16(FP), R2
	MOVD tables+24(FP), R3
	MOVD length+32(FP), R4

	LSR $3, R4, R4

filter_loop:
	MOVBU (R1), R5
	ADD $1, R1
	CBZ R5, filter_next
	CMPW $255, R5
	BEQ filter_all

	ANDW $15, R5, R6
	ADD $512, R3, R8
	MOVBU (R8)(R6<<0), R9
	CBZ R9, filter_high

	VLD1 (R0), [V0.D2]
	ADD $16, R0, R8
	VLD1 (R8), [V1.D2]

	LSL $5, R6, R10
	ADD R3, R10, R10
	VLD1 (R10), [V2.B16]
	VTBL V2.B16, [V0.B16, V1.B16], V4.B16
	ADD $16, R10, R10
	VLD1 (R10), [V2.B16]
	VTBL V2.B16, [V0.B16, V1.B16], V5.B16

	CMPW $1, R9
	BEQ filter_low_store1
	CMPW $2, R9
	BEQ filter_low_store2
	CMPW $3, R9
	BEQ filter_low_store3
	VST1 [V4.D2], (R2)
	ADD $16, R2
	VST1 [V5.D2], (R2)
	ADD $16, R2
	B filter_high

filter_low_store1:
	VMOV V4.D[0], R8
	MOVD R8, (R2)
	ADD $8, R2
	B filter_high

filter_low_store2:
	VST1 [V4.D2], (R2)
	ADD $16, R2
	B filter_high

filter_low_store3:
	VST1 [V4.D2], (R2)
	ADD $16, R2
	VMOV V5.D[0], R8
	MOVD R8, (R2)
	ADD $8, R2

filter_high:
	LSR $4, R5, R6
	ADD $512, R3, R8
	MOVBU (R8)(R6<<0), R9
	CBZ R9, filter_next

	ADD $32, R0, R8
	VLD1 (R8), [V0.D2]
	ADD $48, R0, R8
	VLD1 (R8), [V1.D2]

	LSL $5, R6, R10
	ADD R3, R10, R10
	VLD1 (R10), [V2.B16]
	VTBL V2.B16, [V0.B16, V1.B16], V4.B16
	ADD $16, R10, R10
	VLD1 (R10), [V2.B16]
	VTBL V2.B16, [V0.B16, V1.B16], V5.B16

	CMPW $1, R9
	BEQ filter_high_store1
	CMPW $2, R9
	BEQ filter_high_store2
	CMPW $3, R9
	BEQ filter_high_store3
	VST1 [V4.D2], (R2)
	ADD $16, R2
	VST1 [V5.D2], (R2)
	ADD $16, R2
	B filter_next

filter_high_store1:
	VMOV V4.D[0], R8
	MOVD R8, (R2)
	ADD $8, R2
	B filter_next

filter_high_store2:
	VST1 [V4.D2], (R2)
	ADD $16, R2
	B filter_next

filter_high_store3:
	VST1 [V4.D2], (R2)
	ADD $16, R2
	VMOV V5.D[0], R8
	MOVD R8, (R2)
	ADD $8, R2
	B filter_next

filter_all:
	VLD1 (R0), [V0.D2]
	ADD $16, R0, R8
	VLD1 (R8), [V1.D2]
	ADD $32, R0, R8
	VLD1 (R8), [V2.D2]
	ADD $48, R0, R8
	VLD1 (R8), [V3.D2]
	VST1 [V0.D2], (R2)
	ADD $16, R2
	VST1 [V1.D2], (R2)
	ADD $16, R2
	VST1 [V2.D2], (R2)
	ADD $16, R2
	VST1 [V3.D2], (R2)
	ADD $16, R2

filter_next:
	ADD $64, R0
	SUBS $1, R4, R4
	BNE filter_loop
	RET
