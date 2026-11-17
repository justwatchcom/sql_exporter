package config

import (
	"crypto/tls"
	"sync"
)

var (
	tlsConfigLock     sync.RWMutex
	tlsConfigRegistry = make(map[string]*tls.Config)
)

// ResetTLSConfigRegistry clears the TLS config registry. Used in tests.
func ResetTLSConfigRegistry() {
	tlsConfigLock.Lock()
	tlsConfigRegistry = make(map[string]*tls.Config)
	tlsConfigLock.Unlock()
}

// RegisterTLSConfig registers the tls.Config in configs registry.
// Use the key as a value in the DSN where tlsConfigName=value.
func RegisterTLSConfig(key string, config *tls.Config) error {
	tlsConfigLock.Lock()
	logger.Infof("Registering TLS config for key: %s", key)
	tlsConfigRegistry[key] = config.Clone()
	tlsConfigLock.Unlock()
	return nil
}

// DeregisterTLSConfig removes the tls.Config associated with key.
func DeregisterTLSConfig(key string) error {
	tlsConfigLock.Lock()
	logger.Infof("Deregistering TLS config for key: %s", key)
	delete(tlsConfigRegistry, key)
	tlsConfigLock.Unlock()
	return nil
}

// GetTLSConfig returns a TLS config from the registry.
func GetTLSConfig(key string) (*tls.Config, bool) {
	tlsConfigLock.RLock()
	tlsConfig, ok := tlsConfigRegistry[key]
	tlsConfigLock.RUnlock()
	if !ok {
		return nil, false
	}
	return tlsConfig.Clone(), true
}
