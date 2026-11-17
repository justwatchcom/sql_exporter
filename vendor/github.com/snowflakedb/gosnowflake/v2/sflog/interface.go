// Package sflog package defines the logging interface for Snowflake's Go driver.
// If you want to implement a custom logger, you should implement the SFLogger interface defined in this package.
package sflog

import (
	"context"
	"io"
)

// ClientLogContextHook is a client-defined hook that can be used to insert log
// fields based on the Context.
type ClientLogContextHook func(context.Context) string

// LogEntry allows for logging using a snapshot of field values.
// No implementation-specific logging details should be placed into this interface.
type LogEntry interface {
	Tracef(format string, args ...any)
	Debugf(format string, args ...any)
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)

	Trace(msg string)
	Debug(msg string)
	Info(msg string)
	Warn(msg string)
	Error(msg string)
	Fatal(msg string)
}

// SFLogger Snowflake logger interface which abstracts away the underlying logging mechanism.
// No implementation-specific logging details should be placed into this interface.
type SFLogger interface {
	LogEntry
	WithField(key string, value any) LogEntry
	WithFields(fields map[string]any) LogEntry

	SetLogLevel(level string) error
	SetLogLevelInt(level Level) error
	GetLogLevel() string
	GetLogLevelInt() Level
	WithContext(ctx context.Context) LogEntry
	SetOutput(output io.Writer)
}
