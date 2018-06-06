# Prometheus SQL Exporter [![Build Status](https://travis-ci.org/justwatchcom/sql_exporter.svg?branch=master)](https://travis-ci.org/justwatchcom/sql_exporter)

[![Docker Pulls](https://img.shields.io/docker/pulls/justwatch/sql_exporter.svg?maxAge=604800)](https://hub.docker.com/r/justwatch/sql_exporter)
[![Go Report Card](https://goreportcard.com/badge/github.com/justwatchcom/sql_exporter)](https://goreportcard.com/report/github.com/justwatchcom/sql_exporter)

This repository contains an service that runs user-defined SQL queries at flexible intervals and exports the resulting metrics via HTTP for Prometheus consumption.

Status
======

Actively used with PostgreSQL in production. We'd like to eventually support all databases for which stable Go database [drivers](https://github.com/golang/go/wiki/SQLDrivers) are available. Contributions welcome.

What does it look like?
=======================

![Grafana DB Dashboard](/examples/grafana/screenshot.jpg?raw=true)

Getting Started
===============

Create a config.yml and run the service:

```
go get github.com/justwatchcom/sql_exporter
cp config.yml.dist config.yml
./prom-sql-exporter
```

Running in Docker:

```
docker run -v `pwd`/config.yml:/config/config.yml -e CONFIG=/config/config.yml -d -p 9237:9237 --name sql_exporter justwatch/sql_exporter
```

Manual `scrape_configs` snippet:

```
scrape_configs:
- job_name: sql_exporter
  static_configs:
  - targets: ['localhost:9237']
```

Flags
-----

Name    | Description
--------|------------
version | Print version information
web.listen-address | Address to listen on for web interface and telemetry
web.telemetry-path | Path under which to expose metrics
config.file | SQL Exporter configuration file name

Environment Variables
---------------------

Name    | Description
--------|------------
CONFIG  | Location of Configuration File (yaml)

Usage
=====

We recommend to deploy and run the SQL exporter in Kubernetes.

Kubernetes
----------

See `examples/kubernetes`

Grafana
-------

See `examples/grafana`

Prometheus
----------

Example recording and alerting rules are available in `examples/prometheus`.

Configuration
-------------

When writing queries for this exporter please keep in mind that Prometheus data
model assigns exactly one `float` to a metric , possibly further identified by a
set of zero or more labels. These labels need to be of type `string` or `text`.

If your SQL dialect supports explicit type casts, you should always cast your
label columns to `text` and the metric colums to `float`. The SQL exporter will
try hard to support other types or drivers w/o support for explicit cast as well,
but the results may not be what you expect.

Below is a documented configuration example showing all available options.
For a more realistic example please have a look at `examples/kubernetes/configmap.yml`.

```
---
# jobs is a map of jobs, define any number but please keep the connection usage on the DBs in mind
jobs:
  # each job needs a unique name, it's used for logging and as an default label
- name: "example"
  # interval defined the pause between the runs of this job
  interval: '5m'
  # connections is an array of connection URLs
  # each query will be executed on each connection
  connections:
  - 'postgres://postgres@localhost/postgres?sslmode=disable'
  # startup_sql is an array of SQL statements
  # each statements is executed once after connecting
  startup_sql:
  - 'SET lock_timeout = 1000'
  - 'SET idle_in_transaction_session_timeout = 100'
  # queries is a map of Metric/Query mappings
  queries:
    # name is prefied with sql_ and used as the metric name
  - name: "running_queries"
    # help is a requirement of the Prometheus default registry, currently not
    # used by the Prometheus server. Important: Must be the same for all metrics
    # with the same name!
    help: "Number of running queries"
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
    # Query is the SQL query that is run unalterted on the each of the connections
    # for this job
    query:  |
            SELECT datname::text, usename::text, COUNT(*)::float AS count
            FROM pg_stat_activity GROUP BY datname, usename;
```

Running as non-superuser on PostgreSQL
--------------------------------------

Some queries require superuser privileges on PostgreSQL.
If you prefer not to run the exporter with superuser privileges, you can use some views/functions to get around this limitation.

```
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

Why this exporter exists
========================

The other projects with similar goals did not meet our requirements on either
maturity or flexibility. This exporter does not rely on any other service and
runs in production for some time already.

License
=======

MIT License
