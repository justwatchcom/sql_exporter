package sflog

import (
	"fmt"
	"math"
	"strings"
)

// Level represents the log level for a log message. It extends slog's standard levels with custom levels.
type Level int

// Custom level constants that extend slog's standard levels
const (
	LevelTrace = Level(-8)
	LevelDebug = Level(-4)
	LevelInfo  = Level(0)
	LevelWarn  = Level(4)
	LevelError = Level(8)
	LevelFatal = Level(12)
	LevelOff   = Level(math.MaxInt)
)

// ParseLevel converts a string level to Level
func ParseLevel(level string) (Level, error) {
	switch strings.ToUpper(level) {
	case "TRACE":
		return LevelTrace, nil
	case "DEBUG":
		return LevelDebug, nil
	case "INFO":
		return LevelInfo, nil
	case "WARN":
		return LevelWarn, nil
	case "ERROR":
		return LevelError, nil
	case "FATAL":
		return LevelFatal, nil
	case "OFF":
		return LevelOff, nil
	default:
		return LevelInfo, fmt.Errorf("unknown log level: %s", level)
	}
}

// LevelToString converts Level to string
func LevelToString(level Level) (string, error) {
	switch level {
	case LevelTrace:
		return "TRACE", nil
	case LevelDebug:
		return "DEBUG", nil
	case LevelInfo:
		return "INFO", nil
	case LevelWarn:
		return "WARN", nil
	case LevelError:
		return "ERROR", nil
	case LevelFatal:
		return "FATAL", nil
	case LevelOff:
		return "OFF", nil
	default:
		return "", fmt.Errorf("unknown log level: %d", level)
	}
}
