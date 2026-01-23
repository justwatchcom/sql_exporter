package arrowbased

import (
	"bytes"
	"context"
	"database/sql/driver"
	"io"
	"time"

	"github.com/apache/arrow/go/v12/arrow"
	"github.com/apache/arrow/go/v12/arrow/array"
	"github.com/apache/arrow/go/v12/arrow/ipc"
	dbsqlerr "github.com/databricks/databricks-sql-go/errors"
	"github.com/databricks/databricks-sql-go/internal/cli_service"
	"github.com/databricks/databricks-sql-go/internal/config"
	dbsqlerrint "github.com/databricks/databricks-sql-go/internal/errors"
	"github.com/databricks/databricks-sql-go/internal/rows/rowscanner"
	dbsqllog "github.com/databricks/databricks-sql-go/logger"
	dbsqlrows "github.com/databricks/databricks-sql-go/rows"
	"github.com/pkg/errors"
)

// Abstraction for a set of arrow records
type SparkArrowBatch interface {
	rowscanner.Delimiter
	Next() (SparkArrowRecord, error)
	HasNext() bool
	Close()
}

// Abstraction for an arrow record
type SparkArrowRecord interface {
	rowscanner.Delimiter
	arrow.Record
}

type timeStampFn func(arrow.Timestamp) time.Time

type colInfo struct {
	name      string
	arrowType arrow.DataType
	dbType    cli_service.TTypeId
}

// arrowRowScanner handles extracting values from arrow records
type arrowRowScanner struct {
	rowscanner.Delimiter
	valueContainerMaker

	// configuration of different arrow options for retrieving results
	config.ArrowConfig

	// arrow schema corresponding to the TTableSchema
	arrowSchema *arrow.Schema

	// serialized form of arrow format schema
	arrowSchemaBytes []byte

	// database types for the columns
	colInfo []colInfo

	currentBatch SparkArrowBatch

	// Currently loaded field values for a set of rows
	rowValues RowValues

	// function to convert arrow timestamp when using native arrow format
	toTimestampFn timeStampFn

	// hold on to a logger instance with context, rather than just use the global variable
	*dbsqllog.DBSQLLogger

	location *time.Location

	ctx context.Context

	batchIterator BatchIterator
}

// Make sure arrowRowScanner fulfills the RowScanner interface
var _ rowscanner.RowScanner = (*arrowRowScanner)(nil)

// NewArrowRowScanner returns an instance of RowScanner which handles arrow format results
func NewArrowRowScanner(resultSetMetadata *cli_service.TGetResultSetMetadataResp, rowSet *cli_service.TRowSet, cfg *config.Config, logger *dbsqllog.DBSQLLogger, ctx context.Context) (rowscanner.RowScanner, dbsqlerr.DBError) {

	// we take a passed in logger, rather than just using the global from dbsqllog, so that the containing rows
	// instance can pass in a logger with context such as correlation ID and operation ID
	if logger == nil {
		logger = dbsqllog.Logger
	}

	logger.Debug().Msgf("databricks: creating arrow row scanner, nArrowBatches: %d", len(rowSet.ArrowBatches))

	var arrowConfig config.ArrowConfig
	if cfg != nil {
		arrowConfig = cfg.ArrowConfig
	}

	schemaBytes, arrowSchema, metadataErr := tGetResultSetMetadataRespToArrowSchema(resultSetMetadata, arrowConfig, ctx, logger)
	if metadataErr != nil {
		return nil, metadataErr
	}

	// Create column info
	colInfos := getColumnInfo(arrowSchema, resultSetMetadata.Schema)

	// get the function for converting arrow timestamps to a time.Time
	// time values from the server are returned as UTC with microsecond precision
	ttsf, err := arrow.FixedWidthTypes.Timestamp_us.(*arrow.TimestampType).GetToTimeFunc()
	if err != nil {
		logger.Err(err).Msg(errArrowRowsToTimestampFn)
		return nil, dbsqlerrint.NewDriverError(ctx, errArrowRowsToTimestampFn, err)
	}

	var bi BatchIterator
	var err2 dbsqlerr.DBError
	if len(rowSet.ResultLinks) > 0 {
		logger.Debug().Msgf("Initialize CloudFetch loader, row set start offset: %d, file list:", rowSet.StartRowOffset)
		for _, resultLink := range rowSet.ResultLinks {
			logger.Debug().Msgf("- start row offset: %d, row count: %d", resultLink.StartRowOffset, resultLink.RowCount)
		}
		bi, err2 = NewCloudBatchIterator(context.Background(), rowSet.ResultLinks, rowSet.StartRowOffset, cfg)
	} else {
		bi, err2 = NewLocalBatchIterator(context.Background(), rowSet.ArrowBatches, rowSet.StartRowOffset, schemaBytes, cfg)
	}
	if err2 != nil {
		return nil, err2
	}

	var location *time.Location = time.UTC
	if cfg != nil {
		if cfg.Location != nil {
			location = cfg.Location
		}
	}

	rs := &arrowRowScanner{
		Delimiter:           rowscanner.NewDelimiter(rowSet.StartRowOffset, rowscanner.CountRows(rowSet)),
		valueContainerMaker: &arrowValueContainerMaker{},
		ArrowConfig:         arrowConfig,
		arrowSchemaBytes:    schemaBytes,
		arrowSchema:         arrowSchema,
		toTimestampFn:       ttsf,
		colInfo:             colInfos,
		DBSQLLogger:         logger,
		location:            location,
		batchIterator:       bi,
	}

	return rs, nil
}

