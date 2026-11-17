## Migrating to v2

**Version 2.0.0 of the Go Snowflake Driver was released on March 3rd, 2026.** This major version includes breaking changes that require code updates when migrating from v1.x.

### Key Changes and Migration Steps

#### 1. Update Import Paths

Update your `go.mod` to use v2:

```sh
go get -u github.com/snowflakedb/gosnowflake/v2
```

Update imports in your code:

```go
// Old (v1)
import "github.com/snowflakedb/gosnowflake"

// New (v2)
import "github.com/snowflakedb/gosnowflake/v2"
```

#### 2. Arrow Batches Moved to Separate Package

The public Arrow batches API now lives in `github.com/snowflakedb/gosnowflake/v2/arrowbatches`.
Importing that sub-package pulls in the additional Arrow compute dependency only for applications
that use Arrow batches directly.

**Migration:**

```go
import (
    "context"
    "database/sql/driver"

    sf "github.com/snowflakedb/gosnowflake/v2"
    "github.com/snowflakedb/gosnowflake/v2/arrowbatches"
)

ctx := arrowbatches.WithArrowBatches(context.Background())

var rows driver.Rows
err := conn.Raw(func(x any) error {
    rows, err = x.(driver.QueryerContext).QueryContext(ctx, query, nil)
    return err
})
if err != nil {
    // handle error
}

batches, err := arrowbatches.GetArrowBatches(rows.(sf.SnowflakeRows))
if err != nil {
    // handle error
}
```

**Optional helper mapping:**
- `sf.WithArrowBatchesTimestampOption` → `arrowbatches.WithTimestampOption`
- `sf.WithArrowBatchesUtf8Validation` → `arrowbatches.WithUtf8Validation`
- `sf.ArrowSnowflakeTimestampToTime` → `arrowbatches.ArrowSnowflakeTimestampToTime`
- `sf.WithOriginalTimestamp` → `arrowbatches.WithTimestampOption(ctx, arrowbatches.UseOriginalTimestamp)`

#### 3. Configuration Struct Changes

**Renamed fields:**
```go
// Old (v1)
config := &gosnowflake.Config{
    KeepSessionAlive: true,
    InsecureMode: true,
    DisableTelemetry: true,
}

// New (v2)
config := &gosnowflake.Config{
    ServerSessionKeepAlive: true,  // Renamed for consistency with other drivers
    DisableOCSPChecks: true,        // Replaces InsecureMode
    // DisableTelemetry removed - use CLIENT_TELEMETRY_ENABLED session parameter
}
```

**Removed fields:**
- `ClientIP` - No longer used
- `MfaToken` and `IdToken` - Now unexported
- `DisableTelemetry` - Use `CLIENT_TELEMETRY_ENABLED` session parameter instead

#### 4. Logger Changes

The built-in logger is now based on Go's standard `log/slog`:

```go
logger := gosnowflake.GetLogger()
_ = logger.SetLogLevel("debug")
```

For custom logging, continue implementing `SFLogger`.
If you want to customize the built-in slog handler, type-assert `GetLogger()` to `SFSlogLogger`
and call `SetHandler`.

#### 5. File Transfer Changes

**Configuration options:**

```go
// Old (v1)
options := &gosnowflake.SnowflakeFileTransferOptions{
    RaisePutGetError: true,
    GetFileToStream: true,
}
ctx = gosnowflake.WithFileStream(ctx, stream)

// New (v2)
// RaisePutGetError removed - errors always raised
// GetFileToStream removed - use WithFileGetStream instead
ctx = gosnowflake.WithFilePutStream(ctx, stream)  // Renamed from WithFileStream
ctx = gosnowflake.WithFileGetStream(ctx, stream)  // For GET operations
```

#### 6. Context and Function Changes

```go
// Old (v1)
ctx, err := gosnowflake.WithMultiStatement(ctx, 0)
if err != nil {
    // handle error
}

// New (v2)
ctx = gosnowflake.WithMultiStatement(ctx, 0)  // No error returned
```

