package gosnowflake

import "errors"

// QueryStatus denotes the status of a query.
type QueryStatus string

const (
	// QueryStatusInProgress denotes a query execution in progress
	QueryStatusInProgress QueryStatus = "queryStatusInProgress"
	// QueryStatusComplete denotes a completed query execution
	QueryStatusComplete QueryStatus = "queryStatusComplete"
	// QueryFailed denotes a failed query
	QueryFailed QueryStatus = "queryFailed"
)

// SnowflakeResult provides an API for methods exposed to the clients
type SnowflakeResult interface {
	GetQueryID() string
	GetStatus() QueryStatus
	GetArrowBatches() ([]*ArrowBatch, error)
}

type snowflakeResult struct {
	affectedRows int64
	insertID     int64 // Snowflake doesn't support last insert id
	queryID      string
	status       QueryStatus
	err          error
	errChannel   chan error
}

func (res *snowflakeResult) LastInsertId() (int64, error) {
	if err := res.waitForAsyncExecStatus(); err != nil {
		return -1, err
	}
	return res.insertID, nil
}

func (res *snowflakeResult) RowsAffected() (int64, error) {
	if err := res.waitForAsyncExecStatus(); err != nil {
		return -1, err
	}
	return res.affectedRows, nil
}

func (res *snowflakeResult) GetQueryID() string {
	return res.queryID
}

func (res *snowflakeResult) GetStatus() QueryStatus {
	return res.status
}

func (res *snowflakeResult) GetArrowBatches() ([]*ArrowBatch, error) {
	return nil, &SnowflakeError{
		Number:  ErrNotImplemented,
		Message: errMsgNotImplemented,
	}
}

func (res *snowflakeResult) waitForAsyncExecStatus() error {
	// if async exec, block until execution is finished
	switch res.status {
	case QueryStatusInProgress:
		err := <-res.errChannel
		res.status = QueryStatusComplete
		if err != nil {
			res.status = QueryFailed
			res.err = err
			return err
		}
		return nil
	case QueryFailed:
		return res.err
	default:
		return nil
	}
}

type snowflakeResultNoRows struct {
	queryID string
}

func (*snowflakeResultNoRows) LastInsertId() (int64, error) {
	return 0, errors.New("no LastInsertId available")
}

func (*snowflakeResultNoRows) RowsAffected() (int64, error) {
	return 0, errors.New("no RowsAffected available")
}

func (rnr *snowflakeResultNoRows) GetQueryID() string {
	return rnr.queryID
}
