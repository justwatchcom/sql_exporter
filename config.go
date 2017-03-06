package main

import (
	"io/ioutil"
	"net/url"
	"os"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/jmoiron/sqlx"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v2"
)

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
	Jobs []*Job `yaml:"jobs"`
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
	log    log.Logger
	prom   *prometheus.GaugeVec
	Name   string   `yaml:"name"`   // the prometheus metric name
	Help   string   `yaml:"help"`   // the prometheus metric help text
	Labels []string `yaml:"labels"` // expose these columns as labels per gauge
	Values []string `yaml:"values"` // expose each of these as an gauge
	Query  string   `yaml:"query"`
}
