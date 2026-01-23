package rows

import (
	"context"

	"github.com/apache/arrow/go/v12/arrow"
)

type Rows interface {
	GetArrowBatches(context.Context) (ArrowBatchIterator, error)
}

type ArrowBatchIterator interface {
	// Retrieve the next arrow.Record.
	// Will return io.EOF if there are no more records
	Next() (arrow.Record, error)

	// Return true if the iterator contains more batches, false otherwise.
	HasNext() bool

	// Release any resources in use by the iterator.
	Close()
}
