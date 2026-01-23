# Release History

## v1.6.1 (2024-08-27)

- Fix CloudFetch "row number N is not contained in any arrow batch" error (databricks/databricks-sql-go#234)

## v1.6.0 (2024-07-31)

- Security: Resolve HIGH vulnerability in x/net (CVE-2023-39325) (databricks/databricks-sql-go#233 by @anthonycrobinson)
- Expose `dbsql.ConnOption` type (databricks/databricks-sql-go#202 by @shelldandy)
- Fix a connection leak in PingContext (databricks/databricks-sql-go#240 by @jackyhu-db)

## v1.5.7 (2024-06-05)

- Reverted dependencies upgrade because of compatibility issues (databricks/databricks-sql-go#228)
- Add more debug logging (databricks/databricks-sql-go#227)

## v1.5.6 (2024-05-28)

- Added connection option `WithSkipTLSHostVerify` (databricks/databricks-sql-go#225 by @jackyhu-db)

## v1.5.5 (2024-04-16)

- Fix: handle `nil` values passed as query parameter (databricks/databricks-sql-go#199 by @esdrasbeleza)
- Fix: provide content length on staging file put (databricks/databricks-sql-go#217 by @candiduslynx)
- Fix formatting of *float64 parameters (databricks/databricks-sql-go#215 by @esdrasbeleza)
- Fix: use correct tenant ID for different Azure domains (databricks/databricks-sql-go#210 by @tubiskasaroos)

## v1.5.4 (2024-04-10)

- Added OAuth support for GCP (databricks/databricks-sql-go#189 by @rcypher-databricks)
- Staging operations: stream files instead of loading into memory (databricks/databricks-sql-go#197 by @mdibaiee)
- Staging operations: don't panic on REMOVE (databricks/databricks-sql-go#205 by @candiduslynx)
- Fix formatting of Date/Time query parameters (databricks/databricks-sql-go#207 by @candiduslynx)

## v1.5.3 (2024-01-17)
- Bug fix for ArrowBatchIterator.HasNext(). Incorrectly returned true for result sets with zero rows.

## v1.5.2 (2023-11-17)
- Added .us domain to inference list for AWS OAuth
- Bug fix for OAuth m2m scopes, updated m2m authenticator to use "all-apis" scope.

## v1.5.1 (2023-10-17)
- Logging improvements
- Added handling for staging remove

## v1.5.0 (2023-10-02)
- Named parameter support
- Better handling of bad connection errors and specifying server protocol
- OAuth implementation
- Expose Arrow batches to users
- Add support for staging operations

## v1.4.0 (2023-08-09)
- Improve error information when query terminates in unexpected state
- Do not override global logger time format
- Enable Transport configuration for http client
- fix: update arrow to v12
- Updated doc.go for retrieving query id and connection id
- Bug fix issue 147: BUG with reading table that contains copied map
- Allow WithServerHostname to specify protocol

## v1.3.1 (2023-06-23)

- bug fix for panic when executing non record producing statements using DB.Query()/DB.QueryExec()

## v1.3.0 (2023-06-07)

- allow client provided authenticator
- more robust retry behaviour
- bug fix for null values in complex types

## v1.2.0 (2023-04-20)

- Improved error types and info

## v1.1.0 (2023-03-06)

- Feat: Support ability to retry on specific failures
- Fetch results in arrow format 
- Improve error message and retry behaviour

## v1.0.1 (2023-01-05)

Fixing cancel race condition 

## v1.0.0 (2022-12-20)

- Package doc (doc.go)
- Handle FLOAT values as float32
- Fix for result.AffectedRows
- Use new ctx when closing operation after cancel 
- Set default port to 443 

## v1.0.0-rc.1 (2022-12-19)

- Package doc (doc.go)
- Handle FLOAT values as float32
- Fix for result.AffectedRows
- Add or edit documentation above methods
- Tweaks to readme 
- Use new ctx when closing operation after cancel

## 0.2.2 (2022-12-12)

- Handle parsing negative years in dates
- fix thread safety issue 

## 0.2.1 (2022-12-05)

- Don't ignore error in InitThriftClient 
- Close optimization for Rows 
- Close operation after executing statement
- Minor change to examples
- P&R improvements 

## 0.1.x (Unreleased)

- Fix thread safety issue in connector

## 0.2.0 (2022-11-18)

- Support for DirectResults
- Support for context cancellation and timeout
- Session parameters (e.g.: timezone)
- Thrift Protocol update
- Several logging improvements
- Added better examples. See [workflow](https://github.com/databricks/databricks-sql-go/blob/main/examples/workflow/main.go)
- Added dbsql.NewConnector() function to help initialize DB
- Many other small improvements and bug fixes
- Removed support for client-side query parameterization
- Removed need to start DSN with "databricks://"

## 0.1.4 (2022-07-30)

- Fix: Could not fetch rowsets greater than the value of `maxRows` (#18)
- Updated default user agent
- Updated README and CONTRIBUTING

## 0.1.3 (2022-06-16)

- Add escaping of string parameters.

## 0.1.2 (2022-06-10)

- Fix timeout units to be milliseconds instead of nanos.

## 0.1.1 (2022-05-19)

- Fix module name

## 0.1.0 (2022-05-19)

- Initial release
