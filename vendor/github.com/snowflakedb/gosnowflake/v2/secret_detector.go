package gosnowflake

import loggerinternal "github.com/snowflakedb/gosnowflake/v2/internal/logger"

// maskSecrets masks secrets in text (unexported for internal use within main package)
func maskSecrets(text string) string {
	return loggerinternal.MaskSecrets(text)
}
