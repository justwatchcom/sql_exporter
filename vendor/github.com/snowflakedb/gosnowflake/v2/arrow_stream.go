package gosnowflake

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/snowflakedb/gosnowflake/v2/internal/query"
)

// ArrowStreamLoader is a convenience interface for downloading
// Snowflake results via multiple Arrow Record Batch streams.
//
// Some queries from Snowflake do not return Arrow data regardless
// of the settings, such as "SHOW WAREHOUSES" or SQL stored procedures
// using CALL with RETURNS TABLE(). In these cases the server returns
// JSON even though the client requested Arrow. Small JSON results are
// accessible via JSONData(); column metadata is available via RowTypes().
//
// To determine the actual response format, use QueryResultFormatProvider.
type ArrowStreamLoader interface {
	GetBatches() ([]ArrowStreamBatch, error)
	NextResultSet(ctx context.Context) error
	TotalRows() int64
	RowTypes() []query.ExecResponseRowType
	Location() *time.Location
	JSONData() [][]*string
}

// QueryResultFormatProvider is an optional interface that an
// ArrowStreamLoader may implement to expose the server-reported result
// format. The returned value is typically "arrow" or "json".
// Callers should check the format before interpreting data from
// GetBatches, as the batch content may be Arrow IPC or JSON depending
// on the statement type.
//
//	if p, ok := loader.(QueryResultFormatProvider); ok {
//	    fmt.Println(p.QueryResultFormat())
//	}
type QueryResultFormatProvider interface {
	QueryResultFormat() string
}

// ArrowStreamBatch is a type describing a potentially yet-to-be-downloaded
// chunk of query result data. The content format depends on the current
// QueryResultFormat: Arrow IPC record batches (use ipc.NewReader) when
// the format is "arrow", or JSON (row fragments) when it is "json".
type ArrowStreamBatch struct {
	idx        int
	numrows    int64
	scd        *snowflakeArrowStreamChunkDownloader
	Loc        *time.Location
	rr         io.ReadCloser
	inlineData []byte // non-nil for inline (RowSetBase64) batches
}

// NumRows returns the total number of rows that the metadata stated should
// be in this stream of record batches.
func (asb *ArrowStreamBatch) NumRows() int64 { return asb.numrows }

// Reset closes any existing stream and clears the cached reader, allowing
// GetStream to re-download the chunk on the next call. This enables callers
// to retry after a mid-stream failure (e.g. TCP RST) without re-executing
// the entire query.
//
// For inline batches (those produced from RowSetBase64 in the initial
// query response), there is no remote chunk to re-download. In that
// case Reset rewinds the reader by re-wrapping the cached inline bytes
// so the batch can be read again from the beginning. This is done
// even when the underlying Close returns an error, so the batch is
// always left in a usable state for retry; the close error is still
// returned to the caller.
func (asb *ArrowStreamBatch) Reset() error {
	var closeErr error
	if asb.rr != nil {
		closeErr = asb.rr.Close()
		asb.rr = nil
	}
	if asb.inlineData != nil {
		asb.rr = io.NopCloser(bytes.NewReader(asb.inlineData))
	}
	return closeErr
}

// GetStream downloads the chunk (if not already cached) and returns a
// stream of bytes. The content may be Arrow IPC or JSON (row fragments)
// depending on the current QueryResultFormat. Close should be called
// on the returned stream when done to ensure no leaked memory.
//
// If a previous stream failed mid-read, call Reset() first to clear the
// cached reader and allow re-download.
func (asb *ArrowStreamBatch) GetStream(ctx context.Context) (io.ReadCloser, error) {
	if asb.rr == nil {
		if err := asb.downloadChunkStreamHelper(ctx); err != nil {
			return nil, err
		}
	}
	return newCancelableStream(ctx, asb.rr), nil
}

// streamWrapReader wraps an io.Reader so that Close closes the underlying body.
type streamWrapReader struct {
	io.Reader
	wrapped io.ReadCloser
}

func (w *streamWrapReader) Close() error {
	if cl, ok := w.Reader.(io.ReadCloser); ok {
		if err := cl.Close(); err != nil {
			return err
		}
	}
	return w.wrapped.Close()
}

// interrupt closes the raw response body directly. This bypasses gzip.Reader
// cleanup because the gzip layer may still be blocked waiting for more bytes,
// while closing the transport body is what unblocks the in-flight read.
func (w *streamWrapReader) interrupt() error {
	return w.wrapped.Close()
}

