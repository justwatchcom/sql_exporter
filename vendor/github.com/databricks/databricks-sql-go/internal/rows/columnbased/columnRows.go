package columnbased

import (
	"context"
	"database/sql/driver"
	"time"

	dbsqlerr "github.com/databricks/databricks-sql-go/errors"
	"github.com/databricks/databricks-sql-go/internal/cli_service"
	"github.com/databricks/databricks-sql-go/internal/config"
	dbsqlerr_int "github.com/databricks/databricks-sql-go/internal/errors"
	"github.com/databricks/databricks-sql-go/internal/rows/rowscanner"
	dbsqllog "github.com/databricks/databricks-sql-go/logger"
	dbsqlrows "github.com/databricks/databricks-sql-go/rows"
)

var errRowsParseDateTime = "databricks: column row scanner failed to parse date/time"

// row scanner for query results in column based format
type columnRowScanner struct {
	*dbsqllog.DBSQLLogger
	// TRowSet with query results in column format
	rowSet *cli_service.TRowSet
	schema *cli_service.TTableSchema

	location *time.Location
	ctx      context.Context
	rowscanner.Delimiter
}

var _ rowscanner.RowScanner = (*columnRowScanner)(nil)

// NewColumnRowScanner returns a columnRowScanner initialized with the provided
// values.
func NewColumnRowScanner(schema *cli_service.TTableSchema, rowSet *cli_service.TRowSet, cfg *config.Config, logger *dbsqllog.DBSQLLogger, ctx context.Context) (rowscanner.RowScanner, dbsqlerr.DBError) {
	if logger == nil {
		logger = dbsqllog.Logger
	}

	var location *time.Location = time.UTC
	if cfg != nil {
		if cfg.Location != nil {
			location = cfg.Location
		}
	}

	logger.Debug().Msg("databricks: creating column row scanner")
	rs := &columnRowScanner{
		Delimiter:   rowscanner.NewDelimiter(rowSet.StartRowOffset, rowscanner.CountRows(rowSet)),
		schema:      schema,
		rowSet:      rowSet,
		DBSQLLogger: logger,
		location:    location,
		ctx:         ctx,
	}

	return rs, nil
}

// Close is called when the Rows instance is closed.
func (crs *columnRowScanner) Close() {}

// NRows returns the number or rows in the current TRowSet
func (crs *columnRowScanner) NRows() int64 {
	if crs == nil {
		return 0
	}
	return crs.Count()
}

// ScanRow is called to populate the provided slice with the
// content of the current row. The provided slice will be the same
// size as the number of columns.
// The dest should not be written to outside of ScanRow. Care
// should be taken when closing a RowScanner not to modify
// a buffer held in dest.
func (crs *columnRowScanner) ScanRow(
	dest []driver.Value,
	rowNumber int64) dbsqlerr.DBError {

	rowIndex := rowNumber - crs.Start()
	// populate the destinatino slice
	for i := range dest {
		val, err := crs.value(crs.rowSet.Columns[i], crs.schema.Columns[i], rowIndex)

		if err != nil {
			return err
		}

		dest[i] = val
	}

	return nil
}

// value retrieves the value for the specified colum/row
func (crs *columnRowScanner) value(tColumn *cli_service.TColumn, tColumnDesc *cli_service.TColumnDesc, rowNum int64) (val interface{}, err dbsqlerr.DBError) {
	// default to UTC time
	if crs.location == nil {
		crs.location = time.UTC
	}

	// Database type name
	dbtype := rowscanner.GetDBTypeName(tColumnDesc)

	if tVal := tColumn.GetStringVal(); tVal != nil && !rowscanner.IsNull(tVal.Nulls, rowNum) {
		val = tVal.Values[rowNum]
		var err1 error
		// DATE and TIMESTAMP are returned as strings so we need to handle that possibility
		val, err1 = rowscanner.HandleDateTime(val, dbtype, tColumnDesc.ColumnName, crs.location)
		if err1 != nil {
			crs.Err(err).Msg("databrics: column row scanner failed to parse date/time")
			err = dbsqlerr_int.NewDriverError(crs.ctx, errRowsParseDateTime, err1)
		}
	} else if tVal := tColumn.GetByteVal(); tVal != nil && !rowscanner.IsNull(tVal.Nulls, rowNum) {
		val = tVal.Values[rowNum]
	} else if tVal := tColumn.GetI16Val(); tVal != nil && !rowscanner.IsNull(tVal.Nulls, rowNum) {
		val = tVal.Values[rowNum]
	} else if tVal := tColumn.GetI32Val(); tVal != nil && !rowscanner.IsNull(tVal.Nulls, rowNum) {
		val = tVal.Values[rowNum]
	} else if tVal := tColumn.GetI64Val(); tVal != nil && !rowscanner.IsNull(tVal.Nulls, rowNum) {
		val = tVal.Values[rowNum]
	} else if tVal := tColumn.GetBoolVal(); tVal != nil && !rowscanner.IsNull(tVal.Nulls, rowNum) {
		val = tVal.Values[rowNum]
	} else if tVal := tColumn.GetDoubleVal(); tVal != nil && !rowscanner.IsNull(tVal.Nulls, rowNum) {
		if dbtype == "FLOAT" {
			// database types FLOAT and DOUBLE are both returned as a float64
			// convert to a float32 is valid because the FLOAT type would have
			// only been four bytes on the server
			val = float32(tVal.Values[rowNum])
		} else {
			val = tVal.Values[rowNum]
		}
	} else if tVal := tColumn.GetBinaryVal(); tVal != nil && !rowscanner.IsNull(tVal.Nulls, rowNum) {
		val = tVal.Values[rowNum]
	}

	return val, err
}

func (crs *columnRowScanner) GetArrowBatches(
	ctx context.Context,
	cfg config.Config,
	rpi rowscanner.ResultPageIterator) (dbsqlrows.ArrowBatchIterator, error) {
	return nil, dbsqlerr_int.NewDriverError(ctx, "databricks: result set is not in arrow format", nil)
}