```go
// Old (v1)
values := gosnowflake.Array(data)

// New (v2)
values, err := gosnowflake.Array(data)  // Now returns error for unsupported types
if err != nil {
    // handle error
}
```

#### 7. Nullable Options Combined

```go
// Old (v1)
ctx = gosnowflake.WithMapValuesNullable(ctx)
ctx = gosnowflake.WithArrayValuesNullable(ctx)

// New (v2)
ctx = gosnowflake.WithEmbeddedValuesNullable(ctx)  // Handles both maps and arrays
```

#### 8. Session Parameter Changes

**Chunk download workers:**

```go
// Old (v1)
gosnowflake.MaxChunkDownloadWorkers = 10  // Global variable

// New (v2)
// Configure via CLIENT_PREFETCH_THREADS session parameter.
// NOTE: The default is 4.
db.Exec("ALTER SESSION SET CLIENT_PREFETCH_THREADS = 10")
```

#### 9. Transport Configuration

```go
import "crypto/tls"

// Old (v1)
gosnowflake.SnowflakeTransport = yourTransport

// New (v2)
config := &gosnowflake.Config{
    Transporter: yourCustomTransport,
}

// Or, if you only need custom TLS settings/certificates:
tlsConfig := &tls.Config{
    // ...
}
_ = gosnowflake.RegisterTLSConfig("custom", tlsConfig)
config.TLSConfigName = "custom"
```

#### 10. Environment Variable Fix

If you use the skip registration environment variable:

```sh
# Old (v1)
GOSNOWFLAKE_SKIP_REGISTERATION=true  # Note the typo

# New (v2)
GOSNOWFLAKE_SKIP_REGISTRATION=true  # Typo fixed
```

### Additional Resources

