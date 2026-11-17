//go:build !minicore_disabled

package gosnowflake

import (
	// embed is used only to initialize go:embed directive
	_ "embed"
)

//go:embed libsf_mini_core_linux_arm64_glibc.so
var coreLibLinuxArm64Glibc []byte

//go:embed libsf_mini_core_linux_arm64_musl.so
var coreLibLinuxArm64Musl []byte

var _ = initMinicoreProvider()

func initMinicoreProvider() any {
	switch detectLibc() {
	case libcTypeGlibc:
		corePlatformConfig.coreLib = coreLibLinuxArm64Glibc
		corePlatformConfig.coreLibFileName = "libsf_mini_core_linux_arm64_glibc.so"
	case libcTypeMusl:
		corePlatformConfig.coreLib = coreLibLinuxArm64Musl
		corePlatformConfig.coreLibFileName = "libsf_mini_core_linux_arm64_musl.so"
	default:
		minicoreDebugf("unknown libc")
		return nil
	}
	corePlatformConfig.initialized = true
	return nil
}
