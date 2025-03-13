package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

// Run executes a single Query on a single connection
func (q *Query) Run(conn *connection) error {
	if q.log == nil {
		q.log = log.NewNopLogger()
	}
	queryCounter.WithLabelValues(q.jobName, q.Name).Inc()
	if q.desc == nil {
		failedQueryCounter.WithLabelValues(q.jobName, q.Name).Inc()
		return fmt.Errorf("metrics descriptor is nil")
	}
	if q.Query == "" {
		failedQueryCounter.WithLabelValues(q.jobName, q.Name).Inc()
		return fmt.Errorf("query is empty")
	}
	if conn == nil || conn.conn == nil {
		failedQueryCounter.WithLabelValues(q.jobName, q.Name).Inc()
		return fmt.Errorf("db connection not initialized (should not happen)")
	}
	// execute query
	now := time.Now()
	rows, err := conn.conn.Queryx(q.Query)
	if err != nil {
		failedScrapes.WithLabelValues(conn.driver, conn.host, conn.database, conn.user, q.jobName, q.Name).Set(1.0)
		failedQueryCounter.WithLabelValues(q.jobName, q.Name).Inc()
		return err
	}
	defer rows.Close()
	duration := time.Since(now)
	queryDurationHistogram.WithLabelValues(q.jobName, q.Name).Observe(duration.Seconds())

	updated := 0
	metrics := make([]prometheus.Metric, 0, len(q.metrics))
	for rows.Next() {
		res := make(map[string]interface{})
		err := rows.MapScan(res)
		if err != nil {
			level.Error(q.log).Log("msg", "Failed to scan", "err", err, "host", conn.host, "db", conn.database)
			failedScrapes.WithLabelValues(conn.driver, conn.host, conn.database, conn.user, q.jobName, q.Name).Set(1.0)
			continue
		}
		m, err := q.updateMetrics(conn, res, "", "")
		if err != nil {
			level.Error(q.log).Log("msg", "Failed to update metrics", "err", err, "host", conn.host, "db", conn.database)
			failedScrapes.WithLabelValues(conn.driver, conn.host, conn.database, conn.user, q.jobName, q.Name).Set(1.0)
			continue
		}
		metrics = append(metrics, m...)
		updated++
		failedScrapes.WithLabelValues(conn.driver, conn.host, conn.database, conn.user, q.jobName, q.Name).Set(0.0)
	}

	if updated < 1 {
		if q.AllowZeroRows {
			failedScrapes.WithLabelValues(conn.driver, conn.host, conn.database, conn.user, q.jobName, q.Name).Set(0.0)
		} else {
			failedScrapes.WithLabelValues(conn.driver, conn.host, conn.database, conn.user, q.jobName, q.Name).Set(1.0)
			failedQueryCounter.WithLabelValues(q.jobName, q.Name).Inc()
			return fmt.Errorf("zero rows returned")
		}
	}

	// update the metrics cache
	q.Lock()
	q.metrics[conn] = metrics
	q.Unlock()

	return nil
}

// RunIterator runs the query for each iterator value on a single connection
func (q *Query) RunIterator(conn *connection, ph string, ivs []string, il string) error {
	if q.log == nil {
		q.log = log.NewNopLogger()
	}
	queryCounter.WithLabelValues(q.jobName, q.Name).Inc()
	if q.desc == nil {
		failedQueryCounter.WithLabelValues(q.jobName, q.Name).Inc()
		return fmt.Errorf("metrics descriptor is nil")
	}
	if q.Query == "" {
		failedQueryCounter.WithLabelValues(q.jobName, q.Name).Inc()
		return fmt.Errorf("query is empty")
	}
	if conn == nil || conn.conn == nil {
		failedQueryCounter.WithLabelValues(q.jobName, q.Name).Inc()
		return fmt.Errorf("db connection not initialized (should not happen)")
	}

	// execute query for each iterator value
	now := time.Now()
	metrics := make([]prometheus.Metric, 0, len(q.metrics))
	updated := 0
	for _, iv := range ivs {
		rows, err := conn.conn.Queryx(q.ReplaceIterator(ph, iv))
		if err != nil {
			failedScrapes.WithLabelValues(conn.driver, conn.host, conn.database, conn.user, q.jobName, q.Name).Set(1.0)
			failedQueryCounter.WithLabelValues(q.jobName, q.Name).Inc()
			return err
		}
		defer rows.Close()

		for rows.Next() {
			res := make(map[string]interface{})
			err := rows.MapScan(res)
			if err != nil {
				level.Error(q.log).Log("msg", "Failed to scan", "err", err, "host", conn.host, "db", conn.database)
				failedScrapes.WithLabelValues(conn.driver, conn.host, conn.database, conn.user, q.jobName, q.Name).Set(1.0)
				continue
			}
			m, err := q.updateMetrics(conn, res, iv, il)
			if err != nil {
				level.Error(q.log).Log("msg", "Failed to update metrics", "err", err, "host", conn.host, "db", conn.database)
				failedScrapes.WithLabelValues(conn.driver, conn.host, conn.database, conn.user, q.jobName, q.Name).Set(1.0)
				continue
			}
			metrics = append(metrics, m...)
			updated++
			failedScrapes.WithLabelValues(conn.driver, conn.host, conn.database, conn.user, q.jobName, q.Name).Set(0.0)
		}
	}

	duration := time.Since(now)
	queryDurationHistogram.WithLabelValues(q.jobName, q.Name).Observe(duration.Seconds())

	if updated < 1 {
		if q.AllowZeroRows {
			failedScrapes.WithLabelValues(conn.driver, conn.host, conn.database, conn.user, q.jobName, q.Name).Set(0.0)
		} else {
			return fmt.Errorf("zero rows returned")
		}
	}

	// update the metrics cache
	q.Lock()
	q.metrics[conn] = metrics
	q.Unlock()

	return nil
}

