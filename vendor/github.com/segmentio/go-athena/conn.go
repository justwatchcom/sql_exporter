package athena

import (
	"context"
	"database/sql/driver"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	"github.com/aws/aws-sdk-go-v2/service/athena/types"
)

type conn struct {
	athena         athenaAPI
	db             string
	OutputLocation string

	pollFrequency time.Duration
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if len(args) > 0 {
		panic("The go-athena driver doesn't support prepared statements yet. Format your own arguments.")
	}

	rows, err := c.runQuery(ctx, query)
	return rows, err
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if len(args) > 0 {
		panic("The go-athena driver doesn't support prepared statements yet. Format your own arguments.")
	}

	_, err := c.runQuery(ctx, query)
	return nil, err
}

func (c *conn) runQuery(ctx context.Context, query string) (driver.Rows, error) {
	queryID, err := c.startQuery(ctx, query)
	if err != nil {
		return nil, err
	}

	if err := c.waitOnQuery(ctx, queryID); err != nil {
		return nil, err
	}

	return newRows(ctx, rowsConfig{
		Athena:  c.athena,
		QueryID: queryID,
		// todo add check for ddl queries to not skip header(#10)
		SkipHeader: true,
	})
}

// startQuery starts an Athena query and returns its ID.
func (c *conn) startQuery(ctx context.Context, query string) (string, error) {
	resp, err := c.athena.StartQueryExecution(ctx, &athena.StartQueryExecutionInput{
		QueryString: aws.String(query),
		QueryExecutionContext: &types.QueryExecutionContext{
			Database: aws.String(c.db),
		},
		ResultConfiguration: &types.ResultConfiguration{
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
		statusResp, err := c.athena.GetQueryExecution(ctx, &athena.GetQueryExecutionInput{
			QueryExecutionId: aws.String(queryID),
		})
		if err != nil {
			return err
		}

		switch statusResp.QueryExecution.Status.State {
		case types.QueryExecutionStateCancelled:
			return context.Canceled
		case types.QueryExecutionStateFailed:
			reason := *statusResp.QueryExecution.Status.StateChangeReason
			return errors.New(reason)
		case types.QueryExecutionStateSucceeded:
			return nil
		case types.QueryExecutionStateQueued:
		case types.QueryExecutionStateRunning:
		}

		select {
		case <-ctx.Done():
			c.athena.StopQueryExecution(ctx, &athena.StopQueryExecutionInput{
				QueryExecutionId: aws.String(queryID),
			})

			return ctx.Err()
		case <-time.After(c.pollFrequency):
			continue
		}
	}
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	panic("The go-athena driver doesn't support prepared statements yet")
}

func (c *conn) Begin() (driver.Tx, error) {
	panic("Athena doesn't support transactions")
}

func (c *conn) Close() error {
	return nil
}

var _ driver.QueryerContext = (*conn)(nil)
var _ driver.ExecerContext = (*conn)(nil)
