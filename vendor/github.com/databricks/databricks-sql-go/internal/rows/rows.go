package rows

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"math"
	"reflect"
	"time"

	"github.com/databricks/databricks-sql-go/driverctx"
	dbsqlerr "github.com/databricks/databricks-sql-go/errors"
	"github.com/databricks/databricks-sql-go/internal/cli_service"
	dbsqlclient "github.com/databricks/databricks-sql-go/internal/client"
	"github.com/databricks/databricks-sql-go/internal/config"
	dbsqlerr_int "github.com/databricks/databricks-sql-go/internal/errors"
	"github.com/databricks/databricks-sql-go/internal/rows/arrowbased"
	"github.com/databricks/databricks-sql-go/internal/rows/columnbased"
	"github.com/databricks/databricks-sql-go/internal/rows/rowscanner"
	dbsqllog "github.com/databricks/databricks-sql-go/logger"
	dbsqlrows "github.com/databricks/databricks-sql-go/rows"
)

// rows implements the following interfaces from database.sql.driver
// Rows
// RowsColumnTypeScanType
// RowsColumnTypeDatabaseTypeName
// RowsColumnTypeNullable
// RowsColumnTypeLength
type rows struct {
	// The RowScanner is responsible for handling the different
	// formats in which the query results can be returned
	rowscanner.RowScanner
	rowscanner.ResultPageIterator

	// Handle for the associated database operation.
	opHandle *cli_service.TOperationHandle

	client cli_service.TCLIService

	location *time.Location

	// Metadata for result set
	resultSetMetadata *cli_service.TGetResultSetMetadataResp
	schema            *cli_service.TTableSchema

	config *config.Config

	// connId and correlationId are used for creating a context
	// when accessing the server and when logging
	connId        string
	correlationId string

	// Row number within the overall result set
	nextRowNumber int64

	logger_ *dbsqllog.DBSQLLogger

	ctx context.Context
}

var _ driver.Rows = (*rows)(nil)
var _ driver.RowsColumnTypeScanType = (*rows)(nil)
var _ driver.RowsColumnTypeDatabaseTypeName = (*rows)(nil)
var _ driver.RowsColumnTypeNullable = (*rows)(nil)
var _ driver.RowsColumnTypeLength = (*rows)(nil)
var _ dbsqlrows.Rows = (*rows)(nil)

func NewRows(
	connId string,
	correlationId string,
	opHandle *cli_service.TOperationHandle,
	client cli_service.TCLIService,
	config *config.Config,
	directResults *cli_service.TSparkDirectResults,
) (driver.Rows, dbsqlerr.DBError) {

	var logger *dbsqllog.DBSQLLogger
	var ctx context.Context
	if opHandle != nil {
		logger = dbsqllog.WithContext(connId, correlationId, dbsqlclient.SprintGuid(opHandle.OperationId.GUID))
		ctx = driverctx.NewContextWithQueryId(driverctx.NewContextWithCorrelationId(driverctx.NewContextWithConnId(context.Background(), connId), correlationId), dbsqlclient.SprintGuid(opHandle.OperationId.GUID))
	} else {
		logger = dbsqllog.WithContext(connId, correlationId, "")
		ctx = driverctx.NewContextWithCorrelationId(driverctx.NewContextWithConnId(context.Background(), connId), correlationId)
	}

	if client == nil {
		logger.Error().Msg(errRowsNoClient)
		return nil, dbsqlerr_int.NewDriverError(ctx, errRowsNoClient, nil)
	}

	var pageSize int64 = 10000
	var location *time.Location = time.UTC
	if config != nil {
		pageSize = int64(config.MaxRows)

		if config.Location != nil {
			location = config.Location
		}
	}

	logger.Debug().Msgf("databricks: creating Rows, pageSize: %d, location: %v", pageSize, location)

	r := &rows{
		client:        client,
		opHandle:      opHandle,
		connId:        connId,
		correlationId: correlationId,
		location:      location,
		config:        config,
		logger_:       logger,
		ctx:           ctx,
	}

	// if we already have results for the query do some additional initialization
	if directResults != nil {
		logger.Debug().Msgf("databricks: creating Rows with direct results")
		// set the result set metadata
		if directResults.ResultSetMetadata != nil {
			r.resultSetMetadata = directResults.ResultSetMetadata
			r.schema = directResults.ResultSetMetadata.Schema
		}

		// initialize the row scanner
		err := r.makeRowScanner(directResults.ResultSet)
		if err != nil {
			return r, err
		}
	}

	var d rowscanner.Delimiter
	if r.RowScanner != nil {
		d = rowscanner.NewDelimiter(r.RowScanner.Start(), r.RowScanner.Count())
	} else {
		d = rowscanner.NewDelimiter(0, 0)
	}

	// If the entire query result set fits in direct results the server closes
	// the operations.
	closedOnServer := directResults != nil && directResults.CloseOperation != nil
	r.ResultPageIterator = rowscanner.NewResultPageIterator(
		d,
		pageSize,
		opHandle,
		closedOnServer,
		client,
		connId,
		correlationId,
		r.logger(),
	)

	return r, nil
}

