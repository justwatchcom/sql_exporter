package gosnowflake

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/propagation"

	"github.com/apache/arrow-go/v18/arrow/ipc"
)

const (
	httpHeaderContentType      = "Content-Type"
	httpHeaderAccept           = "accept"
	httpHeaderUserAgent        = "User-Agent"
	httpHeaderServiceName      = "X-Snowflake-Service"
	httpHeaderContentLength    = "Content-Length"
	httpHeaderHost             = "Host"
	httpHeaderValueOctetStream = "application/octet-stream"
	httpHeaderContentEncoding  = "Content-Encoding"
	httpClientAppID            = "CLIENT_APP_ID"
	httpClientAppVersion       = "CLIENT_APP_VERSION"
)

const (
	statementTypeIDSelect           = int64(0x1000)
	statementTypeIDDml              = int64(0x3000)
	statementTypeIDMultiTableInsert = statementTypeIDDml + int64(0x500)
	statementTypeIDMultistatement   = int64(0xA000)
)

const (
	sessionClientSessionKeepAlive                   = "client_session_keep_alive"
	sessionClientSessionKeepAliveHeartbeatFrequency = "client_session_keep_alive_heartbeat_frequency"
	sessionClientValidateDefaultParameters          = "CLIENT_VALIDATE_DEFAULT_PARAMETERS"
	sessionArrayBindStageThreshold                  = "client_stage_array_binding_threshold"
	serviceName                                     = "service_name"
)

type resultType string

const (
	snowflakeResultType contextKey = "snowflakeResultType"
	execResultType      resultType = "exec"
	queryResultType     resultType = "query"
)

type execKey string

const (
	executionType          execKey = "executionType"
	executionTypeStatement string  = "statement"
)

// snowflakeConn manages its own context.
// External cancellation should not be supported because the connection
// may be reused after the original query/request has completed.
type snowflakeConn struct {
	ctx  context.Context
	cfg  *Config
	rest *snowflakeRestful
	// Deprecated: this field will be removed in future releases.
	SequenceCounter     uint64
	telemetry           *snowflakeTelemetry
	internal            InternalClient
	queryContextCache   *queryContextCache
	currentTimeProvider currentTimeProvider
}

var (
	queryIDPattern = `[\w\-_]+`
	queryIDRegexp  = regexp.MustCompile(queryIDPattern)
)