// interruptibleReader lets cancelableStream bypass wrapper cleanup and close
// the raw transport body that actually unblocks a stalled read.
type interruptibleReader interface {
	interrupt() error
}

// cancelableStream watches the stream context so cancellation can interrupt a
// stalled read, normalizes terminal transport errors caused by that interrupt
// to ctx.Err(), and still preserves true successful completion and EOF.
type cancelableStream struct {
	ctx context.Context

	inner     io.ReadCloser
	interrupt func() error

	done          chan struct{}
	stopOnce      sync.Once
	interruptOnce sync.Once
	completed     atomic.Bool
}

func newCancelableStream(ctx context.Context, inner io.ReadCloser) io.ReadCloser {
	out := &cancelableStream{
		ctx:       ctx,
		inner:     inner,
		interrupt: inner.Close,
		done:      make(chan struct{}),
	}
	if interruptible, ok := inner.(interruptibleReader); ok {
		out.interrupt = interruptible.interrupt
	}
	if ctx.Done() == nil {
		return out
	}

	go func() {
		select {
		case <-ctx.Done():
			if out.watchingStopped() || out.completed.Load() {
				return
			}
			out.interruptOnce.Do(func() {
				_ = out.interrupt()
			})
		case <-out.done:
		}
	}()
	return out
}

func (r *cancelableStream) stopWatching() {
	r.stopOnce.Do(func() {
		close(r.done)
	})
}

func (r *cancelableStream) watchingStopped() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func (r *cancelableStream) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if err == nil {
		return n, nil
	}
	defer r.stopWatching()

	// Preserve the successful final read where io.Reader returns payload and EOF
	// together. Only terminal reads with no delivered bytes are candidates for
	// cancellation rewriting.
	if err == io.EOF && n > 0 {
		r.completed.Store(true)
		return n, err
	}

	if normalized := r.canceledReadErr(); normalized != nil {
		return n, normalized
	}

	if err == io.EOF {
		r.completed.Store(true)
	}
	return n, err
}

func (r *cancelableStream) canceledReadErr() error {
	ctxErr := r.ctx.Err()
	if ctxErr == nil || r.completed.Load() {
		return nil
	}
	// A canceled context wins over terminal transport errors even if the watcher
	// goroutine has not yet unblocked the read before the terminal error arrives.
	return ctxErr
}

func (r *cancelableStream) Close() error {
	r.stopWatching()
	return r.inner.Close()
}

func (asb *ArrowStreamBatch) downloadChunkStreamHelper(ctx context.Context) error {
	headers := make(map[string]string)
	if len(asb.scd.ChunkHeader) > 0 {
		maps.Copy(headers, asb.scd.ChunkHeader)
	} else {
		headers[headerSseCAlgorithm] = headerSseCAes
		headers[headerSseCKey] = asb.scd.Qrmk
	}

	resp, err := asb.scd.FuncGet(ctx, asb.scd.sc, asb.scd.ChunkMetas[asb.idx].URL, headers, asb.scd.sc.rest.RequestTimeout)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		defer func() {
			_ = resp.Body.Close()
		}()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		_ = b
		return &SnowflakeError{
			Number:      ErrFailedToGetChunk,
			SQLState:    SQLStateConnectionFailure,
			Message:     fmt.Sprintf("failed to get chunk. idx: %v", asb.idx),
			MessageArgs: []any{asb.idx},
		}
	}

	defer func() {
		if asb.rr == nil {
			_ = resp.Body.Close()
		}
	}()

	bufStream := bufio.NewReader(resp.Body)
	gzipMagic, err := bufStream.Peek(2)
	if err != nil {
		return err
	}

	if gzipMagic[0] == 0x1f && gzipMagic[1] == 0x8b {
		bufStream0, err := gzip.NewReader(bufStream)
		if err != nil {
			return err
		}
		asb.rr = &streamWrapReader{Reader: bufStream0, wrapped: resp.Body}
	} else {
		asb.rr = &streamWrapReader{Reader: bufStream, wrapped: resp.Body}
	}
	return nil
}

type snowflakeArrowStreamChunkDownloader struct {
	sc                *snowflakeConn
	ChunkMetas        []query.ExecResponseChunk
	Total             int64
	Qrmk              string
	ChunkHeader       map[string]string
	FuncGet           func(context.Context, *snowflakeConn, string, map[string]string, time.Duration) (*http.Response, error)
	RowSet            rowSetType
	resultIDs         []string
	queryResultFormat string
}