// Columns returns the names of the columns. The number of
// columns of the result is inferred from the length of the
// slice. If a particular column name isn't known, an empty
// string should be returned for that entry.
func (r *rows) Columns() []string {
	err := isValidRows(r)
	if err != nil {
		return []string{}
	}

	schema, err := r.getResultSetSchema()
	if err != nil {
		return []string{}
	}

	tColumns := schema.GetColumns()
	colNames := make([]string, len(tColumns))

	for i := range tColumns {
		colNames[i] = tColumns[i].ColumnName
	}

	return colNames
}

// Close closes the rows iterator.
func (r *rows) Close() error {
	if r == nil {
		return nil
	}

	if r.RowScanner != nil {
		// make sure the row scanner frees up any resources
		r.RowScanner.Close()
	}

	if r.ResultPageIterator != nil {
		r.logger().Debug().Msgf("databricks: closing Rows operation")
		err := r.ResultPageIterator.Close()
		if err != nil {
			r.logger().Err(err).Msg(errRowsCloseFailed)
			return dbsqlerr_int.NewRequestError(r.ctx, errRowsCloseFailed, err)
		}
	}

	return nil
}

// Next is called to populate the next row of data into
// the provided slice. The provided slice will be the same
// size as the number of columns.
//
// Next should return io.EOF when there are no more rows.
//
// The dest should not be written to outside of Next. Care
// should be taken when closing Rows not to modify
// a buffer held in dest.
func (r *rows) Next(dest []driver.Value) error {
	err := isValidRows(r)
	if err != nil {
		return err
	}

	// if the next row is not in the current result page
	// fetch the containing page
	var b bool
	var e error
	if b, e = r.isNextRowInPage(); !b && e == nil {
		err := r.fetchResultPage()
		if err != nil {
			return err
		}
	}

	if e != nil {
		return e
	}

	// Put values into the destination slice
	err = r.ScanRow(dest, r.nextRowNumber)
	if err != nil {
		return err
	}

	r.nextRowNumber++

	return nil
}

// ColumnTypeScanType returns column's native type
func (r *rows) ColumnTypeScanType(index int) reflect.Type {
	err := isValidRows(r)
	if err != nil {
		return nil
	}

	column, err := r.getColumnMetadataByIndex(index)
	if err != nil {
		return nil
	}

	scanType := getScanType(column)
	return scanType
}

// ColumnTypeDatabaseTypeName returns column's database type name
func (r *rows) ColumnTypeDatabaseTypeName(index int) string {
	err := isValidRows(r)
	if err != nil {
		return ""
	}

	column, err := r.getColumnMetadataByIndex(index)
	if err != nil {
		return ""
	}

	dbtype := rowscanner.GetDBTypeName(column)

	return dbtype
}

// ColumnTypeNullable returns a flag indicating whether the column is nullable
// and an ok value of true if the status of the column is known. Otherwise
// a value of false is returned for ok.
func (r *rows) ColumnTypeNullable(index int) (nullable, ok bool) {
	return false, false
}

func (r *rows) ColumnTypeLength(index int) (length int64, ok bool) {
	columnInfo, err := r.getColumnMetadataByIndex(index)
	if err != nil {
		return 0, false
	}

	typeName := rowscanner.GetDBTypeID(columnInfo)

	switch typeName {
	case cli_service.TTypeId_STRING_TYPE,
		cli_service.TTypeId_VARCHAR_TYPE,
		cli_service.TTypeId_BINARY_TYPE,
		cli_service.TTypeId_ARRAY_TYPE,
		cli_service.TTypeId_MAP_TYPE,
		cli_service.TTypeId_STRUCT_TYPE:
		return math.MaxInt64, true
	default:
		return 0, false
	}
}

