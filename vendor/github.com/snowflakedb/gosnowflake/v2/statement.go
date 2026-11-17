package gosnowflake

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"
)

// SnowflakeStmt represents the prepared statement in driver.
type SnowflakeStmt interface {
	GetQueryID() string
}

type snowflakeStmt struct {
	sc          *snowflakeConn
	query       string
	lastQueryID string
}

func (stmt *snowflakeStmt) Close() error {
	logger.WithContext(stmt.sc.ctx).Info("Stmt.Close")
	// noop
	return nil
}

func (stmt *snowflakeStmt) NumInput() int {
	logger.WithContext(stmt.sc.ctx).Info("Stmt.NumInput")
	// Go Snowflake doesn't know the number of binding parameters.
	return -1
}

func (stmt *snowflakeStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	logger.WithContext(stmt.sc.ctx).Info("Stmt.ExecContext")
	return stmt.execInternal(ctx, args)
}

func (stmt *snowflakeStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	logger.WithContext(stmt.sc.ctx).Info("Stmt.QueryContext")
	rows, err := stmt.sc.QueryContext(ctx, stmt.query, args)
	if err != nil {
		stmt.setQueryIDFromError(err)
		return nil, err
	}
	r, ok := rows.(SnowflakeRows)
	if !ok {
		return nil, fmt.Errorf("interface convertion. expected type SnowflakeRows but got %T", rows)
	}
	stmt.lastQueryID = r.GetQueryID()
	return rows, nil
}

func (stmt *snowflakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	logger.WithContext(stmt.sc.ctx).Info("Stmt.Exec")
	return stmt.execInternal(context.Background(), toNamedValues(args))
}

func (stmt *snowflakeStmt) execInternal(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	logger.WithContext(stmt.sc.ctx).Debug("Stmt.execInternal")
	if ctx == nil {
		ctx = context.Background()
	}
	stmtCtx := context.WithValue(ctx, executionType, executionTypeStatement)
	timer := time.Now()
	result, err := stmt.sc.ExecContext(stmtCtx, stmt.query, args)
	if err != nil {
		stmt.setQueryIDFromError(err)
		logger.WithContext(ctx).Errorf("QueryID: %v failed to execute because of the error %v. It took %v ms.", stmt.lastQueryID, err, time.Since(timer).String())
		return nil, err
	}
	rnr, ok := result.(*snowflakeResultNoRows)
	if ok {
		stmt.lastQueryID = rnr.GetQueryID()
		logger.WithContext(ctx).Debugf("Query ID: %v has no result. It took %v ms.,", stmt.lastQueryID, time.Since(timer).String())
		return driver.ResultNoRows, nil
	}
	r, ok := result.(SnowflakeResult)
	if !ok {
		return nil, fmt.Errorf("interface convertion. expected type SnowflakeResult but got %T", result)
	}
	stmt.lastQueryID = r.GetQueryID()
	logger.WithContext(ctx).Debugf("Query ID: %v has no result. It took %v ms.,", stmt.lastQueryID, time.Since(timer).String())

	return result, err
}

func (stmt *snowflakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	logger.WithContext(stmt.sc.ctx).Info("Stmt.Query")
	timer := time.Now()
	rows, err := stmt.sc.Query(stmt.query, args)
	if err != nil {
		logger.WithContext(stmt.sc.ctx).Errorf("QueryID: %v failed to execute because of the error %v. It took %v ms.", stmt.lastQueryID, err, time.Since(timer).String())
		stmt.setQueryIDFromError(err)
		return nil, err
	}
	r, ok := rows.(SnowflakeRows)
	if !ok {
		logger.WithContext(stmt.sc.ctx).Errorf("Query ID: %v failed to convert the rows to SnowflakeRows. It took %v ms.,", stmt.lastQueryID, time.Since(timer).String())
		return nil, fmt.Errorf("interface convertion. expected type SnowflakeRows but got %T", rows)
	}
	stmt.lastQueryID = r.GetQueryID()
	logger.WithContext(stmt.sc.ctx).Debugf("Query ID: %v has no result. It took %v ms.,", stmt.lastQueryID, time.Since(timer).String())
	return rows, err
}

func (stmt *snowflakeStmt) GetQueryID() string {
	return stmt.lastQueryID
}

func (stmt *snowflakeStmt) setQueryIDFromError(err error) {
	var snowflakeError *SnowflakeError
	if errors.As(err, &snowflakeError) {
		stmt.lastQueryID = snowflakeError.QueryID
	}
}
