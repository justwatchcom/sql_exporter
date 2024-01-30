[![](https://godoc.org/github.com/segmentio/go-athena?status.svg)](https://godoc.org/github.com/segmentio/go-athena)
# go-athena

> **Note**  
> Segment has paused maintenance on this project, but may return it to an active status in the future. Issues and pull requests from external contributors are not being considered, although internal contributions may appear from time to time. The project remains available under its open source license for anyone to use.

go-athena is a simple Golang [database/sql] driver for [Amazon Athena](https://aws.amazon.com/athena/).

```go
import (
    "database/sql"
    _ "github.com/segmentio/go-athena"
)

func main() {
  db, _ := sql.Open("athena", "db=default&output_location=s3://results")
  rows, _ := db.Query("SELECT url, code from cloudfront")

  for rows.Next() {
    var url string
    var code int
    rows.Scan(&url, &code)
  }
}

```

It provides a higher-level, idiomatic wrapper over the
[AWS Go SDK](https://docs.aws.amazon.com/sdk-for-go/api/service/athena/),
comparable to the [Athena JDBC driver](http://docs.aws.amazon.com/athena/latest/ug/athena-jdbc-driver.html)
AWS provides for Java users.

For example,

- Instead of manually parsing types from strings, you can use [database/sql.Rows.Scan()](https://golang.org/pkg/database/sql/#Rows.Scan)
- Instead of reaching for semaphores, you can use [database/sql.DB.SetMaxOpenConns](https://golang.org/pkg/database/sql/#DB.SetMaxOpenConns)
- And, so on...


## Caveats

[database/sql] exposes lots of methods that aren't supported in Athena.
For example, Athena doesn't support transactions so `Begin()` is irrelevant.
If a method must be supplied to satisfy a standard library interface but is unsupported,
the driver will **panic** indicating so. If there are new offerings in Athena and/or
helpful additions, feel free to PR.


## Testing

Athena doesn't have a local version and revolves around S3 so our tests are
integration tests against AWS itself. Thus, our tests require AWS credentials.
The simplest way to provide them is via `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`
environment variables, but you can use anything supported by the
[Default Credential Provider Chain].

The tests support a few environment variables:
- `ATHENA_DATABASE` can be used to override the default database "go_athena_tests"
- `S3_BUCKET` can be used to override the default S3 bucket of "go-athena-tests"


[database/sql]: https://golang.org/pkg/database/sql/
[Default Credential Provider Chain]: http://docs.aws.amazon.com/sdk-for-java/v1/developer-guide/credentials.html#credentials-default
