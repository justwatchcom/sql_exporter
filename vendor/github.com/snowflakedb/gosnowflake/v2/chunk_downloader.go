package gosnowflake

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	errors2 "github.com/snowflakedb/gosnowflake/v2/internal/errors"
	"github.com/snowflakedb/gosnowflake/v2/internal/query"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	ia "github.com/snowflakedb/gosnowflake/v2/internal/arrow"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

var (
	errNoConnection = errors.New("failed to retrieve connection")
)

type chunkDownloader interface {
	totalUncompressedSize() (acc int64)
	start() error
	next() (chunkRowType, error)
	reset()
	getChunkMetas() []query.ExecResponseChunk
	getQueryResultFormat() resultFormat
	getRowType() []query.ExecResponseRowType
	setNextChunkDownloader(downloader chunkDownloader)
	getNextChunkDownloader() chunkDownloader
	getRawArrowBatches() []*rawArrowBatchData
}

type snowflakeChunkDownloader struct {
	sc                 *snowflakeConn
	ctx                context.Context
	pool               memory.Allocator
	Total              int64
	TotalRowIndex      int64
	CellCount          int
	CurrentChunk       []chunkRowType
	CurrentChunkIndex  int
	CurrentChunkSize   int
	CurrentIndex       int
	ChunkHeader        map[string]string
	ChunkMetas         []query.ExecResponseChunk
	Chunks             map[int][]chunkRowType
	ChunksChan         chan int
	ChunksError        chan *chunkError
	ChunksErrorCounter int
	ChunksFinalErrors  []*chunkError
	ChunksMutex        *sync.Mutex
	DoneDownloadCond   *sync.Cond
	firstBatchRaw      *rawArrowBatchData
	NextDownloader     chunkDownloader
	Qrmk               string
	QueryResultFormat  string
	rawBatches         []*rawArrowBatchData
	RowSet             rowSetType
	FuncDownload       func(context.Context, *snowflakeChunkDownloader, int)
	FuncDownloadHelper func(context.Context, *snowflakeChunkDownloader, int) error
	FuncGet            func(context.Context, *snowflakeConn, string, map[string]string, time.Duration) (*http.Response, error)
}

func (scd *snowflakeChunkDownloader) totalUncompressedSize() (acc int64) {
	for _, c := range scd.ChunkMetas {
		acc += c.UncompressedSize
	}
	return
}

func (scd *snowflakeChunkDownloader) start() error {
	if usesArrowBatches(scd.ctx) && scd.getQueryResultFormat() == arrowFormat {
		return scd.startArrowBatches()
	}
	scd.CurrentChunkSize = len(scd.RowSet.JSON) // cache the size
	scd.CurrentIndex = -1                       // initial chunks idx
	scd.CurrentChunkIndex = -1                  // initial chunk

	scd.CurrentChunk = make([]chunkRowType, scd.CurrentChunkSize)
	populateJSONRowSet(scd.CurrentChunk, scd.RowSet.JSON)

	if scd.getQueryResultFormat() == arrowFormat && scd.RowSet.RowSetBase64 != "" {
		params, err := scd.getConfigParams()
		if err != nil {
			return fmt.Errorf("getting config params: %w", err)
		}
		// if the rowsetbase64 retrieved from the server is empty, move on to downloading chunks
		loc := getCurrentLocation(params)
		firstArrowChunk, err := buildFirstArrowChunk(scd.RowSet.RowSetBase64, loc, scd.pool)
		if err != nil {
			return fmt.Errorf("building first arrow chunk: %w", err)
		}
		higherPrecision := higherPrecisionEnabled(scd.ctx)
		scd.CurrentChunk, err = firstArrowChunk.decodeArrowChunk(scd.ctx, scd.RowSet.RowType, higherPrecision, params)
		scd.CurrentChunkSize = firstArrowChunk.rowCount
		if err != nil {
			return fmt.Errorf("decoding arrow chunk: %w", err)
		}
	}

	// start downloading chunks if exists
	chunkMetaLen := len(scd.ChunkMetas)
	if chunkMetaLen > 0 {
		chunkDownloadWorkers := defaultMaxChunkDownloadWorkers
		chunkDownloadWorkersStr, ok := scd.sc.syncParams.get(clientPrefetchThreadsKey)
		if ok {
			var err error
			chunkDownloadWorkers, err = strconv.Atoi(*chunkDownloadWorkersStr)
			if err != nil {
				logger.Warnf("invalid value for CLIENT_PREFETCH_THREADS: %v", *chunkDownloadWorkersStr)
				chunkDownloadWorkers = defaultMaxChunkDownloadWorkers
			}
		}
		if chunkDownloadWorkers <= 0 {
			logger.Warnf("invalid value for CLIENT_PREFETCH_THREADS: %v. It should be a positive integer. Defaulting to %v", chunkDownloadWorkers, defaultMaxChunkDownloadWorkers)
			chunkDownloadWorkers = defaultMaxChunkDownloadWorkers
		}

		logger.WithContext(scd.ctx).Debugf("chunkDownloadWorkers: %v", chunkDownloadWorkers)
		logger.WithContext(scd.ctx).Debugf("chunks: %v, total bytes: %d", chunkMetaLen, scd.totalUncompressedSize())
		scd.ChunksMutex = &sync.Mutex{}
		scd.DoneDownloadCond = sync.NewCond(scd.ChunksMutex)
		scd.Chunks = make(map[int][]chunkRowType)
		scd.ChunksChan = make(chan int, chunkMetaLen)
		scd.ChunksError = make(chan *chunkError, chunkDownloadWorkers)
		for i := range chunkMetaLen {
			chunk := scd.ChunkMetas[i]
			logger.WithContext(scd.ctx).Debugf("Result Format: %v, add chunk to channel ChunksChan: %v, URL: %v, RowCount: %v, UncompressedSize: %v, ChunkResultFormat: %v",
				scd.getQueryResultFormat(), i+1, chunk.URL, chunk.RowCount, chunk.UncompressedSize, scd.QueryResultFormat)
			scd.ChunksChan <- i
		}
		for i := 0; i < min(chunkDownloadWorkers, chunkMetaLen); i++ {
			scd.schedule()
		}
	}
	return nil
}

