package logger

import (
	"fmt"
	"os"
)

// CloseFileOnLoggerReplace closes a log file when the logger is replaced.
// This is used by the easy logging feature to manage log file handles.
func CloseFileOnLoggerReplace(sflog any, file *os.File) error {
	// Try to get the underlying default logger
	if ell, ok := unwrapToEasyLoggingLogger(sflog); ok {
		return ell.CloseFileOnLoggerReplace(file)
	}
	return fmt.Errorf("logger does not support closeFileOnLoggerReplace")
}

// IsEasyLoggingLogger checks if the given logger is based on the default logger implementation.
// This is used by easy logging to determine if reconfiguration is allowed.
func IsEasyLoggingLogger(sflog any) bool {
	_, ok := unwrapToEasyLoggingLogger(sflog)
	return ok
}

// unwrapToEasyLoggingLogger unwraps a logger to get to the underlying default logger if present
func unwrapToEasyLoggingLogger(sflog any) (EasyLoggingSupport, bool) {
	current := sflog

	// Special case: if this is a Proxy, get the actual global logger
	if _, isProxy := current.(*Proxy); isProxy {
		current = GetLogger()
	}

	// Unwrap all layers
	for {
		if u, ok := current.(Unwrapper); ok {
			current = u.Unwrap()
			continue
		}
		break
	}

	// Check if it's a default logger by checking if it has EasyLoggingSupport
	if ell, ok := current.(EasyLoggingSupport); ok {
		return ell, true
	}

	return nil, false
}
