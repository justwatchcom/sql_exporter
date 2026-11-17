package gosnowflake

import (
	"context"
	"database/sql/driver"
	"github.com/snowflakedb/gosnowflake/v2/internal/errors"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	ia "github.com/snowflakedb/gosnowflake/v2/internal/arrow"
	"github.com/snowflakedb/gosnowflake/v2/internal/query"
	"github.com/snowflakedb/gosnowflake/v2/internal/types"
)

const (
	headerSseCAlgorithm = "x-amz-server-side-encryption-customer-algorithm"
	headerSseCKey       = "x-amz-server-side-encryption-customer-key"
	headerSseCAes       = "AES256"
)

var (
	// customJSONDecoderEnabled has the chunk downloader use the custom JSON decoder to reduce memory footprint.
	customJSONDecoderEnabled = false

	maxChunkDownloaderErrorCounter = 5
)

const defaultMaxChunkDownloadWorkers = 10
const clientPrefetchThreadsKey = "client_prefetch_threads"

// SnowflakeRows provides an API for methods exposed to the clients
type SnowflakeRows interface {
	GetQueryID() string
	GetStatus() QueryStatus
	// NextResultSet switches Arrow Batches to the next result set.
	// Returns io.EOF if there are no more result sets.
	NextResultSet() error
}

type snowflakeRows struct {
	sc                  *snowflakeConn
	ChunkDownloader     chunkDownloader
	tailChunkDownloader chunkDownloader
	queryID             string
	status              QueryStatus
	err                 error
	errChannel          chan error
	location            *time.Location
	ctx                 context.Context
}

func (rows *snowflakeRows) getLocation() *time.Location {
	if rows.location == nil && rows.sc != nil && rows.sc.cfg != nil {
		rows.location = getCurrentLocation(&rows.sc.syncParams)
	}
	return rows.location
}

type snowflakeValue any

type chunkRowType struct {
	RowSet   []*string
	ArrowRow []snowflakeValue
}

type rowSetType struct {
	RowType      []query.ExecResponseRowType
	JSON         [][]*string
	RowSetBase64 string
}

type chunkError struct {
	Index int
	Error error
}

func (rows *snowflakeRows) Close() (err error) {
	if err := rows.waitForAsyncQueryStatus(); err != nil {
		return err
	}
	logger.WithContext(rows.sc.ctx).Debug("Rows.Close")
	if scd, ok := rows.ChunkDownloader.(*snowflakeChunkDownloader); ok {
		scd.releaseRawArrowBatches()
	}
	return nil
}

// ColumnTypeDatabaseTypeName returns the database column name.
func (rows *snowflakeRows) ColumnTypeDatabaseTypeName(index int) string {
	if err := rows.waitForAsyncQueryStatus(); err != nil {
		return err.Error()
	}
	return strings.ToUpper(rows.ChunkDownloader.getRowType()[index].Type)
}

// ColumnTypeLength returns the length of the column
func (rows *snowflakeRows) ColumnTypeLength(index int) (length int64, ok bool) {
	if err := rows.waitForAsyncQueryStatus(); err != nil {
		return 0, false
	}
	if index < 0 || index > len(rows.ChunkDownloader.getRowType()) {
		return 0, false
	}
	switch rows.ChunkDownloader.getRowType()[index].Type {
	case "text", "variant", "object", "array", "binary":
		return rows.ChunkDownloader.getRowType()[index].Length, true
	}
	return 0, false
}

func (rows *snowflakeRows) ColumnTypeNullable(index int) (nullable, ok bool) {
	if err := rows.waitForAsyncQueryStatus(); err != nil {
		return false, false
	}
	if index < 0 || index > len(rows.ChunkDownloader.getRowType()) {
		return false, false
	}
	return rows.ChunkDownloader.getRowType()[index].Nullable, true
}

func (rows *snowflakeRows) ColumnTypePrecisionScale(index int) (precision, scale int64, ok bool) {
	if err := rows.waitForAsyncQueryStatus(); err != nil {
		return 0, 0, false
	}
	rowType := rows.ChunkDownloader.getRowType()
	if index < 0 || index > len(rowType) {
		return 0, 0, false
	}
	switch rowType[index].Type {
	case "fixed":
		return rowType[index].Precision, rowType[index].Scale, true
	case "time":
		return rowType[index].Scale, 0, true
	case "timestamp":
		return rowType[index].Scale, 0, true
	}
	return 0, 0, false
}

func (rows *snowflakeRows) Columns() []string {
	if err := rows.waitForAsyncQueryStatus(); err != nil {
		return make([]string, 0)
	}
	logger.WithContext(rows.ctx).Debug("Rows.Columns")
	ret := make([]string, len(rows.ChunkDownloader.getRowType()))
	for i, n := 0, len(rows.ChunkDownloader.getRowType()); i < n; i++ {
		ret[i] = rows.ChunkDownloader.getRowType()[i].Name
	}
	return ret
}

