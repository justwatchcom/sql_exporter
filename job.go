package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/go-kit/kit/log"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq" // register the SQL driver
	"github.com/prometheus/client_golang/prometheus"
)

var (
	// MetricNameRE matches any invalid metric name
	// characters, see github.com/prometheus/common/model.MetricNameRE
	MetricNameRE = regexp.MustCompile("[^a-zA-Z0-9_:]+")
)

// Run prepares and runs the job
func (j *Job) Run() {
	if j.log == nil {
		j.log = log.NewNopLogger()
	}
	// if there are no connection URLs for this job it can't be run
	if j.Connections == nil {
		return
	}
	// make space for the connection objects
	if j.conns == nil {
		j.conns = make([]*connection, 0, len(j.Connections))
	}
	// parse the connection URLs and create an connection object for each
	if len(j.conns) < len(j.Connections) {
		for _, conn := range j.Connections {
			u, err := url.Parse(conn)
			if err != nil {
				j.log.Log("level", "error", "msg", "Failed to parse URL", "url", conn, "err", err)
				continue
			}
			user := ""
			if u.User != nil {
				user = u.User.Username()
			}
			// we expose some of the connection variables as labels, so we need to
			// remember them
			j.conns = append(j.conns, &connection{
				conn:     nil,
				url:      u,
				driver:   u.Scheme,
				host:     u.Host,
				database: strings.TrimPrefix(u.Path, "/"),
				user:     user,
			})
		}
	}
	j.log.Log("level", "debug", "msg", "Starting")

	// register each query as an metric
	for _, q := range j.Queries {
		if q == nil {
			j.log.Log("level", "warning", "msg", "Skipping invalid query")
			continue
		}
		// try to satisfy prometheus naming restrictions
		name := MetricNameRE.ReplaceAllString("sql_"+q.Name, "")
		help := q.Help
		q.log = log.NewContext(j.log).With("query", q.Name)
		p := prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: name,
				Help: help,
				ConstLabels: prometheus.Labels{
					"sql_job": j.Name,
				},
			},
			append(q.Labels, "driver", "host", "database", "user", "col"),
		)
		// this may fail due to a number of restrictions the default registry places
		// on the metrics registered
		if err := prometheus.Register(p); err != nil {
			j.log.Log("level", "error", "msg", "Failed to register collector", "err", err)
			continue
		}
		q.prom = p
	}

	// enter the run loop
	// tries to run each query on each connection at approx the interval
	for {
		bo := backoff.NewExponentialBackOff()
		bo.MaxElapsedTime = j.Interval
		if err := backoff.Retry(j.runOnce, bo); err != nil {
			j.log.Log("level", "error", "msg", "Failed to run", "err", err)
		}
		j.log.Log("level", "debug", "msg", "Sleeping until next run", "sleep", j.Interval.String())
		time.Sleep(j.Interval)
	}
}

func (j *Job) runOnce() error {
	updated := 0
	// execute queries for each connection in order
	for _, conn := range j.conns {
		// connect to DB if not connected already
		if err := conn.connect(j.Interval); err != nil {
			j.log.Log("level", "warn", "msg", "Failed to connect", "err", err)
			continue
		}
		for _, q := range j.Queries {
			if q == nil {
				continue
			}
			if q.prom == nil {
				// this may happen if the metric registration failed
				q.log.Log("level", "warning", "msg", "Skipping query. Collector is nil")
				continue
			}
			q.log.Log("level", "debug", "msg", "Running Query")
			// execute the query on the connection
			if err := q.Run(conn); err != nil {
				q.log.Log("level", "warning", "msg", "Failed to run query", "err", err)
				continue
			}
			q.log.Log("level", "debug", "msg", "Query finished")
			updated++
		}
	}
	if updated < 1 {
		return fmt.Errorf("zero queries ran")
	}
	return nil
}

func (c *connection) connect(iv time.Duration) error {
	// already connected
	if c.conn != nil {
		return nil
	}
	conn, err := sqlx.Connect(c.url.Scheme, c.url.String())
	if err != nil {
		return err
	}
	// be nice and don't use up too many connections for mere metrics
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	conn.SetConnMaxLifetime(iv * 2)
	c.conn = conn
	return nil
}
