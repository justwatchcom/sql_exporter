package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2" // register the ClickHouse driver
	"github.com/cenkalti/backoff"
	_ "github.com/denisenkom/go-mssqldb" // register the MS-SQL driver
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/go-sql-driver/mysql" // register the MySQL driver
	"github.com/gobwas/glob"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq" // register the PostgreSQL driver
	"github.com/prometheus/client_golang/prometheus"
	_ "github.com/segmentio/go-athena" // register the AWS Athena driver
	"github.com/snowflakedb/gosnowflake"
	_ "github.com/vertica/vertica-sql-go" // register the Vertica driver
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/rds/rdsutils"
)

var (
	// MetricNameRE matches any invalid metric name
	// characters, see github.com/prometheus/common/model.MetricNameRE
	MetricNameRE = regexp.MustCompile("[^a-zA-Z0-9_:]+")
	// CloudSQLPrefix is the prefix which trigger the connection to be done via the cloudsql connection client
	CloudSQLPrefix = "cloudsql+"
)

// Init will initialize the metric descriptors
func (j *Job) Init(logger log.Logger, queries map[string]string) error {
	j.log = log.With(logger, "job", j.Name)
	// register each query as an metric
	for _, q := range j.Queries {
		if q == nil {
			level.Warn(j.log).Log("msg", "Skipping invalid query")
			continue
		}
		q.log = log.With(j.log, "query", q.Name)
		q.jobName = j.Name
		if q.Query == "" && q.QueryRef != "" {
			if qry, found := queries[q.QueryRef]; found {
				q.Query = qry
			}
		}
		if q.Query == "" {
			level.Warn(q.log).Log("msg", "Skipping empty query")
			continue
		}
		if q.metrics == nil {
			// we have no way of knowing how many metrics will be returned by the
			// queries, so we just assume that each query returns at least one metric.
			// after the each round of collection this will be resized as necessary.
			q.metrics = make(map[*connection][]prometheus.Metric, len(j.Queries))
		}
		// try to satisfy prometheus naming restrictions
		name := MetricNameRE.ReplaceAllString("sql_"+q.Name, "")
		help := q.Help
		// prepare a new metrics descriptor
		//
		// the tricky part here is that the *order* of labels has to match the
		// order of label values supplied to NewConstMetric later
		q.desc = prometheus.NewDesc(
			name,
			help,
			append(q.Labels, "driver", "host", "database", "user", "col"),
			prometheus.Labels{
				"sql_job": j.Name,
			},
		)
	}
	j.updateConnections()
	return nil
}

