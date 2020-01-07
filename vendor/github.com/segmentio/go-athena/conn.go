package athena

import (
	"context"
	"database/sql/driver"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/athena"
	"github.com/aws/aws-sdk-go/service/athena/athenaiface"
)

type conn struct {
	athena         athenaiface.AthenaAPI
	db             string
	OutputLocation string

	pollFrequency time.Duration
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if len(args) > 0 {
		panic("Athena doesn't support prepared statements. Format your own arguments.")
	}

	rows, err := c.runQuery(ctx, query)
	return rows, err
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if len(args) > 0 {
		panic("Athena doesn't support prepared statements. Format your own arguments.")
	}

	_, err := c.runQuery(ctx, query)
	return nil, err
}

func (c *conn) runQuery(ctx context.Context, query string) (driver.Rows, error) {
	queryID, err := c.startQuery(query)
	if err != nil {
		return nil, err
	}

	if err := c.waitOnQuery(ctx, queryID); err != nil {
		return nil, err
	}

	return newRows(rowsConfig{
		Athena:  c.athena,
		QueryID: queryID,
		// todo add check for ddl queries to not skip header(#10)
		SkipHeader: true,
	})
}

// startQuery starts an Athena query and returns its ID.
func (c *conn) startQuery(query string) (string, error) {
	resp, err := c.athena.StartQueryExecution(&athena.StartQueryExecutionInput{
		QueryString: aws.String(query),
		QueryExecutionContext: &athena.QueryExecutionContext{
			Database: aws.String(c.db),
		},
		ResultConfiguration: &athena.ResultConfiguration{
			OutputLocation: aws.String(c.OutputLocation),
		},
	})
	if err != nil {
		return "", err
	}

	return *resp.QueryExecutionId, nil
}

// waitOnQuery blocks until a query finishes, returning an error if it failed.
func (c *conn) waitOnQuery(ctx context.Context, queryID string) error {
	for {
		statusResp, err := c.athena.GetQueryExecutionWithContext(ctx, &athena.GetQueryExecutionInput{
			QueryExecutionId: aws.String(queryID),
		})
		if err != nil {
			return err
		}

		switch *statusResp.QueryExecution.Status.State {
		case athena.QueryExecutionStateCancelled:
			return context.Canceled
		case athena.QueryExecutionStateFailed:
			reason := *statusResp.QueryExecution.Status.StateChangeReason
			return errors.New(reason)
		case athena.QueryExecutionStateSucceeded:
			return nil
		case athena.QueryExecutionStateQueued:
		case athena.QueryExecutionStateRunning:
		}

		select {
		case <-ctx.Done():
			c.athena.StopQueryExecution(&athena.StopQueryExecutionInput{
				QueryExecutionId: aws.String(queryID),
			})

			return ctx.Err()
		case <-time.After(c.pollFrequency):
			continue
		}
	}
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	panic("Athena doesn't support prepared statements")
}

func (c *conn) Begin() (driver.Tx, error) {
	panic("Athena doesn't support transactions")
}

func (c *conn) Close() error {
	return nil
}

var _ driver.QueryerContext = (*conn)(nil)
var _ driver.ExecerContext = (*conn)(nil)

// HACK(tejasmanohar): database/sql calls Prepare() if your driver doesn't implement
// Queryer. Regardless, db.Query/Exec* calls Query/Exec-Context so I've filed a bug--
// https://github.com/golang/go/issues/22980.
func (c *conn) Query(query string, args []driver.Value) (driver.Rows, error) {
	panic("Query() is noop")
}

func (c *conn) Exec(query string, args []driver.Value) (driver.Result, error) {
	panic("Exec() is noop")
}

var _ driver.Queryer = (*conn)(nil)
var _ driver.Execer = (*conn)(nil)
