package main

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2" // register the ClickHouse driver
	"github.com/cenkalti/backoff"
	dbsql "github.com/databricks/databricks-sql-go" // register the Databricks driver
	"github.com/databricks/databricks-sql-go/auth/oauth/m2m"
	"github.com/databricks/databricks-sql-go/auth/oauth/u2m"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/go-sql-driver/mysql" // register the MySQL driver
	"github.com/gobwas/glob"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"                                   // register the PostgreSQL driver
	_ "github.com/microsoft/go-mssqldb"                     // register the MS-SQL driver
	_ "github.com/microsoft/go-mssqldb/integratedauth/krb5" // Register integrated auth for MS-SQL
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

func handleRDSMySQLIAMAuth(conn string) (string, time.Time, error) {
	dsn := strings.TrimPrefix(conn, "rds-mysql://")
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to parse MySQL DSN: %v", err)
	}

	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))

	token, err := rdsutils.BuildAuthToken(config.Addr, os.Getenv("AWS_REGION"), config.User, sess.Config.Credentials)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to build RDS auth token: %v", err)
	}

	expirationTime := time.Now().Add(14 * time.Minute)

	return token, expirationTime, nil
}

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

		// append the iterator label if it is set
		if j.Iterator.Label != "" {
			q.Labels = append(q.Labels, j.Iterator.Label)
		}

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

			// Handle both RDS MySQL and regular MySQL connections
			if strings.HasPrefix(conn, "rds-mysql://") || strings.HasPrefix(conn, "mysql://") {
				isRDS := strings.HasPrefix(conn, "rds-mysql://")
				var dsn string
				var expirationTime time.Time

				trimmedConn := conn
				if isRDS {
					trimmedConn = strings.TrimPrefix(conn, "rds-mysql://")
				} else {
					trimmedConn = strings.TrimPrefix(conn, "mysql://")
				}

				config, err := mysql.ParseDSN(trimmedConn)
				if err != nil {
					level.Error(j.log).Log("msg", "Failed to parse MySQL DSN", "url", conn, "err", err)
					continue
				}

				if isRDS {
					authToken, tokenExpiration, err := handleRDSMySQLIAMAuth(conn)
					if err != nil {
						level.Error(j.log).Log("msg", "Failed to build RDS auth token", "url", conn, "err", err)
						continue
					}
					config.Passwd = authToken
					config.AllowCleartextPasswords = true
					expirationTime = tokenExpiration
				}

				dsn = config.FormatDSN()
				if isRDS {
					dsn = "rds-mysql://" + dsn
				}

				j.conns = append(j.conns, &connection{
					conn:                nil,
					url:                 dsn,
					driver:              "mysql",
					host:                config.Addr,
					database:            config.DBName,
					user:                config.User,
					tokenExpirationTime: expirationTime,
				})
				continue
			}

			// Handle Databricks connections
			// Supports standard token auth and OAuth (M2M and U2M)
			// OAuth M2M: databricks://host/httpPath?catalog=xxx&authType=m2m&clientId=xxx&clientSecret=xxx
			// OAuth U2M: databricks://host/httpPath?catalog=xxx&authType=u2m
			// Token auth: databricks://token:xxx@host/httpPath?catalog=xxx
			if strings.HasPrefix(conn, "databricks://") {
				u, err := url.Parse(conn)
				if err != nil {
					level.Error(j.log).Log("msg", "Failed to parse Databricks URL", "url", conn, "err", err)
					continue
				}

				user := ""
				if u.User != nil {
					user = u.User.Username()
				}

				// Extract catalog/database from query params or path
				queryParams := u.Query()
				database := queryParams.Get("catalog")
				httpPath := strings.TrimPrefix(u.Path, "/")

				// Check for OAuth authentication type
				authType := queryParams.Get("authType")
				clientID := queryParams.Get("clientId")
				clientSecret := queryParams.Get("clientSecret")

				newConn := &connection{
					conn:     nil,
					url:      conn,
					driver:   "databricks",
					host:     u.Host,
					database: database,
					user:     user,
				}

				// Configure OAuth if specified
				if authType == "m2m" || authType == "u2m" {
					newConn.databricksOAuthType = authType
					newConn.databricksClientID = clientID
					newConn.databricksClientSecret = clientSecret
					newConn.databricksHost = u.Host
					newConn.databricksHTTPPath = httpPath
					newConn.databricksCatalog = database
				}

				j.conns = append(j.conns, newConn)
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

			if strings.HasPrefix(conn, "postgres://") || strings.HasPrefix(conn, "pg://") {
				u, err := url.Parse(conn)
				var filteredDBs []string
				if err != nil {
					level.Error(j.log).Log("msg", "Failed to parse URL", "url", conn, "err", err)
					continue
				}
				if strings.Contains(u.Path, "include") || strings.Contains(u.Path, "exclude") {
					if strings.Contains(u.Path, "include") && strings.Contains(u.Path, "exclude") {
						level.Error(j.log).Log("msg", "You cannot use exclude and include:", "url", conn, "err", err)
						return
					} else {
						extractedPath := u.Path //save pattern
						u.Path = "/postgres"
						dsn := u.String()
						databases, err := listDatabases(dsn)
						if err != nil {
							level.Error(j.log).Log("msg", "Error listing databases", "url", conn, "err", err)
							continue
						}
						filteredDBs, err = filterDatabases(databases, extractedPath)
						if err != nil {
							level.Error(j.log).Log("msg", "Error filtering databases", "url", conn, "err", err)
							continue
						}

						for _, db := range filteredDBs {
							u.Path = "/" + db // Set the path to the filtered database name
							newUserDSN := u.String()
							j.conns = append(j.conns, &connection{
								conn:     nil,
								url:      newUserDSN,
								driver:   u.Scheme,
								host:     u.Host,
								database: db,
								user:     u.User.Username(),
							})
						}
						continue
					}
				}
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

			// For SQL Server connections,
			// url.path is reserved for sql server instance not the database name
			// database name can be specified in multiple ways:
			// 1. Query parameter: database=dbname
			// 2. Query parameter: initial catalog=dbname
			database := ""
			if strings.HasPrefix(conn, "sqlserver://") {
				// Check for 'database' parameter first
				if dbParam := getQueryStringCaseInsensitive(u.Query(), "database"); dbParam != "" {
					database = dbParam
				} else if catalogParam := getQueryStringCaseInsensitive(u.Query(), "initial catalog"); catalogParam != "" {
					// 'initial catalog' is an alternative to 'database' parameter
					database = catalogParam
				}
			} else {
				database = strings.TrimPrefix(u.Path, "/")
			}

			// we expose some of the connection variables as labels, so we need to
			// remember them
			newConn := &connection{
				conn:     nil,
				url:      conn,
				driver:   u.Scheme,
				host:     u.Host,
				database: database,
				user:     user,
			}
			if newConn.driver == "athena" {
				newConn.conn, err = sqlx.Open("athena", u.RawQuery)
				if err != nil {
					level.Error(j.log).Log("msg", "Failed to open Athena connection", "connection", conn, "err", err)
					continue
				}
			}
			if newConn.driver == "snowflake" {
				u, err := url.Parse(conn)
				if err != nil {
					level.Error(j.log).Log("msg", "Failed to parse Snowflake URL", "url", conn, "err", err)
					continue
				}

				queryParams := u.Query()
				privateKeyPath := os.ExpandEnv(queryParams.Get("private_key_file"))

				cfg := &gosnowflake.Config{
					Account:  u.Host,
					User:     u.User.Username(),
					Role:     queryParams.Get("role"),
					Database: queryParams.Get("database"),
					Schema:   queryParams.Get("schema"),
				}

				if privateKeyPath != "" {
					// RSA key auth
					keyBytes, err := os.ReadFile(privateKeyPath)
					if err != nil {
						level.Error(j.log).Log("msg", "Failed to read private key file", "path", privateKeyPath, "err", err)
						continue
					}

					keyBlock, _ := pem.Decode(keyBytes)
					if keyBlock == nil {
						level.Error(j.log).Log("msg", "Failed to decode PEM block", "path", privateKeyPath)
						continue
					}

					var privateKey *rsa.PrivateKey
					if parsedKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); err == nil {
						privateKey, _ = parsedKey.(*rsa.PrivateKey)
					} else if parsedKey, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err == nil {
						privateKey = parsedKey
					} else {
						level.Error(j.log).Log("msg", "Failed to parse private key", "err", err)
						continue
					}

					cfg.Authenticator = gosnowflake.AuthTypeJwt
					cfg.PrivateKey = privateKey

					dsn, err := gosnowflake.DSN(cfg)
					if err != nil {
						level.Error(j.log).Log("msg", "Failed to create Snowflake DSN with RSA", "err", err)
						continue
					}

					newConn.snowflakeConfig = cfg
					newConn.snowflakeDSN = dsn
					newConn.host = u.Host
					newConn.tokenExpirationTime = time.Now().Add(time.Hour)
				} else {
					// Password auth
					if pw, set := u.User.Password(); set {
						cfg.Password = pw
					}
					if u.Port() != "" {
						if port, err := strconv.Atoi(u.Port()); err == nil {
							cfg.Port = port
						}
					}

					dsn, err := gosnowflake.DSN(cfg)
					if err != nil {
						level.Error(j.log).Log("msg", "Failed to create Snowflake DSN with password", "err", err)
						continue
					}

					newConn.conn, err = sqlx.Open("snowflake", dsn)
					if err != nil {
						level.Error(j.log).Log("msg", "Failed to open Snowflake connection", "err", err)
						continue
					}
				}

				j.conns = append(j.conns, newConn)
				continue
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

	// execute iterator SQL
	if j.Iterator.SQL != "" {
		level.Debug(j.log).Log("msg", "IteratorSQL", "Query:", j.Iterator.SQL)
		rows, err := conn.conn.Queryx(j.Iterator.SQL)
		if err != nil {
			level.Warn(j.log).Log("msg", "Failed to run iterator query", "err", err, "host", conn.host)
			j.markFailed(conn)
			// we don't have the query name yet.
			failedQueryCounter.WithLabelValues(j.Name, "").Inc()
			return
		}

		defer rows.Close()

		var ivs []string
		for rows.Next() {
			var value string
			err := rows.Scan(&value)
			if err != nil {
				level.Warn(j.log).Log("msg", "Failed to read iterator values", "err", err, "host", conn.host)
				j.markFailed(conn)
				// we don't have the query name yet.
				failedQueryCounter.WithLabelValues(j.Name, "").Inc()
				return
			}
			ivs = append(ivs, value)
		}
		conn.iteratorValues = ivs
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
		// repeat query with iterator values if set and the query has the iterator placeholder
		if conn.iteratorValues != nil && q.HasIterator(j.Iterator.Placeholder) {
			level.Debug(q.log).Log("msg", "Running Iterator Query")
			// execute the query with iterator on the connection
			if err := q.RunIterator(conn, j.Iterator.Placeholder, conn.iteratorValues, j.Iterator.Label); err != nil {
				level.Warn(q.log).Log("msg", "Failed to run query", "err", err)
				continue
			}
		} else {
			level.Debug(q.log).Log("msg", "Running Query")
			// execute the query on the connection
			if err := q.Run(conn); err != nil {
				level.Warn(q.log).Log("msg", "Failed to run query", "err", err)
				continue
			}
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
		if strings.HasPrefix(c.url, "rds-mysql://") && time.Now().After(c.tokenExpirationTime) {
			level.Warn(job.log).Log("msg", "Connection token expired, reconnecting")

			authToken, expirationTime, err := handleRDSMySQLIAMAuth(c.url)
			if err != nil {
				return fmt.Errorf("failed to refresh RDS MySQL IAM Auth token: %w", err)
			}

			config, err := mysql.ParseDSN(strings.TrimPrefix(c.url, "rds-mysql://"))
			if err != nil {
				return fmt.Errorf("failed to parse MySQL DSN: %w", err)
			}

			config.Passwd = authToken
			dsn := "rds-mysql://" + config.FormatDSN()

			// Close the existing connection
			c.conn.Close()
			c.conn = nil

			// Update the connection details
			c.tokenExpirationTime = expirationTime
			c.url = dsn

			// Connect to the database with the new token
			conn, err := sqlx.Connect(c.driver, strings.TrimPrefix(dsn, "rds-mysql://"))
			if err != nil {
				return fmt.Errorf("failed to connect to the database: %w", err)
			}
			c.conn = conn
			return nil
		}
		return nil
	}
	if c.driver == "snowflake" {
		if c.snowflakeDSN != "" {
			if time.Now().After(c.tokenExpirationTime) {
				if c.conn != nil {
					c.conn.Close()
					c.conn = nil
				}
				c.tokenExpirationTime = time.Now().Add(time.Hour)
			}

			db, err := sqlx.Open("snowflake", c.snowflakeDSN)
			if err != nil {
				return fmt.Errorf("failed to open Snowflake connection: %w (host: %s)", err, c.host)
			}

			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(0)
			db.SetConnMaxLifetime(30 * time.Minute)

			if err := db.Ping(); err != nil {
				db.Close()
				return fmt.Errorf("failed to ping Snowflake: %w (host: %s)", err, c.host)
			}

			c.conn = db
			return nil
		}
	}
	// Handle Databricks OAuth connections (M2M and U2M)
	if c.driver == "databricks" && c.databricksOAuthType != "" {
		var connector driver.Connector
		var err error

		connectorOptions := []dbsql.ConnOption{
			dbsql.WithServerHostname(c.databricksHost),
			dbsql.WithHTTPPath(c.databricksHTTPPath),
		}

		if c.databricksCatalog != "" {
			connectorOptions = append(connectorOptions, dbsql.WithInitialNamespace(c.databricksCatalog, ""))
		}

		switch c.databricksOAuthType {
		case "m2m":
			// Machine-to-Machine OAuth using client credentials
			if c.databricksClientID == "" || c.databricksClientSecret == "" {
				return fmt.Errorf("databricks M2M OAuth requires clientId and clientSecret parameters")
			}
			authenticator := m2m.NewAuthenticator(c.databricksClientID, c.databricksClientSecret, c.databricksHost)
			connectorOptions = append(connectorOptions, dbsql.WithAuthenticator(authenticator))

		case "u2m":
			// User-to-Machine OAuth (interactive browser-based authentication)
			authenticator, err := u2m.NewAuthenticator(c.databricksHost, 2*time.Minute)
			if err != nil {
				return fmt.Errorf("failed to create Databricks U2M authenticator: %w", err)
			}
			connectorOptions = append(connectorOptions, dbsql.WithAuthenticator(authenticator))
		}

		connector, err = dbsql.NewConnector(connectorOptions...)
		if err != nil {
			return fmt.Errorf("failed to create Databricks connector: %w (host: %s)", err, c.host)
		}

		db := sql.OpenDB(connector)
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(job.Interval * 2)

		// Wrap with sqlx
		c.conn = sqlx.NewDb(db, "databricks")

		// execute StartupSQL
		for _, query := range job.StartupSQL {
			level.Debug(job.log).Log("msg", "StartupSQL", "Query:", query)
			c.conn.MustExec(query)
		}

		return nil
	}
	dsn := c.url
	switch c.driver {
	case "mysql":
		dsn = strings.TrimPrefix(dsn, "mysql://")
		dsn = strings.TrimPrefix(dsn, "rds-mysql://")
	case "databricks":
		dsn = strings.TrimPrefix(dsn, "databricks://")
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

func getQueryStringCaseInsensitive(values url.Values, key string) string {
	key = strings.ToLower(key)
	for k, v := range values {
		if strings.ToLower(k) == key && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}