// Close is called when the Rows instance is closed.
func (ars *arrowRowScanner) Close() {
	if ars.rowValues != nil {
		ars.rowValues.Close()
	}

	if ars.batchIterator != nil {
		ars.batchIterator.Close()
	}

	if ars.currentBatch != nil {
		ars.currentBatch.Close()
	}
}

// NRows returns the number of rows in the current set of batches
func (ars *arrowRowScanner) NRows() int64 {
	if ars != nil {
		return ars.Count()
	}

	return 0
}

var complexTypes map[cli_service.TTypeId]struct{} = map[cli_service.TTypeId]struct{}{
	cli_service.TTypeId_ARRAY_TYPE:  {},
	cli_service.TTypeId_MAP_TYPE:    {},
	cli_service.TTypeId_STRUCT_TYPE: {}}

var intervalTypes map[cli_service.TTypeId]struct{} = map[cli_service.TTypeId]struct{}{
	cli_service.TTypeId_INTERVAL_DAY_TIME_TYPE:   {},
	cli_service.TTypeId_INTERVAL_YEAR_MONTH_TYPE: {}}

// ScanRow is called to populate the provided slice with the
// content of the current row. The provided slice will be the same
// size as the number of columns.
// The dest should not be written to outside of ScanRow. Care
// should be taken when closing a RowScanner not to modify
// a buffer held in dest.
func (ars *arrowRowScanner) ScanRow(
	destination []driver.Value,
	rowNumber int64) dbsqlerr.DBError {

	// load the error batch for the specified row, if necessary
	err := ars.loadBatchFor(rowNumber)
	if err != nil {
		return err
	}

	// if no location is provided default to UTC
	if ars.location == nil {
		ars.location = time.UTC
	}

	// loop over the destination slice filling in values
	for i := range destination {
		// clear the destination
		destination[i] = nil

		// if there is a corresponding column and the value for the specified row
		// is not null we put the value in the destination
		if !ars.rowValues.IsNull(i, rowNumber) {

			col := ars.colInfo[i]
			dbType := col.dbType

			if (dbType == cli_service.TTypeId_DECIMAL_TYPE && ars.UseArrowNativeDecimal) ||
				(isIntervalType(dbType) && ars.UseArrowNativeIntervalTypes) {
				//	not yet fully supported
				ars.Error().Msgf(errArrowRowsUnsupportedNativeType(dbType.String()))
				return dbsqlerrint.NewDriverError(ars.ctx, errArrowRowsUnsupportedNativeType(dbType.String()), nil)
			}

			// get the value from the column values holder
			var err1 error
			destination[i], err1 = ars.rowValues.Value(i, rowNumber)
			if err1 != nil {
				err = dbsqlerrint.NewDriverError(ars.ctx, errArrowRowsColumnValue(col.name), err1)
			}
		}
	}

	return err
}

