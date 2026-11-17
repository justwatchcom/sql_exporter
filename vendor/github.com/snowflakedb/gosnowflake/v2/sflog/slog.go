package sflog

import "log/slog"

// SFSlogLogger is an optional interface for advanced slog handler configuration.
// This interface is separate from SFLogger to maintain framework-agnostic design.
// Users can type-assert the logger to check if slog handler configuration is supported.
//
// Example usage:
//
//	logger := gosnowflake.GetLogger()
//	if slogLogger, ok := logger.(gosnowflake.SFSlogLogger); ok {
//	    customHandler := slog.NewJSONHandler(os.Stdout, nil)
//	    slogLogger.SetHandler(customHandler)
//	}
type SFSlogLogger interface {
	SetHandler(handler slog.Handler) error
}
