package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/jmoiron/sqlx"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/robfig/cron/v3"
	"github.com/snowflakedb/gosnowflake"
	"gopkg.in/yaml.v2"
)

func getenv(key, defaultVal string) string {
	if val, found := os.LookupEnv(key); found {
		return val
	}
	return defaultVal
}

var (
	metricsPrefix = "sql_exporter"
	failedScrapes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: fmt.Sprintf("%s_last_scrape_failed", metricsPrefix),
			Help: "Failed scrapes",
		},
		[]string{"driver", "host", "database", "user", "sql_job", "query"},
	)
	tmplStart                 = getenv("TEMPLATE_START", "{{")
	tmplEnd                   = getenv("TEMPLATE_END", "}}")
	reEnvironmentPlaceholders = regexp.MustCompile(
		fmt.Sprintf(
			"%s.+?%s",
			regexp.QuoteMeta(tmplStart),
			regexp.QuoteMeta(tmplEnd),
		),
	)
	QueryMetricsLabels = []string{"sql_job", "query"}
	queryCounter       = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: fmt.Sprintf("%s_queries_total", metricsPrefix),
	}, QueryMetricsLabels)
	failedQueryCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: fmt.Sprintf("%s_query_failures_total", metricsPrefix),
	}, QueryMetricsLabels)

	// Those are the default buckets
	DefaultQueryDurationHistogramBuckets = prometheus.DefBuckets
	// To make the buckets configurable lets init it after loading the configuration.
	queryDurationHistogram *prometheus.HistogramVec
)

func init() {
	prometheus.MustRegister(failedScrapes)
}

// Read attempts to parse the given config and return a file
// object
func Read(path string) (File, error) {
	f := File{}

	fh, err := os.Open(path)
	if err != nil {
		return f, err
	}
	defer fh.Close()

	buf, err := io.ReadAll(fh)
	if err != nil {
		return f, err
	}

	placeholders := reEnvironmentPlaceholders.FindAllString(string(buf), -1)
	replacer := strings.NewReplacer(tmplStart, "", tmplEnd, "")
	var replacements []string
	for _, placeholder := range placeholders {
		environmentVariableName := strings.TrimSpace(
			strings.ToUpper(replacer.Replace(placeholder)),
		)
		environmentVariableValue := os.Getenv(environmentVariableName)

		// We extracted a placeholder and found the value in the env variables to replace it with
		if environmentVariableName != "" && environmentVariableValue != "" {
			replacements = append(replacements, placeholder)
			replacements = append(replacements, environmentVariableValue)
		}
	}
	if len(replacements)%2 == 1 {
		return f, errors.New("uneven amount of replacement arguments")
	}
	replacerSecrets := strings.NewReplacer(replacements...)
	processedConfig := replacerSecrets.Replace(string(buf))

	if err := yaml.Unmarshal([]byte(processedConfig), &f); err != nil {
		return f, err
	}
	return f, nil
}

// CloudSQLConfig is required for configuring the cloudsql connections.
//
//	If it is not set, no CloudSQL connection will be created
type CloudSQLConfig struct {
	// If KeyFile is set, then we load the IAM key from there
	KeyFile string `yaml:"key_file"`
}

// File is a collection of jobs
type File struct {
	Configuration  Configuration     `yaml:"configuration,omitempty"`
	Jobs           []*Job            `yaml:"jobs"`
	Queries        map[string]string `yaml:"queries"`
	CloudSQLConfig *CloudSQLConfig   `yaml:"cloudsql_config"`
}

type Configuration struct {
	HistogramBuckets []float64 `yaml:"histogram_buckets"`
}

type cronConfig struct {
	definition string
	schedule   cron.Schedule
}

func (c *cronConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	if err := unmarshal(&c.definition); err != nil {
		return fmt.Errorf("invalid cron_schedule, must be a string: %w", err)
	}
	var err error
	c.schedule, err = cron.ParseStandard(c.definition)
	if err != nil {
		return fmt.Errorf("invalid cron_schedule syntax for `%s`: %w", c.definition, err)
	}
	return nil
}

// Job is a collection of connections and queries
type Job struct {
	log          log.Logger
	conns        []*connection
	Name         string        `yaml:"name"`          // name of this job
	KeepAlive    bool          `yaml:"keepalive"`     // keep connection between runs?
	Interval     time.Duration `yaml:"interval"`      // interval at which this job is run
	CronSchedule cronConfig    `yaml:"cron_schedule"` // if specified, the interval is ignored and the job will be executed at the specified time in CRON syntax
	Connections  []string      `yaml:"connections"`
	Queries      []*Query      `yaml:"queries"`
	StartupSQL   []string      `yaml:"startup_sql"` // SQL executed on startup
	Iterator     Iterator      `yaml:"iterator"`    // Iterator configuration
}

type connection struct {
	conn                *sqlx.DB
	url                 string
	driver              string
	host                string
	database            string
	user                string
	tokenExpirationTime time.Time
	iteratorValues      []string
	snowflakeConfig     *gosnowflake.Config
	snowflakeDSN        string
	// Databricks OAuth configuration
	databricksOAuthType    string // "m2m" or "u2m"
	databricksClientID     string
	databricksClientSecret string
	databricksHost         string
	databricksHTTPPath     string
	databricksCatalog      string
}

// Query is an SQL query that is executed on a connection
type Query struct {
	sync.Mutex
	log           log.Logger
	desc          *prometheus.Desc
	metrics       map[*connection][]prometheus.Metric
	jobName       string
	AllowZeroRows bool     `yaml:"allow_zero_rows"`
	Name          string   `yaml:"name"`      // the prometheus metric name
	Help          string   `yaml:"help"`      // the prometheus metric help text
	Labels        []string `yaml:"labels"`    // expose these columns as labels per gauge
	Values        []string `yaml:"values"`    // expose each of these as a gauge
	Timestamp     string   `yaml:"timestamp"` // expose as metric timestamp
	Query         string   `yaml:"query"`     // a literal query
	QueryRef      string   `yaml:"query_ref"` // references a query in the query map
}

// Iterator is a mechanism to repeat queries from a job based on the results of another query
type Iterator struct {
	SQL         string `yaml:"sql"`         // SQL to execute to retrieve iterator values
	Placeholder string `yaml:"placeholder"` // Placeholder in query to be replaced
	Label       string `yaml:"label"`       // Label to assign iterator values to
}
