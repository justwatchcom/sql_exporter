package main

import (
	"database/sql"
	"fmt"
	"regexp"
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

	// Split the dbs string into individual patterns
	dbPatterns := strings.Split(dbs, ",")

	// Process each database name against the patterns
	for _, dbname := range databases {
		include := false

		for _, dbPattern := range dbPatterns {
			matched, err := regexp.MatchString(dbPattern, dbname)
			if err != nil {
				return nil, fmt.Errorf("invalid pattern: %s", dbPattern)
			}
			if matched {
				include = true
				break
			}
		}

		if (mode == INCLUDE_DBS && include) || (mode == EXCLUDE_DBS && !include) {
			filtered = append(filtered, dbname)
		}
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
