package gosnowflake

import (
	"crypto/tls"
	"sync"
)

var (
	tlsConfigLock     sync.RWMutex
	tlsConfigRegistry = make(map[string]*tls.Config)
)

// RegisterTLSConfig registers a custom tls.Config to be used with sql.Open.
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

// getTLSConfig returns a TLS config from the registry.
func getTLSConfig(key string) (*tls.Config, bool) {
	tlsConfigLock.RLock()
	tlsConfig, ok := tlsConfigRegistry[key]
	tlsConfigLock.RUnlock()
	if !ok {
		return nil, false
	}
	return tlsConfig.Clone(), true
}
