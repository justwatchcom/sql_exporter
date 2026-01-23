/*
Package dbsql implements the go driver to Databricks SQL

# Usage

Clients should use the database/sql package in conjunction with the driver:

	import (
		"database/sql"

		_ "github.com/databricks/databricks-sql-go"
	)

	func main() {
		db, err := sql.Open("databricks", "token:<token>@<hostname>:<port>/<endpoint_path>")

		if err != nil {
			log.Fatal(err)
		}
		defer db.Close()
	}

# Connection via DSN (Data Source Name)

Use sql.Open() to create a database handle via a data source name string:

	db, err := sql.Open("databricks", "<dsn_string>")

The DSN format is:

	token:[my_token]@[hostname]:[port]/[endpoint http path]?param=value

Supported optional connection parameters can be specified in param=value and include:

  - catalog: Sets the initial catalog name in the session
  - schema: Sets the initial schema name in the session
  - maxRows: Sets up the max rows fetched per request. Default is 100000
  - timeout: Adds timeout (in seconds) for the server query execution. Default is no timeout
  - userAgentEntry: Used to identify partners. Set as a string with format <isv-name+product-name>
  - useCloudFetch: Used to enable cloud fetch for the query execution. Default is false
  - maxDownloadThreads: Sets up the max number of concurrent workers for cloud fetch. Default is 10
  - authType: Specifies the desired authentication type. Valid values are: Pat, OauthM2M, OauthU2M
  - accessToken: Personal access token. Required if authType set to Pat
  - clientID: Specifies the client ID to use with OauthM2M
  - clientSecret: Specifies the client secret to use with OauthM2M

Supported optional session parameters can be specified in param=value and include:

  - ansi_mode: (Boolean string). Session statements will adhere to rules defined by ANSI SQL specification.
  - timezone: (e.g. "America/Los_Angeles"). Sets the timezone of the session

# Connection via new connector object

Use sql.OpenDB() to create a database handle via a new connector object created with dbsql.NewConnector():

	import (
		"database/sql"
		dbsql "github.com/databricks/databricks-sql-go"
	)

	func main() {
		connector, err := dbsql.NewConnector(
			dbsql.WithServerHostname(<hostname>),
			dbsql.WithPort(<port>),
			dbsql.WithHTTPPath(<http_path>),
			dbsql.WithAccessToken(<my_token>)
		)
		if err != nil {
			log.Fatal(err)
		}

		db := sql.OpenDB(connector)
		defer db.Close()
		...
	}

Supported functional options include:

  - WithServerHostname(<hostname> string): Sets up the server hostname. The hostname can be prefixed with "http:" or "https:" to specify a protocol to use. Mandatory
  - WithPort(<port> int): Sets up the server port. Mandatory
  - WithAccessToken(<my_token> string): Sets up the Personal Access Token. Mandatory
  - WithHTTPPath(<http_path> string): Sets up the endpoint to the warehouse. Mandatory
  - WithInitialNamespace(<catalog> string, <schema> string): Sets up the catalog and schema name in the session. Optional
  - WithMaxRows(<max_rows> int): Sets up the max rows fetched per request. Default is 100000. Optional
  - WithSessionParams(<params_map> map[string]string): Sets up session parameters including "timezone" and "ansi_mode". Optional
  - WithTimeout(<timeout> Duration). Adds timeout (in time.Duration) for the server query execution. Default is no timeout. Optional
  - WithUserAgentEntry(<isv-name+product-name> string). Used to identify partners. Optional
  - WithCloudFetch (bool). Used to enable cloud fetch for the query execution. Default is false. Optional
  - WithMaxDownloadThreads (<num_threads> int). Sets up the max number of concurrent workers for cloud fetch. Default is 10. Optional
  - WithAuthenticator (<authenticator> auth.Authenticator). Sets up authentication. Required if neither access token or client credentials are provided.
  - WithClientCredentials(<clientID> string, <clientSecret> string). Sets up Oauth M2M authentication.

# Query cancellation and timeout

Cancelling a query via context cancellation or timeout is supported.

	// Set up context timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30 * time.Second)
	defer cancel()

	// Execute query. Query will be cancelled after 30 seconds if still running
	res, err := db.ExecContext(ctx, "CREATE TABLE example(id int, message string)")

# CorrelationId and ConnId

Use the driverctx package under driverctx/ctx.go to add CorrelationId and ConnId to the context.
CorrelationId and ConnId makes it convenient to parse and create metrics in logging.

**Connection Id**
Internal id to track what happens under a connection. Connections can be reused so this would track across queries.

**Query Id**
Internal id to track what happens under a query. Useful because the same query can be used with multiple connections.

**Correlation Id**
External id, such as request ID, to track what happens under a request. Useful to track multiple connections in the same request.

	ctx := dbsqlctx.NewContextWithCorrelationId(context.Background(), "workflow-example")

# Logging

Use the logger package under logger.go to set up logging (from zerolog).
By default, logging level is `warn`. If you want to disable logging, use `disabled`.
The user can also utilize Track() and Duration() to custom log the elapsed time of anything tracked.

	import (
		dbsqllog "github.com/databricks/databricks-sql-go/logger"
		dbsqlctx "github.com/databricks/databricks-sql-go/driverctx"
	)

	func main() {
		// Optional. Set the logging level with SetLogLevel()
		if err := dbsqllog.SetLogLevel("debug"); err != nil {
			log.Fatal(err)
		}

		// Optional. Set logging output with SetLogOutput()
		// Default is os.Stderr. If running in terminal, logger will use ConsoleWriter to prettify logs
		dbsqllog.SetLogOutput(os.Stdout)

		// Optional. Set correlation id with NewContextWithCorrelationId
		ctx := dbsqlctx.NewContextWithCorrelationId(context.Background(), "workflow-example")


		// Optional. Track time spent and log elapsed time
		msg, start := logger.Track("Run Main")
		defer log.Duration(msg, start)

		db, err := sql.Open("databricks", "<dsn_string>")
		...
	}

The result log may look like this:

	{"level":"debug","connId":"01ed6545-5669-1ec7-8c7e-6d8a1ea0ab16","corrId":"workflow-example","queryId":"01ed6545-57cc-188a-bfc5-d9c0eaf8e189","time":1668558402,"message":"Run Main elapsed time: 1.298712292s"}

# Programmatically Retrieving Connection and Query Id

Use the driverctx package under driverctx/ctx.go to add callbacks to the query context to receive the connection id and query id.

	import (
		"github.com/databricks/databricks-sql-go/driverctx"
	)

	func main() {

		...

		qidCallback := func(id string) {
			fmt.Println("query id: " + id)
		}

		connIdCallback := func(id string) {
			fmt.Println("connection id: " + id)
		}

		ctx := context.Background()
		ctx = driverctx.NewContextWithQueryIdCallback(ctx, qidCallback)
		ctx = driverctx.NewContextWithConnIdCallback(ctx, connIdCallback)

		rows, err1 := db.QueryContext(ctx, `select * from sometable`)

		...

	}

# Query parameters

Passing parameters to a query is supported when run against servers with version DBR 14.1.

	p := dbsql.Parameter{Name: "p_bool", Value: true},
	rows, err1 := db.QueryContext(ctx, `select * from sometable where condition=:p_bool`,dbsql.Parameter{Name: "p_bool", Value: true})

For complex types, you can specify the SQL type using the dbsql.Parameter type field. If this field is set, the value field MUST be set to a string.

# Staging Ingestion

The Go driver now supports staging operations. In order to use a staging operation, you first must update the context with a list of folders that you are allowing the driver to access.

	ctx := driverctx.NewContextWithStagingInfo(context.Background(), []string{"staging/"})

After doing so, you can execute staging operations using this context using the exec context.

	_, err1 := db.ExecContext(ctx, `PUT 'staging/file.csv' INTO '/Volumes/main/staging_test/e2etests/file.csv' OVERWRITE`)

# Errors

There are three error types exposed via dbsql/errors

	DBDriverError - An error in the go driver. Example: unimplemented functionality, invalid driver state, errors processing a server response, etc.

	DBRequestError - An error that is caused by an invalid request. Example: permission denied, invalid http path or other connection parameter, resource not available, etc.

	DBExecutionError - Any error that occurs after the SQL statement has been accepted such as a SQL syntax error, missing table, etc.

Each type has a corresponding sentinel value which can be used with errors.Is() to determine if one of the types is present in an error chain.

	DriverError
	RequestError
	ExecutionError

Example usage:

	import (
		fmt
		errors
		dbsqlerr "github.com/databricks/databricks-sql-go/errors"
	)

	func main() {
		...
		_, err := db.ExecContext(ogCtx, `Select id from range(100)`)
		if err != nil {
			if errors.Is(err, dbsqlerr.ExecutionError) {
				var execErr dbsqlerr.DBExecutionError
				if ok := errors.As(err, &execError); ok {
						fmt.Printf("%s, corrId: %s, connId: %s, queryId: %s, sqlState: %s, isRetryable: %t, retryAfter: %f seconds",
						execErr.Error(),
						execErr.CorrelationId(),
						execErr.ConnectionId(),
						execErr.QueryId(),
						execErr.SqlState(),
					    execErr.IsRetryable(),
						execErr.RetryAfter().Seconds(),
					)
				}
			}
			...
		}
		...
	}

See the documentation for dbsql/errors for more information.

# Retrieving Arrow Batches

The driver supports the ability to retrieve Apache Arrow record batches.
To work with record batches it is necessary to use sql.Conn.Raw() to access the underlying driver connection to retrieve a driver.Rows instance.
The driver exposes two public interfaces for working with record batches from the rows sub-package:

	type Rows interface {
		GetArrowBatches(context.Context) (ArrowBatchIterator, error)
	}

	type ArrowBatchIterator interface {
		// Retrieve the next arrow.Record.
		// Will return io.EOF if there are no more records
		Next() (arrow.Record, error)

		// Return true if the iterator contains more batches, false otherwise.
		HasNext() bool

		// Release any resources in use by the iterator.
		Close()
	}

The driver.Rows instance retrieved using Conn.Raw() can be converted to a Databricks Rows instance via a type assertion, then use GetArrowBatches() to retrieve a batch iterator.
If the ArrowBatchIterator is not closed it will leak resources, such as the underlying connection.
Calling code must call Release() on records returned by DBSQLArrowBatchIterator.Next().

Example usage:

	import (
		...
		dbsqlrows "github.com/databricks/databricks-sql-go/rows"
	)

	func main() {
		...
		db := sql.OpenDB(connector)
		defer db.Close()

		conn, _ := db.Conn(context.BackGround())
		defer conn.Close()

		query := `select * from main.default.taxi_trip_data`

		var rows driver.Rows
		var err error
		err = conn.Raw(func(d interface{}) error {
			rows, err = d.(driver.QueryerContext).QueryContext(ctx, query, nil)
			return err
		})

		if err != nil {
			log.Fatalf("unable to run the query. err: %v", err)
		}
		defer rows.Close()

		batches, err := rows.(dbsqlrows.Rows).GetArrowBatches(context.BackGround())
		if err != nil {
			log.Fatalf("unable to get arrow batches. err: %v", err)
		}

		var iBatch, nRows int
		for batches.HasNext() {
			b, err := batches.Next()
			if err != nil {
				log.Fatalf("Failure retrieving batch. err: %v", err)
			}

			log.Printf("batch %v: nRecords=%v\n", iBatch, b.NumRows())
			iBatch += 1
			nRows += int(b.NumRows())
			b.Release()
		}
		log.Printf("NRows: %v\n", nRows)
	}

# Supported Data Types

==================================

Databricks Type --> Golang Type

==================================

BOOLEAN --> bool

TINYINT --> int8

SMALLINT --> int16

INT --> int32

BIGINT --> int64

FLOAT --> float32

DOUBLE --> float64

VOID --> nil

STRING --> string

DATE --> time.Time

TIMESTAMP --> time.Time

DECIMAL(p,s) --> sql.RawBytes

BINARY --> sql.RawBytes

ARRAY<elementType> --> sql.RawBytes

STRUCT --> sql.RawBytes

MAP<keyType, valueType> --> sql.RawBytes

INTERVAL (year-month) --> string

INTERVAL (day-time) --> string

For ARRAY, STRUCT, and MAP types, sql.Scan can cast sql.RawBytes to JSON string, which can be unmarshalled to Golang
arrays, maps, and structs. For example:

	type structVal struct {
			StringField string `json:"string_field"`
			ArrayField  []int  `json:"array_field"`
	}
	type row struct {
		arrayVal  []int
		mapVal    map[string]int
		structVal structVal
	}
	res := []row{}

	for rows.Next() {
		r := row{}
		tempArray := []byte{}
		tempStruct := []byte{}
		tempMap := []byte{}
		if err := rows.Scan(&tempArray, &tempMap, &tempStruct); err != nil {
			log.Fatal(err)
		}
		if err := json.Unmarshal(tempArray, &r.arrayVal); err != nil {
			log.Fatal(err)
		}
		if err := json.Unmarshal(tempMap, &r.mapVal); err != nil {
			log.Fatal(err)
		}
		if err := json.Unmarshal(tempStruct, &r.structVal); err != nil {
			log.Fatal(err)
		}
		res = append(res, r)
	}

May generate the following row:

	{arrayVal:[1,2,3] mapVal:{"key1":1} structVal:{"string_field":"string_val","array_field":[4,5,6]}}
*/
package dbsql