func isIntervalType(typeId cli_service.TTypeId) bool {
	_, ok := intervalTypes[typeId]
	return ok
}

// loadBatchFor loads the batch containing the specified row if necessary
func (ars *arrowRowScanner) loadBatchFor(rowNumber int64) dbsqlerr.DBError {

	if ars == nil {
		return dbsqlerrint.NewDriverError(context.Background(), errArrowRowsNoArrowBatches, nil)
	}

	if ars.batchIterator == nil {
		return dbsqlerrint.NewDriverError(ars.ctx, errArrowRowsNoArrowBatches, nil)
	}

	// if the batch already loaded we can just return
	if ars.rowValues != nil && ars.rowValues.Contains(rowNumber) {
		return nil
	}

	// check for things like going backwards, rowNumber < 0, etc.
	err := ars.validateRowNumber(rowNumber)
	if err != nil {
		return err
	}

	// Find the batch containing the row number, if necessary
	for ars.currentBatch == nil || !ars.currentBatch.Contains(rowNumber) {
		batch, err := ars.batchIterator.Next()
		if err != nil {
			if dbErr, ok := err.(dbsqlerr.DBError); ok {
				return dbErr
			} else {
				return dbsqlerrint.NewDriverError(ars.ctx, errArrowRowsInvalidRowNumber(rowNumber), err)
			}
		}

		ars.currentBatch = batch
	}

	// Get the next arrow record from the current batch
	sar, err2 := ars.currentBatch.Next()
	if err2 != nil {
		ars.Err(err2).Msg(errArrowRowsUnableToReadBatch)
		return dbsqlerrint.NewDriverError(ars.ctx, errArrowRowsUnableToReadBatch, err2)
	}

	defer sar.Release()

	// set up the column values containers
	if ars.rowValues == nil {
		err := ars.makeColumnValuesContainers(ars, rowscanner.NewDelimiter(sar.Start(), sar.Count()))
		if err != nil {
			return dbsqlerrint.NewDriverError(ars.ctx, errArrowRowsMakeColumnValueContainers, err)
		}
	}

	// for each column we want to create an arrow array specific to the data type
	for i, col := range sar.Columns() {
		func() {
			col.Retain()
			defer col.Release()

			colData := col.Data()
			colData.Retain()
			defer colData.Release()

			err := ars.rowValues.SetColumnValues(i, colData)
			if err != nil {
				ars.Error().Msg(err.Error())
			}
		}()
	}

	// Update the delimiter in rowValues to reflect the currently loaded set of rows
	ars.rowValues.SetDelimiter(rowscanner.NewDelimiter(sar.Start(), sar.Count()))
	return nil
}

// Check that the row number falls within the range of this row scanner and that
// it is not moving backwards.
func (ars *arrowRowScanner) validateRowNumber(rowNumber int64) dbsqlerr.DBError {
	if rowNumber < 0 || rowNumber > ars.End() || (ars.currentBatch != nil && ars.currentBatch.Direction(rowNumber) == rowscanner.DirBack) {
		return dbsqlerrint.NewDriverError(ars.ctx, errArrowRowsInvalidRowNumber(rowNumber), nil)
	}
	return nil
}

func (ars *arrowRowScanner) GetArrowBatches(ctx context.Context, cfg config.Config, rpi rowscanner.ResultPageIterator) (dbsqlrows.ArrowBatchIterator, error) {
	ri := NewArrowRecordIterator(ctx, rpi, ars.batchIterator, ars.arrowSchemaBytes, cfg)
	return ri, nil
}

