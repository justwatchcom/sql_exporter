package gosnowflake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	sfconfig "github.com/snowflakedb/gosnowflake/v2/internal/config"
)

func (sc *snowflakeConn) isClientSessionKeepAliveEnabled() bool {
	v, ok := sc.syncParams.get(sessionClientSessionKeepAlive)
	if !ok {
		return false
	}
	return strings.Compare(*v, "true") == 0
}

func (sc *snowflakeConn) getClientSessionKeepAliveHeartbeatFrequency() (time.Duration, bool) {
	v, ok := sc.syncParams.get(sessionClientSessionKeepAliveHeartbeatFrequency)

	if !ok {
		return 0, false
	}

	num, err := strconv.Atoi(*v)
	if err != nil {
		logger.Warnf("Failed to parse client session keepalive heartbeat frequency: %v. Falling back to default.", err)
		return 0, false
	}

	return time.Duration(num) * time.Second, true
}

func (sc *snowflakeConn) startHeartBeat() {
	if sc.cfg != nil && !sc.isClientSessionKeepAliveEnabled() {
		return
	}
	if sc.rest != nil {
		if heartbeatFrequency, ok := sc.getClientSessionKeepAliveHeartbeatFrequency(); ok {
			sc.rest.HeartBeat = newHeartBeat(sc.rest, heartbeatFrequency)
		} else {
			sc.rest.HeartBeat = newDefaultHeartBeat(sc.rest)
		}
		logger.WithContext(sc.ctx).Debug("Start heart beat")
		sc.rest.HeartBeat.start()
	}
}

func (sc *snowflakeConn) stopHeartBeat() {
	if sc.cfg != nil && !sc.isClientSessionKeepAliveEnabled() {
		return
	}
	if sc.rest != nil && sc.rest.HeartBeat != nil {
		logger.WithContext(sc.ctx).Debug("Stop heart beat")
		sc.rest.HeartBeat.stop()
	}
}

func (sc *snowflakeConn) getArrayBindStageThreshold() int {
	v, ok := sc.syncParams.get(sessionArrayBindStageThreshold)
	if !ok {
		return 0
	}
	num, err := strconv.Atoi(*v)
	if err != nil {
		return 0
	}
	return num
}

func (sc *snowflakeConn) connectionTelemetry(cfg *Config) {
	data := &telemetryData{
		Message: map[string]string{
			typeKey:          connectionParameters,
			sourceKey:        telemetrySource,
			driverTypeKey:    "Go",
			driverVersionKey: SnowflakeGoDriverVersion,
			golangVersionKey: runtime.Version(),
		},
		Timestamp: time.Now().UnixNano() / int64(time.Millisecond),
	}
	maps.Insert(data.Message, sc.syncParams.All())
	if err := sc.telemetry.addLog(data); err != nil {
		logger.WithContext(sc.ctx).Warnf("cannot add telemetry log: %v", err)
	}
	if err := sc.telemetry.sendBatch(); err != nil {
		logger.WithContext(sc.ctx).Warnf("cannot send telemetry batch: %v", err)
	}
}

// connectionIdentifierShapeTelemetry queues a single in-band telemetry record
// describing the shape of the connection-identifier input the user supplied.
// It does not send a batch on its own; the caller is expected to flush
// telemetry shortly after (see OpenWithConfig). When shape capture has not
// run, or the env-var kill switch is set, this is a no-op.
//
// TODO(SNOW-3548350): remove this method and its call site after the
// connection-identifier-shape data collection wraps up (target: 2026-11-30).
func (sc *snowflakeConn) connectionIdentifierShapeTelemetry(cfg *Config) {
	if connectionShapeTelemetryDisabledByEnv() {
		logger.WithContext(sc.ctx).Debug("connection-identifier-shape telemetry disabled via " + disableConnectionShapeEnv)
		return
	}
	shape := sfconfig.InputShapeOf(cfg)
	if shape == nil {
		logger.WithContext(sc.ctx).Debug("connection-identifier-shape telemetry skipped: shape not captured")
		return
	}
	data := &telemetryData{
		Message: map[string]string{
			typeKey:               connectionIdentifierShape,
			sourceKey:             telemetrySource,
			driverTypeKey:         "Go",
			driverVersionKey:      SnowflakeGoDriverVersion,
			golangVersionKey:      runtime.Version(),
			accountProvidedKey:    strconv.FormatBool(shape.AccountProvided),
			accountWithRegionKey:  strconv.FormatBool(shape.AccountWithRegion),
			accountOrgProvidedKey: strconv.FormatBool(shape.AccountOrgProvided),
			regionProvidedKey:     strconv.FormatBool(shape.RegionProvided),
			hostProvidedKey:       strconv.FormatBool(shape.HostProvided),
		},
		Timestamp: time.Now().UnixNano() / int64(time.Millisecond),
	}
	if err := sc.telemetry.addLog(data); err != nil {
		logger.WithContext(sc.ctx).Warnf("cannot add connection-identifier-shape telemetry log: %v", err)
	}
}