func (sc *snowflakeConn) exec(
	ctx context.Context,
	query string,
	noResult bool,
	isInternal bool,
	describeOnly bool,
	bindings []driver.NamedValue) (
	*execResponse, error) {
	if sc.cfg.LogQueryText || isLogQueryTextEnabled(ctx) {
		if len(bindings) > 0 && (sc.cfg.LogQueryParameters || isLogQueryParametersEnabled(ctx)) {
			logger.WithContext(ctx).Infof("Executing query: %v with bindings: %v", query, bindings)
		} else {
			logger.WithContext(ctx).Infof("Executing query: %v", query)
		}
	} else {
		logger.WithContext(ctx).Infof("Executing query")
	}

	var err error
	counter := atomic.AddUint64(&sc.SequenceCounter, 1) // query sequence counter
	_, _, sessionID := safeGetTokens(sc.rest)
	ctx = context.WithValue(ctx, SFSessionIDKey, sessionID)
	queryContext, err := buildQueryContext(sc.queryContextCache)
	if err != nil {
		logger.WithContext(ctx).Errorf("error while building query context: %v", err)
	}
	req := execRequest{
		SQLText:      query,
		AsyncExec:    noResult,
		Parameters:   map[string]interface{}{},
		IsInternal:   isInternal,
		DescribeOnly: describeOnly,
		SequenceID:   counter,
		QueryContext: queryContext,
	}
	if key := ctx.Value(multiStatementCount); key != nil {
		req.Parameters[string(multiStatementCount)] = key
	}
	if tag := ctx.Value(queryTag); tag != nil {
		req.Parameters[string(queryTag)] = tag
	}
	logger.WithContext(ctx).Debugf("parameters: %v", req.Parameters)

	// handle bindings, if required
	requestID := getOrGenerateRequestIDFromContext(ctx)
	if len(bindings) > 0 {
		if err = sc.processBindings(ctx, bindings, describeOnly, requestID, &req); err != nil {
			return nil, err
		}
	}
	logger.WithContext(ctx).Debugf("bindings: %v", req.Bindings)

	// populate headers
	headers := getHeaders()
	if isFileTransfer(query) {
		headers[httpHeaderAccept] = headerContentTypeApplicationJSON
	}

	// propagate traceID and spanID via traceparent header. this is a no-op if invalid IDs
	propagator := propagation.TraceContext{}
	propagator.Inject(ctx, propagation.MapCarrier(headers))

	paramsMutex.Lock()
	if serviceName, ok := sc.cfg.Params[serviceName]; ok {
		headers[httpHeaderServiceName] = *serviceName
	}
	paramsMutex.Unlock()

	jsonBody, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	data, err := sc.rest.FuncPostQuery(ctx, sc.rest, &url.Values{}, headers,
		jsonBody, sc.rest.RequestTimeout, requestID, sc.cfg)
	if err != nil {
		return data, err
	}
	code := -1
	if data.Code != "" {
		code, err = strconv.Atoi(data.Code)
		if err != nil {
			return data, err
		}
	}
	logger.WithContext(ctx).Debugf("Success: %v, Code: %v", data.Success, code)
	if !data.Success {
		err = (populateErrorFields(code, data)).exceptionTelemetry(sc)
		return nil, err
	}

	if !sc.cfg.DisableQueryContextCache && data.Data.QueryContext != nil {
		queryContext, err := extractQueryContext(data)
		if err != nil {
			logger.WithContext(ctx).Errorf("error while decoding query context: %v", err)
		} else {
			sc.queryContextCache.add(sc, queryContext.Entries...)
		}
	}

	// handle PUT/GET commands
	fileTransferChan := make(chan error, 1)
	if isFileTransfer(query) {
		go func() {
			data, err = sc.processFileTransfer(ctx, data, query, isInternal)
			fileTransferChan <- err
		}()

		select {
		case <-ctx.Done():
			logger.WithContext(ctx).Debugf("File transfer has been cancelled")
			return nil, ctx.Err()
		case err := <-fileTransferChan:
			if err != nil {
				return nil, err
			}
		}
	}

	logger.WithContext(ctx).Debugf("Exec/Query: queryId=%v SUCCESS with total=%v, returned=%v ", data.Data.QueryID, data.Data.Total, data.Data.Returned)
	if data.Data.FinalDatabaseName != "" {
		sc.cfg.Database = data.Data.FinalDatabaseName
	}
	if data.Data.FinalSchemaName != "" {
		sc.cfg.Schema = data.Data.FinalSchemaName
	}
	if data.Data.FinalWarehouseName != "" {
		sc.cfg.Warehouse = data.Data.FinalWarehouseName
	}
	if data.Data.FinalRoleName != "" {
		sc.cfg.Role = data.Data.FinalRoleName
	}
	sc.populateSessionParameters(data.Data.Parameters)
	return data, err
}

func extractQueryContext(data *execResponse) (queryContext, error) {
	var queryContext queryContext
	err := json.Unmarshal(data.Data.QueryContext, &queryContext)
	return queryContext, err
}

func buildQueryContext(qcc *queryContextCache) (requestQueryContext, error) {
	rqc := requestQueryContext{}
	if qcc == nil || len(qcc.entries) == 0 {
		logger.Debugf("empty qcc")
		return rqc, nil
	}
	for _, qce := range qcc.entries {
		contextData := contextData{}
		if qce.Context == "" {
			contextData.Base64Data = qce.Context
		}
		rqc.Entries = append(rqc.Entries, requestQueryContextEntry{
			ID:        qce.ID,
			Priority:  qce.Priority,
			Timestamp: qce.Timestamp,
			Context:   contextData,
		})
	}
	return rqc, nil
}

func (sc *snowflakeConn) Begin() (driver.Tx, error) {
	return sc.BeginTx(context.Background(), driver.TxOptions{})
}

