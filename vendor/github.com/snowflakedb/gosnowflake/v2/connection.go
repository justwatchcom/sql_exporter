package gosnowflake

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	sfconfig "github.com/snowflakedb/gosnowflake/v2/internal/config"
	"github.com/snowflakedb/gosnowflake/v2/internal/errors"

	ia "github.com/snowflakedb/gosnowflake/v2/internal/arrow"
	"go.opentelemetry.io/otel/propagation"
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
	snowflakeResultType ContextKey = "snowflakeResultType"
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
	ctx                 context.Context
	cfg                 *Config
	rest                *snowflakeRestful
	sequenceCounter     uint64
	telemetry           *snowflakeTelemetry
	internal            InternalClient
	queryContextCache   queryContextCache
	currentTimeProvider currentTimeProvider
	syncParams          syncParams
	idToken             string
	mfaToken            string
}

var (
	queryIDPattern                     = `[\w\-_]+`
	queryIDRegexp                      = regexp.MustCompile(queryIDPattern)
	_              SnowflakeConnection = (*snowflakeConn)(nil)
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
	counter := atomic.AddUint64(&sc.sequenceCounter, 1) // query sequence counter
	_, _, sessionID := safeGetTokens(sc.rest)
	ctx = context.WithValue(ctx, SFSessionIDKey, sessionID)
	queryContext, err := buildQueryContext(&sc.queryContextCache)
	if err != nil {
		logger.WithContext(ctx).Errorf("error while building query context: %v", err)
	}
	req := execRequest{
		SQLText:      query,
		AsyncExec:    noResult,
		Parameters:   map[string]any{},
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

	if sn, ok := sc.syncParams.get(serviceName); ok {
		headers[httpHeaderServiceName] = *sn
	}

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

	if !sc.cfg.DisableQueryContextCache && data.Data.QueryContext != nil {
		queryContext, err := extractQueryContext(data)
		if err != nil {
			logger.WithContext(ctx).Errorf("error while decoding query context: %v", err)
		} else {
			sc.queryContextCache.add(sc, queryContext.Entries...)
		}
	}

	if !data.Success {
		err = exceptionTelemetry(populateErrorFields(code, data), sc)
		return nil, err
	}

	// handle PUT/GET commands
	fileTransferChan := make(chan error, 1)
	if isFileTransfer(query) && !isSkipFileTransfer(ctx) {
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
		return nil, exceptionTelemetry(&SnowflakeError{
			Number:   ErrNoReadOnlyTransaction,
			SQLState: SQLStateFeatureNotSupported,
			Message:  errors.ErrMsgNoReadOnlyTransaction,
		}, sc)
	}
	if int(opts.Isolation) != int(sql.LevelDefault) {
		return nil, exceptionTelemetry(&SnowflakeError{
			Number:   ErrNoDefaultTransactionIsolationLevel,
			SQLState: SQLStateFeatureNotSupported,
			Message:  errors.ErrMsgNoDefaultTransactionIsolationLevel,
		}, sc)
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
	logger.WithContext(sc.ctx).Info("Closing connection")
	if err := sc.telemetry.sendBatch(); err != nil {
		logger.WithContext(sc.ctx).Warnf("error while sending telemetry. %v", err)
	}
	sc.stopHeartBeat()
	sc.rest.HeartBeat = nil
	defer sc.cleanup()

	if sc.cfg != nil && !sc.cfg.ServerSessionKeepAlive {
		logger.WithContext(sc.ctx).Debug("Closing session since ServerSessionKeepAlive is false")
		// we have to strip cancellation, otherwise we can use a ctx that is cancelled or timed out
		// We reset the request ID so the abort gets its own fresh one instead of reusing the aborted query's
		// ID (which WithRequestID would otherwise leak into the abort request URL).
		closeCtx := context.WithValue(context.WithoutCancel(cmp.Or(sc.ctx, context.Background())), snowflakeRequestIDKey, nilUUID)
		if sc.cfg.CleanupTimeout > 0 {
			// Bound the logout so Close does not block past CleanupTimeout. Stays
			// synchronous to honor the io.Closer expectation of blocking on close.
			var cancelClose context.CancelFunc
			closeCtx, cancelClose = context.WithTimeout(closeCtx, sc.cfg.CleanupTimeout)
			defer cancelClose()
		}
		if err = sc.rest.FuncCloseSession(closeCtx, sc.rest, sc.rest.RequestTimeout); err != nil {
			logger.WithContext(sc.ctx).Errorf("error while closing session: %v", err)
		}
	} else {
		logger.WithContext(sc.ctx).Info("Skipping session close since ServerSessionKeepAlive is true")
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
			return nil, exceptionTelemetry(&SnowflakeError{
				Number:   code,
				SQLState: data.Data.SQLState,
				Message:  err.Error(),
				QueryID:  data.Data.QueryID,
			}, sc)
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
			return nil, exceptionTelemetry(&SnowflakeError{
				Number:   code,
				SQLState: data.Data.SQLState,
				Message:  err.Error(),
				QueryID:  data.Data.QueryID,
			}, sc)
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
		SQLText:      queryRet.SQLText,
		StartTime:    queryRet.StartTime,
		EndTime:      queryRet.EndTime,
		ErrorCode:    queryRet.ErrorCode,
		ErrorMessage: queryRet.ErrorMessage,
		ScanBytes:    queryRet.Stats.ScanBytes,
		ProducedRows: queryRet.Stats.ProducedRows,

		TotalDuration:     queryRet.TotalDuration,
		GsCompileDuration: queryRet.GsCompileDuration,
		GsExecDuration:    queryRet.GsExecDuration,
		XpExecDuration:    queryRet.XpExecDuration,
		QueryRetryTime:    queryRet.QueryRetryTime,
		FaultHandlingTime: queryRet.FaultHandlingTime,
	}, nil
}

func (sc *snowflakeConn) AddTelemetryData(_ context.Context, eventDate time.Time, data map[string]string) error {
	td := &telemetryData{
		Timestamp: eventDate.UnixMilli(),
		Message:   data,
	}
	return sc.telemetry.addLog(td)
}

// QueryArrowStream executes a query and returns an ArrowStreamLoader for
// streaming query results. The server may respond with Arrow IPC or JSON
// depending on the statement type (e.g. CALL, SHOW). The returned loader
// also implements QueryResultFormatProvider; callers should check the
// format before passing batch streams to ipc.NewReader.
func (sc *snowflakeConn) QueryArrowStream(ctx context.Context, query string, bindings ...driver.NamedValue) (ArrowStreamLoader, error) {
	ctx = ia.EnableArrowBatches(context.WithValue(ctx, asyncMode, false))
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
			return nil, exceptionTelemetry(&SnowflakeError{
				Number:   code,
				SQLState: data.Data.SQLState,
				Message:  err.Error(),
				QueryID:  data.Data.QueryID,
			}, sc)
		}
		return nil, err
	}

	var resultIDs []string
	if len(data.Data.ResultIDs) > 0 {
		resultIDs = strings.Split(data.Data.ResultIDs, ",")
	}

	scd := &snowflakeArrowStreamChunkDownloader{
		sc:                sc,
		ChunkMetas:        data.Data.Chunks,
		Total:             data.Data.Total,
		Qrmk:              data.Data.Qrmk,
		ChunkHeader:       data.Data.ChunkHeaders,
		FuncGet:           getChunk,
		queryResultFormat: data.Data.QueryResultFormat,
		RowSet: rowSetType{
			RowType:      data.Data.RowType,
			JSON:         data.Data.RowSet,
			RowSetBase64: data.Data.RowSetBase64,
		},
		resultIDs: resultIDs,
	}

	if scd.hasNextResultSet() {
		if err = scd.NextResultSet(ctx); err != nil {
			return nil, err
		}
	}

	logger.WithContext(ctx).Debugf("QueryArrowStream: server returned resultset in %q format.", scd.queryResultFormat)
	return scd, nil
}

// buildSnowflakeConn creates a new snowflakeConn.
// The provided context is used only for establishing the initial connection.
func buildSnowflakeConn(ctx context.Context, config Config) (*snowflakeConn, error) {
	sc := &snowflakeConn{
		sequenceCounter:     0,
		ctx:                 ctx,
		cfg:                 &config,
		currentTimeProvider: defaultTimeProvider,
	}
	initPlatformDetection()
	err := initEasyLogging(config.ClientConfigFile)
	if err != nil {
		return nil, err
	}

	logger.Debugf("Building snowflakeConn: %v", fmt.Sprintf("host: %v, account: %v, user: %v, password existed: %v, role: %v, database: %v, schema: %v, warehouse: %v, %v",
		config.Host, config.Account, config.User, config.Password != "", config.Role, config.Database, config.Schema, config.Warehouse, sfconfig.DescribeProxy(&config)))
	telemetry := &snowflakeTelemetry{}

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
	sc.syncParams = newSyncParams(sc.cfg.Params)

	return sc, nil
}
