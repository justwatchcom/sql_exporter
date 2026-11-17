package logger

import "os"

// EasyLoggingSupport is an optional interface for loggers that support easy_logging.go
// functionality. This is used for file-based logging configuration.
type EasyLoggingSupport interface {
	// CloseFileOnLoggerReplace closes the logger's file handle when logger is replaced
	CloseFileOnLoggerReplace(file *os.File) error
}

// Unwrapper is a common interface for unwrapping wrapped loggers
type Unwrapper interface {
	Unwrap() any
}
