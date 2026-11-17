//go:build minicore_disabled

package compilation

// MinicoreEnabled is set to false when building with -tags minicore_disabled.
// This disables minicore at compile time, which is useful for statically linked binaries
// that cannot use dynamic library loading (dlopen).
//
// Example: go build -tags minicore_disabled ./...
var MinicoreEnabled = false
