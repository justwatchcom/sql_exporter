package os

import (
	"bufio"
	"debug/elf"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var (
	libcInfo     LibcInfo
	libcInfoOnce sync.Once
)

// LibcInfo contains information about the C standard library in use.
type LibcInfo struct {
	Family  string // "glibc", "musl", or "" if not detected
	Version string // e.g., "2.31", "1.2.4", or "" if not determined
}

// parseProcMapsForLibc scans the contents of /proc/self/maps and returns
// the libc family ("glibc" or "musl") and the filesystem path to the mapped library.
func parseProcMapsForLibc(r io.Reader) (family string, libcPath string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		// /proc/self/maps format: addr perms offset dev inode pathname
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		path := fields[len(fields)-1]
		if strings.Contains(path, "musl") {
			return "musl", path
		}
		if strings.Contains(path, "libc.so.6") {
			return "glibc", path
		}
	}
	return "", ""
}

var glibcVersionPattern = regexp.MustCompile(`^GLIBC_(\d+\.\d+(?:\.\d+)?)$`)

// glibcVersionFromELF opens the given ELF file (libc.so.6) and extracts the
// glibc version from its SHT_GNU_verdef section via DynamicVersions().
// It returns the highest GLIBC_x.y[.z] version found.
func glibcVersionFromELF(path string) string {
	f, err := elf.Open(path)
	if err != nil {
		return ""
	}
	defer func() {
		_ = f.Close()
	}()

	versions, err := f.DynamicVersions()
	if err != nil {
		return ""
	}

	var best string
	for _, v := range versions {
		m := glibcVersionPattern.FindStringSubmatch(v.Name)
		if m != nil {
			if best == "" || compareVersions(m[1], best) > 0 {
				best = m[1]
			}
		}
	}
	return best
}

var muslVersionPattern = regexp.MustCompile(`Version (\d+\.\d+\.\d+)`)

// muslVersionFromBinary reads the musl library binary and searches for the
// embedded version string pattern "Version X.Y.Z".
func muslVersionFromBinary(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() {
		_ = f.Close()
	}()

	buf := make([]byte, 1<<20) // 1MB limit
	n, _ := io.ReadFull(f, buf)
	content := string(buf[:n])

	m := muslVersionPattern.FindStringSubmatch(content)
	if m != nil {
		return m[1]
	}
	return ""
}

// compareVersions compares two dotted version strings numerically.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
func compareVersions(a, b string) int {
	partsA := strings.Split(a, ".")
	partsB := strings.Split(b, ".")
	maxLen := max(len(partsB), len(partsA))
	for i := range maxLen {
		var va, vb int
		if i < len(partsA) {
			va, _ = strconv.Atoi(partsA[i])
		}
		if i < len(partsB) {
			vb, _ = strconv.Atoi(partsB[i])
		}
		if va < vb {
			return -1
		}
		if va > vb {
			return 1
		}
	}
	return 0
}