func connectionShapeTelemetryDisabledByEnv() bool {
	// CHANGELOG documents the value as exactly "true". Match minicore's
	// SF_DISABLE_MINICORE convention by accepting case-insensitive "true"
	// only — no truthy aliases.
	return strings.EqualFold(os.Getenv(disableConnectionShapeEnv), "true")
}

// processFileTransfer creates a snowflakeFileTransferAgent object to process
// any PUT/GET commands with their specified options
func (sc *snowflakeConn) processFileTransfer(
	ctx context.Context,
	data *execResponse,
	query string,
	isInternal bool) (
	*execResponse, error) {
	options := &SnowflakeFileTransferOptions{}
	sfa := snowflakeFileTransferAgent{
		ctx:          ctx,
		sc:           sc,
		data:         &data.Data,
		command:      query,
		options:      options,
		streamBuffer: new(bytes.Buffer),
	}
	fs, err := getFileStream(ctx)
	if err != nil {
		return nil, err
	}
	if fs != nil {
		sfa.sourceStream = fs
		if isInternal {
			sfa.data.AutoCompress = false
		}
	}
	if op := getFileTransferOptions(ctx); op != nil {
		sfa.options = op
	}
	if sfa.options.MultiPartThreshold == 0 {
		sfa.options.MultiPartThreshold = multiPartThreshold
		// for streaming download, use a smaller default part size
		if sfa.commandType == downloadCommand && isFileGetStream(ctx) {
			sfa.options.MultiPartThreshold = streamingMultiPartThreshold
		}
	}
	if err := sfa.execute(); err != nil {
		return nil, err
	}
	data, err = sfa.result()
	if err != nil {
		return nil, err
	}
	if sfa.options != nil && isFileGetStream(ctx) {
		if err := writeFileStream(ctx, sfa.streamBuffer); err != nil {
			return nil, err
		}
	}
	return data, nil
}

func getFileStream(ctx context.Context) (io.Reader, error) {
	s := ctx.Value(filePutStream)
	if s == nil {
		return nil, nil
	}
	r, ok := s.(io.Reader)
	if !ok {
		return nil, errors.New("incorrect io.Reader")
	}
	return r, nil
}

func isFileGetStream(ctx context.Context) bool {
	v := ctx.Value(fileGetStream)
	return v != nil
}

func getFileTransferOptions(ctx context.Context) *SnowflakeFileTransferOptions {
	v := ctx.Value(fileTransferOptions)
	if v == nil {
		return nil
	}
	o, ok := v.(*SnowflakeFileTransferOptions)
	if !ok {
		return nil
	}
	return o
}

func writeFileStream(ctx context.Context, streamBuf *bytes.Buffer) error {
	s := ctx.Value(fileGetStream)
	w, ok := s.(io.Writer)
	if !ok {
		return errors.New("expected an io.Writer")
	}
	_, err := streamBuf.WriteTo(w)
	if err != nil {
		return err
	}
	return nil
}

func (sc *snowflakeConn) populateSessionParameters(parameters []nameValueParameter) {
	// other session parameters (not all)
	logger.WithContext(sc.ctx).Tracef("params: %#v", parameters)
	for _, param := range parameters {
		v := ""
		switch param.Value.(type) {
		case int64:
			if vv, ok := param.Value.(int64); ok {
				v = strconv.FormatInt(vv, 10)
			}
		case float64:
			if vv, ok := param.Value.(float64); ok {
				v = strconv.FormatFloat(vv, 'g', -1, 64)
			}
		case bool:
			if vv, ok := param.Value.(bool); ok {
				v = strconv.FormatBool(vv)
			}
		default:
			if vv, ok := param.Value.(string); ok {
				v = vv
			}
		}
		logger.WithContext(sc.ctx).Tracef("parameter. name: %v, value: %v", param.Name, v)
		sc.syncParams.set(strings.ToLower(param.Name), &v)
	}
}

func (sc *snowflakeConn) configureTelemetry() {
	telemetryEnabled, ok := sc.syncParams.get("client_telemetry_enabled")
	// In-band telemetry is enabled by default on the backend side.
	if ok && telemetryEnabled != nil && *telemetryEnabled == "true" {
		sc.telemetry.flushSize = defaultFlushSize
		sc.telemetry.sr = sc.rest
		sc.telemetry.mutex = &sync.Mutex{}
		sc.telemetry.enabled = true
	}
}