func (j *Job) updateConnections() {
	// if there are no connection URLs for this job it can't be run
	if j.Connections == nil {
		level.Error(j.log).Log("msg", "no connections for job", "job_name", j.Name)
	}
	// make space for the connection objects
	if j.conns == nil {
		j.conns = make([]*connection, 0, len(j.Connections))
	}
	// parse the connection URLs and create a connection object for each
	if len(j.conns) < len(j.Connections) {
		for _, conn := range j.Connections {
			// Check if we need to use cloudsql driver
			if useCloudSQL, cloudsqlDriver := isValidCloudSQLDriver(conn); useCloudSQL {
				// Do CloudSQL stuff
				parsedU, err := ParseCloudSQLUrl(conn)
				if err != nil {
					level.Error(j.log).Log("msg", "could not parse cloudsql conn", "conn", conn)
					continue
				}

				user := ""
				if parsedU.User != nil {
					user = parsedU.User.Username()
				}

				database := strings.TrimPrefix(parsedU.Path, "/")

				if strings.ContainsRune(parsedU.Instance, '*') {
					// We have a glob for the instance.
					//	List all CloudSQL instance and figure out which ones match
					ctx := context.Background()
					instanceGlob := glob.MustCompile(parsedU.Instance)
					databaseGlob := glob.MustCompile(database)

					// Create the Google Cloud SQL service.
					service, err := sqladmin.NewService(ctx)
					if err != nil {
						level.Error(j.log).Log("msg", "could not create sqladmin client", "conn", conn, "err", err)
						continue
					}

					// List instances for the project ID.
					instances, err := service.Instances.List(parsedU.Project).Do()
					if err != nil {
						level.Error(j.log).Log("msg", "could not list cloudsql instances", "conn", conn, "err", err)
						continue
					}

					for _, instance := range instances.Items {

						if !instanceGlob.Match(instance.Name) || parsedU.Region != instance.Region {
							continue
						}

						if strings.ContainsRune(database, '*') {
							// We have a glob for the database.
							//	List all databases in instance and figure out which ones match

							// List databases for the instance.
							databases, err := service.Databases.List(parsedU.Project, instance.Name).Do()
							if err != nil {
								level.Error(j.log).Log("msg", "could not list cloudsql databases", "instance", instance.Name, "err", err)
								continue
							}

							for _, db := range databases.Items {
								if databaseGlob.Match(db.Name) {
									connectionURL, err := parsedU.GetConnectionURL(cloudsqlDriver, instance.ConnectionName, db.Name)
									if err != nil {
										level.Error(j.log).Log("msg", "could not generate connection url", "err", err)
										continue
									}
									newConn := &connection{
										conn:     nil,
										url:      connectionURL,
										driver:   cloudsqlDriver,
										host:     instance.Name,
										database: db.Name,
										user:     user,
									}
									j.conns = append(j.conns, newConn)
								}
							}
						} else {
							connectionURL, err := parsedU.GetConnectionURL(cloudsqlDriver, instance.ConnectionName, database)
							if err != nil {
								level.Error(j.log).Log("msg", "could not generate connection url", "err", err)
								continue
							}

							newConn := &connection{
								conn:     nil,
								url:      connectionURL,
								driver:   cloudsqlDriver,
								host:     instance.Name,
								database: database,
								user:     user,
							}
							j.conns = append(j.conns, newConn)
						}
					}

				} else {
					connectionName := fmt.Sprintf("%s:%s:%s", parsedU.Project, parsedU.Region, parsedU.Instance)
					connectionURL, err := parsedU.GetConnectionURL(cloudsqlDriver, connectionName, database)
					if err != nil {
						level.Error(j.log).Log("msg", "could not generate connection url", "err", err)
						continue
					}
					newConn := &connection{
						conn:     nil,
						url:      connectionURL,
						driver:   cloudsqlDriver,
						host:     parsedU.Host,
						database: database,
						user:     user,
					}
					j.conns = append(j.conns, newConn)
				}

				continue
			}

			// MySQL DSNs do not parse cleanly as URLs as of Go 1.12.8+
			if strings.HasPrefix(conn, "mysql://") {
				config, err := mysql.ParseDSN(strings.TrimPrefix(conn, "mysql://"))
				if err != nil {
					level.Error(j.log).Log("msg", "Failed to parse MySQL DSN", "url", conn, "err", err)
				}

				j.conns = append(j.conns, &connection{
					conn:     nil,
					url:      conn,
					driver:   "mysql",
					host:     config.Addr,
					database: config.DBName,
					user:     config.User,
				})
				continue
			}
			if strings.HasPrefix(conn, "rds-postgres://") {
				// Reuse Postgres driver by stripping "rds-" from connection URL after building the RDS authentication token
				conn = strings.TrimPrefix(conn, "rds-")
				u, err := url.Parse(conn)
				if err != nil {
					level.Error(j.log).Log("msg", "failed to parse connection url", "url", conn, "err", err)
					continue
				}
				sess := session.Must(session.NewSessionWithOptions(session.Options{
					SharedConfigState: session.SharedConfigEnable,
				}))
				token, err := rdsutils.BuildAuthToken(u.Host, os.Getenv("AWS_REGION"), u.User.Username(), sess.Config.Credentials)
				if err != nil {
					level.Error(j.log).Log("msg", "failed to parse connection url", "url", conn, "err", err)
					continue
				}
				conn = strings.Replace(conn, "AUTHTOKEN", url.QueryEscape(token), 1)
			}

			u, err := url.Parse(conn)
			if err != nil {
				level.Error(j.log).Log("msg", "Failed to parse URL", "url", conn, "err", err)
				continue
			}
			user := ""
			if u.User != nil {
				user = u.User.Username()
			}
			// we expose some of the connection variables as labels, so we need to
			// remember them
			newConn := &connection{
				conn:     nil,
				url:      conn,
				driver:   u.Scheme,
				host:     u.Host,
				database: strings.TrimPrefix(u.Path, "/"),
				user:     user,
			}
			if newConn.driver == "athena" {
				// call go-athena's Open() to ensure conn.db is set,
				// otherwise API calls will complain about an empty database field:
				// "InvalidParameter: 1 validation error(s) found. - minimum field size of 1, StartQueryExecutionInput.QueryExecutionContext.Database."
				newConn.conn, err = sqlx.Open("athena", u.String())
				if err != nil {
					level.Error(j.log).Log("msg", "Failed to open Athena connection", "connection", conn, "err", err)
					continue
				}
			}
			if newConn.driver == "snowflake" {
				cfg := &gosnowflake.Config{
					Account: u.Host,
					User:    u.User.Username(),
				}

				pw, set := u.User.Password()
				if set {
					cfg.Password = pw
				}

				if u.Port() != "" {
					portStr, err := strconv.Atoi(u.Port())
					if err != nil {
						level.Error(j.log).Log("msg", "Failed to parse Snowflake port", "connection", conn, "err", err)
						continue
					}
					cfg.Port = portStr
				}

				dsn, err := gosnowflake.DSN(cfg)
				if err != nil {
					level.Error(j.log).Log("msg", "Failed to create Snowflake DSN", "connection", conn, "err", err)
					continue
				}

				newConn.conn, err = sqlx.Open("snowflake", dsn)
				if err != nil {
					level.Error(j.log).Log("msg", "Failed to open Snowflake connection", "connection", conn, "err", err)
					continue
				}
			}

			j.conns = append(j.conns, newConn)
		}
	}
}

