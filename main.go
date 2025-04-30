package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	prom_collectors_version "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"
	_ "go.uber.org/automaxprocs"
)

func init() {
	prometheus.MustRegister(prom_collectors_version.NewCollector("sql_exporter"))
}

func main() {
	var (
		showVersion                 = flag.Bool("version", false, "Print version information.")
		listenAddress               = flag.String("web.listen-address", ":9237", "Address to listen on for web interface and telemetry.")
		metricsPath                 = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
		configFile                  = flag.String("config.file", os.Getenv("CONFIG"), "SQL Exporter configuration file name.")
		dbConnectivityAsHealthCheck = flag.Bool("db.connectivity-as-healthz", false, "Use database connectivity check as healthz probe")
	)

	flag.Parse()

	if *showVersion {
		fmt.Fprintln(os.Stdout, version.Print("sql_exporter"))
		os.Exit(0)
	}

	// init logger
	logger := log.NewJSONLogger(os.Stdout)
	// set the allowed log level filter
	switch strings.ToLower(os.Getenv("LOGLEVEL")) {
	case "debug":
		logger = level.NewFilter(logger, level.AllowDebug())
	case "info":
		logger = level.NewFilter(logger, level.AllowInfo())
	case "warn":
		logger = level.NewFilter(logger, level.AllowWarn())
	case "error":
		logger = level.NewFilter(logger, level.AllowError())
	default:
		logger = level.NewFilter(logger, level.AllowAll())
	}
	logger = log.With(logger,
		"ts", log.DefaultTimestampUTC,
		"caller", log.DefaultCaller,
	)

	logger.Log("msg", "Starting sql_exporter", "version_info", version.Info(), "build_context", version.BuildContext())

	exporter, err := NewExporter(logger, *configFile)
	if err != nil {
		level.Error(logger).Log("msg", "Error starting exporter", "err", err)
		os.Exit(1)
	}
	prometheus.MustRegister(exporter)

	// setup and start webserver
	http.Handle(*metricsPath, promhttp.Handler())

	if *dbConnectivityAsHealthCheck {
		http.HandleFunc("/healthz",
			func(w http.ResponseWriter, r *http.Request) {
				for _, job := range exporter.jobs {

					if job == nil {
						continue
					}

					for _, connection := range job.conns {

						if connection == nil {
							continue
						}

						if connection.conn != nil {
							if err := connection.conn.Ping(); err != nil {
								// if any of the connections fails to be established/verified, fail the /healthz request
								http.Error(w, err.Error(), http.StatusInternalServerError)
								return
							}
							// otherwise we've successfully verified the connection, continue to the next one
							continue
						}

						if err := connection.connect(job); err != nil {
							// if any of the connections fails to be established, fail the /healthz request
							http.Error(w, err.Error(), http.StatusInternalServerError)
							return
						}

					}
				}

				// otherwise return OK
				http.Error(w, "OK", http.StatusOK)

			})
	} else {
		http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "OK", http.StatusOK) })
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
		<head><title>SQL Exporter</title></head>
		<body>
		<h1>SQL Exporter</h1>
		<p><a href="` + *metricsPath + `">Metrics</a></p>
		</body>
		</html>
		`))
	})

	level.Info(logger).Log("msg", "Listening", "listenAddress", *listenAddress)
	if err := http.ListenAndServe(*listenAddress, nil); err != nil {
		level.Error(logger).Log("msg", "Error starting HTTP server:", "err", err)
		os.Exit(1)
	}
}
