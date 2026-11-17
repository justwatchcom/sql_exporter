package logger

import (
	"github.com/snowflakedb/gosnowflake/v2/sflog"
)

// Re-export types from sflog package to avoid circular dependencies
// while maintaining a clean internal API
type (
	// LogEntry reexports the LogEntry interface from sflog package.
	LogEntry = sflog.LogEntry
	// SFLogger reexports the SFLogger interface from sflog package.
	SFLogger = sflog.SFLogger
	// ClientLogContextHook reexports the ClientLogContextHook type from sflog package.
	ClientLogContextHook = sflog.ClientLogContextHook
)
