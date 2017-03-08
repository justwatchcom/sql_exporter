package main

import (
	"fmt"
	"strconv"

	"github.com/go-kit/kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

// Run executes a single Query
func (q *Query) Run(conn *connection) error {
	if q.log == nil {
		q.log = log.NewNopLogger()
	}
	if q.prom == nil {
		return fmt.Errorf("Collector is nil. Refusing to continue")
	}
	if q.Query == "" {
		return fmt.Errorf("Query is empty")
	}
	if conn == nil || conn.conn == nil {
		return fmt.Errorf("DB connection not initialized (should not happen)")
	}
	// execute query
	rows, err := conn.conn.Queryx(q.Query)
	if err != nil {
		return err
	}
	defer rows.Close()

	updated := 0
	for rows.Next() {
		res := make(map[string]interface{})
		err := rows.MapScan(res)
		if err != nil {
			q.log.Log("level", "error", "msg", "Failed to scan", "err", err, "host", conn.host, "db", conn.database)
			continue
		}
		if err := q.updateMetrics(conn, res); err != nil {
			q.log.Log("level", "error", "msg", "Failed to update metrics", "err", err, "host", conn.host, "db", conn.database)
			continue
		}
		updated++
	}

	if updated < 1 {
		return fmt.Errorf("zero rows returned")
	}
	return nil
}

func (q *Query) updateMetrics(conn *connection, res map[string]interface{}) error {
	updated := 0
	for _, valueName := range q.Values {
		if err := q.updateMetric(conn, res, valueName); err != nil {
			q.log.Log("level", "error", "msg", "Failed to update metric", "value", valueName, "err", err, "host", conn.host, "db", conn.database)
			continue
		}
		updated++
	}
	if updated < 1 {
		return fmt.Errorf("zero values found")
	}
	return nil
}

func (q *Query) updateMetric(conn *connection, res map[string]interface{}, valueName string) error {
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
				return fmt.Errorf("Column '%s' must be type float, is '%T' (val: %s)", valueName, i, f)
			}
			value = val
		case string:
			val, err := strconv.ParseFloat(f, 64)
			if err != nil {
				return fmt.Errorf("Column '%s' must be type float, is '%T' (val: %s)", valueName, i, f)
			}
			value = val
		default:
			return fmt.Errorf("Column '%s' must be type float, is '%T' (val: %s)", valueName, i, f)
		}
	}
	labels := prometheus.Labels{
		"driver":   conn.driver,
		"host":     conn.host,
		"database": conn.database,
		"user":     conn.user,
		"col":      valueName,
	}
	for _, label := range q.Labels {
		labels[label] = ""
		if i, ok := res[label]; ok {
			switch str := i.(type) {
			case string:
				labels[label] = str
			case []uint8:
				labels[label] = string(str)
			default:
				return fmt.Errorf("Column '%s' must be type text (string)", label)
			}
		}
	}
	q.prom.With(labels).Set(value)
	return nil
}