// HasIterator returns true if the query contains the given placeholder
func (q *Query) HasIterator(ph string) bool {
	return strings.Contains(q.Query, ph)
}

// ReplaceIterator replaces a given placeholder with an iterator value and returns a new query
func (q *Query) ReplaceIterator(ph string, iv string) string {
	iteratorReplacer := strings.NewReplacer(fmt.Sprint("{{", ph, "}}"), iv)
	return iteratorReplacer.Replace(q.Query)
}

// updateMetrics parses the result set and returns a slice of const metrics
func (q *Query) updateMetrics(conn *connection, res map[string]interface{}, iv string, il string) ([]prometheus.Metric, error) {
	// if no value were defined to be parsed, return immediately
	if len(q.Values) == 0 {
		level.Debug(q.log).Log("msg", "No values defined in configuration, skipping metric update")
		return nil, nil
	}
	updated := 0
	metrics := make([]prometheus.Metric, 0, len(q.Values))
	for _, valueName := range q.Values {
		m, err := q.updateMetric(conn, res, valueName, iv, il)
		if err != nil {
			level.Error(q.log).Log(
				"msg", "Failed to update metric",
				"value", valueName,
				"err", err,
				"host", conn.host,
				"db", conn.database,
			)
			continue
		}
		metrics = append(metrics, m)
		updated++
	}
	if updated < 1 {
		return nil, fmt.Errorf("zero values found")
	}
	return metrics, nil
}

// updateMetrics parses a single row and returns a const metric
func (q *Query) updateMetric(conn *connection, res map[string]interface{}, valueName string, iv string, il string) (prometheus.Metric, error) {
	var value float64
	if i, ok := res[valueName]; ok {
		switch f := i.(type) {
		case int:
			value = float64(f)
		case int32:
			value = float64(f)
		case int64:
			value = float64(f)
		case uint:
			value = float64(f)
		case uint32:
			value = float64(f)
		case uint64:
			value = float64(f)
		case float32:
			value = float64(f)
		case float64:
			value = float64(f)
		case []uint8:
			val, err := strconv.ParseFloat(string(f), 64)
			if err != nil {
				return nil, fmt.Errorf("column '%s' must be type float, is '%T' (val: %s)", valueName, i, f)
			}
			value = val
		case string:
			val, err := strconv.ParseFloat(f, 64)
			if err != nil {
				return nil, fmt.Errorf("column '%s' must be type float, is '%T' (val: %s)", valueName, i, f)
			}
			value = val
		default:
			return nil, fmt.Errorf("column '%s' must be type float, is '%T' (val: %s)", valueName, i, f)
		}
	} else {
		level.Warn(q.log).Log(
			"msg", "Column not found in query result",
			"column", valueName,
			"resultColumns", res,
		)
	}
	// make space for all defined variable label columns and the "static" labels
	// added below
	labels := make([]string, 0, len(q.Labels)+5)
	for _, label := range q.Labels {
		// append iterator value to the labels
		if label == il && iv != "" {
			labels = append(labels, iv)
			continue
		}

		// we need to fill every spot in the slice or the key->value mapping
		// won't match up in the end.
		//
		// ORDER MATTERS!
		lv := ""
		if i, ok := res[label]; ok {
			switch str := i.(type) {
			case string:
				lv = str
			case []uint8:
				lv = string(str)
			default:
				return nil, fmt.Errorf("column '%s' must be type text (string)", label)
			}
		}
		labels = append(labels, lv)
	}
	labels = append(labels, conn.driver)
	labels = append(labels, conn.host)
	labels = append(labels, conn.database)
	labels = append(labels, conn.user)
	labels = append(labels, valueName)
	// create a new immutable const metric that can be cached and returned on
	// every scrape. Remember that the order of the label values in the labels
	// slice must match the order of the label names in the descriptor!
	metric, err := prometheus.NewConstMetric(
		q.desc, prometheus.GaugeValue, value, labels...,
	)
	if err != nil {
		return nil, err
	}
	if q.Timestamp != "" {
		if tsRaw, ok := res[q.Timestamp]; ok {
			switch ts := tsRaw.(type) {
			case time.Time:
				return prometheus.NewMetricWithTimestamp(ts, metric), nil
			default:
				level.Warn(q.log).Log(
					"msg", "timestamp label %q is of type %T, expected time.Time",
					"column", tsRaw,
				)
			}
		}
	}
	return metric, nil
}