func (sc *snowflakeConn) BeginTx(
	ctx context.Context,
	opts driver.TxOptions) (
	driver.Tx, error) {
	logger.WithContext(ctx).Debug("BeginTx")
	if opts.ReadOnly {
		return nil, (&SnowflakeError{
			Number:   ErrNoReadOnlyTransaction,
			SQLState: SQLStateFeatureNotSupported,
			Message:  errMsgNoReadOnlyTransaction,
		}).exceptionTelemetry(sc)
	}
	if int(opts.Isolation) != int(sql.LevelDefault) {
		return nil, (&SnowflakeError{
			Number:   ErrNoDefaultTransactionIsolationLevel,
			SQLState: SQLStateFeatureNotSupported,
			Message:  errMsgNoDefaultTransactionIsolationLevel,
		}).exceptionTelemetry(sc)
	}
	if sc.rest == nil {
		return nil, driver.ErrBadConn
	}
	isDesc := isDescribeOnly(ctx)
	isInternal := isInternal(ctx)
	if _, err := sc.exec(ctx, "BEGIN", false, /* noResult */
		isInternal, isDesc, nil); err != nil {
		return nil, err
	}
	return &snowflakeTx{sc, ctx}, nil
}

func (sc *snowflakeConn) cleanup() {
	// must flush log buffer while the process is running.
	logger.WithContext(sc.ctx).Debug("Snowflake connection closing.")
	if sc.rest != nil && sc.rest.Client != nil {
		sc.rest.Client.CloseIdleConnections()
	}
}

func (sc *snowflakeConn) Close() (err error) {
	logger.WithContext(sc.ctx).Infoln("Close")
	if err := sc.telemetry.sendBatch(); err != nil {
		logger.WithContext(sc.ctx).Warnf("error while sending telemetry. %v", err)
	}
	sc.stopHeartBeat()
	sc.rest.HeartBeat = nil
	defer sc.cleanup()

	if sc.cfg != nil && !sc.cfg.KeepSessionAlive {
		// we have to replace context with background, otherwise we can use a one that is cancelled or timed out
		if err = sc.rest.FuncCloseSession(context.Background(), sc.rest, sc.rest.RequestTimeout); err != nil {
			logger.WithContext(sc.ctx).Error(err)
		}
	}
	return nil
}

func (sc *snowflakeConn) PrepareContext(
	ctx context.Context,
	query string) (
	driver.Stmt, error) {
	logger.WithContext(sc.ctx).Debugf("Prepare Context")
	if sc.rest == nil {
		return nil, driver.ErrBadConn
	}
	stmt := &snowflakeStmt{
		sc:    sc,
		query: query,
	}
	return stmt, nil
}

func (sc *snowflakeConn) ExecContext(
	ctx context.Context,
	query string,
	args []driver.NamedValue) (
	driver.Result, error) {
	if sc.rest == nil {
		return nil, driver.ErrBadConn
	}
	_, _, sessionID := safeGetTokens(sc.rest)
	ctx = context.WithValue(ctx, SFSessionIDKey, sessionID)
	logger.WithContext(ctx).Debug("ExecContext:")
	noResult := isAsyncMode(ctx)
	isDesc := isDescribeOnly(ctx)
	isInternal := isInternal(ctx)
	ctx = setResultType(ctx, execResultType)
	data, err := sc.exec(ctx, query, noResult, isInternal, isDesc, args)
	if err != nil {
		logger.WithContext(ctx).Errorf("error: %v", err)
		if data != nil {
			code, e := strconv.Atoi(data.Code)
			if e != nil {
				return nil, e
			}
			return nil, (&SnowflakeError{
				Number:   code,
				SQLState: data.Data.SQLState,
				Message:  err.Error(),
				QueryID:  data.Data.QueryID,
			}).exceptionTelemetry(sc)
		}
		return nil, err
	}

	// if async exec, return result object right away
	if noResult {
		return data.Data.AsyncResult, nil
	}

	if isDml(data.Data.StatementTypeID) {
		// collects all values from the returned row sets
		updatedRows, err := updateRows(data.Data)
		if err != nil {
			return nil, err
		}
		logger.WithContext(ctx).Debugf("number of updated rows: %#v", updatedRows)
		return &snowflakeResult{
			affectedRows: updatedRows,
			insertID:     -1,
			queryID:      data.Data.QueryID,
		}, nil // last insert id is not supported by Snowflake
	} else if isMultiStmt(&data.Data) {
		return sc.handleMultiExec(ctx, data.Data)
	} else if isDql(&data.Data) {
		logger.WithContext(ctx).Debug("This query is DQL")
		if isStatementContext(ctx) {
			return &snowflakeResultNoRows{queryID: data.Data.QueryID}, nil
		}
		return driver.ResultNoRows, nil
	}
	logger.WithContext(ctx).Debug("This query is DDL")
	if isStatementContext(ctx) {
		return &snowflakeResultNoRows{queryID: data.Data.QueryID}, nil
	}
	return driver.ResultNoRows, nil
}

