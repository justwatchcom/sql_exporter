package main

import (
	"io/ioutil"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/jmoiron/sqlx"
	"github.com/prometheus/client_golang/prometheus"

	yaml "gopkg.in/yaml.v2"
	//"github.com/ghodss/yaml"
)

const TimeFormat = "15:04:05"

// Read attempts to parse the given config and return a file
// object
func Read(path string) (File, error) {
	f := File{}

	fh, err := os.Open(path)
	if err != nil {
		return f, err
	}
	defer fh.Close()

	buf, err := ioutil.ReadAll(fh)
	if err != nil {
		return f, err
	}

	if err := yaml.Unmarshal(buf, &f); err != nil {
		return f, err
	}
	return f, nil
}

// File is a collection of jobs
type File struct {
	Jobs    []*Job            `yaml:"jobs"`
	Queries map[string]string `yaml:"queries"`
}

// Job is a collection of connections and queries
type Job struct {
	log               log.Logger
	conns             []*connection
	Name              string        `yaml:"name"`      // name of this job
	KeepAlive         bool          `yaml:"keepalive"` // keep connection between runs?
	Interval          time.Duration `yaml:"interval"`  // interval at which this job is run
	ExecutionTimeFrom TimeOfDay     `yaml:"execution_time_from"`
	ExecutionTimeTo   TimeOfDay     `yaml:"execution_time_to"`
	Connections       []string      `yaml:"connections"`
	Queries           []*Query      `yaml:"queries"`
	StartupSQL        []string      `yaml:"startup_sql"` // SQL executed on startup
}

type connection struct {
	conn     *sqlx.DB
	url      *url.URL
	driver   string
	host     string
	database string
	user     string
}

// Query is an SQL query that is executed on a connection
type Query struct {
	sync.Mutex
	log      log.Logger
	desc     *prometheus.Desc
	metrics  map[*connection][]prometheus.Metric
	Name     string   `yaml:"name"`      // the prometheus metric name
	Help     string   `yaml:"help"`      // the prometheus metric help text
	Labels   []string `yaml:"labels"`    // expose these columns as labels per gauge
	Values   []string `yaml:"values"`    // expose each of these as an gauge
	Query    string   `yaml:"query"`     // a literal query
	QueryRef string   `yaml:"query_ref"` // references an query in the query map
}

// TimeOfDay wrapper for time.Time. It implements the yaml.Unmarshaler interface.
type TimeOfDay struct {
	time.Time
}

// UnmarshalYAML allows unmarshalling of time.Time from YAML with custom time format.
func (t *TimeOfDay) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var (
		s   string
		err error
	)
	err = unmarshal(&s)
	if err != nil {
		return err
	}
	t.Time, err = time.ParseInLocation(TimeFormat, s, time.UTC)
	if err != nil {
		return err
	}
	// zero time in time package is year 1 but parse time without year gives year 0
	// so we need to add 1 year to be consistent
	t.Time = t.Time.AddDate(1, 0, 0)
	return nil
}

// GetTime returns wrapped time.Time
func (t *TimeOfDay) GetTime() time.Time {
	return t.Time
}
