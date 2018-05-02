package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
)

// handlerFunc can be used as handler for http.HandleFunc()
// all synchronous jobs will be triggered and waited for,
// then the promhttp handler is executed
func (ex *Exporter) handlerFunc(w http.ResponseWriter, req *http.Request) {
	// pull all triggers on jobs with interval 0
	for _, job := range ex.jobs {
		// if job is nil or is async then continue to next job
		if job == nil || job.Interval > 0 {
			continue
		}
		job.Trigger <- true
	}

	// wait for all sync jobs to finish
	for _, job := range ex.jobs {
		if job == nil || job.Interval > 0 {
			continue
		}
		<-job.Done
	}

	// get the prometheus handler
	handler := promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{})

	// execute the ServeHTTP function
	handler.ServeHTTP(w, req)
}
