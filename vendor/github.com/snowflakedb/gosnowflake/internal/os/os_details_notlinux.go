//go:build !linux

package os

// GetOsDetails returns nil on non-Linux platforms.
func GetOsDetails() map[string]string {
	return nil
}
