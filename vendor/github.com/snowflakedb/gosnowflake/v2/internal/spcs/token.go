package spcs

import (
	"context"
	"os"
	"strings"

	loggerinternal "github.com/snowflakedb/gosnowflake/v2/internal/logger"
)

const (
	// RunningInsideEnv signals that the driver is running inside a Snowpark
	// Container Services (SPCS) workload. Presence activates SPCS-specific
	// behavior; absence disables it.
	RunningInsideEnv = "SNOWFLAKE_RUNNING_INSIDE_SPCS"

	defaultTokenFilePath = "/snowflake/session/spcs_token"
)

var logger = loggerinternal.NewLoggerProxy()

// GetToken returns the SPCS service token when the driver is running
// inside an SPCS container, or "" otherwise or on any read failure.
// We deliberately don't cache it, since it can change at any moment.
func GetToken(ctx context.Context) string {
	return readSpcsTokenFromDisk(ctx, defaultTokenFilePath)
}

func readSpcsTokenFromDisk(ctx context.Context, path string) string {
	if os.Getenv(RunningInsideEnv) == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		logger.WithContext(ctx).Warnf("Failed to read SPCS token from %q: %v", path, err)
		return ""
	}
	return strings.TrimSpace(string(data))
}