func (sc *snowflakeConn) QueryContext(
	ctx context.Context,
	query string,
	args []driver.NamedValue) (
	driver.Rows, error) {
	qid, err := getResumeQueryID(ctx)
	if err != nil {
		return nil, err
	}
	if qid == "" {
		return sc.queryContextInternal(ctx, query, args)
	}

	// check the query status to find out if there is a result to fetch
	_, err = sc.checkQueryStatus(ctx, qid)
	snowflakeErr, isSnowflakeError := err.(*SnowflakeError)
	if err == nil || (isSnowflakeError && snowflakeErr.Number == ErrQueryIsRunning) {
		// the query is running. Rows object will be returned from here.
		return sc.buildRowsForRunningQuery(ctx, qid)
	}
	return nil, err
}

func (sc *snowflakeConn) queryContextInternal(
	ctx context.Context,
	query string,
	args []driver.NamedValue) (
	driver.Rows, error) {
	if sc.rest == nil {
		return nil, driver.ErrBadConn
	}

	_, _, sessionID := safeGetTokens(sc.rest)
	ctx = context.WithValue(setResultType(ctx, queryResultType), SFSessionIDKey, sessionID)
	logger.WithContext(ctx).Debug("QueryContextInternal")
	noResult := isAsyncMode(ctx)
	isDesc := isDescribeOnly(ctx)
	isInternal := isInternal(ctx)
	data, err := sc.exec(ctx, query, noResult, isInternal, isDesc, args)
	if err != nil {
		logger.WithContext(ctx).Errorf("error: %v", err)
		if data != nil {
			code, e := strconv.Atoi(data.Code)
			if e != nil {
				return nil, e
			}
			return nil, (&SnowflakeError{
				Number:   code,
				SQLState: data.Data.SQLState,
				Message:  err.Error(),
				QueryID:  data.Data.QueryID,
			}).exceptionTelemetry(sc)
		}
		return nil, err
	}

	// if async query, return row object right away
	if noResult {
		return data.Data.AsyncRows, nil
	}

	rows := new(snowflakeRows)
	rows.sc = sc
	rows.queryID = data.Data.QueryID
	rows.ctx = ctx

	if isMultiStmt(&data.Data) {
		// handleMultiQuery is responsible to fill rows with childResults
		if err = sc.handleMultiQuery(ctx, data.Data, rows); err != nil {
			return nil, err
		}
	} else {
		rows.addDownloader(populateChunkDownloader(ctx, sc, data.Data))
	}

	err = rows.ChunkDownloader.start()
	return rows, err
}

func (sc *snowflakeConn) Prepare(query string) (driver.Stmt, error) {
	return sc.PrepareContext(context.Background(), query)
}

func (sc *snowflakeConn) Exec(
	query string,
	args []driver.Value) (
	driver.Result, error) {
	return sc.ExecContext(context.Background(), query, toNamedValues(args))
}

func (sc *snowflakeConn) Query(
	query string,
	args []driver.Value) (
	driver.Rows, error) {
	return sc.QueryContext(context.Background(), query, toNamedValues(args))
}