func (j *Job) ExecutePeriodically() {
	level.Debug(j.log).Log("msg", "Starting")
	for {
		j.Run()
		level.Debug(j.log).Log("msg", "Sleeping until next run", "sleep", j.Interval.String())
		time.Sleep(j.Interval)
	}
}

func (j *Job) runOnceConnection(conn *connection, done chan int) {
	updated := 0
	defer func() {
		done <- updated
	}()

	// connect to DB if not connected already
	if err := conn.connect(j); err != nil {
		level.Warn(j.log).Log("msg", "Failed to connect", "err", err, "host", conn.host)
		j.markFailed(conn)
		// we don't have the query name yet.
		failedQueryCounter.WithLabelValues(j.Name, "").Inc()
		return
	}

	for _, q := range j.Queries {
		if q == nil {
			continue
		}
		if q.desc == nil {
			// this may happen if the metric registration failed
			level.Warn(q.log).Log("msg", "Skipping query. Collector is nil")
			continue
		}
		level.Debug(q.log).Log("msg", "Running Query")
		// execute the query on the connection
		if err := q.Run(conn); err != nil {
			level.Warn(q.log).Log("msg", "Failed to run query", "err", err)
			continue
		}
		level.Debug(q.log).Log("msg", "Query finished")
		updated++
	}
}

func (j *Job) markFailed(conn *connection) {
	for _, q := range j.Queries {
		failedScrapes.WithLabelValues(conn.driver, conn.host, conn.database, conn.user, q.jobName, q.Name).Set(1.0)
	}
}

// Run the job queries with exponential backoff, implements the cron.Job interface
func (j *Job) Run() {
	bo := backoff.NewExponentialBackOff()
	bo.MaxElapsedTime = j.Interval
	if bo.MaxElapsedTime == 0 {
		bo.MaxElapsedTime = time.Minute
	}
	if err := backoff.Retry(j.runOnce, bo); err != nil {
		level.Error(j.log).Log("msg", "Failed to run", "err", err)
	}
}

func (j *Job) runOnce() error {
	doneChan := make(chan int, len(j.conns))

	// execute queries for each connection in parallel
	for _, conn := range j.conns {
		go j.runOnceConnection(conn, doneChan)
	}

	// connections now run in parallel, wait for and collect results
	updated := 0
	for range j.conns {
		updated += <-doneChan
	}

	if updated < 1 {
		return fmt.Errorf("zero queries ran")
	}
	return nil
}

func (c *connection) connect(job *Job) error {
	// already connected
	if c.conn != nil {
		return nil
	}
	dsn := c.url
	switch c.driver {
	case "mysql":
		dsn = strings.TrimPrefix(dsn, "mysql://")
	case "clickhouse+tcp", "clickhouse+http": // Support both http and tcp connections
		dsn = strings.TrimPrefix(dsn, "clickhouse+")
		c.driver = "clickhouse"
	case "clickhouse": // Backward compatible alias
		dsn = "tcp://" + strings.TrimPrefix(dsn, "clickhouse://")
	}
	conn, err := sqlx.Connect(c.driver, dsn)
	if err != nil {
		return err
	}
	// be nice and don't use up too many connections for mere metrics
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	// Disable SetConnMaxLifetime if MSSQL as it is causing issues with the MSSQL driver we are using. See #60
	if c.driver != "sqlserver" {
		conn.SetConnMaxLifetime(job.Interval * 2)
	}

	// execute StartupSQL
	for _, query := range job.StartupSQL {
		level.Debug(job.log).Log("msg", "StartupSQL", "Query:", query)
		conn.MustExec(query)
	}

	c.conn = conn
	return nil
}
