//go:build !linux

package os

// GetLibcInfo returns an empty LibcInfo on non-Linux platforms.
func GetLibcInfo() LibcInfo {
	return LibcInfo{}
}
