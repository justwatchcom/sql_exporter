package main

import "slices"

func GetLabelsForFailedScrapes() []string {
	standardLabels := [6]string{"driver", "host", "database", "user", "sql_job", "query"}
	var labels []string
	for _, l := range standardLabels {
		if !slices.Contains(redactedLabels, l) {
			labels = append(labels, l)
		}
	}

	return labels
}

func GetLabelsForSQLGauges() []string {
	standardLabels := [5]string{"driver", "host", "database", "user", "col"}
	var labels []string
	for _, l := range standardLabels {
		if !slices.Contains(redactedLabels, l) {
			labels = append(labels, l)
		}
	}

	return labels
}

func AppendLabelValuesForSQLGauges(labels []string, conn *connection, valueName string) []string {
	labels = append(labels, conn.driver)

	if !slices.Contains(redactedLabels, "host") {
		labels = append(labels, conn.host)
	}

	if !slices.Contains(redactedLabels, "database") {
		labels = append(labels, conn.database)
	}

	if !slices.Contains(redactedLabels, "user") {
		labels = append(labels, conn.user)
	}

	labels = append(labels, valueName)

	return labels
}

func FilteredLabelValuesForFailedScrapes(conn *connection, q *Query) []string {
	var labels []string

	labels = append(labels, conn.driver)

	if !slices.Contains(redactedLabels, "host") {
		labels = append(labels, conn.host)
	}

	if !slices.Contains(redactedLabels, "database") {
		labels = append(labels, conn.database)
	}

	if !slices.Contains(redactedLabels, "user") {
		labels = append(labels, conn.user)
	}

	labels = append(labels, q.jobName)
	labels = append(labels, q.Name)

	return labels
}