func (sc *snowflakeConn) Ping(ctx context.Context) error {
	logger.WithContext(ctx).Debug("Ping")
	if sc.rest == nil {
		return driver.ErrBadConn
	}
	noResult := isAsyncMode(ctx)
	isDesc := isDescribeOnly(ctx)
	isInternal := isInternal(ctx)
	ctx = setResultType(ctx, execResultType)
	_, err := sc.exec(ctx, "SELECT 1", noResult, isInternal,
		isDesc, []driver.NamedValue{})
	return err
}

// CheckNamedValue determines which types are handled by this driver aside from
// the instances captured by driver.Value
func (sc *snowflakeConn) CheckNamedValue(nv *driver.NamedValue) error {
	if supportedNullBind(nv) || supportedDecfloatBind(nv) || supportedArrayBind(nv) || supportedStructuredObjectWriterBind(nv) || supportedStructuredArrayBind(nv) || supportedStructuredMapBind(nv) {
		return nil
	}
	return driver.ErrSkip
}

func (sc *snowflakeConn) GetQueryStatus(
	ctx context.Context,
	queryID string) (
	*SnowflakeQueryStatus, error) {
	queryRet, err := sc.checkQueryStatus(ctx, queryID)
	if err != nil {
		return nil, err
	}
	return &SnowflakeQueryStatus{
		queryRet.SQLText,
		queryRet.StartTime,
		queryRet.EndTime,
		queryRet.ErrorCode,
		queryRet.ErrorMessage,
		queryRet.Stats.ScanBytes,
		queryRet.Stats.ProducedRows,
	}, nil
}

func (sc *snowflakeConn) AddTelemetryData(_ context.Context, eventDate time.Time, data map[string]string) error {
	td := &telemetryData{
		Timestamp: eventDate.UnixMilli(),
		Message:   data,
	}
	return sc.telemetry.addLog(td)
}

// QueryArrowStream returns batches which can be queried for their raw arrow
// ipc stream of bytes. This way consumers don't need to be using the exact
// same version of Arrow as the connection is using internally in order
// to consume Arrow data.
func (sc *snowflakeConn) QueryArrowStream(ctx context.Context, query string, bindings ...driver.NamedValue) (ArrowStreamLoader, error) {
	ctx = WithArrowBatches(context.WithValue(ctx, asyncMode, false))
	ctx = setResultType(ctx, queryResultType)
	isDesc := isDescribeOnly(ctx)
	isInternal := isInternal(ctx)
	data, err := sc.exec(ctx, query, false, isInternal, isDesc, bindings)
	if err != nil {
		logger.WithContext(ctx).Errorf("error: %v", err)
		if data != nil {
			code, e := strconv.Atoi(data.Code)
			if e != nil {
				return nil, e
			}
			return nil, (&SnowflakeError{
				Number:   code,
				SQLState: data.Data.SQLState,
				Message:  err.Error(),
				QueryID:  data.Data.QueryID,
			}).exceptionTelemetry(sc)
		}
		return nil, err
	}

	var resultIDs []string
	if len(data.Data.ResultIDs) > 0 {
		resultIDs = strings.Split(data.Data.ResultIDs, ",")
	}
	scd := &snowflakeArrowStreamChunkDownloader{
		sc:          sc,
		ChunkMetas:  data.Data.Chunks,
		Total:       data.Data.Total,
		Qrmk:        data.Data.Qrmk,
		ChunkHeader: data.Data.ChunkHeaders,
		FuncGet:     getChunk,
		RowSet: rowSetType{
			RowType:      data.Data.RowType,
			JSON:         data.Data.RowSet,
			RowSetBase64: data.Data.RowSetBase64,
		},
		resultIDs: resultIDs,
	}
	// if multistatement is used, we need to set the first result set to actual result set, not the aggregated response
	if scd.hasNextResultSet() {
		if err = scd.NextResultSet(ctx); err != nil {
			return nil, err
		}
	}
	return scd, nil
}

