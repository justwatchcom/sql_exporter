//go:build !minicore_disabled

package gosnowflake

import (
	// embed is used only to initialize go:embed directive
	_ "embed"
)

//go:embed libsf_mini_core_windows_arm64.dll
var coreLibWindowsArm64Glibc []byte

var _ = initMinicoreProvider()

func initMinicoreProvider() any {
	corePlatformConfig.coreLib = coreLibWindowsArm64Glibc
	corePlatformConfig.coreLibFileName = "libsf_mini_core_windows_arm64.dll"
	corePlatformConfig.initialized = true
	return nil
}