// getArrowSchemaBytes returns the serialized schema in ipc format
func getArrowSchemaBytes(schema *arrow.Schema, ctx context.Context) ([]byte, dbsqlerr.DBError) {
	if schema == nil {
		return nil, dbsqlerrint.NewDriverError(ctx, errArrowRowsNilArrowSchema, nil)
	}

	var output bytes.Buffer
	w := ipc.NewWriter(&output, ipc.WithSchema(schema))
	err := w.Close()
	if err != nil {
		return nil, dbsqlerrint.NewDriverError(ctx, errArrowRowsUnableToWriteArrowSchema, err)
	}

	arrowSchemaBytes := output.Bytes()

	// the writer serializes to an arrow batch but we just want the
	// schema bytes so we strip off the empty Record at the end
	arrowSchemaBytes = arrowSchemaBytes[:len(arrowSchemaBytes)-8]

	return arrowSchemaBytes, nil
}

// tTableSchemaToArrowSchema convers the TTableSchema retrieved by the thrift server into an arrow.Schema instance
func tTableSchemaToArrowSchema(schema *cli_service.TTableSchema, arrowConfig *config.ArrowConfig) (*arrow.Schema, error) {
	columns := schema.GetColumns()
	fields := make([]arrow.Field, len(columns))

	for i := range columns {
		field, err := tColumnDescToArrowField(columns[i], arrowConfig)
		if err != nil {
			return nil, err
		}

		fields[i] = field
	}

	arrowSchema := arrow.NewSchema(fields, nil)

	return arrowSchema, nil
}

// map the thrift data types to the corresponding arrow data type
var toArrowTypeMap map[cli_service.TTypeId]arrow.DataType = map[cli_service.TTypeId]arrow.DataType{
	cli_service.TTypeId_BOOLEAN_TYPE:  arrow.FixedWidthTypes.Boolean,
	cli_service.TTypeId_TINYINT_TYPE:  arrow.PrimitiveTypes.Int8,
	cli_service.TTypeId_SMALLINT_TYPE: arrow.PrimitiveTypes.Int16,
	cli_service.TTypeId_INT_TYPE:      arrow.PrimitiveTypes.Int32,
	cli_service.TTypeId_BIGINT_TYPE:   arrow.PrimitiveTypes.Int64,
	cli_service.TTypeId_FLOAT_TYPE:    arrow.PrimitiveTypes.Float32,
	cli_service.TTypeId_DOUBLE_TYPE:   arrow.PrimitiveTypes.Float64,
	cli_service.TTypeId_STRING_TYPE:   arrow.BinaryTypes.String,
	// cli_service.TTypeId_TIMESTAMP_TYPE:    see tColumnDescToArrowDataType
	cli_service.TTypeId_BINARY_TYPE: arrow.BinaryTypes.Binary,
	// cli_service.TTypeId_ARRAY_TYPE:        see tColumnDescToArrowDataType
	// cli_service.TTypeId_MAP_TYPE:          see tColumnDescToArrowDataType
	// cli_service.TTypeId_STRUCT_TYPE:       see tColumnDescToArrowDataType
	cli_service.TTypeId_UNION_TYPE:        arrow.BinaryTypes.String,
	cli_service.TTypeId_USER_DEFINED_TYPE: arrow.BinaryTypes.String,
	// cli_service.TTypeId_DECIMAL_TYPE:  see tColumnDescToArrowDataType
	cli_service.TTypeId_NULL_TYPE:    arrow.Null,
	cli_service.TTypeId_DATE_TYPE:    arrow.FixedWidthTypes.Date32,
	cli_service.TTypeId_VARCHAR_TYPE: arrow.BinaryTypes.String,
	cli_service.TTypeId_CHAR_TYPE:    arrow.BinaryTypes.String,
	// cli_service.TTypeId_INTERVAL_YEAR_MONTH_TYPE: see tColumnDescToArrowDataType
	// cli_service.TTypeId_INTERVAL_DAY_TIME_TYPE:   see tColumnDescToArrowDataType
}

