//go:build !minicore_disabled

package gosnowflake

import (
	// embed is used only to initialize go:embed directive
	_ "embed"
)

//go:embed libsf_mini_core_windows_amd64.dll
var coreLibWindowsAmd64Glibc []byte

var _ = initMinicoreProvider()

func initMinicoreProvider() any {
	corePlatformConfig.coreLib = coreLibWindowsAmd64Glibc
	corePlatformConfig.coreLibFileName = "libsf_mini_core_windows_amd64.dll"
	corePlatformConfig.initialized = true
	return nil
}
