package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/snowflakedb/gosnowflake/v2/sflog"
)

// formatSource formats caller information for logging
func formatSource(frame *runtime.Frame) (string, string) {
	return path.Base(frame.Function), fmt.Sprintf("%s:%d", path.Base(frame.File), frame.Line)
}

// findActualCaller walks the call stack to find the first caller outside internal/logger package.
// This allows the logger to report the correct source location regardless of wrapper layers.
func findActualCaller() uintptr {
	var pcs [16]uintptr
	n := runtime.Callers(1, pcs[:])

	frames := runtime.CallersFrames(pcs[:n])

	// Track the last logger frame's PC as we iterate
	var lastLoggerPC uintptr
	sawLogger := false

	for {
		frame, more := frames.Next()

		// A frame is considered part of the logger if it's in internal/logger package
		// but NOT a test file (we want test files to be considered as callers).
		// Normalize path separators to handle both Unix (/) and Windows (\) paths.
		isLogger := strings.Contains(strings.ReplaceAll(frame.File, "\\", "/"), "internal/logger") && !strings.HasSuffix(frame.File, "_test.go")

		if isLogger {
			lastLoggerPC = frame.PC
			sawLogger = true
		} else if sawLogger {
			// We've transitioned from logger to non-logger - this is the caller
			return frame.PC
		}

		if !more {
			break
		}
	}

	// Fallback: if we saw logger frames, return the last one; otherwise return the last frame overall
	if lastLoggerPC != 0 {
		return lastLoggerPC
	}
	if n > 0 {
		return pcs[n-1]
	}
	return 0
}

// rawLogger implements SFLogger using slog
type rawLogger struct {
	inner   *slog.Logger
	handler *snowflakeHandler
	// level is read on every filtered log call. It is atomic so that read does not take mu.
	// int64 because sflog.LevelOff is math.MaxInt, which does not fit in an int32 on 64-bit platforms.
	level   atomic.Int64
	enabled bool // For OFF level support
	file    *os.File
	output  io.Writer
	mu      sync.Mutex
}

// Compile-time verification that rawLogger implements SFLogger
var _ SFLogger = (*rawLogger)(nil)

// newRawLogger creates the internal default logger using slog
func newRawLogger() SFLogger {
	level := sflog.LevelInfo

	opts := createOpts(slog.Level(level))

	textHandler := slog.NewTextHandler(os.Stderr, opts)
	handler := newSnowflakeHandler(textHandler, level)

	slogLogger := slog.New(handler)

	log := &rawLogger{
		inner:   slogLogger,
		handler: handler,
		enabled: true,
		output:  os.Stderr,
	}
	log.level.Store(int64(level))
	return log
}

// isEnabled checks if logging is enabled (for OFF level)
func (log *rawLogger) isEnabled() bool {
	log.mu.Lock()
	defer log.mu.Unlock()
	return log.enabled
}

// SetLogLevel sets the log level
func (log *rawLogger) SetLogLevel(level string) error {
	upperLevel, err := sflog.ParseLevel(strings.ToUpper(level))
	if err != nil {
		return fmt.Errorf("error while setting log level. %v", err)
	}

	if upperLevel == sflog.LevelOff {
		log.mu.Lock()
		log.level.Store(int64(sflog.LevelOff))
		log.enabled = false
		log.mu.Unlock()
		return nil
	}

	log.mu.Lock()
	log.enabled = true
	log.level.Store(int64(upperLevel))
	log.mu.Unlock()

	return nil
}

func (log *rawLogger) SetLogLevelInt(level sflog.Level) error {
	log.mu.Lock()
	defer log.mu.Unlock()

	_, err := sflog.LevelToString(level)
	if err != nil {
		return fmt.Errorf("invalid log level: %d", level)
	}
	log.level.Store(int64(level))
	return nil
}

// GetLogLevel returns the current log level
func (log *rawLogger) GetLogLevel() string {
	if levelStr, err := sflog.LevelToString(log.logLevel()); err == nil {
		return levelStr
	}
	return "unknown"
}

func (log *rawLogger) GetLogLevelInt() sflog.Level {
	return log.logLevel()
}

