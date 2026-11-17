package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/snowflakedb/gosnowflake/v2/sflog"
)

// Proxy is a proxy that delegates all calls to the global logger.
// This ensures a single source of truth for the current logger.
type Proxy struct{}

// Compile-time verification that Proxy implements SFLogger
var _ sflog.SFLogger = (*Proxy)(nil)

// Tracef implements the Tracef method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) Tracef(format string, args ...any) {
	GetLogger().Tracef(format, args...)
}

// Debugf implements the Debugf method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) Debugf(format string, args ...any) {
	GetLogger().Debugf(format, args...)
}

// Infof implements the Infof method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) Infof(format string, args ...any) {
	GetLogger().Infof(format, args...)
}

// Warnf implements the Warnf method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) Warnf(format string, args ...any) {
	GetLogger().Warnf(format, args...)
}

// Errorf implements the Errorf method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) Errorf(format string, args ...any) {
	GetLogger().Errorf(format, args...)
}

// Fatalf implements the Fatalf method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) Fatalf(format string, args ...any) {
	GetLogger().Fatalf(format, args...)
}

// Trace implements the Trace method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) Trace(msg string) {
	GetLogger().Trace(msg)
}

// Debug implements the Debug method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) Debug(msg string) {
	GetLogger().Debug(msg)
}

// Info implements the Info method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) Info(msg string) {
	GetLogger().Info(msg)
}

// Warn implements the Warn method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) Warn(msg string) {
	GetLogger().Warn(msg)
}

// Error implements the Error method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) Error(msg string) {
	GetLogger().Error(msg)
}

// Fatal implements the Fatal method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) Fatal(msg string) {
	GetLogger().Fatal(msg)
}

// WithField implements the WithField method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) WithField(key string, value any) sflog.LogEntry {
	return GetLogger().WithField(key, value)
}

// WithFields implements the WithFields method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) WithFields(fields map[string]any) sflog.LogEntry {
	return GetLogger().WithFields(fields)
}

// WithContext implements the WithContext method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) WithContext(ctx context.Context) sflog.LogEntry {
	return GetLogger().WithContext(ctx)
}

// SetLogLevel implements the SetLogLevel method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) SetLogLevel(level string) error {
	return GetLogger().SetLogLevel(level)
}

// SetLogLevelInt implements the SetLogLevelInt method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) SetLogLevelInt(level sflog.Level) error {
	return GetLogger().SetLogLevelInt(level)
}

// GetLogLevel implements the GetLogLevel method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) GetLogLevel() string {
	return GetLogger().GetLogLevel()
}

// GetLogLevelInt implements the GetLogLevelInt method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) GetLogLevelInt() sflog.Level {
	return GetLogger().GetLogLevelInt()
}

// SetOutput implements the SetOutput method of the SFLogger interface by delegating to the global logger.
func (p *Proxy) SetOutput(output io.Writer) {
	GetLogger().SetOutput(output)
}

// SetHandler implements SFSlogLogger interface for advanced slog handler configuration.
// This delegates to the underlying logger if it supports SetHandler.
func (p *Proxy) SetHandler(handler slog.Handler) error {
	logger := GetLogger()

	if sl, ok := logger.(sflog.SFSlogLogger); ok {
		return sl.SetHandler(handler)
	}

	return fmt.Errorf("underlying logger does not support SetHandler")
}

// NewLoggerProxy creates a new logger proxy that delegates all calls
// to the global logger managed by the internal package.
func NewLoggerProxy() sflog.SFLogger {
	return &Proxy{}
}
