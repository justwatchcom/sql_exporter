// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tel

// This file holds hooks used by tests in this package and in the root
// cloudsqlconn package (whose e2e tests live in an external test package and
// therefore cannot see _test.go files from here). They are not part of the
// public API: internal/ packages are not importable outside this module.

import (
	"log"
	"sync"

	"cloud.google.com/go/cloudsqlconn/debug"
)

// SetWarnLoggerForTest redirects the once-per-process permanent-error warning
// to l and resets the once guard. It returns a function that restores the
// previous state.
func SetWarnLoggerForTest(l *log.Logger) (restore func()) {
	prevLogger, prevOnce := warnLogger, permanentErrorOnce
	warnLogger = l
	permanentErrorOnce = new(sync.Once)
	return func() {
		warnLogger = prevLogger
		permanentErrorOnce = prevOnce
	}
}

// IsPermanentExportError exposes the export error classifier for tests.
var IsPermanentExportError = isPermanentExportError

// NextBackoff and Jitter expose the backoff helpers for tests.
var (
	NextBackoff = nextBackoff
	Jitter      = jitter
)

// NewPanickingRecorderForTest returns a metricRecorder whose instruments are
// nil, so every Record* call would panic without the recoverPanic guard.
func NewPanickingRecorderForTest(l debug.ContextLogger) MetricRecorder {
	return &metricRecorder{logger: l, name: "p:i"}
}

// ResetPanicWarnOnceForTest re-arms the process-wide panic warning.
func ResetPanicWarnOnceForTest() { panicWarnOnce = sync.Once{} }

// ValidComputePlatformsForTest returns the allowed compute_platform values,
// excluding UnknownValue (which only auto-detection produces), so the root
// package can check that its public list stays in sync.
func ValidComputePlatformsForTest() []string {
	var out []string
	for p := range validComputePlatforms {
		if p != UnknownValue {
			out = append(out, p)
		}
	}
	return out
}
