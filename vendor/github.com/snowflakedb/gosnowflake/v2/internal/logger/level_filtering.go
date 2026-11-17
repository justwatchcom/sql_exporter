package logger

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/snowflakedb/gosnowflake/v2/sflog"
)

// levelFilteringLogger wraps any logger and filters log messages based on log level.
// This prevents expensive operations (like secret masking and formatting) from running
// when the message wouldn't be logged anyway.
type levelFilteringLogger struct {
	inner SFLogger
}

// Compile-time verification that levelFilteringLogger implements SFLogger
var _ SFLogger = (*levelFilteringLogger)(nil)

// Unwrap returns the inner logger (for introspection by easy_logging)
func (l *levelFilteringLogger) Unwrap() any {
	return l.inner
}

// shouldLog determines if a message at messageLevel should be logged
// given the current configured level.
func (l *levelFilteringLogger) shouldLog(messageLevel sflog.Level) bool {
	return messageLevel >= l.inner.GetLogLevelInt()
}

// newLevelFilteringLogger creates a new level filtering wrapper around the provided logger
func newLevelFilteringLogger(inner SFLogger) SFLogger {
	if inner == nil {
		panic("inner logger cannot be nil")
	}
	return &levelFilteringLogger{inner: inner}
}

// Implement all formatted logging methods (*f variants)
func (l *levelFilteringLogger) Tracef(format string, args ...any) {
	if !l.shouldLog(sflog.LevelTrace) {
		return
	}
	l.inner.Tracef(format, args...)
}

func (l *levelFilteringLogger) Debugf(format string, args ...any) {
	if !l.shouldLog(sflog.LevelDebug) {
		return
	}
	l.inner.Debugf(format, args...)
}

func (l *levelFilteringLogger) Infof(format string, args ...any) {
	if !l.shouldLog(sflog.LevelInfo) {
		return
	}
	l.inner.Infof(format, args...)
}

func (l *levelFilteringLogger) Warnf(format string, args ...any) {
	if !l.shouldLog(sflog.LevelWarn) {
		return
	}
	l.inner.Warnf(format, args...)
}

func (l *levelFilteringLogger) Errorf(format string, args ...any) {
	if !l.shouldLog(sflog.LevelError) {
		return
	}
	l.inner.Errorf(format, args...)
}

func (l *levelFilteringLogger) Fatalf(format string, args ...any) {
	l.inner.Fatalf(format, args...)
}

// Implement all direct logging methods
func (l *levelFilteringLogger) Trace(msg string) {
	if !l.shouldLog(sflog.LevelTrace) {
		return
	}
	l.inner.Trace(msg)
}

func (l *levelFilteringLogger) Debug(msg string) {
	if !l.shouldLog(sflog.LevelDebug) {
		return
	}
	l.inner.Debug(msg)
}

func (l *levelFilteringLogger) Info(msg string) {
	if !l.shouldLog(sflog.LevelInfo) {
		return
	}
	l.inner.Info(msg)
}

func (l *levelFilteringLogger) Warn(msg string) {
	if !l.shouldLog(sflog.LevelWarn) {
		return
	}
	l.inner.Warn(msg)
}

func (l *levelFilteringLogger) Error(msg string) {
	if !l.shouldLog(sflog.LevelError) {
		return
	}
	l.inner.Error(msg)
}

func (l *levelFilteringLogger) Fatal(msg string) {
	l.inner.Fatal(msg)
}

// Implement structured logging methods - these return wrapped entries
func (l *levelFilteringLogger) WithField(key string, value any) sflog.LogEntry {
	innerEntry := l.inner.WithField(key, value)
	return &levelFilteringEntry{
		parent: l,
		inner:  innerEntry,
	}
}

func (l *levelFilteringLogger) WithFields(fields map[string]any) sflog.LogEntry {
	innerEntry := l.inner.WithFields(fields)
	return &levelFilteringEntry{
		parent: l,
		inner:  innerEntry,
	}
}

func (l *levelFilteringLogger) WithContext(ctx context.Context) sflog.LogEntry {
	innerEntry := l.inner.WithContext(ctx)
	return &levelFilteringEntry{
		parent: l,
		inner:  innerEntry,
	}
}

// Delegate configuration methods to inner logger
func (l *levelFilteringLogger) SetLogLevel(level string) error {
	return l.inner.SetLogLevel(level)
}

func (l *levelFilteringLogger) SetLogLevelInt(level sflog.Level) error {
	return l.inner.SetLogLevelInt(level)
}

func (l *levelFilteringLogger) GetLogLevel() string {
	return l.inner.GetLogLevel()
}

func (l *levelFilteringLogger) GetLogLevelInt() sflog.Level {
	return l.inner.GetLogLevelInt()
}

func (l *levelFilteringLogger) SetOutput(output io.Writer) {
	l.inner.SetOutput(output)
}

// SetHandler implements SFSlogLogger interface for advanced slog handler configuration
func (l *levelFilteringLogger) SetHandler(handler slog.Handler) error {
	if sh, ok := l.inner.(sflog.SFSlogLogger); ok {
		return sh.SetHandler(handler)
	}
	return errors.New("underlying logger does not support SetHandler")
}

// levelFilteringEntry wraps a log entry and filters by level
type levelFilteringEntry struct {
	parent *levelFilteringLogger
	inner  sflog.LogEntry
}

// Implement all formatted logging methods for entry
func (e *levelFilteringEntry) Tracef(format string, args ...any) {
	if !e.parent.shouldLog(sflog.LevelTrace) {
		return
	}
	e.inner.Tracef(format, args...)
}

func (e *levelFilteringEntry) Debugf(format string, args ...any) {
	if !e.parent.shouldLog(sflog.LevelDebug) {
		return
	}
	e.inner.Debugf(format, args...)
}

func (e *levelFilteringEntry) Infof(format string, args ...any) {
	if !e.parent.shouldLog(sflog.LevelInfo) {
		return
	}
	e.inner.Infof(format, args...)
}

func (e *levelFilteringEntry) Warnf(format string, args ...any) {
	if !e.parent.shouldLog(sflog.LevelWarn) {
		return
	}
	e.inner.Warnf(format, args...)
}

func (e *levelFilteringEntry) Errorf(format string, args ...any) {
	if !e.parent.shouldLog(sflog.LevelError) {
		return
	}
	e.inner.Errorf(format, args...)
}

func (e *levelFilteringEntry) Fatalf(format string, args ...any) {
	e.inner.Fatalf(format, args...)
}

// Implement all direct logging methods for entry
func (e *levelFilteringEntry) Trace(msg string) {
	if !e.parent.shouldLog(sflog.LevelTrace) {
		return
	}
	e.inner.Trace(msg)
}

func (e *levelFilteringEntry) Debug(msg string) {
	if !e.parent.shouldLog(sflog.LevelDebug) {
		return
	}
	e.inner.Debug(msg)
}

func (e *levelFilteringEntry) Info(msg string) {
	if !e.parent.shouldLog(sflog.LevelInfo) {
		return
	}
	e.inner.Info(msg)
}

func (e *levelFilteringEntry) Warn(msg string) {
	if !e.parent.shouldLog(sflog.LevelWarn) {
		return
	}
	e.inner.Warn(msg)
}

func (e *levelFilteringEntry) Error(msg string) {
	if !e.parent.shouldLog(sflog.LevelError) {
		return
	}
	e.inner.Error(msg)
}

func (e *levelFilteringEntry) Fatal(msg string) {
	e.inner.Fatal(msg)
}
