//go:build !minicore_disabled

package gosnowflake

import (
	// embed is used only to initialize go:embed directive
	_ "embed"
)

//go:embed libsf_mini_core_linux_amd64_glibc.so
var coreLibLinuxAmd64Glibc []byte

//go:embed libsf_mini_core_linux_amd64_musl.so
var coreLibLinuxAmd64Musl []byte

var _ = initMinicoreProvider()

func initMinicoreProvider() any {
	switch detectLibc() {
	case libcTypeGlibc:
		corePlatformConfig.coreLib = coreLibLinuxAmd64Glibc
		corePlatformConfig.coreLibFileName = "libsf_mini_core_linux_amd64_glibc.so"
	case libcTypeMusl:
		corePlatformConfig.coreLib = coreLibLinuxAmd64Musl
		corePlatformConfig.coreLibFileName = "libsf_mini_core_linux_amd64_musl.so"
	default:
		minicoreDebugf("unknown libc")
		return nil
	}
	corePlatformConfig.initialized = true
	return nil
}
