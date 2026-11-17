package gosnowflake

import (
	"context"
	"database/sql/driver"
	sfconfig "github.com/snowflakedb/gosnowflake/v2/internal/config"
)

// InternalSnowflakeDriver is the interface for an internal Snowflake driver
// Deprecated: will be removed in a future release.
type InternalSnowflakeDriver interface {
	Open(dsn string) (driver.Conn, error)
	OpenWithConfig(ctx context.Context, config Config) (driver.Conn, error)
}

// Connector creates Driver with the specified Config
type Connector struct {
	driver InternalSnowflakeDriver
	cfg    Config
}

// NewConnector creates a new connector with the given SnowflakeDriver and Config.
func NewConnector(driver InternalSnowflakeDriver, config Config) driver.Connector {
	// Begin loading minicore asynchronously as soon as the application
	// references the driver. database/sql.Open() invokes OpenConnector ->
	// NewConnector synchronously, so this gives the version a head start to be
	// warm by first login, without loading at package-import time when
	// gosnowflake is merely a transitive dependency (SNOW-3561155 / #1800).
	_ = getMiniCore()
	return Connector{driver, config}
}

// Connect creates a new connection.
func (t Connector) Connect(ctx context.Context) (driver.Conn, error) {
	cfg := t.cfg
	err := sfconfig.FillMissingConfigParameters(&cfg)
	if err != nil {
		return nil, err
	}
	return t.driver.OpenWithConfig(ctx, cfg)
}

// Driver creates a new driver.
func (t Connector) Driver() driver.Driver {
	return t.driver
}
