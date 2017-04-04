package leveled

import (
	"os"
	"strings"

	"github.com/go-kit/kit/log"
)

// Levels:
// 0 - trace / default
// 1 - debug
// 2 - info
// 3 - warn
// 4 - error
// 5 - fatal
var lvlMap = map[string]uint32{
	"trace":   0,
	"debug":   1,
	"info":    2,
	"warn":    3,
	"warning": 3,
	"error":   4,
	"fatal":   5,
}

var revLvlMap map[uint32]string

func init() {
	revLvlMap = make(map[uint32]string, len(lvlMap))

	for k, v := range lvlMap {
		revLvlMap[v] = k
	}
}

// Logger is a level-filtering logger
type Logger struct {
	logger   log.Logger
	minLevel uint32
}

// NewFromEnv creates a new leveled logger by reading the
// desired level from LOGLEVEL
func NewFromEnv(l log.Logger) *Logger {
	loglevel := "info"
	if l := os.Getenv("LOGLEVEL"); l != "" {
		loglevel = l
	}
	return New(l, loglevel)
}

// New creates a new level filtering logger
func New(l log.Logger, lvl string) *Logger {
	return &Logger{
		logger:   l,
		minLevel: strToLvl(lvl),
	}
}

// Log replaces all value elements (odd indexes) containing a Valuer in the
// stored context with their generated value, appends keyvals, and passes the
// result to the wrapped Logger.
func (l *Logger) Log(keyvals ...interface{}) error {
	for i := 0; i < len(keyvals); i += 2 {
		k := keyvals[i]
		kStr, ok := k.(string)
		if !ok {
			continue
		}
		if kStr != "level" {
			continue
		}
		var v interface{} = log.ErrMissingValue
		if i+1 < len(keyvals) {
			v = keyvals[i+1]
		}
		if strToLvl(v) < l.minLevel {
			return nil
		}
	}
	kvs := append([]interface{}{}, keyvals...)
	bindValues(kvs[:len(keyvals)])
	return l.logger.Log(kvs...)
}

func strToLvl(v interface{}) uint32 {
	s, ok := v.(string)
	if !ok {
		return 0
	}
	s = strings.ToLower(s)
	if val, found := lvlMap[s]; found {
		return val
	}
	return 0
}

// bindValues replaces all value elements (odd indexes) containing a Valuer
// with their generated value.
func bindValues(keyvals []interface{}) {
	for i := 1; i < len(keyvals); i += 2 {
		if v, ok := keyvals[i].(log.Valuer); ok {
			keyvals[i] = v()
		}
	}
}
