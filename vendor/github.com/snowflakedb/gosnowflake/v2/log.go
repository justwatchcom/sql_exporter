package gosnowflake

import (
	loggerinternal "github.com/snowflakedb/gosnowflake/v2/internal/logger"
	"github.com/snowflakedb/gosnowflake/v2/sflog"
)

// SFSessionIDKey is context key of session id
const SFSessionIDKey ContextKey = "LOG_SESSION_ID"

// SFSessionUserKey is context key of user id of a session
const SFSessionUserKey ContextKey = "LOG_USER"

func init() {
	// Set default log keys in internal package
	SetLogKeys(SFSessionIDKey, SFSessionUserKey)
}

// Re-export types from sflog package for backward compatibility
type (
	// ClientLogContextHook is a client-defined hook that can be used to insert log
	// fields based on the Context.
	ClientLogContextHook = sflog.ClientLogContextHook

	// LogEntry allows for logging using a snapshot of field values.
	// No implementation-specific logging details should be placed into this interface.
	LogEntry = sflog.LogEntry

	// SFLogger Snowflake logger interface which abstracts away the underlying logging mechanism.
	// No implementation-specific logging details should be placed into this interface.
	SFLogger = sflog.SFLogger

	// SFSlogLogger is an optional interface for advanced slog handler configuration.
	// This interface is separate from SFLogger to maintain framework-agnostic design.
	// Users can type-assert the logger to check if slog handler configuration is supported.
	SFSlogLogger = sflog.SFSlogLogger

	// Level is the log level. Info is set to 0. For more details, see sflog.Level.
	Level = sflog.Level
)

// SetLogKeys sets the context keys to be written to logs when logger.WithContext is used.
// This function is thread-safe and can be called at runtime.
func SetLogKeys(keys ...ContextKey) {
	// Convert ContextKey to []any for internal package
	ikeys := make([]any, len(keys))
	for i, k := range keys {
		ikeys[i] = k
	}
	loggerinternal.SetLogKeys(ikeys)
}

// GetLogKeys returns the currently configured context keys.
func GetLogKeys() []ContextKey {
	ikeys := loggerinternal.GetLogKeys()

	// Convert []any back to []ContextKey
	keys := make([]ContextKey, 0, len(ikeys))
	for _, k := range ikeys {
		if ck, ok := k.(ContextKey); ok {
			keys = append(keys, ck)
		}
	}
	return keys
}

// RegisterLogContextHook registers a hook that can be used to extract fields
// from the Context and associated with log messages using the provided key.
// This function is thread-safe and can be called at runtime.
func RegisterLogContextHook(contextKey string, ctxExtractor ClientLogContextHook) {
	// Delegate directly to internal package
	loggerinternal.RegisterLogContextHook(contextKey, ctxExtractor)
}

// GetClientLogContextHooks returns the registered log context hooks.
func GetClientLogContextHooks() map[string]ClientLogContextHook {
	return loggerinternal.GetClientLogContextHooks()
}

// logger is a proxy that delegates all calls to the internal global logger.
// This ensures a single source of truth for the current logger.
// This variable is private and should only be used internally within the main package.
var logger SFLogger = loggerinternal.NewLoggerProxy()

// SetLogger sets a custom logger implementation for gosnowflake.
// The provided logger will be used as the base logger and automatically wrapped with:
//   - Secret masking (to protect sensitive data like passwords and tokens)
//   - Level filtering (for performance optimization)
//
// You cannot bypass these protective layers. If you need to configure them, use the
// returned logger's methods (SetLogLevel, etc.).
//
// Example:
//
//	customLogger := mylogger.New()
//	gosnowflake.SetLogger(customLogger)
func SetLogger(logger SFLogger) error {
	return loggerinternal.SetLogger(logger)
}

// GetLogger returns the current global logger with all protective layers applied
// (secret masking and level filtering). This is the actual wrapped logger instance,
// not a proxy.
//
// Example:
//
//	logger := gosnowflake.GetLogger()
//	logger.Info("message")
func GetLogger() SFLogger {
	return loggerinternal.GetLogger()
}

// CreateDefaultLogger creates and returns a new instance of SFLogger with default config.
// The returned logger is automatically wrapped with secret masking and level filtering.
// This is a pure factory function and does NOT modify global state.
// If you want to set it as the global logger, call SetLogger(newLogger).
//
// The wrapping chain is: levelFilteringLogger → secretMaskingLogger → rawLogger
func CreateDefaultLogger() SFLogger {
	return loggerinternal.CreateDefaultLogger()
}
