//go:build !minicore_disabled

package compilation

// MinicoreEnabled is set to true by default. Build with -tags minicore_disabled to disable
// minicore at compile time. This is useful when building statically linked binaries,
// as minicore requires dynamic library loading (dlopen) which is incompatible with static linking.
//
// Example: go build -tags minicore_disabled ./...
var MinicoreEnabled = true
