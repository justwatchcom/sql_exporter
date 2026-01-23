package rowscanner

import (
	"context"
	"fmt"
	"io"

	"github.com/databricks/databricks-sql-go/driverctx"
	"github.com/databricks/databricks-sql-go/internal/cli_service"
	dbsqlerrint "github.com/databricks/databricks-sql-go/internal/errors"
	dbsqllog "github.com/databricks/databricks-sql-go/logger"
)

var errRowsResultFetchFailed = "databricks: Rows instance failed to retrieve results"
var ErrRowsFetchPriorToStart = "databricks: unable to fetch row page prior to start of results"
var errRowsNilResultPageFetcher = "databricks: nil ResultPageFetcher instance"

func errRowsUnandledFetchDirection(dir string) string {
	return fmt.Sprintf("databricks: unhandled fetch direction %s", dir)
}

// Interface for iterating over the pages in the result set of a query
type ResultPageIterator interface {
	Next() (*cli_service.TFetchResultsResp, error)
	HasNext() bool
	Close() error
	Delimiter
}

// Define directions for seeking in the pages of a query result
type Direction int

const (
	DirUnknown Direction = iota
	DirNone
	DirForward
	DirBack
)

var directionNames []string = []string{"Unknown", "None", "Forward", "Back"}

func (d Direction) String() string {
	return directionNames[d]
}

// Create a new result page iterator.
func NewResultPageIterator(
	delimiter Delimiter,
	maxPageSize int64,
	opHandle *cli_service.TOperationHandle,
	closedOnServer bool,
	client cli_service.TCLIService,
	connectionId string,
	correlationId string,
	logger *dbsqllog.DBSQLLogger,
) ResultPageIterator {

	// delimiter and hasMoreRows are used to set up the point in the paginated
	// result set that this iterator starts from.
	return &resultPageIterator{
		Delimiter:      delimiter,
		isFinished:     closedOnServer,
		maxPageSize:    maxPageSize,
		opHandle:       opHandle,
		closedOnServer: closedOnServer,
		client:         client,
		connectionId:   connectionId,
		correlationId:  correlationId,
		logger:         logger,
	}
}

type resultPageIterator struct {
	// Gives the parameters of the current result page
	Delimiter

	//	indicates whether there are any more pages in the result set
	isFinished bool

	// max number of rows to fetch in a page
	maxPageSize int64

	// handle of the operation producing the result set
	opHandle *cli_service.TOperationHandle

	// If the server returns an entire result set
	// in the direct results it may have already
	// closed the operation.
	closedOnServer bool

	// client for communicating with the server
	client cli_service.TCLIService

	// connectionId to include in logging messages
	connectionId string

	// user provided value to include in logging messages
	correlationId string

	logger *dbsqllog.DBSQLLogger

	// In some cases we don't know whether there are any records until we fetch
	// the first result page. So our behaviour is to fetch a result page as necessary
	// before Next() is called.
	nextResultPage *cli_service.TFetchResultsResp

	// Hold on to errors so they can be returned by Next()
	err error
}

var _ ResultPageIterator = (*resultPageIterator)(nil)

// Returns true if there are more pages in the result set.
func (rpf *resultPageIterator) HasNext() bool {
	if rpf.isFinished && rpf.nextResultPage == nil {
		// There are no more pages to load and there isn't an already fetched
		// page waiting to retrieved by Next()
		rpf.err = io.EOF
		return false
	}

	// If there isn't an already fetched result page try to fetch one now
	if rpf.nextResultPage == nil {
		nrp, err := rpf.getNextPage()
		if err != nil {
			rpf.Close()
			rpf.isFinished = true
			rpf.err = err
			return false
		}

		rpf.err = nil
		rpf.nextResultPage = nrp
		if !nrp.GetHasMoreRows() {
			rpf.Close()
		}
	}

	return rpf.nextResultPage != nil
}

// Returns the next page of the result set. io.EOF will be returned if there are
// no more pages.
func (rpf *resultPageIterator) Next() (*cli_service.TFetchResultsResp, error) {

	if rpf == nil {
		return nil, dbsqlerrint.NewDriverError(context.Background(), errRowsNilResultPageFetcher, nil)
	}

	if !rpf.HasNext() && rpf.nextResultPage == nil {
		return nil, rpf.err
	}

	nrp := rpf.nextResultPage
	rpf.nextResultPage = nil
	return nrp, rpf.err
}