func (scd *snowflakeArrowStreamChunkDownloader) Location() *time.Location {
	if scd.sc != nil {
		return getCurrentLocation(&scd.sc.syncParams)
	}
	return nil
}

func (scd *snowflakeArrowStreamChunkDownloader) TotalRows() int64 { return scd.Total }

func (scd *snowflakeArrowStreamChunkDownloader) RowTypes() []query.ExecResponseRowType {
	return scd.RowSet.RowType
}

func (scd *snowflakeArrowStreamChunkDownloader) JSONData() [][]*string {
	return scd.RowSet.JSON
}

// QueryResultFormat returns the server-reported result format for the
// current result set (typically "arrow" or "json"). Callers should
// check this before interpreting batch streams from GetBatches, as
// the content may be Arrow IPC or JSON depending on the statement type.
// The value is updated on each NextResultSet call.
func (scd *snowflakeArrowStreamChunkDownloader) QueryResultFormat() string {
	return scd.queryResultFormat
}

func (scd *snowflakeArrowStreamChunkDownloader) maybeFirstBatch() ([]byte, error) {
	if scd.RowSet.RowSetBase64 == "" {
		return nil, nil
	}

	rowSetBytes, err := base64.StdEncoding.DecodeString(scd.RowSet.RowSetBase64)
	if err != nil {
		logger.Warnf("skipping first batch as it is not a valid base64 response. %v", err)
		return nil, err
	}

	rr, err := ipc.NewReader(bytes.NewReader(rowSetBytes))
	if err != nil {
		logger.Warnf("skipping first batch as it is not a valid IPC stream. %v", err)
		return nil, err
	}
	rr.Release()

	return rowSetBytes, nil
}

func (scd *snowflakeArrowStreamChunkDownloader) GetBatches() (out []ArrowStreamBatch, err error) {
	chunkMetaLen := len(scd.ChunkMetas)
	loc := scd.Location()

	out = make([]ArrowStreamBatch, chunkMetaLen, chunkMetaLen+1)
	toFill := out
	rowSetBytes, err := scd.maybeFirstBatch()
	if err != nil {
		return nil, err
	}
	if len(rowSetBytes) > 0 {
		out = out[:chunkMetaLen+1]
		out[0] = ArrowStreamBatch{
			scd:        scd,
			Loc:        loc,
			rr:         io.NopCloser(bytes.NewReader(rowSetBytes)),
			inlineData: rowSetBytes,
		}
		toFill = out[1:]
	}

	var totalCounted int64
	for i := range toFill {
		toFill[i] = ArrowStreamBatch{
			idx:     i,
			numrows: int64(scd.ChunkMetas[i].RowCount),
			Loc:     loc,
			scd:     scd,
		}
		totalCounted += int64(scd.ChunkMetas[i].RowCount)
	}

	if len(rowSetBytes) > 0 {
		out[0].numrows = scd.Total - totalCounted
	}
	return
}

func (scd *snowflakeArrowStreamChunkDownloader) NextResultSet(ctx context.Context) error {
	if !scd.hasNextResultSet() {
		return io.EOF
	}
	resultID := scd.resultIDs[0]
	scd.resultIDs = scd.resultIDs[1:]
	resultPath := fmt.Sprintf(urlQueriesResultFmt, resultID)
	resp, err := scd.sc.getQueryResultResp(ctx, resultPath)
	if err != nil {
		return err
	}
	if !resp.Success {
		code, err := strconv.Atoi(resp.Code)
		if err != nil {
			logger.WithContext(ctx).Errorf("error while parsing code: %v", err)
		}
		return exceptionTelemetry(&SnowflakeError{
			Number:   code,
			SQLState: resp.Data.SQLState,
			Message:  resp.Message,
			QueryID:  resp.Data.QueryID,
		}, scd.sc)
	}
	scd.ChunkMetas = resp.Data.Chunks
	scd.Total = resp.Data.Total
	scd.Qrmk = resp.Data.Qrmk
	scd.ChunkHeader = resp.Data.ChunkHeaders
	scd.queryResultFormat = resp.Data.QueryResultFormat
	scd.RowSet = rowSetType{
		RowType:      resp.Data.RowType,
		JSON:         resp.Data.RowSet,
		RowSetBase64: resp.Data.RowSetBase64,
	}
	return nil
}

func (scd *snowflakeArrowStreamChunkDownloader) hasNextResultSet() bool {
	return len(scd.resultIDs) > 0
}
