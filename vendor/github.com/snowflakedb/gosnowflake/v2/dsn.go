package gosnowflake

import (
	sfconfig "github.com/snowflakedb/gosnowflake/v2/internal/config"
)

// Type aliases — re-exported from internal/config for backward compatibility.
type (
	// Config is a set of configuration parameters
	Config = sfconfig.Config
	// ConfigBool is a type to represent true or false in the Config
	ConfigBool = sfconfig.Bool
	// ConfigParam is used to bind the name of the Config field with the environment variable and set the requirement for it
	ConfigParam = sfconfig.Param
)

// ConfigBool constants — re-exported from internal/config.
const (
	// configBoolNotSet represents the default value for the config field which is not set
	configBoolNotSet = sfconfig.BoolNotSet
	// ConfigBoolTrue represents true for the config field
	ConfigBoolTrue = sfconfig.BoolTrue
	// ConfigBoolFalse represents false for the config field
	ConfigBoolFalse = sfconfig.BoolFalse
)

// DSN constructs a DSN for Snowflake db.
func DSN(cfg *Config) (string, error) { return sfconfig.DSN(cfg) }

// ParseDSN parses the DSN string to a Config.
func ParseDSN(dsn string) (*Config, error) { return sfconfig.ParseDSN(dsn) }

// GetConfigFromEnv is used to parse the environment variable values to specific fields of the Config
func GetConfigFromEnv(properties []*ConfigParam) (*Config, error) {
	return sfconfig.GetConfigFromEnv(properties)
}

func transportConfigFor(tt transportType) *transportConfig {
	return defaultTransportConfigs.forTransportType(tt)
}