func (scd *snowflakeChunkDownloader) schedule() {
	timer := time.Now()
	select {
	case nextIdx := <-scd.ChunksChan:
		logger.WithContext(scd.ctx).Infof("schedule chunk: %v", nextIdx+1)
		go GoroutineWrapper(
			scd.ctx,
			func() {
				scd.FuncDownload(scd.ctx, scd, nextIdx)
			},
		)
	default:
		// no more download
		chunkCount := len(scd.ChunkMetas)
		avgTime := 0.0
		if chunkCount > 0 {
			avgTime = float64(time.Since(timer)) / float64(chunkCount)
		}
		logger.WithContext(scd.ctx).Infof("Processed %v chunks. It took %v ms, average chunk processing time: %v ms", len(scd.ChunkMetas), time.Since(timer).String(), avgTime)
	}
}

func (scd *snowflakeChunkDownloader) checkErrorRetry() error {
	select {
	case errc := <-scd.ChunksError:
		if scd.ChunksErrorCounter >= maxChunkDownloaderErrorCounter ||
			errors.Is(errc.Error, context.Canceled) ||
			errors.Is(errc.Error, context.DeadlineExceeded) {

			scd.ChunksFinalErrors = append(scd.ChunksFinalErrors, errc)
			logger.WithContext(scd.ctx).Warnf("chunk idx: %v, err: %v. no further retry", errc.Index, errc.Error)
			return errc.Error
		}

		// add the index to the chunks channel so that the download will be retried.
		go GoroutineWrapper(
			scd.ctx,
			func() {
				scd.FuncDownload(scd.ctx, scd, errc.Index)
			},
		)
		scd.ChunksErrorCounter++
		logger.WithContext(scd.ctx).Warnf("chunk idx: %v, err: %v. retrying (%v/%v)...",
			errc.Index, errc.Error, scd.ChunksErrorCounter, maxChunkDownloaderErrorCounter)
		return nil
	default:
		logger.WithContext(scd.ctx).Info("no error is detected.")
		return nil
	}
}