func (log *rawLogger) logLevel() sflog.Level {
	return sflog.Level(log.level.Load())
}

// SetOutput sets the output writer
func (log *rawLogger) SetOutput(output io.Writer) {
	log.mu.Lock()
	defer log.mu.Unlock()

	log.output = output

	// Create new handler with new output
	opts := createOpts(slog.Level(log.logLevel()))

	textHandler := slog.NewTextHandler(output, opts)
	log.handler = newSnowflakeHandler(textHandler, log.logLevel())
	log.inner = slog.New(log.handler)
}

func createOpts(level slog.Level) *slog.HandlerOptions {
	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: true,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					return slog.String(slog.TimeKey, t.Format(time.RFC3339Nano))
				}
			}
			if a.Key == slog.SourceKey {
				if src, ok := a.Value.Any().(*slog.Source); ok {
					frame := &runtime.Frame{
						File:     src.File,
						Line:     src.Line,
						Function: src.Function,
					}
					_, location := formatSource(frame)
					return slog.String(slog.SourceKey, location)
				}
			}
			return a
		},
	}
	return opts
}

// SetHandler sets a custom slog handler (implements SFSlogLogger interface)
// The provided handler will be wrapped with snowflakeHandler to preserve context extraction.
// Secret masking is handled at a higher level (secretMaskingLogger wrapper).
func (log *rawLogger) SetHandler(handler slog.Handler) error {
	log.mu.Lock()
	defer log.mu.Unlock()

	// Wrap user's handler with snowflakeHandler to preserve context extraction
	log.handler = newSnowflakeHandler(handler, log.logLevel())
	log.inner = slog.New(log.handler)

	return nil
}

// log logs a message at the given level, automatically finding the actual caller outside internal/logger.
func (log *rawLogger) log(level sflog.Level, msg string) {
	if !log.isEnabled() {
		return
	}
	pc := findActualCaller()
	r := slog.NewRecord(time.Now(), slog.Level(level), msg, pc)
	_ = log.handler.Handle(context.Background(), r)
}

// Implement all formatted logging methods (*f variants)
// Uses automatic caller detection to find the actual caller outside internal/logger package.
func (log *rawLogger) Tracef(format string, args ...any) {
	log.log(sflog.LevelTrace, fmt.Sprintf(format, args...))
}

func (log *rawLogger) Debugf(format string, args ...any) {
	log.log(sflog.LevelDebug, fmt.Sprintf(format, args...))
}

func (log *rawLogger) Infof(format string, args ...any) {
	log.log(sflog.LevelInfo, fmt.Sprintf(format, args...))
}

func (log *rawLogger) Warnf(format string, args ...any) {
	log.log(sflog.LevelWarn, fmt.Sprintf(format, args...))
}

func (log *rawLogger) Errorf(format string, args ...any) {
	log.log(sflog.LevelError, fmt.Sprintf(format, args...))
}

func (log *rawLogger) Fatalf(format string, args ...any) {
	log.log(sflog.LevelFatal, fmt.Sprintf(format, args...))
	os.Exit(1)
}

// Implement all direct logging methods
// Uses automatic caller detection to find the actual caller outside internal/logger package.
func (log *rawLogger) Trace(msg string) {
	log.log(sflog.LevelTrace, msg)
}

func (log *rawLogger) Debug(msg string) {
	log.log(sflog.LevelDebug, msg)
}

func (log *rawLogger) Info(msg string) {
	log.log(sflog.LevelInfo, msg)
}

func (log *rawLogger) Warn(msg string) {
	log.log(sflog.LevelWarn, msg)
}

func (log *rawLogger) Error(msg string) {
	log.log(sflog.LevelError, msg)
}

func (log *rawLogger) Fatal(msg string) {
	log.log(sflog.LevelFatal, msg)
	os.Exit(1)
}

// Structured logging methods
func (log *rawLogger) WithField(key string, value any) LogEntry {
	return &slogEntry{
		logger:  log.inner.With(slog.Any(key, value)),
		enabled: &log.enabled,
		mu:      &log.mu,
	}
}

func (log *rawLogger) WithFields(fields map[string]any) LogEntry {
	attrs := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		attrs = append(attrs, k, v)
	}
	return &slogEntry{
		logger:  log.inner.With(attrs...),
		enabled: &log.enabled,
		mu:      &log.mu,
	}
}

