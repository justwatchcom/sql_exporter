# Prometheus SQL Exporter [![Build Status](https://travis-ci.org/justwatchcom/sql_exporter.svg?branch=master)](https://travis-ci.org/justwatchcom/sql_exporter)

[![Docker Pulls](https://img.shields.io/docker/pulls/justwatch/sql_exporter.svg?maxAge=604800)](https://hub.docker.com/r/justwatch/sql_exporter)
[![Go Report Card](https://goreportcard.com/badge/github.com/justwatchcom/sql_exporter)](https://goreportcard.com/report/github.com/justwatchcom/sql_exporter)

This repository contains a service that runs user-defined SQL queries at flexible intervals and exports the resulting metrics via HTTP for Prometheus consumption.

Status
======

Actively used with PostgreSQL in production. We'd like to eventually support all databases for which stable Go database [drivers](https://github.com/golang/go/wiki/SQLDrivers) are available. Contributions welcome.

Currently supported:

- Postgres
- ClickHouse
- AWS Athena
- Databricks
- MS-SQL
- MySQL
- Snowflake
- Vertica
- Materialize
- CloudSQL


What does it look like?
=======================

![Grafana DB Dashboard](/examples/grafana/screenshot.jpg?raw=true)

Getting Started
===============

Create a _config.yml_ and run the service:

```
go get github.com/justwatchcom/sql_exporter
cp config.yml.dist config.yml
./prom-sql-exporter
```

Running in Docker:

```bash
docker run \
  -v `pwd`/config.yml:/config/config.yml \
  -e CONFIG=/config/config.yml \
  -d \
  -p 9237:9237 \
  --name sql_exporter \
  ghcr.io/justwatchcom/sql_exporter
```

Manual `scrape_configs` snippet:

```yaml
scrape_configs:
- job_name: sql_exporter
  static_configs:
  - targets: ['localhost:9237']
```

Flags
-----

Name    | Description
--------|------------
`version` | Print version information
`web.listen-address` | Address to listen on for web interface and telemetry
`web.telemetry-path` | Path under which to expose metrics
`config.file` | SQL Exporter configuration file name

Environment Variables
---------------------

Name    | Description
--------|------------
`CONFIG`  | Location of Configuration File (yaml)

Usage
=====

We recommend to deploy and run the SQL exporter in Kubernetes.

Kubernetes
----------

See [examples/kubernetes](https://github.com/justwatchcom/sql_exporter/tree/master/examples/kubernetes).

Grafana
-------

See [examples/grafana](https://github.com/justwatchcom/sql_exporter/tree/master/examples/grafana).

Prometheus
----------

Example recording and alerting rules are available in [examples/prometheus](https://github.com/justwatchcom/sql_exporter/tree/master/examples/prometheus).

Configuration
-------------

When writing queries for this exporter please keep in mind that Prometheus data
model assigns exactly one `float` to a metric, possibly further identified by a
set of zero or more labels. These labels need to be of type `string` or `text`.

If your SQL dialect supports explicit type casts, you should always cast your
label columns to `text` and the metric columns to `float`. The SQL exporter will
try hard to support other types or drivers w/o support for explicit cast as well,
but the results may not be what you expect.

Below is a documented configuration example showing all available options.
For a more realistic example please have a look at [examples/kubernetes/configmap.yml](https://github.com/justwatchcom/sql_exporter/blob/master/examples/kubernetes/configmap.yml).

```yaml
---
# jobs is a map of jobs, define any number but please keep the connection usage on the DBs in mind
jobs:
  # each job needs a unique name, it's used for logging and as a default label
- name: "example"
  # interval defined the pause between the runs of this job
  interval: '5m'
  # cron_schedule when to execute the job in the standard CRON syntax
  # if specified, the interval is ignored
  cron_schedule: "0 0 * * *"
  # connections is an array of connection URLs
  # each query will be executed on each connection
  connections:
  - 'postgres://postgres@localhost/postgres?sslmode=disable'
  # startup_sql is an array of SQL statements
  # each statements is executed once after connecting
  startup_sql:
  - 'SET lock_timeout = 1000'
  - 'SET idle_in_transaction_session_timeout = 100'
  # iterator is an optional mechanism to iterate over a series of values, e.g. multiple databases
  iterator:
    # sql is the SQL to execute to retrieve the list of values to iterate over -
    # query result must be a single column
    sql: 'SELECT database_name FROM databases'
    # placeholder should be present in the original query and not also used as an environment variable
    # e.g. {{PLACEHOLDER}} - it will be replaced by the values retrieved by the query
    placeholder: PLACEHOLDER
    # label is the label name to which the iterator value gets assigned
    label: database
  # queries is a map of Metric/Query mappings
  queries:
    # name is prefixed with sql_ and used as the metric name
  - name: "running_queries"
    # help is a requirement of the Prometheus default registry, currently not
    # used by the Prometheus server. Important: Must be the same for all metrics
    # with the same name!
    help: "Number of running queries"
    # Optional: Column to use as a metric timestamp source.
    # Leave unset if it's not needed
    timestamp: "created_at"
    # Labels is an array of columns which will be used as additional labels.
    # Must be the same for all metrics with the same name!
    # All labels columns should be of type text, varchar or string
    labels:
      - "datname"
      - "usename"
    # Values is an array of columns used as metric values. All values should be
    # of type float
    values:
      - "count"
    # Query is the SQL query that is run unalterted on each of the connections
    # for this job
    query:  |
            SELECT now() as created_at, datname::text, usename::text, COUNT(*)::float AS count
            FROM pg_stat_activity GROUP BY created_at, datname, usename;
    # Consider the query failed if it returns zero rows
    allow_zero_rows: false
```

Running as non-superuser on PostgreSQL
--------------------------------------

Some queries require superuser privileges on PostgreSQL.
If you prefer not to run the exporter with superuser privileges, you can use some views/functions to get around this limitation.

```sql
CREATE USER postgres_exporter PASSWORD 'pw';
ALTER USER postgres_exporter SET SEARCH_PATH TO postgres_exporter,pg_catalog;

CREATE SCHEMA postgres_exporter AUTHORIZATION postgres_exporter;

CREATE FUNCTION postgres_exporter.f_select_pg_stat_activity()
RETURNS setof pg_catalog.pg_stat_activity
LANGUAGE sql
SECURITY DEFINER
AS $$
  SELECT * from pg_catalog.pg_stat_activity;
$$;

CREATE FUNCTION postgres_exporter.f_select_pg_stat_replication()
RETURNS setof pg_catalog.pg_stat_replication
LANGUAGE sql
SECURITY DEFINER
AS $$
  SELECT * from pg_catalog.pg_stat_replication;
$$;

CREATE VIEW postgres_exporter.pg_stat_replication
AS
  SELECT * FROM postgres_exporter.f_select_pg_stat_replication();

CREATE VIEW postgres_exporter.pg_stat_activity
AS
  SELECT * FROM postgres_exporter.f_select_pg_stat_activity();

GRANT SELECT ON postgres_exporter.pg_stat_replication TO postgres_exporter;
GRANT SELECT ON postgres_exporter.pg_stat_activity TO postgres_exporter;
```

Logging
-------

You can change the loglevel by setting the `LOGLEVEL` variable in the exporters
environment.

```
LOGLEVEL=info ./sql_exporter
```

Database specific configurations
--------------------------------

For some database backends some special functionality is available:

* cloudsql-postgres: A special `*` character can be used to query all databases accessible by the account
* cloudsql-mysql: Same as above
* databricks: Supports access token, OAuth M2M (machine-to-machine) or U2M (user-to-machine) authentication as supported by the [driver](https://github.com/databricks/databricks-sql-go).
* rds-postgres: This type of URL expects a working AWS configuration
  which will use the equivalent of `rds generate-db-auth-token`
  for the password. For this driver, the `AWS_REGION` environment variable
  must be set.
* rds-mysql: This type of URL expects a working AWS configuration
  which will use the equivalent of `rds generate-db-auth-token`
  for the password. For this driver, the `AWS_REGION` environment variable
  must be set.


Why this exporter exists
========================

The other projects with similar goals did not meet our requirements on either
maturity or flexibility. This exporter does not rely on any other service and
runs in production for some time already.

License
=======

MIT License
