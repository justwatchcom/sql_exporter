package gosnowflake

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var paramsMutex *sync.Mutex

// SnowflakeDriver is a context of Go Driver
type SnowflakeDriver struct{}

// Open creates a new connection.
func (d SnowflakeDriver) Open(dsn string) (driver.Conn, error) {
	var cfg *Config
	var err error
	logger.Info("Open")
	ctx := context.Background()
	if dsn == "autoConfig" {
		cfg, err = loadConnectionConfig()
	} else {
		cfg, err = ParseDSN(dsn)
	}
	if err != nil {
		return nil, err
	}
	return d.OpenWithConfig(ctx, *cfg)
}

// OpenConnector creates a new connector with parsed DSN.
func (d SnowflakeDriver) OpenConnector(dsn string) (driver.Connector, error) {
	var cfg *Config
	var err error
	if dsn == "autoConfig" {
		cfg, err = loadConnectionConfig()
	} else {
		cfg, err = ParseDSN(dsn)
	}
	if err != nil {
		return Connector{}, err
	}
	return NewConnector(d, *cfg), nil
}

// OpenWithConfig creates a new connection with the given Config.
func (d SnowflakeDriver) OpenWithConfig(ctx context.Context, config Config) (driver.Conn, error) {
	timer := time.Now()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.Params == nil {
		config.Params = make(map[string]*string)
	}
	if config.Tracing != "" {
		if err := logger.SetLogLevel(config.Tracing); err != nil {
			return nil, err
		}
	}
	logger.WithContext(ctx).Info("OpenWithConfig")

	if config.ConnectionDiagnosticsEnabled {
		connDiagDownloadCrl := (config.CertRevocationCheckMode.String() == "ADVISORY") || (config.CertRevocationCheckMode.String() == "ENABLED")
		logger.WithContext(ctx).Infof("Connection diagnostics enabled. Allowlist file specified in config: %s, will download CRLs in certificates: %s",
			config.ConnectionDiagnosticsAllowlistFile, strconv.FormatBool(connDiagDownloadCrl))
		performDiagnosis(&config, connDiagDownloadCrl)
		logger.WithContext(ctx).Info("Connection diagnostics finished.")
		logger.WithContext(ctx).Warn("A connection to Snowflake was not created because the driver is running in diagnostics mode. If this is unintended then disable diagnostics check by removing the ConnectionDiagnosticsEnabled connection parameter")
		os.Exit(0)
	}
	sc, err := buildSnowflakeConn(ctx, config)
	if err != nil {
		return nil, err
	}

	if strings.HasSuffix(strings.ToLower(config.Host), cnDomain) {
		logger.WithContext(ctx).Info("Connecting to CHINA Snowflake domain")
	} else {
		logger.WithContext(ctx).Info("Connecting to GLOBAL Snowflake domain")
	}

	if err = authenticateWithConfig(sc); err != nil {
		logger.WithContext(ctx).Errorf("Failed to authenticate. Connection failed after %v milliseconds", time.Since(timer).String())
		return nil, err
	}
	sc.connectionTelemetry(&config)

	sc.startHeartBeat()
	sc.internal = &httpClient{sr: sc.rest}
	// Check context before returning since connectionTelemetry doesn't handle cancellation
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	logger.WithContext(ctx).Infof("Connected successfully after %v milliseconds", time.Since(timer).String())
	return sc, nil
}

func runningOnGithubAction() bool {
	return os.Getenv("GITHUB_ACTIONS") != ""
}

// GOSNOWFLAKE_SKIP_REGISTERATION is an environment variable which can be set client side to
// bypass dbSql driver registration. This should not be used if sql.Open() is used as the method
// to connect to the server, as sql.Open will require registration so it can map the driver name
// to the driver type, which in this case is "snowflake" and SnowflakeDriver{}. If you wish to call
// into multiple versions of the driver from one client, this is needed because calling register
// twice with the same name on init will cause the driver to panic.
func skipRegistration() bool {
	return os.Getenv("GOSNOWFLAKE_SKIP_REGISTERATION") != ""
}

var logger = CreateDefaultLogger()

func init() {
	if !skipRegistration() {
		sql.Register("snowflake", &SnowflakeDriver{})
	}
	_ = logger.SetLogLevel("error")
	if runningOnGithubAction() {
		_ = logger.SetLogLevel("fatal")
	}
	paramsMutex = &sync.Mutex{}
}
