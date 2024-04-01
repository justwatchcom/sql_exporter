package main

import (
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/lib/pq"
)

const (
	INCLUDE_DBS = "/include:"
	EXCLUDE_DBS = "/exclude:"
)

func listDatabases(connStr string) ([]string, error) {

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query("SELECT datname FROM pg_database WHERE datistemplate = false;")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var databases []string
	for rows.Next() {
		var dbname string
		if err := rows.Scan(&dbname); err != nil {
			return nil, err
		}
		databases = append(databases, dbname)
	}

	return databases, nil
}

func filterDatabases(databases []string, pattern string) ([]string, error) {
	var filtered []string
	mode, dbs := parsePattern(pattern)

	dbMap := make(map[string]bool)
	for _, db := range strings.Split(dbs, ",") {
		dbMap[db] = true
	}

	if mode == INCLUDE_DBS {
		for _, dbname := range databases {
			if _, ok := dbMap[dbname]; ok {
				filtered = append(filtered, dbname)
			}
		}
	} else if mode == EXCLUDE_DBS {
		for _, dbname := range databases {
			if _, ok := dbMap[dbname]; !ok {
				filtered = append(filtered, dbname)
			}
		}
	} else {
		// If mode is neither include nor exclude, return an error
		return nil, fmt.Errorf("invalid pattern: %s", pattern)
	}

	return filtered, nil
}

func parsePattern(pattern string) (mode string, dbs string) {
	if strings.HasPrefix(pattern, INCLUDE_DBS) {
		return INCLUDE_DBS, pattern[len(INCLUDE_DBS):]
	} else if strings.HasPrefix(pattern, EXCLUDE_DBS) {
		return EXCLUDE_DBS, pattern[len(EXCLUDE_DBS):]
	}
	return "", ""
}
