//go:build !minicore_disabled

package gosnowflake

import (
	// embed is used only to initialize go:embed directive
	_ "embed"
)

//go:embed libsf_mini_core_darwin_amd64.dylib
var coreLibDarwinAmd64 []byte

var _ = initMinicoreProvider()

func initMinicoreProvider() any {
	corePlatformConfig.coreLib = coreLibDarwinAmd64
	corePlatformConfig.coreLibFileName = "libsf_mini_core_darwin_amd64.dylib"
	corePlatformConfig.initialized = true
	return nil
}
