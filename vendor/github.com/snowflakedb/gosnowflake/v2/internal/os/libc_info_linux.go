//go:build linux

package os

import "os"

// GetLibcInfo returns the libc family and version on Linux.
// The result is cached so the detection only runs once.
func GetLibcInfo() LibcInfo {
	libcInfoOnce.Do(func() {
		libcInfo = detectLibcInfo()
	})
	return libcInfo
}

func detectLibcInfo() LibcInfo {
	fd, err := os.Open("/proc/self/maps")
	if err != nil {
		return LibcInfo{}
	}
	defer func() {
		_ = fd.Close()
	}()

	family, libcPath := parseProcMapsForLibc(fd)
	if family == "" {
		return LibcInfo{}
	}

	var version string
	switch family {
	case "glibc":
		version = glibcVersionFromELF(libcPath)
	case "musl":
		version = muslVersionFromBinary(libcPath)
	}

	return LibcInfo{Family: family, Version: version}
}
