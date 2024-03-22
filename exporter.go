package main

import (
	"context"
	"fmt"

	"cloud.google.com/go/cloudsqlconn"
	"cloud.google.com/go/cloudsqlconn/mysql/mysql"
	"cloud.google.com/go/cloudsqlconn/postgres/pgxv4"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/robfig/cron/v3"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"
)

// Exporter collects SQL metrics. It implements prometheus.Collector.
type Exporter struct {
	jobs            []*Job
	logger          log.Logger
	cronScheduler   *cron.Cron
	sqladminService *sqladmin.Service
}

// NewExporter returns a new SQL Exporter for the provided config.
func NewExporter(logger log.Logger, configFile string) (*Exporter, error) {
	if configFile == "" {
		configFile = "config.yml"
	}

	// read config
	cfg, err := Read(configFile)
	if err != nil {
		return nil, err
	}

	var queryDurationHistogramBuckets []float64
	if len(cfg.Configuration.HistogramBuckets) == 0 {
		queryDurationHistogramBuckets = DefaultQueryDurationHistogramBuckets
	} else {
		queryDurationHistogramBuckets = cfg.Configuration.HistogramBuckets
	}
	queryDurationHistogram = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    fmt.Sprintf("%s_query_duration_seconds", metricsPrefix),
		Help:    "Time spent by querying the database.",
		Buckets: queryDurationHistogramBuckets,
	}, QueryMetricsLabels)

	exp := &Exporter{
		jobs:          make([]*Job, 0, len(cfg.Jobs)),
		logger:        logger,
		cronScheduler: cron.New(),
	}

	if cfg.CloudSQLConfig != nil {
		if cfg.CloudSQLConfig.KeyFile == "" {
			return nil, fmt.Errorf("as cloudsql_config is not empty, then cloudsql_config.key_file must be set")
		}

		// We currently only support keyfile. Additional authentication options would be via automatic IAM
		//	 with cloudsqlconn.WithIAMAuthN()
		cloudsqlconnection := cloudsqlconn.WithCredentialsFile(cfg.CloudSQLConfig.KeyFile)
		sqladminService, err := sqladmin.NewService(context.Background(), option.WithAPIKey(cfg.CloudSQLConfig.KeyFile))
		if err != nil {
			return nil, fmt.Errorf("could not create new cloud sqladmin service: %w", err)
		}
		exp.sqladminService = sqladminService

		//
		// Register all possible cloudsql drivers

		// drop cleanup as we don't really know when to end this
		_, err = pgxv4.RegisterDriver(CLOUDSQL_POSTGRES, cloudsqlconnection)
		if err != nil {
			return nil, fmt.Errorf("could not register cloudsql-postgres driver: %w", err)
		}

		// drop cleanup as we don't really know when to end this
		_, err = mysql.RegisterDriver(CLOUDSQL_MYSQL, cloudsqlconnection)
		if err != nil {
			return nil, fmt.Errorf("could not register cloudsql-mysql driver: %w", err)
		}
	}

	// dispatch all jobs
	for _, job := range cfg.Jobs {
		if job == nil {
			continue
		}

		if err := job.Init(logger, cfg.Queries); err != nil {
			level.Warn(logger).Log("msg", "Skipping job. Failed to initialize", "err", err, "job", job.Name)
			continue
		}
		exp.jobs = append(exp.jobs, job)
		if job.CronSchedule.schedule != nil {
			exp.cronScheduler.Schedule(job.CronSchedule.schedule, job)
			level.Info(logger).Log("msg", "Scheduled CRON job", "name", job.Name, "cron_schedule", job.CronSchedule.definition)
		} else {
			go job.ExecutePeriodically()
			level.Info(logger).Log("msg", "Started periodically execution of job", "name", job.Name, "interval", job.Interval)
		}
	}
	exp.cronScheduler.Start()
	return exp, nil
}

// Describe implements prometheus.Collector
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, job := range e.jobs {
		if job == nil {
			continue
		}
		for _, query := range job.Queries {
			if query == nil {
				continue
			}
			if query.desc == nil {
				level.Error(e.logger).Log("msg", "Query has no descriptor", "query", query.Name)
				continue
			}
			ch <- query.desc
		}
	}
}

// Collect implements prometheus.Collector
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	for _, job := range e.jobs {
		if job == nil {
			continue
		}
		for _, query := range job.Queries {
			if query == nil {
				continue
			}
			for _, metrics := range query.metrics {
				for _, metric := range metrics {
					ch <- metric
				}
			}
		}
	}
}