func (rows *snowflakeRows) ColumnTypeScanType(index int) reflect.Type {
	if err := rows.waitForAsyncQueryStatus(); err != nil {
		return nil
	}
	return snowflakeTypeToGo(rows.ctx, types.GetSnowflakeType(rows.ChunkDownloader.getRowType()[index].Type), rows.ChunkDownloader.getRowType()[index].Precision, rows.ChunkDownloader.getRowType()[index].Scale, rows.ChunkDownloader.getRowType()[index].Fields)
}

func (rows *snowflakeRows) GetQueryID() string {
	return rows.queryID
}

func (rows *snowflakeRows) GetStatus() QueryStatus {
	return rows.status
}

// GetArrowBatches returns raw arrow batch data for use by the arrowbatches sub-package.
// Implements ia.BatchDataProvider.
func (rows *snowflakeRows) GetArrowBatches() (*ia.BatchDataInfo, error) {
	if err := rows.waitForAsyncQueryStatus(); err != nil {
		return nil, err
	}

	if rows.ChunkDownloader.getQueryResultFormat() != arrowFormat {
		return nil, exceptionTelemetry(errors.ErrNonArrowResponseForArrowBatches(rows.queryID), rows.sc)
	}

	scd, ok := rows.ChunkDownloader.(*snowflakeChunkDownloader)
	if !ok {
		return nil, &SnowflakeError{
			Number:  ErrNotImplemented,
			Message: "chunk downloader does not support arrow batch data",
		}
	}

	rawBatches := scd.getRawArrowBatches()
	batches := make([]ia.BatchRaw, len(rawBatches))
	for i, raw := range rawBatches {
		batch := ia.BatchRaw{
			Records:  raw.records,
			Index:    i,
			RowCount: raw.rowCount,
			Location: raw.loc,
		}
		raw.records = nil
		if batch.Records == nil {
			capturedIdx := i
			if scd.firstBatchRaw != nil {
				capturedIdx = i - 1
			}
			batch.Download = func(ctx context.Context) (*[]arrow.Record, int, error) {
				if err := scd.FuncDownloadHelper(ctx, scd, capturedIdx); err != nil {
					return nil, 0, err
				}
				actualRaw := scd.rawBatches[capturedIdx]
				return actualRaw.records, actualRaw.rowCount, nil
			}
		}
		batches[i] = batch
	}

	return &ia.BatchDataInfo{
		Batches:   batches,
		RowTypes:  scd.RowSet.RowType,
		Allocator: scd.pool,
		Ctx:       scd.ctx,
		QueryID:   rows.queryID,
	}, nil
}

func (rows *snowflakeRows) Next(dest []driver.Value) (err error) {
	if err = rows.waitForAsyncQueryStatus(); err != nil {
		return err
	}
	row, err := rows.ChunkDownloader.next()
	if err != nil {
		// includes io.EOF
		if err == io.EOF {
			rows.ChunkDownloader.reset()
		}
		return err
	}

	if rows.ChunkDownloader.getQueryResultFormat() == arrowFormat {
		for i, n := 0, len(row.ArrowRow); i < n; i++ {
			dest[i] = row.ArrowRow[i]
		}
	} else {
		for i, n := 0, len(row.RowSet); i < n; i++ {
			// could move to chunk downloader so that each go routine
			// can convert data
			err = stringToValue(rows.ctx, &dest[i], rows.ChunkDownloader.getRowType()[i], row.RowSet[i], rows.getLocation(), &rows.sc.syncParams)
			if err != nil {
				return err
			}
		}
	}
	return err
}

func (rows *snowflakeRows) HasNextResultSet() bool {
	if err := rows.waitForAsyncQueryStatus(); err != nil {
		return false
	}
	hasNextResultSet := rows.ChunkDownloader.getNextChunkDownloader() != nil
	logger.WithContext(rows.ctx).Debugf("[queryId: %v] Rows.HasNextResultSet: %v", rows.queryID, hasNextResultSet)
	return hasNextResultSet
}

func (rows *snowflakeRows) NextResultSet() error {
	logger.WithContext(rows.ctx).Debugf("[queryId: %v] Rows.NextResultSet", rows.queryID)
	if err := rows.waitForAsyncQueryStatus(); err != nil {
		return err
	}
	if rows.ChunkDownloader.getNextChunkDownloader() == nil {
		return io.EOF
	}
	rows.ChunkDownloader = rows.ChunkDownloader.getNextChunkDownloader()
	if err := rows.ChunkDownloader.start(); err != nil {
		return err
	}
	return nil
}

func (rows *snowflakeRows) waitForAsyncQueryStatus() error {
	// if async query, block until query is finished
	switch rows.status {
	case QueryStatusInProgress:
		err := <-rows.errChannel
		rows.status = QueryStatusComplete
		if err != nil {
			rows.status = QueryFailed
			rows.err = err
			return rows.err
		}
	case QueryFailed:
		return rows.err
	default:
		return nil
	}
	return nil
}

func (rows *snowflakeRows) addDownloader(newDL chunkDownloader) {
	if rows.ChunkDownloader == nil {
		rows.ChunkDownloader = newDL
		rows.tailChunkDownloader = newDL
		return
	}
	rows.tailChunkDownloader.setNextChunkDownloader(newDL)
	rows.tailChunkDownloader = newDL
}