func (scd *snowflakeChunkDownloader) next() (chunkRowType, error) {
	for {
		scd.CurrentIndex++
		if scd.CurrentIndex < scd.CurrentChunkSize {
			return scd.CurrentChunk[scd.CurrentIndex], nil
		}
		scd.CurrentChunkIndex++ // next chunk
		scd.CurrentIndex = -1   // reset
		if scd.CurrentChunkIndex >= len(scd.ChunkMetas) {
			break
		}

		scd.ChunksMutex.Lock()
		if scd.CurrentChunkIndex > 0 {
			scd.Chunks[scd.CurrentChunkIndex-1] = nil // detach the previously used chunk
		}

		for scd.Chunks[scd.CurrentChunkIndex] == nil {
			logger.WithContext(scd.ctx).Debugf("waiting for chunk idx: %v/%v",
				scd.CurrentChunkIndex+1, len(scd.ChunkMetas))

			if err := scd.checkErrorRetry(); err != nil {
				scd.ChunksMutex.Unlock()
				return chunkRowType{}, fmt.Errorf("checking for error: %w", err)
			}

			// wait for chunk downloader goroutine to broadcast the event,
			// 1) one chunk download finishes or 2) an error occurs.
			scd.DoneDownloadCond.Wait()
		}
		logger.WithContext(scd.ctx).Debugf("ready: chunk %v", scd.CurrentChunkIndex+1)
		scd.CurrentChunk = scd.Chunks[scd.CurrentChunkIndex]
		scd.ChunksMutex.Unlock()
		scd.CurrentChunkSize = len(scd.CurrentChunk)

		// kick off the next download
		scd.schedule()
	}

	logger.WithContext(scd.ctx).Debugf("no more data")
	if len(scd.ChunkMetas) > 0 {
		close(scd.ChunksError)
		close(scd.ChunksChan)
	}
	return chunkRowType{}, io.EOF
}

func (scd *snowflakeChunkDownloader) reset() {
	scd.Chunks = nil // detach all chunks. No way to go backward without reinitialize it.
}

func (scd *snowflakeChunkDownloader) getChunkMetas() []query.ExecResponseChunk {
	return scd.ChunkMetas
}

func (scd *snowflakeChunkDownloader) getQueryResultFormat() resultFormat {
	return resultFormat(scd.QueryResultFormat)
}

func (scd *snowflakeChunkDownloader) setNextChunkDownloader(nextDownloader chunkDownloader) {
	scd.NextDownloader = nextDownloader
}

func (scd *snowflakeChunkDownloader) getNextChunkDownloader() chunkDownloader {
	return scd.NextDownloader
}

func (scd *snowflakeChunkDownloader) getRowType() []query.ExecResponseRowType {
	return scd.RowSet.RowType
}

// rawArrowBatchData holds raw (untransformed) arrow records for a single batch.
type rawArrowBatchData struct {
	records  *[]arrow.Record
	rowCount int
	loc      *time.Location
}

func (scd *snowflakeChunkDownloader) getRawArrowBatches() []*rawArrowBatchData {
	if scd.firstBatchRaw == nil || scd.firstBatchRaw.records == nil {
		return scd.rawBatches
	}
	return append([]*rawArrowBatchData{scd.firstBatchRaw}, scd.rawBatches...)
}

// releaseRawArrowBatches releases any raw arrow records still owned by the
// chunk downloader. Records whose ownership was transferred to BatchRaw
// (via GetArrowBatches) will already have been nilled out and are skipped.
func (scd *snowflakeChunkDownloader) releaseRawArrowBatches() {
	releaseRecords := func(raw *rawArrowBatchData) {
		if raw == nil || raw.records == nil {
			return
		}
		for _, rec := range *raw.records {
			rec.Release()
		}
		raw.records = nil
	}
	releaseRecords(scd.firstBatchRaw)
	for _, raw := range scd.rawBatches {
		releaseRecords(raw)
	}
}

func (scd *snowflakeChunkDownloader) getConfigParams() (*syncParams, error) {
	if scd.sc == nil || scd.sc.cfg == nil {
		return nil, errNoConnection
	}
	return &scd.sc.syncParams, nil
}

func getChunk(
	ctx context.Context,
	sc *snowflakeConn,
	fullURL string,
	headers map[string]string,
	timeout time.Duration) (
	*http.Response, error,
) {
	u, err := url.Parse(fullURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %w", err)
	}
	return newRetryHTTP(ctx, sc.rest.Client, http.NewRequest, u, headers, timeout, sc.rest.MaxRetryCount, sc.currentTimeProvider, sc.cfg).execute()
}

