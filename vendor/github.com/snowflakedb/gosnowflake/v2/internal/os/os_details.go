package os

import (
	"bufio"
	"os"
	"strings"
	"sync"
)

var (
	osDetails     map[string]string
	osDetailsOnce sync.Once
)

// allowedOsReleaseKeys defines the keys we want to extract from /etc/os-release
var allowedOsReleaseKeys = map[string]bool{
	"NAME":          true,
	"PRETTY_NAME":   true,
	"ID":            true,
	"IMAGE_ID":      true,
	"IMAGE_VERSION": true,
	"BUILD_ID":      true,
	"VERSION":       true,
	"VERSION_ID":    true,
}

// readOsRelease reads and parses an os-release file from the given path.
// Returns nil on any error.
func readOsRelease(filename string) map[string]string {
	file, err := os.Open(filename)
	if err != nil {
		return nil
	}
	defer func() {
		_ = file.Close()
	}()

	result := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)

		// Skip empty lines
		if line == "" {
			continue
		}

		// Parse KEY=VALUE format
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// Only include allowed keys
		if !allowedOsReleaseKeys[key] {
			continue
		}

		value = unquoteOsReleaseValue(value)
		result[key] = value
	}

	if len(result) == 0 {
		return nil
	}

	return result
}

// unquoteOsReleaseValue extracts the value from a possibly quoted string.
// If the value is wrapped in matching single or double quotes, the content
// between the quotes is returned (ignoring anything after the closing quote).
// Otherwise the raw value is returned.
func unquoteOsReleaseValue(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') {
		quote := s[0]
		if end := strings.IndexByte(s[1:], quote); end >= 0 {
			return s[1 : 1+end]
		}
	}
	return s
}
