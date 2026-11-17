package logger

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"
)

// Storage for log keys and hooks (single source of truth)
var (
	contextConfigMu       sync.RWMutex
	logKeys               []any
	clientLogContextHooks map[string]ClientLogContextHook
)

// SetLogKeys sets the context keys to be extracted from context
// This function is thread-safe and can be called at runtime.
func SetLogKeys(keys []any) {
	contextConfigMu.Lock()
	defer contextConfigMu.Unlock()

	logKeys = make([]any, len(keys))
	copy(logKeys, keys)
}

// GetLogKeys returns a copy of the current log keys
func GetLogKeys() []any {
	contextConfigMu.RLock()
	defer contextConfigMu.RUnlock()

	keysCopy := make([]any, len(logKeys))
	copy(keysCopy, logKeys)
	return keysCopy
}

// RegisterLogContextHook registers a hook for extracting context fields
// This function is thread-safe and can be called at runtime.
func RegisterLogContextHook(key string, hook ClientLogContextHook) {
	contextConfigMu.Lock()
	defer contextConfigMu.Unlock()

	if clientLogContextHooks == nil {
		clientLogContextHooks = make(map[string]ClientLogContextHook)
	}
	clientLogContextHooks[key] = hook
}

// GetClientLogContextHooks returns a copy of registered hooks
func GetClientLogContextHooks() map[string]ClientLogContextHook {
	contextConfigMu.RLock()
	defer contextConfigMu.RUnlock()

	hooksCopy := make(map[string]ClientLogContextHook, len(clientLogContextHooks))
	maps.Copy(hooksCopy, clientLogContextHooks)
	return hooksCopy
}

// extractContextFields extracts log fields from context using LogKeys and ClientLogContextHooks
func extractContextFields(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}

	contextConfigMu.RLock()
	defer contextConfigMu.RUnlock()

	attrs := make([]slog.Attr, 0)

	// Built-in LogKeys
	for _, key := range logKeys {
		if val := ctx.Value(key); val != nil {
			keyStr := fmt.Sprint(key)

			if strVal, ok := val.(string); ok {
				attrs = append(attrs, slog.String(keyStr, MaskSecrets(strVal)))
			} else {
				masked := MaskSecrets(fmt.Sprint(val))
				attrs = append(attrs, slog.String(keyStr, masked))
			}
		}
	}

	// Custom hooks
	for key, hook := range clientLogContextHooks {
		if val := hook(ctx); val != "" {
			attrs = append(attrs, slog.String(key, MaskSecrets(val)))
		}
	}

	return attrs
}
