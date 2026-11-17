package config

import (
	"crypto/tls"
	"fmt"
	"os"
	"strings"
)

const envVarMinTLSVersion = "SNOWFLAKE_MIN_TLS_VERSION"

// GetMinTLSVersion reads and parses the SNOWFLAKE_MIN_TLS_VERSION environment variable.
// Returns 0 if not set or invalid. Valid values: "1.2", "1.3", "TLSv1.2", "TLSv1.3" (case-insensitive, trimmed).
// TLS 1.0 and 1.1 are not supported as Snowflake requires TLS 1.2 or higher.
func GetMinTLSVersion() (uint16, error) {
	value := os.Getenv(envVarMinTLSVersion)
	if value == "" {
		return 0, nil
	}

	// Normalize: trim whitespace and convert to lowercase
	value = strings.ToLower(strings.TrimSpace(value))

	switch value {
	case "1.2", "tlsv1.2":
		return tls.VersionTLS12, nil
	case "1.3", "tlsv1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("invalid %s value: %s. Valid values are: 1.2, 1.3, TLSv1.2, TLSv1.3", envVarMinTLSVersion, value)
	}
}

// ApplyMinTLSVersion applies the minimum TLS version from environment variable to the given tls.Config.
// This enforces a global security policy by taking the maximum of the existing MinVersion and the env var value.
func ApplyMinTLSVersion(tlsConfig *tls.Config) (*tls.Config, error) {
	envMinVersion, err := GetMinTLSVersion()
	if err != nil {
		return nil, err
	}

	if envMinVersion == 0 {
		// No minimum version set
		return tlsConfig, nil
	}

	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
	}

	// Use the maximum of existing MinVersion and env var value
	// This ensures the env var sets a floor, but respects higher values already configured
	if tlsConfig.MinVersion < envMinVersion {
		logger.Debugf("Raising minimum TLS version from %s to %s (from %s)",
			getTLSVersionName(tlsConfig.MinVersion),
			getTLSVersionName(envMinVersion),
			envVarMinTLSVersion)
		tlsConfig.MinVersion = envMinVersion
	} else if tlsConfig.MinVersion > 0 {
		logger.Debugf("Keeping existing minimum TLS version %s (>= %s from %s)",
			getTLSVersionName(tlsConfig.MinVersion),
			getTLSVersionName(envMinVersion),
			envVarMinTLSVersion)
	} else {
		// MinVersion was 0 (default), set it to env var value
		logger.Debugf("Setting minimum TLS version to %s (from %s)",
			getTLSVersionName(envMinVersion),
			envVarMinTLSVersion)
		tlsConfig.MinVersion = envMinVersion
	}

	return tlsConfig, nil
}

func getTLSVersionName(version uint16) string {
	switch version {
	case tls.VersionTLS12:
		return "1.2"
	case tls.VersionTLS13:
		return "1.3"
	default:
		return fmt.Sprintf("unknown (0x%04x)", version)
	}
}