func (scd *snowflakeChunkDownloader) startArrowBatches() error {
	var loc *time.Location
	params, err := scd.getConfigParams()
	if err != nil {
		return fmt.Errorf("getting config params: %w", err)
	}
	loc = getCurrentLocation(params)
	if scd.RowSet.RowSetBase64 != "" {
		firstArrowChunk, err := buildFirstArrowChunk(scd.RowSet.RowSetBase64, loc, scd.pool)
		if err != nil {
			return fmt.Errorf("building first arrow chunk: %w", err)
		}
		scd.firstBatchRaw = &rawArrowBatchData{
			loc: loc,
		}
		if firstArrowChunk.allocator != nil {
			scd.firstBatchRaw.records, err = firstArrowChunk.decodeArrowBatchRaw()
			if err != nil {
				return fmt.Errorf("decoding arrow batch: %w", err)
			}
			scd.firstBatchRaw.rowCount = countRawArrowBatchRows(scd.firstBatchRaw.records)
		}
	}
	chunkMetaLen := len(scd.ChunkMetas)
	scd.rawBatches = make([]*rawArrowBatchData, chunkMetaLen)
	for i := range scd.rawBatches {
		scd.rawBatches[i] = &rawArrowBatchData{
			loc:      loc,
			rowCount: scd.ChunkMetas[i].RowCount,
		}
		scd.CurrentChunkIndex++
	}
	return nil
}

/* largeResultSetReader is a reader that wraps the large result set with leading and tailing brackets. */
type largeResultSetReader struct {
	status int
	body   io.Reader
}

func (r *largeResultSetReader) Read(p []byte) (n int, err error) {
	if r.status == 0 {
		p[0] = 0x5b // initial 0x5b ([)
		r.status = 1
		return 1, nil
	}
	if r.status == 1 {
		var len int
		len, err = r.body.Read(p)
		if err == io.EOF {
			r.status = 2
			return len, nil
		}
		if err != nil {
			return 0, fmt.Errorf("reading body: %w", err)
		}
		return len, nil
	}
	if r.status == 2 {
		p[0] = 0x5d // tail 0x5d (])
		r.status = 3
		return 1, nil
	}
	// ensure no data and EOF
	return 0, io.EOF
}

func downloadChunk(ctx context.Context, scd *snowflakeChunkDownloader, idx int) {
	logger.WithContext(ctx).Infof("download start chunk: %v", idx+1)
	defer scd.DoneDownloadCond.Broadcast()

	timer := time.Now()
	if err := scd.FuncDownloadHelper(ctx, scd, idx); err != nil {
		logger.WithContext(ctx).Errorf(
			"failed to extract HTTP response body. URL: %v, err: %v", scd.ChunkMetas[idx].URL, err)
		scd.ChunksError <- &chunkError{Index: idx, Error: err}
	} else if errors.Is(scd.ctx.Err(), context.Canceled) || errors.Is(scd.ctx.Err(), context.DeadlineExceeded) {
		scd.ChunksError <- &chunkError{Index: idx, Error: scd.ctx.Err()}
	}
	elapsedTime := time.Since(timer).String()
	logger.Debugf("“Processed %v chunk %v out of %v. It took %v ms. Chunk size: %v, rows: %v”.", scd.getQueryResultFormat(), idx+1, len(scd.ChunkMetas), elapsedTime, scd.ChunkMetas[idx].UncompressedSize, scd.ChunkMetas[idx].RowCount)
}

func downloadChunkHelper(ctx context.Context, scd *snowflakeChunkDownloader, idx int) error {
	headers := make(map[string]string)
	if len(scd.ChunkHeader) > 0 {
		logger.WithContext(ctx).Debug("chunk header is provided.")
		for k, v := range scd.ChunkHeader {
			logger.WithContext(ctx).Debugf("adding header: %v", k)

			headers[k] = v
		}
	} else {
		headers[headerSseCAlgorithm] = headerSseCAes
		headers[headerSseCKey] = scd.Qrmk
	}

	resp, err := scd.FuncGet(ctx, scd.sc, scd.ChunkMetas[idx].URL, headers, scd.sc.rest.RequestTimeout)
	if err != nil {
		return fmt.Errorf("getting chunk: %w", err)
	}
	body := newCancelableStream(ctx, resp.Body)
	defer func() {
		if err = body.Close(); err != nil {
			logger.Warnf("downloadChunkHelper: closing response body %v: %v", scd.ChunkMetas[idx].URL, err)
		}
	}()
	logger.WithContext(ctx).Debugf("response returned chunk: %v for URL: %v", idx+1, scd.ChunkMetas[idx].URL)
	if resp.StatusCode != http.StatusOK {
		b, err := io.ReadAll(body)
		if err != nil {
			logger.WithContext(ctx).Errorf("reading response body: %v", err)
		}
		logger.WithContext(ctx).Debugf("HTTP: %v, URL: %v, Header: %v, Body: %v", resp.StatusCode, scd.ChunkMetas[idx].URL, resp.Header, b)
		return &SnowflakeError{
			Number:      ErrFailedToGetChunk,
			SQLState:    SQLStateConnectionFailure,
			Message:     errors2.ErrMsgFailedToGetChunk,
			MessageArgs: []any{idx},
		}
	}

	bufStream := bufio.NewReader(body)
	return decodeChunk(ctx, scd, idx, bufStream)
}