- Full list of changes: See [CHANGELOG.md](./CHANGELOG.md)
- Questions or issues: [GitHub Issues](https://github.com/snowflakedb/gosnowflake/issues)


## Support

For official support and urgent, production-impacting issues, please [contact Snowflake Support](https://community.snowflake.com/s/article/How-To-Submit-a-Support-Case-in-Snowflake-Lodge).

# Go Snowflake Driver

<a href="https://codecov.io/github/snowflakedb/gosnowflake?branch=master">
    <img alt="Coverage" src="https://codecov.io/github/snowflakedb/gosnowflake/coverage.svg?branch=master">
</a>
<a href="https://github.com/snowflakedb/gosnowflake/actions?query=workflow%3A%22Build+and+Test%22">
    <img src="https://github.com/snowflakedb/gosnowflake/workflows/Build%20and%20Test/badge.svg?branch=master">
</a>
<a href="http://www.apache.org/licenses/LICENSE-2.0.txt">
    <img src="http://img.shields.io/:license-Apache%202-brightgreen.svg">
</a>
<a href="https://goreportcard.com/report/github.com/snowflakedb/gosnowflake">
    <img src="https://goreportcard.com/badge/github.com/snowflakedb/gosnowflake">
</a>

This topic provides instructions for installing, running, and modifying the Go Snowflake Driver. The driver supports Go's [database/sql](https://golang.org/pkg/database/sql/) package.

# Prerequisites

The following software packages are required to use the Go Snowflake Driver.

## Go

The latest driver requires the [Go language](https://golang.org/) 1.25 or higher. The supported operating systems are 64-bits Linux, Mac OS, and Windows, but you may run the driver on other platforms if the Go language works correctly on those platforms.

# Installation

If you don't have a project initialized, set it up.

```sh
go mod init example.com/snowflake
```

Get Gosnowflake source code, if not installed.

```sh
go get -u github.com/snowflakedb/gosnowflake/v2
```

# Docs

For detailed documentation and basic usage examples, please see the documentation at
[godoc.org](https://godoc.org/github.com/snowflakedb/gosnowflake/v2).

## Notes

This driver currently does not support GCP regional endpoints. Please ensure that any workloads using through this driver do not require support for regional endpoints on GCP. If you have questions about this, please contact Snowflake Support.

The driver uses Rust library called sf_mini_core, you can find its source code [here](https://github.com/snowflakedb/universal-driver/tree/main/sf_mini_core)

# Sample Programs

Snowflake provides a set of sample programs to test with. Set the environment variable ``$GOPATH`` to the top directory of your workspace, e.g., ``~/go`` and make certain to
include ``$GOPATH/bin`` in the environment variable ``$PATH``. Run the ``make`` command to build all sample programs.

```sh
make install
```

In the following example, the program ``select1.go`` is built and installed in ``$GOPATH/bin`` and can be run from the command line:

```sh
SNOWFLAKE_TEST_ACCOUNT=<your_account> \
SNOWFLAKE_TEST_USER=<your_user> \
SNOWFLAKE_TEST_PASSWORD=<your_password> \
select1
Congrats! You have successfully run SELECT 1 with Snowflake DB!
```

# Development

The developer notes are hosted with the source code on [GitHub](https://github.com/snowflakedb/gosnowflake/v2).

## Testing Code


Set the Snowflake connection info in ``parameters.json``:

```json
{
    "testconnection": {
        "SNOWFLAKE_TEST_USER":      "<your_user>",
        "SNOWFLAKE_TEST_PASSWORD":  "<your_password>",
        "SNOWFLAKE_TEST_ACCOUNT":   "<your_account>",
        "SNOWFLAKE_TEST_WAREHOUSE": "<your_warehouse>",
        "SNOWFLAKE_TEST_DATABASE":  "<your_database>",
        "SNOWFLAKE_TEST_SCHEMA":    "<your_schema>",
        "SNOWFLAKE_TEST_ROLE":      "<your_role>",
        "SNOWFLAKE_TEST_DEBUG":     "false"
    }
}
```

Install [jq](https://stedolan.github.io/jq) so that the parameters can get parsed correctly, and run ``make test`` in your Go development environment:

```sh
make test
```

### Setting debug mode during tests
This is for debugging Large SQL statements (greater than 300 characters). If you want to enable debug mode, set `SNOWFLAKE_TEST_DEBUG` to `true` in `parameters.json`, or export it in your shell instance.

## customizing Logging Tags

If you would like to ensure that certain tags are always present in the logs, `RegisterClientLogContextHook` can be used in your init function. See example below.
```go
import "github.com/snowflakedb/gosnowflake/v2"

func init() {
    // each time the logger is used, the logs will contain a REQUEST_ID field with requestID the value extracted 
    // from the context
	gosnowflake.RegisterClientLogContextHook("REQUEST_ID", func(ctx context.Context) interface{} {
		return requestIdFromContext(ctx)
	})
}
```

## Setting Log Level
If you want to change the log level, `SetLogLevel` can be used in your init function like this:
```go
import "github.com/snowflakedb/gosnowflake/v2"

func init() {
    // The following line changes the log level to debug
	_ = gosnowflake.GetLogger().SetLogLevel("debug")
}
```
The following is a list of options you can pass in to set the level from least to most verbose:
- `"OFF"`
- `"fatal"`
- `"error"`
- `"warn"`
- `"info"`
- `"debug"`
- `"trace"`


## Capturing Code Coverage

Configure your testing environment as described above and run ``make cov``. The coverage percentage will be printed on the console when the testing completes.

```sh
make cov
```

For more detailed analysis, results are printed to ``coverage.txt`` in the project directory.

To read the coverage report, run:

```sh
go tool cover -html=coverage.txt
```

## Submitting Pull Requests

You may use your preferred editor to edit the driver code. Make certain to run ``make fmt lint`` before submitting any pull request to Snowflake. This command formats your source code according to the standard Go style and detects any coding style issues.
