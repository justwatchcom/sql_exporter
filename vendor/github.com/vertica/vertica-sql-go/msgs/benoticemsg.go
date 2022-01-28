package msgs

// Copyright (c) 2019-2021 Micro Focus or one of its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

import (
	"fmt"
)

// BENoticeMsg docs
type BENoticeMsg struct {
	NoticeCodes  []byte
	NoticeValues []string
}

func (b *BENoticeMsg) addNoticeMsg(writeIndex int, code byte, value string) {

	if writeIndex == cap(b.NoticeCodes) {
		newNoticeCodes := make([]byte, 2*writeIndex)
		copy(newNoticeCodes, b.NoticeCodes)
		b.NoticeCodes = newNoticeCodes

		newNoticeValues := make([]string, 2*writeIndex)
		copy(newNoticeValues, b.NoticeValues)
		b.NoticeValues = newNoticeValues
	} else {
		b.NoticeCodes = b.NoticeCodes[0 : writeIndex+1]
		b.NoticeValues = b.NoticeValues[0 : writeIndex+1]
	}

	b.NoticeCodes[writeIndex] = code
	b.NoticeValues[writeIndex] = value
}

// CreateFromMsgBody docs
func (b *BENoticeMsg) CreateFromMsgBody(buf *msgBuffer) (BackEndMsg, error) {

	const defaultArray int = 8

	res := &BENoticeMsg{}
	res.NoticeCodes = make([]byte, defaultArray)
	res.NoticeValues = make([]string, defaultArray)

	writeIndex := 0

	for {
		noticeCode := buf.readByte()

		if noticeCode == 0 {
			break
		}

		noticeValue := buf.readString()

		res.addNoticeMsg(writeIndex, noticeCode, noticeValue)
		writeIndex++
	}

	return res, nil
}

func (b *BENoticeMsg) String() string {
	return fmt.Sprintf("Notice: (%d) notice(s)", len(b.NoticeValues))
}

func init() {
	registerBackEndMsgType('N', &BENoticeMsg{})
}
