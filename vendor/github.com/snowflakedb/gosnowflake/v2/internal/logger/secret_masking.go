package logger

import (
	"context"
	"fmt"
	"github.com/snowflakedb/gosnowflake/v2/sflog"
	"io"
	"log/slog"
)

// secretMaskingLogger wraps any logger implementation and ensures
// all log messages have secrets masked before being passed to the inner logger.
type secretMaskingLogger struct {
	inner SFLogger
}

// Compile-time verification that secretMaskingLogger implements SFLogger
var _ SFLogger = (*secretMaskingLogger)(nil)

// Unwrap returns the inner logger (for introspection by easy_logging)
func (l *secretMaskingLogger) Unwrap() any {
	return l.inner
}

// newSecretMaskingLogger creates a new secret masking wrapper around the provided logger.
func newSecretMaskingLogger(inner SFLogger) *secretMaskingLogger {
	if inner == nil {
		panic("inner logger cannot be nil")
	}

	return &secretMaskingLogger{inner: inner}
}

// Helper methods for masking
func (l *secretMaskingLogger) maskValue(value any) any {
	if str, ok := value.(string); ok {
		return l.maskString(str)
	}
	// For other types, convert to string, mask, but return original type if no secrets
	strVal := fmt.Sprint(value)
	masked := l.maskString(strVal)
	if masked != strVal {
		return masked // Secrets found and masked
	}
	return value // No secrets, return original
}

func (l *secretMaskingLogger) maskString(value string) string {
	return MaskSecrets(value)
}

// Implement all formatted logging methods (*f variants)
func (l *secretMaskingLogger) Tracef(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	maskedMessage := l.maskString(message)
	l.inner.Trace(maskedMessage)
}

func (l *secretMaskingLogger) Debugf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	maskedMessage := l.maskString(message)
	l.inner.Debug(maskedMessage)
}

func (l *secretMaskingLogger) Infof(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	maskedMessage := l.maskString(message)
	l.inner.Info(maskedMessage)
}

func (l *secretMaskingLogger) Warnf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	maskedMessage := l.maskString(message)
	l.inner.Warn(maskedMessage)
}

func (l *secretMaskingLogger) Errorf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	maskedMessage := l.maskString(message)
	l.inner.Error(maskedMessage)
}

func (l *secretMaskingLogger) Fatalf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	maskedMessage := l.maskString(message)
	l.inner.Fatal(maskedMessage)
}

// Implement all direct logging methods
func (l *secretMaskingLogger) Trace(msg string) {
	l.inner.Trace(l.maskString(msg))
}

func (l *secretMaskingLogger) Debug(msg string) {
	l.inner.Debug(l.maskString(msg))
}

func (l *secretMaskingLogger) Info(msg string) {
	l.inner.Info(l.maskString(msg))
}

func (l *secretMaskingLogger) Warn(msg string) {
	l.inner.Warn(l.maskString(msg))
}

func (l *secretMaskingLogger) Error(msg string) {
	l.inner.Error(l.maskString(msg))
}

func (l *secretMaskingLogger) Fatal(msg string) {
	l.inner.Fatal(l.maskString(msg))
}

// Implement structured logging methods
// Note: These return LogEntry to maintain compatibility with the adapter layer
func (l *secretMaskingLogger) WithField(key string, value any) LogEntry {
	maskedValue := l.maskValue(value)
	result := l.inner.WithField(key, maskedValue)
	return &secretMaskingEntry{
		inner:  result,
		parent: l,
	}
}

func (l *secretMaskingLogger) WithFields(fields map[string]any) LogEntry {
	maskedFields := make(map[string]any, len(fields))
	for k, v := range fields {
		maskedFields[k] = l.maskValue(v)
	}
	result := l.inner.WithFields(maskedFields)
	return &secretMaskingEntry{
		inner:  result,
		parent: l,
	}
}

func (l *secretMaskingLogger) WithContext(ctx context.Context) LogEntry {
	result := l.inner.WithContext(ctx)
	return &secretMaskingEntry{
		inner:  result,
		parent: l,
	}
}

// Delegate configuration methods
func (l *secretMaskingLogger) SetLogLevel(level string) error {
	return l.inner.SetLogLevel(level)
}

func (l *secretMaskingLogger) SetLogLevelInt(level sflog.Level) error {
	return l.inner.SetLogLevelInt(level)
}

func (l *secretMaskingLogger) GetLogLevel() string {
	return l.inner.GetLogLevel()
}

func (l *secretMaskingLogger) GetLogLevelInt() sflog.Level {
	return l.inner.GetLogLevelInt()
}

func (l *secretMaskingLogger) SetOutput(output io.Writer) {
	l.inner.SetOutput(output)
}

// SetHandler delegates to inner logger's SetHandler (for slog handler configuration)
func (l *secretMaskingLogger) SetHandler(handler slog.Handler) error {
	if logger, ok := l.inner.(sflog.SFSlogLogger); ok {
		return logger.SetHandler(handler)
	}
	return fmt.Errorf("inner logger does not support SetHandler")
}

// secretMaskingEntry wraps a log entry and masks all secrets.
type secretMaskingEntry struct {
	inner  LogEntry
	parent *secretMaskingLogger
}

// Compile-time verification that secretMaskingEntry implements LogEntry
var _ LogEntry = (*secretMaskingEntry)(nil)

// Implement all formatted logging methods (*f variants)
func (e *secretMaskingEntry) Tracef(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	maskedMessage := MaskSecrets(message)
	e.inner.Trace(maskedMessage)
}

func (e *secretMaskingEntry) Debugf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	maskedMessage := MaskSecrets(message)
	e.inner.Debug(maskedMessage)
}

func (e *secretMaskingEntry) Infof(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	maskedMessage := MaskSecrets(message)
	e.inner.Info(maskedMessage)
}

func (e *secretMaskingEntry) Warnf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	maskedMessage := MaskSecrets(message)
	e.inner.Warn(maskedMessage)
}

func (e *secretMaskingEntry) Errorf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	maskedMessage := MaskSecrets(message)
	e.inner.Error(maskedMessage)
}

func (e *secretMaskingEntry) Fatalf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	maskedMessage := MaskSecrets(message)
	e.inner.Fatal(maskedMessage)
}

// Implement all direct logging methods
func (e *secretMaskingEntry) Trace(msg string) {
	e.inner.Trace(e.parent.maskString(msg))
}

func (e *secretMaskingEntry) Debug(msg string) {
	e.inner.Debug(e.parent.maskString(msg))
}

func (e *secretMaskingEntry) Info(msg string) {
	e.inner.Info(e.parent.maskString(msg))
}

func (e *secretMaskingEntry) Warn(msg string) {
	e.inner.Warn(e.parent.maskString(msg))
}

func (e *secretMaskingEntry) Error(msg string) {
	e.inner.Error(e.parent.maskString(msg))
}

func (e *secretMaskingEntry) Fatal(msg string) {
	e.inner.Fatal(e.parent.maskString(msg))
}
