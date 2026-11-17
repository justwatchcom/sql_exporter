//go:build linux

package os

// GetOsDetails returns OS details from /etc/os-release on Linux.
// The result is cached so it's only read once.
func GetOsDetails() map[string]string {
	osDetailsOnce.Do(func() {
		osDetails = readOsRelease("/etc/os-release")
	})
	return osDetails
}
