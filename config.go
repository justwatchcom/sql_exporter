package main

import (
	"errors"
	"io/ioutil"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/jmoiron/sqlx"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v2"
)

var (
	failedScrapes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sql_exporter_last_scrape_failed",
			Help: "Failed scrapes",
		},
		[]string{"driver", "host", "database", "user", "sql_job", "query"},
	)
	reEnvironmentPlaceholders = regexp.MustCompile(`{{.+?}}`)
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

	buf, err := ioutil.ReadAll(fh)
	if err != nil {
		return f, err
	}

	placeholders := reEnvironmentPlaceholders.FindAllString(string(buf), -1)
	replacer := strings.NewReplacer("{{", "", "}}", "")
	var replacements []string
	for _, placeholder := range placeholders {
		environmentVariableName := strings.ToUpper(replacer.Replace(placeholder))
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

// File is a collection of jobs
type File struct {
	Jobs    []*Job            `yaml:"jobs"`
	Queries map[string]string `yaml:"queries"`
}

// Job is a collection of connections and queries
type Job struct {
	log         log.Logger
	conns       []*connection
	Name        string        `yaml:"name"`      // name of this job
	KeepAlive   bool          `yaml:"keepalive"` // keep connection between runs?
	Interval    time.Duration `yaml:"interval"`  // interval at which this job is run
	Connections []string      `yaml:"connections"`
	Queries     []*Query      `yaml:"queries"`
	StartupSQL  []string      `yaml:"startup_sql"` // SQL executed on startup
}

type connection struct {
	conn     *sqlx.DB
	url      string
	driver   string
	host     string
	database string
	user     string
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
	Values        []string `yaml:"values"`    // expose each of these as an gauge
	Query         string   `yaml:"query"`     // a literal query
	QueryRef      string   `yaml:"query_ref"` // references an query in the query map
}