func tColumnDescToArrowDataType(tColumnDesc *cli_service.TColumnDesc, arrowConfig *config.ArrowConfig) (arrow.DataType, error) {
	// get the thrift type id
	tType := rowscanner.GetDBTypeID(tColumnDesc)

	if at, ok := toArrowTypeMap[tType]; ok {
		// simple type mapping
		return at, nil
	} else {
		// for some types there isn't a simple 1:1 correspondence to an arrow data type
		if tType == cli_service.TTypeId_DECIMAL_TYPE {
			// if not using arrow native decimal type decimals are returned as strings
			if !arrowConfig.UseArrowNativeDecimal {
				return arrow.BinaryTypes.String, nil
			}

			// Need to construct an instance of arrow DecimalType with the
			// correct scale and precision
			scale, precision, err := getDecimalScalePrecision(tColumnDesc)
			if err != nil {
				return nil, err
			}

			decimalType, err := arrow.NewDecimalType(arrow.DECIMAL128, precision, scale)
			if err != nil {
				return nil, dbsqlerrint.WrapErr(err, errArrowRowsUnableToCreateDecimalType(scale, precision))
			}

			return decimalType, nil

		} else if tType == cli_service.TTypeId_TIMESTAMP_TYPE {
			// if not using arrow native timestamps thrift server returns strings
			if !arrowConfig.UseArrowNativeTimestamp {
				return arrow.BinaryTypes.String, nil
			}

			// timestamp is UTC with microsecond precision
			return arrow.FixedWidthTypes.Timestamp_us, nil
		} else if _, ok := complexTypes[tType]; ok {
			// if not using arrow native complex types thrift server returns strings
			if !arrowConfig.UseArrowNativeComplexTypes {
				return arrow.BinaryTypes.String, nil
			}

			return nil, errors.New(errArrowRowsUnsupportedWithHiveSchema(rowscanner.GetDBTypeName(tColumnDesc)))
		} else if _, ok := intervalTypes[tType]; ok {
			// if not using arrow native complex types thrift server returns strings
			if !arrowConfig.UseArrowNativeIntervalTypes {
				return arrow.BinaryTypes.String, nil
			}

			return nil, errors.New(errArrowRowsUnsupportedWithHiveSchema(rowscanner.GetDBTypeName(tColumnDesc)))
		} else {
			return nil, errors.New(errArrowRowsUnknownDBType)
		}
	}

}

func getDecimalScalePrecision(tColumnDesc *cli_service.TColumnDesc) (scale, precision int32, err error) {
	fail := errors.New(errArrowRowsInvalidDecimalType)

	typeQualifiers := rowscanner.GetDBTypeQualifiers(tColumnDesc)
	if typeQualifiers == nil || typeQualifiers.Qualifiers == nil {
		err = fail
		return
	}

	scaleHolder, ok := typeQualifiers.Qualifiers["scale"]
	if !ok || scaleHolder == nil || scaleHolder.I32Value == nil {
		err = fail
		return
	} else {
		scale = *scaleHolder.I32Value
	}

	precisionHolder, ok := typeQualifiers.Qualifiers["precision"]
	if !ok || precisionHolder == nil || precisionHolder.I32Value == nil {
		err = fail
		return
	} else {
		precision = *precisionHolder.I32Value
	}

	return
}

func tColumnDescToArrowField(columnDesc *cli_service.TColumnDesc, arrowConfig *config.ArrowConfig) (arrow.Field, error) {
	arrowDataType, err := tColumnDescToArrowDataType(columnDesc, arrowConfig)
	if err != nil {
		return arrow.Field{}, err
	}

	arrowField := arrow.Field{
		Name: columnDesc.ColumnName,
		Type: arrowDataType,
	}

	return arrowField, nil
}

// Build a slice of columnInfo using the arrow schema and the thrift schema
func getColumnInfo(arrowSchema *arrow.Schema, schema *cli_service.TTableSchema) []colInfo {
	if arrowSchema == nil || schema == nil {
		return []colInfo{}
	}

	nFields := len(arrowSchema.Fields())
	if len(schema.Columns) < nFields {
		nFields = len(schema.Columns)
	}

	colInfos := make([]colInfo, nFields)
	for i := 0; i < nFields; i++ {
		col := schema.Columns[i]
		field := arrowSchema.Field(i)
		colInfos[i] = colInfo{name: field.Name, arrowType: field.Type, dbType: rowscanner.GetDBType(col)}
	}

	return colInfos
}

