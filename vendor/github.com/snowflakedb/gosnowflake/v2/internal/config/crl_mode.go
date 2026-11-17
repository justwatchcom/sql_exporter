package config

import (
	"fmt"
	"strings"
)

// CertRevocationCheckMode defines the modes for certificate revocation checks.
type CertRevocationCheckMode int

const (
	// CertRevocationCheckDisabled means that certificate revocation checks are disabled.
	CertRevocationCheckDisabled CertRevocationCheckMode = iota
	// CertRevocationCheckAdvisory means that certificate revocation checks are advisory, and the driver will not fail if the checks end with error (cannot verify revocation status).
	// Driver will fail only if a certicate is revoked.
	CertRevocationCheckAdvisory
	// CertRevocationCheckEnabled means that every certificate revocation check must pass, otherwise the driver will fail.
	CertRevocationCheckEnabled
)

func (m CertRevocationCheckMode) String() string {
	switch m {
	case CertRevocationCheckDisabled:
		return "DISABLED"
	case CertRevocationCheckAdvisory:
		return "ADVISORY"
	case CertRevocationCheckEnabled:
		return "ENABLED"
	default:
		return fmt.Sprintf("unknown CertRevocationCheckMode: %d", m)
	}
}

// ParseCertRevocationCheckMode parses a string into a CertRevocationCheckMode.
func ParseCertRevocationCheckMode(s string) (CertRevocationCheckMode, error) {
	switch strings.ToLower(s) {
	case "disabled":
		return CertRevocationCheckDisabled, nil
	case "advisory":
		return CertRevocationCheckAdvisory, nil
	case "enabled":
		return CertRevocationCheckEnabled, nil
	}
	return 0, fmt.Errorf("unknown CertRevocationCheckMode: %s", s)
}
