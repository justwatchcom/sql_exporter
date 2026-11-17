package config

import (
	"os"
	"strconv"
)

// OCSPFailOpenMode is OCSP fail open mode. NotSet means the user did not
// request OCSP; True/False are explicit fail-open / fail-closed opt-ins.
type OCSPFailOpenMode uint32

const (
	// OCSPFailOpenNotSet represents OCSP fail open mode is not set, which is the default value.
	OCSPFailOpenNotSet OCSPFailOpenMode = iota
	// OCSPFailOpenTrue represents OCSP fail open mode.
	OCSPFailOpenTrue
	// OCSPFailOpenFalse represents OCSP fail closed mode.
	OCSPFailOpenFalse
)

const (
	ocspModeFailOpen   = "FAIL_OPEN"
	ocspModeFailClosed = "FAIL_CLOSED"
	ocspModeInsecure   = "INSECURE"
)

// EnvVarDisableOCSPChecks is the environment variable that opts into or out of
// OCSP when DisableOCSPChecks is false. An explicit false opts in (the name
// is inverted from the default-off). An explicit true disables fail-open
// (including an explicit OCSPFailOpenTrue) but is ignored when fail-closed
// is active. Unset/invalid leaves the default-off in place. Read live in
// OCSPEnabled, not snapshotted at FillMissing. DisableOCSPChecks=true wins
// over this env.
const EnvVarDisableOCSPChecks = "SF_DISABLE_OCSP_CHECKS"

// envDisableOCSPChecks reports whether SF_DISABLE_OCSP_CHECKS parsed as a
// bool. ok is false when the variable is unset, empty, or unparseable.
func envDisableOCSPChecks() (disable, ok bool) {
	val, present := os.LookupEnv(EnvVarDisableOCSPChecks)
	if !present || val == "" {
		return false, false
	}
	disable, err := strconv.ParseBool(val)
	if err != nil {
		return false, false
	}
	return disable, true
}

// OCSPEnabled reports whether this config should run OCSP revocation checks.
//
// DisableOCSPChecks=true always means off, including when OCSPFailOpen is set
// and when SF_DISABLE_OCSP_CHECKS=false. disableOCSPChecks=false is the
// zero-value bool and is not an opt-in. When the bool is false:
//
//   - explicit fail-closed stays on; SF_DISABLE_OCSP_CHECKS=true is ignored
//     and a warning is logged (not for the default-off)
//   - SF_DISABLE_OCSP_CHECKS=true disables fail-open (explicit or default)
//   - explicit fail-open enables OCSP unless the env disabled it
//   - otherwise SF_DISABLE_OCSP_CHECKS=false opts in
//
// Unset/invalid env leaves the default-off in place.
func OCSPEnabled(c *Config) bool {
	if c == nil || c.DisableOCSPChecks {
		return false
	}

	envDisable, envSet := envDisableOCSPChecks()
	if c.OCSPFailOpen == OCSPFailOpenFalse {
		if envSet && envDisable {
			logger.Infof("%s environment variable is set, but OCSP fail-closed mode is active; the environment variable will be ignored", EnvVarDisableOCSPChecks)
		}
		return true
	}
	if envSet && envDisable {
		return false
	}
	return c.OCSPFailOpen == OCSPFailOpenTrue || envSet
}

// OcspMode returns the OCSP mode for login telemetry:
//
//	INSECURE — OCSP is off (default, explicit disable, or env true)
//	FAIL_OPEN / FAIL_CLOSED — OCSP is on
func OcspMode(c *Config) string {
	if !OCSPEnabled(c) {
		return ocspModeInsecure
	}
	if c.OCSPFailOpen == OCSPFailOpenFalse {
		return ocspModeFailClosed
	}
	return ocspModeFailOpen
}