// Derive an arrow.Schema object and the corresponding serialized bytes from TGetResultSetMetadataResp
func tGetResultSetMetadataRespToArrowSchema(resultSetMetadata *cli_service.TGetResultSetMetadataResp, arrowConfig config.ArrowConfig, ctx context.Context, logger *dbsqllog.DBSQLLogger) ([]byte, *arrow.Schema, dbsqlerr.DBError) {

	var arrowSchema *arrow.Schema
	schemaBytes := resultSetMetadata.ArrowSchema
	if schemaBytes == nil {
		var err error
		// convert the TTableSchema to an arrow Schema
		arrowSchema, err = tTableSchemaToArrowSchema(resultSetMetadata.Schema, &arrowConfig)
		if err != nil {
			logger.Err(err).Msg(errArrowRowsConvertSchema)
			return nil, nil, dbsqlerrint.NewDriverError(ctx, errArrowRowsConvertSchema, err)
		}

		// serialize the arrow schema
		schemaBytes, err = getArrowSchemaBytes(arrowSchema, ctx)
		if err != nil {
			logger.Err(err).Msg(errArrowRowsSerializeSchema)
			return nil, nil, dbsqlerrint.NewDriverError(ctx, errArrowRowsSerializeSchema, err)
		}
	} else {
		br := bytes.NewReader(schemaBytes)
		rdr, err := ipc.NewReader(br)
		if err != nil {
			return nil, nil, dbsqlerrint.NewDriverError(ctx, errArrowRowsUnableToReadBatch, err)
		}
		defer rdr.Release()

		arrowSchema = rdr.Schema()
	}

	return schemaBytes, arrowSchema, nil
}

type arrowValueContainerMaker struct{}

var _ valueContainerMaker = (*arrowValueContainerMaker)(nil)

// makeColumnValuesContainers creates appropriately typed  column values holders for each column
func (vcm *arrowValueContainerMaker) makeColumnValuesContainers(ars *arrowRowScanner, d rowscanner.Delimiter) error {
	if ars.rowValues == nil {
		columnValueHolders := make([]columnValues, len(ars.colInfo))
		for i, field := range ars.arrowSchema.Fields() {
			holder, err := vcm.makeColumnValueContainer(field.Type, ars.location, ars.toTimestampFn, &ars.colInfo[i])
			if err != nil {
				ars.Error().Msg(err.Error())
				return err
			}

			columnValueHolders[i] = holder
		}

		ars.rowValues = NewRowValues(d, columnValueHolders)
	}

	return nil
}