var (
	scanTypeNull     = reflect.TypeOf(nil)
	scanTypeBoolean  = reflect.TypeOf(true)
	scanTypeFloat32  = reflect.TypeOf(float32(0))
	scanTypeFloat64  = reflect.TypeOf(float64(0))
	scanTypeInt8     = reflect.TypeOf(int8(0))
	scanTypeInt16    = reflect.TypeOf(int16(0))
	scanTypeInt32    = reflect.TypeOf(int32(0))
	scanTypeInt64    = reflect.TypeOf(int64(0))
	scanTypeString   = reflect.TypeOf("")
	scanTypeDateTime = reflect.TypeOf(time.Time{})
	scanTypeRawBytes = reflect.TypeOf(sql.RawBytes{})
	scanTypeUnknown  = reflect.TypeOf(new(any))
)

func getScanType(column *cli_service.TColumnDesc) reflect.Type {

	// Currently all types are returned from the thrift server using
	// the primitive entry
	entry := column.TypeDesc.Types[0].PrimitiveEntry

	switch entry.Type {
	case cli_service.TTypeId_BOOLEAN_TYPE:
		return scanTypeBoolean
	case cli_service.TTypeId_TINYINT_TYPE:
		return scanTypeInt8
	case cli_service.TTypeId_SMALLINT_TYPE:
		return scanTypeInt16
	case cli_service.TTypeId_INT_TYPE:
		return scanTypeInt32
	case cli_service.TTypeId_BIGINT_TYPE:
		return scanTypeInt64
	case cli_service.TTypeId_FLOAT_TYPE:
		return scanTypeFloat32
	case cli_service.TTypeId_DOUBLE_TYPE:
		return scanTypeFloat64
	case cli_service.TTypeId_NULL_TYPE:
		return scanTypeNull
	case cli_service.TTypeId_STRING_TYPE:
		return scanTypeString
	case cli_service.TTypeId_CHAR_TYPE:
		return scanTypeString
	case cli_service.TTypeId_VARCHAR_TYPE:
		return scanTypeString
	case cli_service.TTypeId_DATE_TYPE, cli_service.TTypeId_TIMESTAMP_TYPE:
		return scanTypeDateTime
	case cli_service.TTypeId_DECIMAL_TYPE, cli_service.TTypeId_BINARY_TYPE, cli_service.TTypeId_ARRAY_TYPE,
		cli_service.TTypeId_STRUCT_TYPE, cli_service.TTypeId_MAP_TYPE, cli_service.TTypeId_UNION_TYPE:
		return scanTypeRawBytes
	case cli_service.TTypeId_USER_DEFINED_TYPE:
		return scanTypeUnknown
	case cli_service.TTypeId_INTERVAL_DAY_TIME_TYPE, cli_service.TTypeId_INTERVAL_YEAR_MONTH_TYPE:
		return scanTypeString
	default:
		return scanTypeUnknown
	}
}

// isValidRows checks that the row instance is not nil
// and that it has a client
func isValidRows(r *rows) dbsqlerr.DBError {
	var err dbsqlerr.DBError
	if r == nil {
		err = dbsqlerr_int.NewDriverError(context.Background(), errRowsNilRows, nil)
	} else if r.client == nil {
		err = dbsqlerr_int.NewDriverError(r.ctx, errRowsNoClient, nil)
		r.logger().Err(err).Msg(errRowsNoClient)
	}

	return err
}

func (r *rows) getColumnMetadataByIndex(index int) (*cli_service.TColumnDesc, dbsqlerr.DBError) {
	err := isValidRows(r)
	if err != nil {
		return nil, err
	}

	schema, err := r.getResultSetSchema()
	if err != nil {
		return nil, err
	}

	columns := schema.GetColumns()
	if index < 0 || index >= len(columns) {
		err = dbsqlerr_int.NewDriverError(r.ctx, errRowsInvalidColumnIndex(index), nil)
		r.logger().Err(err).Msg(err.Error())
		return nil, err
	}

	return columns[index], nil
}

// isNextRowInPage returns a boolean flag indicating whether
// the next result set row is in the current result set page
func (r *rows) isNextRowInPage() (bool, dbsqlerr.DBError) {
	if r == nil || r.RowScanner == nil {
		return false, nil
	}

	return r.RowScanner.Contains(r.nextRowNumber), nil
}

