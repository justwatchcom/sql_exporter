package arrowbased

import (
	"context"
	"io"

	"github.com/apache/arrow/go/v12/arrow"
	"github.com/databricks/databricks-sql-go/internal/cli_service"
	"github.com/databricks/databricks-sql-go/internal/config"
	dbsqlerr "github.com/databricks/databricks-sql-go/internal/errors"
	"github.com/databricks/databricks-sql-go/internal/rows/rowscanner"
	"github.com/databricks/databricks-sql-go/rows"
)

func NewArrowRecordIterator(ctx context.Context, rpi rowscanner.ResultPageIterator, bi BatchIterator, arrowSchemaBytes []byte, cfg config.Config) rows.ArrowBatchIterator {
	ari := arrowRecordIterator{
		cfg:                cfg,
		batchIterator:      bi,
		resultPageIterator: rpi,
		ctx:                ctx,
		arrowSchemaBytes:   arrowSchemaBytes,
	}

	return &ari

}

// A type implemented DBSQLArrowBatchIterator
type arrowRecordIterator struct {
	ctx                context.Context
	cfg                config.Config
	batchIterator      BatchIterator
	resultPageIterator rowscanner.ResultPageIterator
	currentBatch       SparkArrowBatch
	isFinished         bool
	arrowSchemaBytes   []byte
}

var _ rows.ArrowBatchIterator = (*arrowRecordIterator)(nil)

// Retrieve the next arrow record
func (ri *arrowRecordIterator) Next() (arrow.Record, error) {
	if !ri.HasNext() {
		// returning EOF indicates that there are no more records to iterate
		return nil, io.EOF
	}

	// make sure we have the current batch
	err := ri.getCurrentBatch()
	if err != nil {
		return nil, err
	}

	// return next record in current batch
	r, err := ri.currentBatch.Next()

	ri.checkFinished()

	return r, err
}

// Indicate whether there are any more records available
func (ri *arrowRecordIterator) HasNext() bool {
	ri.checkFinished()
	return !ri.isFinished
}

// Free any resources associated with this iterator
func (ri *arrowRecordIterator) Close() {
	if !ri.isFinished {
		ri.isFinished = true
		if ri.currentBatch != nil {
			ri.currentBatch.Close()
		}

		if ri.batchIterator != nil {
			ri.batchIterator.Close()
		}

		if ri.resultPageIterator != nil {
			ri.resultPageIterator.Close()
		}
	}
}

func (ri *arrowRecordIterator) checkFinished() {
	finished := ri.isFinished ||
		((ri.currentBatch == nil || !ri.currentBatch.HasNext()) &&
			(ri.batchIterator == nil || !ri.batchIterator.HasNext()) &&
			(ri.resultPageIterator == nil || !ri.resultPageIterator.HasNext()))

	if finished {
		// Reached end of result set so Close
		ri.Close()
	}
}

// Update the current batch if necessary
func (ri *arrowRecordIterator) getCurrentBatch() error {

	// only need to update if no current batch or current batch has no more records
	if ri.currentBatch == nil || !ri.currentBatch.HasNext() {

		// ensure up to date batch iterator
		err := ri.getBatchIterator()
		if err != nil {
			return err
		}

		// release current batch
		if ri.currentBatch != nil {
			ri.currentBatch.Close()
		}

		// Get next batch from batch iterator
		ri.currentBatch, err = ri.batchIterator.Next()
		if err != nil {
			return err
		}
	}

	return nil
}

// Update batch iterator if necessary
func (ri *arrowRecordIterator) getBatchIterator() error {
	// only need to update if there is no batch iterator or the
	// batch iterator has no more batches
	if ri.batchIterator == nil || !ri.batchIterator.HasNext() {
		if ri.batchIterator != nil {
			// release any resources held by the current batch iterator
			ri.batchIterator.Close()
			ri.batchIterator = nil
		}

		// Get the next page of the result set
		resp, err := ri.resultPageIterator.Next()
		if err != nil {
			return err
		}

		// Check the result format
		resultFormat := resp.ResultSetMetadata.GetResultFormat()
		if resultFormat != cli_service.TSparkRowSetType_ARROW_BASED_SET && resultFormat != cli_service.TSparkRowSetType_URL_BASED_SET {
			return dbsqlerr.NewDriverError(ri.ctx, errArrowRowsNotArrowFormat, nil)
		}

		if ri.arrowSchemaBytes == nil {
			ri.arrowSchemaBytes = resp.ResultSetMetadata.ArrowSchema
		}

		// Create a new batch iterator for the batches in the result page
		bi, err := ri.newBatchIterator(resp)
		if err != nil {
			return err
		}

		ri.batchIterator = bi
	}

	return nil
}

// Create a new batch iterator from a page of the result set
func (ri *arrowRecordIterator) newBatchIterator(fr *cli_service.TFetchResultsResp) (BatchIterator, error) {
	rowSet := fr.Results
	if len(rowSet.ResultLinks) > 0 {
		return NewCloudBatchIterator(ri.ctx, rowSet.ResultLinks, rowSet.StartRowOffset, &ri.cfg)
	} else {
		return NewLocalBatchIterator(ri.ctx, rowSet.ArrowBatches, rowSet.StartRowOffset, ri.arrowSchemaBytes, &ri.cfg)
	}
}