// ArrowStreamBatch is a type describing a potentially yet-to-be-downloaded
// Arrow IPC stream. Call `GetStream` to download and retrieve an io.Reader
// that can be used with ipc.NewReader to get record batch results.
type ArrowStreamBatch struct {
	idx     int
	numrows int64
	scd     *snowflakeArrowStreamChunkDownloader
	Loc     *time.Location
	rr      io.ReadCloser
}

// NumRows returns the total number of rows that the metadata stated should
// be in this stream of record batches.
func (asb *ArrowStreamBatch) NumRows() int64 { return asb.numrows }

// gzip.Reader.Close does NOT close the underlying reader, so we
// need to wrap with wrapReader so that closing will close the
// response body (or any other reader that we want to gzip uncompress)
type wrapReader struct {
	io.Reader
	wrapped io.ReadCloser
}

func (w *wrapReader) Close() error {
	if cl, ok := w.Reader.(io.ReadCloser); ok {
		if err := cl.Close(); err != nil {
			return err
		}
	}
	return w.wrapped.Close()
}

func (asb *ArrowStreamBatch) downloadChunkStreamHelper(ctx context.Context) error {
	headers := make(map[string]string)
	if len(asb.scd.ChunkHeader) > 0 {
		logger.WithContext(ctx).Debug("chunk header is provided")
		for k, v := range asb.scd.ChunkHeader {
			logger.Debugf("adding header: %v, value: %v", k, v)

			headers[k] = v
		}
	} else {
		headers[headerSseCAlgorithm] = headerSseCAes
		headers[headerSseCKey] = asb.scd.Qrmk
	}

	resp, err := asb.scd.FuncGet(ctx, asb.scd.sc, asb.scd.ChunkMetas[asb.idx].URL, headers, asb.scd.sc.rest.RequestTimeout)
	if err != nil {
		return err
	}
	logger.WithContext(ctx).Debugf("response returned chunk: %v for URL: %v", asb.idx+1, asb.scd.ChunkMetas[asb.idx].URL)
	if resp.StatusCode != http.StatusOK {
		defer func() {
			if err = resp.Body.Close(); err != nil {
				logger.WithContext(ctx).Errorf("error closing response body for %v: %v", asb.scd.ChunkMetas[asb.idx].URL, err)
			}
		}()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		logger.WithContext(ctx).Debugf("HTTP: %v, URL: %v, Body: %v", resp.StatusCode, asb.scd.ChunkMetas[asb.idx].URL, b)
		logger.WithContext(ctx).Debugf("Header: %v", resp.Header)
		return &SnowflakeError{
			Number:      ErrFailedToGetChunk,
			SQLState:    SQLStateConnectionFailure,
			Message:     errMsgFailedToGetChunk,
			MessageArgs: []interface{}{asb.idx},
		}
	}

	defer func() {
		if asb.rr == nil {
			if err = resp.Body.Close(); err != nil {
				logger.WithContext(ctx).Errorf("error closing response body for %v: %v", asb.scd.ChunkMetas[asb.idx].URL, err)
			}
		}
	}()

	bufStream := bufio.NewReader(resp.Body)
	gzipMagic, err := bufStream.Peek(2)
	if err != nil {
		return err
	}

	if gzipMagic[0] == 0x1f && gzipMagic[1] == 0x8b {
		// detect and uncompress gzip
		bufStream0, err := gzip.NewReader(bufStream)
		if err != nil {
			return err
		}
		// gzip.Reader.Close() does NOT close the underlying
		// reader, so we need to wrap it and ensure close will
		// close the response body. Otherwise we'll leak it.
		asb.rr = &wrapReader{Reader: bufStream0, wrapped: resp.Body}
	} else {
		asb.rr = &wrapReader{Reader: bufStream, wrapped: resp.Body}
	}
	return nil
}

// GetStream returns a stream of bytes consisting of an Arrow IPC Record
// batch stream. Close should be called on the returned stream when done
// to ensure no leaked memory.
func (asb *ArrowStreamBatch) GetStream(ctx context.Context) (io.ReadCloser, error) {
	if asb.rr == nil {
		if err := asb.downloadChunkStreamHelper(ctx); err != nil {
			return nil, err
		}
	}

	return asb.rr, nil
}