// getResultMetadata does a one time fetch of the result set schema
func (r *rows) getResultSetSchema() (*cli_service.TTableSchema, dbsqlerr.DBError) {
	if r.schema == nil {
		err := isValidRows(r)
		if err != nil {
			return nil, err
		}

		req := cli_service.TGetResultSetMetadataReq{
			OperationHandle: r.opHandle,
		}
		ctx := driverctx.NewContextWithCorrelationId(driverctx.NewContextWithConnId(context.Background(), r.connId), r.correlationId)

		resp, err2 := r.client.GetResultSetMetadata(ctx, &req)
		if err2 != nil {
			r.logger().Err(err2).Msg(err2.Error())
			return nil, dbsqlerr_int.NewRequestError(r.ctx, errRowsMetadataFetchFailed, err)
		}

		r.resultSetMetadata = resp
		r.schema = resp.Schema

	}

	return r.schema, nil
}

// fetchResultPage will fetch the result page containing the next row, if necessary
func (r *rows) fetchResultPage() error {
	var err dbsqlerr.DBError = isValidRows(r)
	if err != nil {
		return err
	}

	if r.RowScanner != nil && r.RowScanner.Contains(r.nextRowNumber) {
		return nil
	}

	if r.RowScanner != nil && r.nextRowNumber < r.RowScanner.Start() {
		return dbsqlerr_int.NewDriverError(r.ctx, errRowsOnlyForward, nil)
	}

	// Close/release the existing row scanner before loading the next result page to
	// help keep memory usage down.
	if r.RowScanner != nil {
		r.RowScanner.Close()
		r.RowScanner = nil
	}

	if !r.ResultPageIterator.HasNext() {
		return io.EOF
	}

	fetchResult, err1 := r.ResultPageIterator.Next()
	if err1 != nil {
		return err1
	}

	err1 = r.makeRowScanner(fetchResult)
	if err1 != nil {
		return err1
	}

	// We should be iterating over the rows so the next row number should be in the
	// next result page
	if !r.RowScanner.Contains(r.nextRowNumber) {
		return dbsqlerr_int.NewDriverError(r.ctx, errInvalidRowNumberState, nil)
	}

	return nil
}

// makeRowScanner creates the embedded RowScanner instance based on the format
// of the returned query results
func (r *rows) makeRowScanner(fetchResults *cli_service.TFetchResultsResp) dbsqlerr.DBError {

	schema, err1 := r.getResultSetSchema()
	if err1 != nil {
		return err1
	}

	if fetchResults == nil {
		return nil
	}

	var rs rowscanner.RowScanner
	var err dbsqlerr.DBError
	if fetchResults.Results != nil {

		if fetchResults.Results.Columns != nil {
			rs, err = columnbased.NewColumnRowScanner(schema, fetchResults.Results, r.config, r.logger(), r.ctx)
		} else if fetchResults.Results.ArrowBatches != nil {
			rs, err = arrowbased.NewArrowRowScanner(r.resultSetMetadata, fetchResults.Results, r.config, r.logger(), r.ctx)
		} else if fetchResults.Results.ResultLinks != nil {
			rs, err = arrowbased.NewArrowRowScanner(r.resultSetMetadata, fetchResults.Results, r.config, r.logger(), r.ctx)
		} else {
			r.logger().Error().Msg(errRowsUnknowRowType)
			err = dbsqlerr_int.NewDriverError(r.ctx, errRowsUnknowRowType, nil)
		}

	} else {
		r.logger().Error().Msg(errRowsUnknowRowType)
		err = dbsqlerr_int.NewDriverError(r.ctx, errRowsUnknowRowType, nil)
	}

	if r.RowScanner != nil {
		r.RowScanner.Close()
	}

	r.RowScanner = rs

	return err
}

func (r *rows) logger() *dbsqllog.DBSQLLogger {
	if r.logger_ == nil {
		if r.opHandle != nil {
			r.logger_ = dbsqllog.WithContext(r.connId, r.correlationId, dbsqlclient.SprintGuid(r.opHandle.OperationId.GUID))
		} else {
			r.logger_ = dbsqllog.WithContext(r.connId, r.correlationId, "")
		}
	}
	return r.logger_
}

func (r *rows) GetArrowBatches(ctx context.Context) (dbsqlrows.ArrowBatchIterator, error) {
	// update context with correlationId and connectionId which will be used in logging and errors
	ctx = driverctx.NewContextWithCorrelationId(driverctx.NewContextWithConnId(ctx, r.connId), r.correlationId)

	// If a row scanner exists we use it to create the iterator, that way the iterator includes
	// data returned as direct results
	if r.RowScanner != nil {
		return r.RowScanner.GetArrowBatches(ctx, *r.config, r.ResultPageIterator)
	}

	return arrowbased.NewArrowRecordIterator(ctx, r.ResultPageIterator, nil, nil, *r.config), nil
}