func isAsyncMode(ctx context.Context) bool {
	return isBooleanContextEnabled(ctx, asyncMode)
}

func isDescribeOnly(ctx context.Context) bool {
	return isBooleanContextEnabled(ctx, describeOnly)
}

func isInternal(ctx context.Context) bool {
	return isBooleanContextEnabled(ctx, internalQuery)
}

func isLogQueryTextEnabled(ctx context.Context) bool {
	return isBooleanContextEnabled(ctx, logQueryText)
}

func isLogQueryParametersEnabled(ctx context.Context) bool {
	return isBooleanContextEnabled(ctx, logQueryParameters)
}

func isBooleanContextEnabled(ctx context.Context, key ContextKey) bool {
	v := ctx.Value(key)
	if v == nil {
		return false
	}
	d, ok := v.(bool)
	return ok && d
}

func setResultType(ctx context.Context, resType resultType) context.Context {
	return context.WithValue(ctx, snowflakeResultType, resType)
}

func getResultType(ctx context.Context) resultType {
	return ctx.Value(snowflakeResultType).(resultType)
}

// isDml returns true if the statement type code is in the range of DML.
func isDml(v int64) bool {
	return statementTypeIDDml <= v && v <= statementTypeIDMultiTableInsert
}

func isDql(data *execResponseData) bool {
	return data.StatementTypeID == statementTypeIDSelect && !isMultiStmt(data)
}

func updateRows(data execResponseData) (int64, error) {
	if data.RowSet == nil {
		return 0, nil
	}
	var count int64
	for i, n := 0, len(data.RowType); i < n; i++ {
		v, err := strconv.ParseInt(*data.RowSet[0][i], 10, 64)
		if err != nil {
			return -1, err
		}
		count += v
	}
	return count, nil
}

// isMultiStmt returns true if the statement code is of type multistatement
// Note that the statement type code is also equivalent to type INSERT, so an
// additional check of the name is required
func isMultiStmt(data *execResponseData) bool {
	var isMultistatementByReturningSelect = data.StatementTypeID == statementTypeIDSelect && data.RowType[0].Name == "multiple statement execution"
	return isMultistatementByReturningSelect || data.StatementTypeID == statementTypeIDMultistatement
}

func getResumeQueryID(ctx context.Context) (string, error) {
	val := ctx.Value(fetchResultByID)
	if val == nil {
		return "", nil
	}
	strVal, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("failed to cast val %+v to string", val)
	}
	// so there is a queryID in context for which we want to fetch the result
	if !queryIDRegexp.MatchString(strVal) {
		return strVal, &SnowflakeError{
			Number:  ErrQueryIDFormat,
			Message: "Invalid QID",
			QueryID: strVal,
		}
	}
	return strVal, nil
}

// returns snowflake chunk downloader by default or stream based chunk
// downloader if option provided through context
func populateChunkDownloader(
	ctx context.Context,
	sc *snowflakeConn,
	data execResponseData) chunkDownloader {

	return &snowflakeChunkDownloader{
		sc:                 sc,
		ctx:                ctx,
		pool:               getAllocator(ctx),
		CurrentChunk:       make([]chunkRowType, len(data.RowSet)),
		ChunkMetas:         data.Chunks,
		Total:              data.Total,
		TotalRowIndex:      int64(-1),
		CellCount:          len(data.RowType),
		Qrmk:               data.Qrmk,
		QueryResultFormat:  data.QueryResultFormat,
		ChunkHeader:        data.ChunkHeaders,
		FuncDownload:       downloadChunk,
		FuncDownloadHelper: downloadChunkHelper,
		FuncGet:            getChunk,
		RowSet: rowSetType{
			RowType:      data.RowType,
			JSON:         data.RowSet,
			RowSetBase64: data.RowSetBase64,
		},
	}
}

// checkIsPrivateLink reports whether host is a recognized PrivateLink endpoint.
// It normalizes first, then gates on an LDH character allowlist and a
// recognized Snowflake domain suffix before checking for the ".privatelink."
// label — a bare substring check on the raw host would accept hosts whose
// trailing labels are not a Snowflake apex.
func checkIsPrivateLink(host string) bool {
	normalized := sfconfig.NormalizeHost(host)
	return sfconfig.IsLdhHost(normalized) &&
		sfconfig.IsSnowflakeHost(normalized) &&
		strings.Contains(normalized, ".privatelink.")
}

func isStatementContext(ctx context.Context) bool {
	v := ctx.Value(executionType)
	return v == executionTypeStatement
}
