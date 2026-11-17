package gosnowflake

import (
	"crypto/tls"

	sfconfig "github.com/snowflakedb/gosnowflake/v2/internal/config"
)

// RegisterTLSConfig registers a custom tls.Config to be used with sql.Open.
// Use the key as a value in the DSN where tlsConfigName=value.
func RegisterTLSConfig(key string, cfg *tls.Config) error {
	return sfconfig.RegisterTLSConfig(key, cfg)
}

// DeregisterTLSConfig removes the tls.Config associated with key.
func DeregisterTLSConfig(key string) error {
	return sfconfig.DeregisterTLSConfig(key)
}