// ArrowStreamLoader is a convenience interface for downloading
// Snowflake results via multiple Arrow Record Batch streams.
//
// Some queries from Snowflake do not return Arrow data regardless
// of the settings, such as "SHOW WAREHOUSES". In these cases,
// you'll find TotalRows() > 0 but GetBatches returns no batches
// and no errors. In this case, the data is accessible via JSONData
// with the actual types matching up to the metadata in RowTypes.
type ArrowStreamLoader interface {
	// GetBatches returns the result for the current result set, if response was Arrow.
	// If multistatement is used, this returns the batches for the current result set.
	GetBatches() ([]ArrowStreamBatch, error)
	// NextResultSet returns an io.EOF if there are no more batches to download.
	NextResultSet(ctx context.Context) error
	// TotalRows returns the total number of rows found.
	// If multistatement is used, this is the total number of rows in the current result set.
	TotalRows() int64
	// RowTypes returns the types of the rows.
	// If multistatement is used, this is the types of the rows in the current result set.
	RowTypes() []execResponseRowType
	// Location returns the location associated with the query response.
	// If multistatement is used, this is the location of the initial query.
	Location() *time.Location
	// JSONData returns the data if JSON was returned instead of Arrow.
	// If multistatement is used, this is the data for the current result set.
	JSONData() [][]*string
}

type snowflakeArrowStreamChunkDownloader struct {
	sc          *snowflakeConn
	ChunkMetas  []execResponseChunk
	Total       int64
	Qrmk        string
	ChunkHeader map[string]string
	FuncGet     func(context.Context, *snowflakeConn, string, map[string]string, time.Duration) (*http.Response, error)
	RowSet      rowSetType
	resultIDs   []string
}

func (scd *snowflakeArrowStreamChunkDownloader) Location() *time.Location {
	if scd.sc != nil && scd.sc.cfg != nil {
		return getCurrentLocation(scd.sc.cfg.Params)
	}
	return nil
}
func (scd *snowflakeArrowStreamChunkDownloader) TotalRows() int64 { return scd.Total }
func (scd *snowflakeArrowStreamChunkDownloader) RowTypes() []execResponseRowType {
	return scd.RowSet.RowType
}
func (scd *snowflakeArrowStreamChunkDownloader) JSONData() [][]*string {
	return scd.RowSet.JSON
}

