package athena

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/athena"
)

var (
	openFromSessionMutex sync.Mutex
	openFromSessionCount int
)

// Driver is a sql.Driver. It's intended for db/sql.Open().
type Driver struct {
	cfg *DriverConfig
}

// NewDriver allows you to register your own driver with `sql.Register`.
// It's useful for more complex use cases. Read more in PR #3.
// https://github.com/segmentio/go-athena/pull/3
//
// Generally, sql.Open() or athena.Open() should suffice.
func NewDriver(cfg *DriverConfig) *Driver {
	return &Driver{cfg}
}

func init() {
	var drv driver.Driver = &Driver{}
	sql.Register("athena", drv)
}

// Open should be used via `db/sql.Open("athena", "<params>")`.
// The following parameters are supported in URI query format (k=v&k2=v2&...)
//
// - `db` (required)
// This is the Athena database name. In the UI, this defaults to "default",
// but the driver requires it regardless.
//
// - `output_location` (required)
// This is the S3 location Athena will dump query results in the format
// "s3://bucket/and/so/forth". In the AWS UI, this defaults to
// "s3://aws-athena-query-results-<ACCOUNTID>-<REGION>", but the driver requires it.
//
// - `poll_frequency` (optional)
// Athena's API requires polling to retrieve query results. This is the frequency at
// which the driver will poll for results. It should be a time/Duration.String().
// A completely arbitrary default of "5s" was chosen.
//
// - `region` (optional)
// Override AWS region. Useful if it is not set with environment variable.
//
// Credentials must be accessible via the SDK's Default Credential Provider Chain.
// For more advanced AWS credentials/session/config management, please supply
// a custom AWS session directly via `athena.Open()`.
func (d *Driver) Open(connStr string) (driver.Conn, error) {
	cfg := d.cfg
	if cfg == nil {
		var err error
		// TODO: Implement DriverContext to get proper access to context
		cfg, err = configFromConnectionString(context.TODO(), connStr)
		if err != nil {
			return nil, err
		}
	}

	if cfg.PollFrequency == 0 {
		cfg.PollFrequency = 5 * time.Second
	}

	return &conn{
		athena:         athena.NewFromConfig(*cfg.Config),
		db:             cfg.Database,
		OutputLocation: cfg.OutputLocation,
		pollFrequency:  cfg.PollFrequency,
	}, nil
}

// Open is a more robust version of `db.Open`, as it accepts a raw aws.Session.
// This is useful if you have a complex AWS session since the driver doesn't
// currently attempt to serialize all options into a string.
func Open(cfg DriverConfig) (*sql.DB, error) {
	if cfg.Database == "" {
		return nil, errors.New("db is required")
	}

	if cfg.OutputLocation == "" {
		return nil, errors.New("s3_staging_url is required")
	}

	if cfg.Config == nil {
		return nil, errors.New("AWS config is required")
	}

	// This hack was copied from jackc/pgx. Sorry :(
	// https://github.com/jackc/pgx/blob/70a284f4f33a9cc28fd1223f6b83fb00deecfe33/stdlib/sql.go#L130-L136
	openFromSessionMutex.Lock()
	openFromSessionCount++
	name := fmt.Sprintf("athena-%d", openFromSessionCount)
	openFromSessionMutex.Unlock()

	sql.Register(name, &Driver{&cfg})
	return sql.Open(name, "")
}

// Config is the input to Open().
type DriverConfig struct {
	Config         *aws.Config
	Database       string
	OutputLocation string

	PollFrequency time.Duration
}

func configFromConnectionString(ctx context.Context, connStr string) (*DriverConfig, error) {
	args, err := url.ParseQuery(connStr)
	if err != nil {
		return nil, err
	}

	var cfg DriverConfig

	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	if region := args.Get("region"); region != "" {
		awsConfig.Region = region
	}
	cfg.Config = &awsConfig

	cfg.Database = args.Get("db")
	cfg.OutputLocation = args.Get("output_location")

	frequencyStr := args.Get("poll_frequency")
	if frequencyStr != "" {
		cfg.PollFrequency, err = time.ParseDuration(frequencyStr)
		if err != nil {
			return nil, fmt.Errorf("invalid poll_frequency parameter: %s", frequencyStr)
		}
	}

	return &cfg, nil
}
