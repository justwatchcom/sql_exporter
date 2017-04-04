package main

import (
	"net/http"
	"os"

	"github.com/go-kit/kit/log"
	"github.com/justwatchcom/sql_exporter/leveled"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Build time vars
var (
	Name      = "prom-sql-exporter"
	Version   string
	BuildTime string
	Commit    string
)

func main() {
	// init logger
	logger := log.NewJSONLogger(os.Stdout)
	logger = log.With(
		logger,
		"ts", log.DefaultTimestampUTC,
		"name", Name,
	)
	logger = leveled.NewFromEnv(logger)
	logger = log.With(logger, "caller", log.DefaultCaller)

	cfgFile := "config.yml"
	if f := os.Getenv("CONFIG"); f != "" {
		cfgFile = f
	}

	// read config
	cfg, err := Read(cfgFile)
	if err != nil {
		panic(err)
	}

	// dispatch all jobs
	for _, job := range cfg.Jobs {
		if job == nil {
			continue
		}
		job.log = log.With(logger, "job", job.Name)
		go job.Run()
	}

	// setup and start webserver
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "OK", http.StatusOK) })
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
		<head><title>SQL Exporter</title></head>
		<body>
		<h1>SQL Exporter</h1>
		<p><a href="/metrics">Metrics</a></p>
		</body>
		</html>
		`))
	})

	addr := ":9237"
	logger.Log("level", "info", "msg", "Starting sql_exporter", "addr", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		logger.Log("level", "error", "msg", "Error starting HTTP server:", "err", err)
		os.Exit(1)
	}
}