func (vcm *arrowValueContainerMaker) makeColumnValueContainer(t arrow.DataType, location *time.Location, toTimestampFn timeStampFn, colInfo *colInfo) (columnValues, error) {
	if location == nil {
		location = time.UTC
	}

	switch t := t.(type) {

	case *arrow.BooleanType:
		return &columnValuesTyped[*array.Boolean, bool]{}, nil

	case *arrow.Int8Type:
		return &columnValuesTyped[*array.Int8, int8]{}, nil

	case *arrow.Int16Type:
		return &columnValuesTyped[*array.Int16, int16]{}, nil

	case *arrow.Int32Type:
		return &columnValuesTyped[*array.Int32, int32]{}, nil

	case *arrow.Int64Type:
		return &columnValuesTyped[*array.Int64, int64]{}, nil

	case *arrow.Float32Type:
		return &columnValuesTyped[*array.Float32, float32]{}, nil

	case *arrow.Float64Type:
		return &columnValuesTyped[*array.Float64, float64]{}, nil

	case *arrow.StringType:
		if colInfo != nil && colInfo.dbType == cli_service.TTypeId_TIMESTAMP_TYPE {
			return &timestampStringValueContainer{location: location, fieldName: colInfo.name}, nil
		} else {
			return &columnValuesTyped[*array.String, string]{}, nil
		}

	case *arrow.Decimal128Type:
		return &decimal128Container{scale: t.Scale}, nil

	case *arrow.Date32Type:
		return &dateValueContainer{location: location}, nil

	case *arrow.TimestampType:
		return &timestampValueContainer{location: location, toTimestampFn: toTimestampFn}, nil

	case *arrow.BinaryType:
		return &columnValuesTyped[*array.Binary, []byte]{}, nil

	case *arrow.ListType:
		lvc := &listValueContainer{listArrayType: t}
		var err error
		lvc.values, err = vcm.makeColumnValueContainer(t.Elem(), location, toTimestampFn, nil)
		if err != nil {
			return nil, err
		}
		switch t.Elem().(type) {
		case *arrow.MapType, *arrow.ListType, *arrow.StructType:
			lvc.complexValue = true
		}
		return lvc, nil

	case *arrow.MapType:
		mvc := &mapValueContainer{mapArrayType: t}
		var err error
		mvc.values, err = vcm.makeColumnValueContainer(t.ItemType(), location, toTimestampFn, nil)
		if err != nil {
			return nil, err
		}
		mvc.keys, err = vcm.makeColumnValueContainer(t.KeyType(), location, toTimestampFn, nil)
		if err != nil {
			return nil, err
		}
		switch t.ItemType().(type) {
		case *arrow.MapType, *arrow.ListType, *arrow.StructType:
			mvc.complexValue = true
		}

		return mvc, nil

	case *arrow.StructType:
		svc := &structValueContainer{structArrayType: t}
		svc.fieldNames = make([]string, len(t.Fields()))
		svc.fieldValues = make([]columnValues, len(t.Fields()))
		svc.complexValue = make([]bool, len(t.Fields()))
		for i, f := range t.Fields() {
			svc.fieldNames[i] = f.Name
			c, err := vcm.makeColumnValueContainer(f.Type, location, toTimestampFn, nil)
			if err != nil {
				return nil, err
			}
			svc.fieldValues[i] = c
			switch f.Type.(type) {
			case *arrow.MapType, *arrow.ListType, *arrow.StructType:
				svc.complexValue[i] = true
			}

		}

		return svc, nil

	case *arrow.NullType:
		return nullContainer, nil

	default:
		return nil, errors.Errorf(errArrowRowsUnhandledArrowType(t.String()))
	}
}

// Container for a set of arrow records
type sparkArrowBatch struct {
	// Delimiter indicating the range of rows covered by the arrow records
	rowscanner.Delimiter
	arrowRecords []SparkArrowRecord
}

var _ SparkArrowBatch = (*sparkArrowBatch)(nil)

func (b *sparkArrowBatch) Next() (SparkArrowRecord, error) {
	if len(b.arrowRecords) > 0 {
		r := b.arrowRecords[0]
		// remove the record from the slice as iteration is only forwards
		b.arrowRecords = b.arrowRecords[1:]
		return r, nil
	}

	// no more records
	return nil, io.EOF
}

func (b *sparkArrowBatch) HasNext() bool { return b != nil && len(b.arrowRecords) > 0 }

func (b *sparkArrowBatch) Close() {
	// Release any arrow records
	for i := range b.arrowRecords {
		b.arrowRecords[i].Release()
	}
	b.arrowRecords = nil
}

// Composite of an arrow record and a delimiter indicating
// the rows corresponding to the record.
type sparkArrowRecord struct {
	rowscanner.Delimiter
	arrow.Record
}

var _ SparkArrowRecord = (*sparkArrowRecord)(nil)

func (sar *sparkArrowRecord) Release() {
	if sar.Record != nil {
		sar.Record.Release()
		sar.Record = nil
	}
}

func (sar *sparkArrowRecord) Retain() {
	if sar.Record != nil {
		sar.Record.Retain()
	}
}
