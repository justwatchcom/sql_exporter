package arrow

import (
	"context"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/snowflakedb/gosnowflake/v2/internal/query"
)

// contextKey is a private type for context keys used by this package.
type contextKey string

// Context keys for arrow batches configuration.
const (
	ctxArrowBatches             contextKey = "ARROW_BATCHES"
	ctxArrowBatchesTimestampOpt contextKey = "ARROW_BATCHES_TIMESTAMP_OPTION"
	ctxArrowBatchesUtf8Validate contextKey = "ENABLE_ARROW_BATCHES_UTF8_VALIDATION"
	ctxHigherPrecision          contextKey = "ENABLE_HIGHER_PRECISION"
)

// --- Timestamp option ---

// TimestampOption controls how Snowflake timestamps are converted in arrow batches.
type TimestampOption int

const (
	// UseNanosecondTimestamp converts Snowflake timestamps to arrow timestamps with nanosecond precision.
	UseNanosecondTimestamp TimestampOption = iota
	// UseMicrosecondTimestamp converts Snowflake timestamps to arrow timestamps with microsecond precision.
	UseMicrosecondTimestamp
	// UseMillisecondTimestamp converts Snowflake timestamps to arrow timestamps with millisecond precision.
	UseMillisecondTimestamp
	// UseSecondTimestamp converts Snowflake timestamps to arrow timestamps with second precision.
	UseSecondTimestamp
	// UseOriginalTimestamp leaves Snowflake timestamps in their original format without conversion.
	UseOriginalTimestamp
)

// --- Context accessors ---

// EnableArrowBatches sets the arrow batches mode flag in the context.
func EnableArrowBatches(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxArrowBatches, true)
}

// BatchesEnabled checks if arrow batches mode is enabled.
func BatchesEnabled(ctx context.Context) bool {
	v := ctx.Value(ctxArrowBatches)
	if v == nil {
		return false
	}
	d, ok := v.(bool)
	return ok && d
}

// WithTimestampOption sets the arrow batches timestamp option in the context.
func WithTimestampOption(ctx context.Context, option TimestampOption) context.Context {
	return context.WithValue(ctx, ctxArrowBatchesTimestampOpt, option)
}

// GetTimestampOption returns the timestamp option from the context.
func GetTimestampOption(ctx context.Context) TimestampOption {
	v := ctx.Value(ctxArrowBatchesTimestampOpt)
	if v == nil {
		return UseNanosecondTimestamp
	}
	o, ok := v.(TimestampOption)
	if !ok {
		return UseNanosecondTimestamp
	}
	return o
}

// EnableUtf8Validation enables UTF-8 validation for arrow batch string columns.
func EnableUtf8Validation(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxArrowBatchesUtf8Validate, true)
}

// Utf8ValidationEnabled checks if UTF-8 validation is enabled.
func Utf8ValidationEnabled(ctx context.Context) bool {
	v := ctx.Value(ctxArrowBatchesUtf8Validate)
	if v == nil {
		return false
	}
	d, ok := v.(bool)
	return ok && d
}

// WithHigherPrecision enables higher precision mode in the context.
func WithHigherPrecision(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxHigherPrecision, true)
}

// HigherPrecisionEnabled checks if higher precision is enabled.
func HigherPrecisionEnabled(ctx context.Context) bool {
	v := ctx.Value(ctxHigherPrecision)
	if v == nil {
		return false
	}
	d, ok := v.(bool)
	return ok && d
}

// BatchRaw holds raw (untransformed) arrow records for a single batch.
type BatchRaw struct {
	Records  *[]arrow.Record
	Index    int
	RowCount int
	Location *time.Location
	Download func(ctx context.Context) (*[]arrow.Record, int, error)
}

// BatchDataInfo contains all information needed to build arrow batches.
type BatchDataInfo struct {
	Batches   []BatchRaw
	RowTypes  []query.ExecResponseRowType
	Allocator memory.Allocator
	Ctx       context.Context
	QueryID   string
}

// BatchDataProvider is implemented by SnowflakeRows to expose raw arrow batch data.
type BatchDataProvider interface {
	GetArrowBatches() (*BatchDataInfo, error)
}