func (log *rawLogger) WithContext(ctx context.Context) LogEntry {
	if ctx == nil {
		return log
	}

	// Extract fields from context
	attrs := extractContextFields(ctx)
	if len(attrs) == 0 {
		return log
	}

	// Convert []slog.Attr to []any for With()
	// slog.Logger.With() can accept slog.Attr directly
	args := make([]any, len(attrs))
	for i, attr := range attrs {
		args[i] = attr
	}

	newLogger := log.inner.With(args...)

	return &slogEntry{
		logger:  newLogger,
		enabled: &log.enabled,
		mu:      &log.mu,
	}
}

// slogEntry implements LogEntry
type slogEntry struct {
	logger  *slog.Logger
	enabled *bool
	mu      *sync.Mutex
}

// Compile-time verification that slogEntry implements LogEntry
var _ LogEntry = (*slogEntry)(nil)

func (e *slogEntry) isEnabled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return *e.enabled
}

// log logs a message at the given level, automatically finding the actual caller outside internal/logger.
func (e *slogEntry) log(level sflog.Level, msg string) {
	if !e.isEnabled() {
		return
	}
	pc := findActualCaller()
	r := slog.NewRecord(time.Now(), slog.Level(level), msg, pc)
	_ = e.logger.Handler().Handle(context.Background(), r)
}

// Implement all formatted logging methods (*f variants)
// Uses automatic caller detection to find the actual caller outside internal/logger package.
func (e *slogEntry) Tracef(format string, args ...any) {
	e.log(sflog.LevelTrace, fmt.Sprintf(format, args...))
}

func (e *slogEntry) Debugf(format string, args ...any) {
	e.log(sflog.LevelDebug, fmt.Sprintf(format, args...))
}

func (e *slogEntry) Infof(format string, args ...any) {
	e.log(sflog.LevelInfo, fmt.Sprintf(format, args...))
}

func (e *slogEntry) Warnf(format string, args ...any) {
	e.log(sflog.LevelWarn, fmt.Sprintf(format, args...))
}

func (e *slogEntry) Errorf(format string, args ...any) {
	e.log(sflog.LevelError, fmt.Sprintf(format, args...))
}

func (e *slogEntry) Fatalf(format string, args ...any) {
	e.log(sflog.LevelFatal, fmt.Sprintf(format, args...))
	os.Exit(1)
}

// Implement all direct logging methods
// Uses automatic caller detection to find the actual caller outside internal/logger package.
func (e *slogEntry) Trace(msg string) {
	e.log(sflog.LevelTrace, msg)
}

func (e *slogEntry) Debug(msg string) {
	e.log(sflog.LevelDebug, msg)
}

func (e *slogEntry) Info(msg string) {
	e.log(sflog.LevelInfo, msg)
}

func (e *slogEntry) Warn(msg string) {
	e.log(sflog.LevelWarn, msg)
}

func (e *slogEntry) Error(msg string) {
	e.log(sflog.LevelError, msg)
}

func (e *slogEntry) Fatal(msg string) {
	e.log(sflog.LevelFatal, msg)
	os.Exit(1)
}

// Helper methods for internal use and easy_logging support
func (log *rawLogger) closeFileOnLoggerReplace(file *os.File) error {
	log.mu.Lock()
	defer log.mu.Unlock()

	if log.file != nil && log.file != file {
		return fmt.Errorf("could not set a file to close on logger reset because there were already set one")
	}
	log.file = file
	return nil
}

// CloseFileOnLoggerReplace is exported for easy_logging support
func (log *rawLogger) CloseFileOnLoggerReplace(file *os.File) error {
	return log.closeFileOnLoggerReplace(file)
}

// ReplaceGlobalLogger closes the current logger's file (for easy_logging support)
// The actual global logger replacement is handled by the main package
func (log *rawLogger) ReplaceGlobalLogger(newLogger any) {
	if log.file != nil {
		_ = log.file.Close()
	}
}

// Ensure rawLogger implements SFLogger
var _ SFLogger = (*rawLogger)(nil)