// the server might have had an empty first batch, check if we can decode
// that first batch, if not we skip it.
func (scd *snowflakeArrowStreamChunkDownloader) maybeFirstBatch() ([]byte, error) {
	if scd.RowSet.RowSetBase64 == "" {
		return nil, nil
	}

	// first batch
	rowSetBytes, err := base64.StdEncoding.DecodeString(scd.RowSet.RowSetBase64)
	if err != nil {
		// match logic in buildFirstArrowChunk
		// assume there's no first chunk if we can't decode the base64 string
		logger.Warnf("skipping first batch as it is not a valid base64 response. %v", err)
		return nil, err
	}

	// verify it's a valid ipc stream, otherwise skip it
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
	// if there was no first batch in the response from the server,
	// skip it and move on. toFill == out
	// otherwise expand out by one to account for the first batch
	// and fill it in. have toFill refer to the slice of out excluding
	// the first batch.
	if len(rowSetBytes) > 0 {
		out = out[:chunkMetaLen+1]
		out[0] = ArrowStreamBatch{
			scd: scd,
			Loc: loc,
			rr:  io.NopCloser(bytes.NewReader(rowSetBytes)),
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
		logger.Debugf("batch %v, numrows: %v", i, toFill[i].numrows)
		totalCounted += int64(scd.ChunkMetas[i].RowCount)
	}

	if len(rowSetBytes) > 0 {
		// if we had a first batch, fill in the numrows
		out[0].numrows = scd.Total - totalCounted
		logger.Debugf("first batch, numrows: %v", out[0].numrows)
	}
	return
}

func (scd *snowflakeArrowStreamChunkDownloader) NextResultSet(ctx context.Context) error {
	if !scd.hasNextResultSet() {
		logger.Debug("no more result sets to download")
		return io.EOF
	}
	resultID := scd.resultIDs[0]
	logger.Debugf("downloading next result set, resultID: %v", resultID)
	scd.resultIDs = scd.resultIDs[1:]
	resultPath := fmt.Sprintf(urlQueriesResultFmt, resultID)
	resp, err := scd.sc.getQueryResultResp(ctx, resultPath)
	if err != nil {
		return err
	}
	if !resp.Success {
		logger.WithContext(ctx).Warnf("error while getting next result set, error code: %v, message: %v", resp.Code, resp.Message)
		code, err := strconv.Atoi(resp.Code)
		if err != nil {
			logger.WithContext(ctx).Errorf("error while parsing code: %v", err)
		}
		return (&SnowflakeError{
			Number:   code,
			SQLState: resp.Data.SQLState,
			Message:  resp.Message,
			QueryID:  resp.Data.QueryID,
		}).exceptionTelemetry(scd.sc)
	}
	scd.ChunkMetas = resp.Data.Chunks
	scd.Total = resp.Data.Total
	scd.Qrmk = resp.Data.Qrmk
	scd.ChunkHeader = resp.Data.ChunkHeaders
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

// buildSnowflakeConn creates a new snowflakeConn.
// The provided context is used only for establishing the initial connection.
func buildSnowflakeConn(ctx context.Context, config Config) (*snowflakeConn, error) {
	sc := &snowflakeConn{
		SequenceCounter:     0,
		ctx:                 ctx,
		cfg:                 &config,
		queryContextCache:   (&queryContextCache{}).init(),
		currentTimeProvider: defaultTimeProvider,
	}
	initPlatformDetection()
	err := initEasyLogging(config.ClientConfigFile)
	if err != nil {
		return nil, err
	}

	logger.Debugf("Building snowflakeConn: %v", config.describeIdentityAttributes())
	telemetry := &snowflakeTelemetry{}
	if config.DisableTelemetry {
		telemetry.enabled = false
	} else {
		telemetry.flushSize = defaultFlushSize
		telemetry.sr = sc.rest
		telemetry.mutex = &sync.Mutex{}
		telemetry.enabled = true
	}

	transportFactory := newTransportFactory(&config, telemetry)
	st, err := transportFactory.createTransport(defaultTransportConfigs.forTransportType(transportTypeSnowflake))
	if err != nil {
		return nil, err
	}

	var tokenAccessor TokenAccessor
	if sc.cfg.TokenAccessor != nil {
		tokenAccessor = sc.cfg.TokenAccessor
	} else {
		tokenAccessor = getSimpleTokenAccessor()
	}

	// authenticate
	sc.rest = &snowflakeRestful{
		Host:     sc.cfg.Host,
		Port:     sc.cfg.Port,
		Protocol: sc.cfg.Protocol,
		Client: &http.Client{
			// request timeout including reading response body
			Timeout:   sc.cfg.ClientTimeout,
			Transport: st,
		},
		JWTClient: &http.Client{
			Timeout:   sc.cfg.JWTClientTimeout,
			Transport: st,
		},
		TokenAccessor:       tokenAccessor,
		LoginTimeout:        sc.cfg.LoginTimeout,
		RequestTimeout:      sc.cfg.RequestTimeout,
		MaxRetryCount:       sc.cfg.MaxRetryCount,
		FuncPost:            postRestful,
		FuncGet:             getRestful,
		FuncAuthPost:        postAuthRestful,
		FuncPostQuery:       postRestfulQuery,
		FuncPostQueryHelper: postRestfulQueryHelper,
		FuncRenewSession:    renewRestfulSession,
		FuncPostAuth:        postAuth,
		FuncCloseSession:    closeSession,
		FuncCancelQuery:     cancelQuery,
		FuncPostAuthSAML:    postAuthSAML,
		FuncPostAuthOKTA:    postAuthOKTA,
		FuncGetSSO:          getSSO,
	}

	telemetry.sr = sc.rest
	sc.telemetry = telemetry

	return sc, nil
}