func (rpf *resultPageIterator) getNextPage() (*cli_service.TFetchResultsResp, error) {
	if rpf.isFinished {
		// no more result pages to fetch
		return nil, io.EOF
	}

	// Starting row number of next result page. This is used to check that the returned page is
	// the expected one.
	nextPageStartRow := rpf.Start() + rpf.Count()

	rpf.logger.Debug().Msgf("databricks: fetching result page for row %d", nextPageStartRow)
	ctx := driverctx.NewContextWithCorrelationId(driverctx.NewContextWithConnId(context.Background(), rpf.connectionId), rpf.correlationId)

	// Keep fetching in the appropriate direction until we have the expected page.
	var fetchResult *cli_service.TFetchResultsResp
	var b bool
	for b = rpf.Contains(nextPageStartRow); !b; b = rpf.Contains(nextPageStartRow) {

		direction := rpf.Direction(nextPageStartRow)
		err := rpf.checkDirectionValid(ctx, direction)
		if err != nil {
			return nil, err
		}

		rpf.logger.Debug().Msgf("fetching next batch of up to %d rows, %s", rpf.maxPageSize, direction.String())

		var includeResultSetMetadata = true
		req := cli_service.TFetchResultsReq{
			OperationHandle:          rpf.opHandle,
			MaxRows:                  rpf.maxPageSize,
			Orientation:              directionToSparkDirection(direction),
			IncludeResultSetMetadata: &includeResultSetMetadata,
		}

		fetchResult, err = rpf.client.FetchResults(ctx, &req)
		if err != nil {
			rpf.logger.Err(err).Msg("databricks: Rows instance failed to retrieve results")
			return nil, dbsqlerrint.NewRequestError(ctx, errRowsResultFetchFailed, err)
		}

		rpf.Delimiter = NewDelimiter(fetchResult.Results.StartRowOffset, CountRows(fetchResult.Results))
		if fetchResult.HasMoreRows != nil {
			rpf.isFinished = !*fetchResult.HasMoreRows
		} else {
			rpf.isFinished = true
		}
		rpf.logger.Debug().Msgf("databricks: new result page startRow: %d, nRows: %v, hasMoreRows: %v", rpf.Start(), rpf.Count(), fetchResult.HasMoreRows)
	}

	return fetchResult, nil
}

func (rpf *resultPageIterator) Close() (err error) {
	// if the operation hasn't already been closed on the server we
	// need to do that now
	if !rpf.closedOnServer {
		rpf.closedOnServer = true
		if rpf.client != nil {
			req := cli_service.TCloseOperationReq{
				OperationHandle: rpf.opHandle,
			}

			_, err = rpf.client.CloseOperation(context.Background(), &req)
			return err
		}
	}

	return
}

// countRows returns the number of rows in the TRowSet
func CountRows(rowSet *cli_service.TRowSet) int64 {
	if rowSet == nil {
		return 0
	}

	if rowSet.ArrowBatches != nil {
		batches := rowSet.ArrowBatches
		var n int64
		for i := range batches {
			n += batches[i].RowCount
		}
		return n
	}

	if rowSet.ResultLinks != nil {
		links := rowSet.ResultLinks
		var n int64
		for i := range links {
			n += links[i].RowCount
		}
		return n
	}

	if rowSet != nil && rowSet.Columns != nil {
		// Find a column/values and return the number of values.
		for _, col := range rowSet.Columns {
			if col.BoolVal != nil {
				return int64(len(col.BoolVal.Values))
			}
			if col.ByteVal != nil {
				return int64(len(col.ByteVal.Values))
			}
			if col.I16Val != nil {
				return int64(len(col.I16Val.Values))
			}
			if col.I32Val != nil {
				return int64(len(col.I32Val.Values))
			}
			if col.I64Val != nil {
				return int64(len(col.I64Val.Values))
			}
			if col.StringVal != nil {
				return int64(len(col.StringVal.Values))
			}
			if col.DoubleVal != nil {
				return int64(len(col.DoubleVal.Values))
			}
			if col.BinaryVal != nil {
				return int64(len(col.BinaryVal.Values))
			}
		}
	}
	return 0
}

// Check if trying to fetch in the specified direction creates an error condition.
func (rpf *resultPageIterator) checkDirectionValid(ctx context.Context, direction Direction) error {
	if direction == DirBack {
		// can't fetch rows previous to the start
		if rpf.Start() == 0 {
			return dbsqlerrint.NewDriverError(ctx, ErrRowsFetchPriorToStart, nil)
		}
	} else if direction == DirForward {
		// can't fetch past the end of the query results
		if rpf.isFinished {
			return io.EOF
		}
	} else {
		rpf.logger.Error().Msgf(errRowsUnandledFetchDirection(direction.String()))
		return dbsqlerrint.NewDriverError(ctx, errRowsUnandledFetchDirection(direction.String()), nil)
	}
	return nil
}

func directionToSparkDirection(d Direction) cli_service.TFetchOrientation {
	switch d {
	case DirBack:
		return cli_service.TFetchOrientation_FETCH_PRIOR
	default:
		return cli_service.TFetchOrientation_FETCH_NEXT
	}
}