func decodeChunk(ctx context.Context, scd *snowflakeChunkDownloader, idx int, bufStream *bufio.Reader) error {
	gzipMagic, err := bufStream.Peek(2)
	if err != nil {
		return fmt.Errorf("peeking for gzip magic bytes: %w", err)
	}
	start := time.Now()
	var source io.Reader
	if gzipMagic[0] == 0x1f && gzipMagic[1] == 0x8b {
		// detects and uncompresses Gzip format data
		bufStream0, err := gzip.NewReader(bufStream)
		if err != nil {
			return fmt.Errorf("creating gzip reader: %w", err)
		}
		defer func() {
			if err = bufStream0.Close(); err != nil {
				logger.Warnf("decodeChunk: closing gzip reader: %v", err)
			}
		}()
		source = bufStream0
	} else {
		source = bufStream
	}
	st := &largeResultSetReader{
		status: 0,
		body:   source,
	}
	var respd []chunkRowType
	if scd.getQueryResultFormat() != arrowFormat {
		var decRespd [][]*string
		if !customJSONDecoderEnabled {
			dec := json.NewDecoder(st)
			for {
				if err := dec.Decode(&decRespd); err == io.EOF {
					break
				} else if err != nil {
					return fmt.Errorf("decoding json: %w", err)
				}
			}
		} else {
			decRespd, err = decodeLargeChunk(st, scd.ChunkMetas[idx].RowCount, scd.CellCount)
			if err != nil {
				return fmt.Errorf("decoding large chunk: %w", err)
			}
		}
		respd = make([]chunkRowType, len(decRespd))
		populateJSONRowSet(respd, decRespd)
	} else {
		ipcReader, err := ipc.NewReader(source, ipc.WithAllocator(scd.pool))
		if err != nil {
			return fmt.Errorf("creating ipc reader: %w", err)
		}
		var loc *time.Location
		params, err := scd.getConfigParams()
		if err != nil {
			return fmt.Errorf("getting config params: %w", err)
		}
		loc = getCurrentLocation(params)
		arc := arrowResultChunk{
			ipcReader,
			0,
			loc,
			scd.pool,
		}
		if usesArrowBatches(scd.ctx) {
			var err error
			scd.rawBatches[idx].records, err = arc.decodeArrowBatchRaw()
			if err != nil {
				return fmt.Errorf("decoding Arrow batch: %w", err)
			}
			scd.rawBatches[idx].rowCount = countRawArrowBatchRows(scd.rawBatches[idx].records)
			return nil
		}
		highPrec := higherPrecisionEnabled(scd.ctx)
		respd, err = arc.decodeArrowChunk(ctx, scd.RowSet.RowType, highPrec, params)
		if err != nil {
			return fmt.Errorf("decoding arrow chunk: %w", err)
		}
	}
	logger.WithContext(scd.ctx).Debugf(
		"decoded %d rows w/ %d bytes in %s (chunk %v)",
		scd.ChunkMetas[idx].RowCount,
		scd.ChunkMetas[idx].UncompressedSize,
		time.Since(start), idx+1,
	)

	scd.ChunksMutex.Lock()
	defer scd.ChunksMutex.Unlock()
	scd.Chunks[idx] = respd
	return nil
}

func populateJSONRowSet(dst []chunkRowType, src [][]*string) {
	// populate string rowset from src to dst's chunkRowType struct's RowSet field
	for i, row := range src {
		dst[i].RowSet = row
	}
}

func countRawArrowBatchRows(recs *[]arrow.Record) (cnt int) {
	if recs == nil {
		return 0
	}
	for _, r := range *recs {
		cnt += int(r.NumRows())
	}
	return
}

func getAllocator(ctx context.Context) memory.Allocator {
	pool, ok := ctx.Value(arrowAlloc).(memory.Allocator)
	if !ok {
		return memory.DefaultAllocator
	}
	return pool
}

func usesArrowBatches(ctx context.Context) bool {
	return ia.BatchesEnabled(ctx)
}
