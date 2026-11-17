package gosnowflake

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"time"

	"github.com/snowflakedb/gosnowflake/v2/internal/errors"
)

const urlQueriesResultFmt = "/queries/%s/result"

// queryResultStatus is status returned from server
type queryResultStatus int

// Query Status defined at server side
const (
	// Deprecated: will be unexported in the future releases.
	SFQueryRunning queryResultStatus = iota
	// Deprecated: will be unexported in the future releases.
	SFQueryAborting
	// Deprecated: will be unexported in the future releases.
	SFQuerySuccess
	// Deprecated: will be unexported in the future releases.
	SFQueryFailedWithError
	// Deprecated: will be unexported in the future releases.
	SFQueryAborted
	// Deprecated: will be unexported in the future releases.
	SFQueryQueued
	// Deprecated: will be unexported in the future releases.
	SFQueryFailedWithIncident
	// Deprecated: will be unexported in the future releases.
	SFQueryDisconnected
	// Deprecated: will be unexported in the future releases.
	SFQueryResumingWarehouse
	// SFQueryQueueRepairingWarehouse present in QueryDTO.java.
	// Deprecated: will be unexported in the future releases.
	SFQueryQueueRepairingWarehouse
	// Deprecated: will be unexported in the future releases.
	SFQueryRestarted
	// SFQueryBlocked is when a statement is waiting on a lock on resource held
	// by another statement.
	// Deprecated: will be unexported in the future releases.
	SFQueryBlocked
	// Deprecated: will be unexported in the future releases.
	SFQueryNoData
)

func (qs queryResultStatus) String() string {
	return [...]string{"RUNNING", "ABORTING", "SUCCESS", "FAILED_WITH_ERROR",
		"ABORTED", "QUEUED", "FAILED_WITH_INCIDENT", "DISCONNECTED",
		"RESUMING_WAREHOUSE", "QUEUED_REPAIRING_WAREHOUSE", "RESTARTED",
		"BLOCKED", "NO_DATA"}[qs]
}

func (qs queryResultStatus) isRunning() bool {
	switch qs {
	case SFQueryRunning, SFQueryResumingWarehouse, SFQueryQueued,
		SFQueryQueueRepairingWarehouse, SFQueryRestarted, SFQueryNoData:
		return true
	default:
		return false
	}
}

func (qs queryResultStatus) isError() bool {
	switch qs {
	case SFQueryAborting, SFQueryFailedWithError, SFQueryAborted,
		SFQueryFailedWithIncident, SFQueryDisconnected, SFQueryBlocked:
		return true
	default:
		return false
	}
}

var strQueryStatusMap = map[string]queryResultStatus{"RUNNING": SFQueryRunning,
	"ABORTING": SFQueryAborting, "SUCCESS": SFQuerySuccess,
	"FAILED_WITH_ERROR": SFQueryFailedWithError, "ABORTED": SFQueryAborted,
	"QUEUED": SFQueryQueued, "FAILED_WITH_INCIDENT": SFQueryFailedWithIncident,
	"DISCONNECTED":               SFQueryDisconnected,
	"RESUMING_WAREHOUSE":         SFQueryResumingWarehouse,
	"QUEUED_REPAIRING_WAREHOUSE": SFQueryQueueRepairingWarehouse,
	"RESTARTED":                  SFQueryRestarted,
	"BLOCKED":                    SFQueryBlocked, "NO_DATA": SFQueryNoData}

type retStatus struct {
	Status       string   `json:"status"`
	SQLText      string   `json:"sqlText"`
	StartTime    int64    `json:"startTime"`
	EndTime      int64    `json:"endTime"`
	ErrorCode    string   `json:"errorCode"`
	ErrorMessage string   `json:"errorMessage"`
	Stats        retStats `json:"stats"`

	TotalDuration     int64 `json:"totalDuration"`
	GsCompileDuration int64 `json:"gsCompileDuration"`
	GsExecDuration    int64 `json:"gsExecDuration"`
	XpExecDuration    int64 `json:"xpExecDuration"`
	QueryRetryTime    int64 `json:"queryRetryTime"`
	FaultHandlingTime int64 `json:"faultHandlingTime"`
}

type retStats struct {
	ScanBytes    int64 `json:"scanBytes"`
	ProducedRows int64 `json:"producedRows"`
}

type statusResponse struct {
	Data struct {
		Queries []retStatus `json:"queries"`
	} `json:"data"`
	Message string `json:"message"`
	Code    string `json:"code"`
	Success bool   `json:"success"`
}

func strToQueryStatus(in string) queryResultStatus {
	return strQueryStatusMap[in]
}

// SnowflakeQueryStatus is the query status metadata of a snowflake query
type SnowflakeQueryStatus struct {
	SQLText      string
	StartTime    int64
	EndTime      int64
	ErrorCode    string
	ErrorMessage string
	ScanBytes    int64
	ProducedRows int64

	TotalDuration     int64
	GsCompileDuration int64
	GsExecDuration    int64
	XpExecDuration    int64
	QueryRetryTime    int64
	FaultHandlingTime int64
}

// SnowflakeConnection is a wrapper to snowflakeConn that exposes API functions
type SnowflakeConnection interface {
	GetQueryStatus(ctx context.Context, queryID string) (*SnowflakeQueryStatus, error)
	AddTelemetryData(ctx context.Context, eventDate time.Time, data map[string]string) error
	// DownloadStream downloads one stage object as a readable stream.
	// stageName is an @-stage name or a snow:// embedded-stage URL; see the
	// package documentation.
	// ctx is used for the metadata GET and the cloud body: canceling it aborts a
	// blocked Read. The returned reader must be closed. Concurrent Read and Close
	// are not supported. When used via database/sql Conn.Raw, consume and close
	// the reader before returning from the Raw callback so the connection is not
	// released while the body is still open. ctx must remain valid until Close.
	// With Decompress: true, an invalid gzip header is reported as
	// ErrDownloadStreamDecompress by the first Read, not by
	// DownloadStreamWithConfig. Truncated gzip may fail on a later Read.
	DownloadStream(ctx context.Context, stageName, sourceFileName string) (io.ReadCloser, error)
	DownloadStreamWithConfig(ctx context.Context, stageName, sourceFileName string, config DownloadStreamConfig) (io.ReadCloser, error)
	// UploadStream uploads one stage object from src. destFileName is a basename;
	// directories belong in UploadStreamConfig.DestPrefix. Compression defaults
	// to gzip. The driver reads src and does not close it; the caller closes it
	// after UploadStream returns. Canceling ctx fails the next read of src,
	// aborts an in-flight cloud PUT, and stops the storage retry loop, including
	// its backoff sleep. A Read already blocked inside src is not interrupted.
	// The dummy PUT only reserves the stage object. The payload is spooled, then
	// uploaded, outside that statement. When used via database/sql Conn.Raw,
	// return from the callback only after UploadStream returns so the connection
	// is not released during the upload. The payload stays in memory until
	// 128 MiB, then spills to disk.
	UploadStream(ctx context.Context, stageName, destFileName string, src io.Reader) error
	UploadStreamWithConfig(ctx context.Context, stageName, destFileName string, src io.Reader, config UploadStreamConfig) error
}

// checkQueryStatus returns the status given the query ID. If successful,
// the error will be nil, indicating there is a complete query result to fetch.
// Other than nil, there are three error types that can be returned:
// 1. ErrQueryStatus, if GS cannot return any kind of status due to any reason,
// i.e. connection, permission, if a query was just submitted, etc.
// 2, ErrQueryReportedError, if the requested query was terminated or aborted
// and GS returned an error status included in query. SFQueryFailedWithError
// 3, ErrQueryIsRunning, if the requested query is still running and might have
// a complete result later, these statuses were listed in query. SFQueryRunning
func (sc *snowflakeConn) checkQueryStatus(
	ctx context.Context,
	qid string) (
	*retStatus, error) {
	headers := make(map[string]string)
	param := make(url.Values)
	param.Set(requestGUIDKey, NewUUID().String())
	if tok, _, _ := sc.rest.TokenAccessor.GetTokens(); tok != "" {
		headers[headerAuthorizationKey] = fmt.Sprintf(headerSnowflakeToken, tok)
	}
	resultPath := fmt.Sprintf("%s/%s", monitoringQueriesPath, qid)
	url := sc.rest.getFullURL(resultPath, &param)

	res, err := sc.rest.FuncGet(ctx, sc.rest, url, headers, sc.rest.RequestTimeout)
	if err != nil {
		logger.WithContext(ctx).Errorf("failed to get response. err: %v", err)
		return nil, err
	}
	defer func() {
		if err = res.Body.Close(); err != nil {
			logger.WithContext(ctx).Warnf("failed to close response body. err: %v", err)
		}
	}()
	var statusResp = statusResponse{}
	if err = json.NewDecoder(res.Body).Decode(&statusResp); err != nil {
		logger.WithContext(ctx).Errorf("failed to decode JSON. err: %v", err)
		return nil, err
	}

	if !statusResp.Success || len(statusResp.Data.Queries) == 0 {
		logger.WithContext(ctx).Errorf("status query returned not-success or no status returned.")
		return nil, exceptionTelemetry(&SnowflakeError{
			Number:  ErrQueryStatus,
			Message: "status query returned not-success or no status returned. Please retry",
		}, sc)
	}

	queryRet := statusResp.Data.Queries[0]
	if queryRet.ErrorCode != "" {
		return &queryRet, exceptionTelemetry(&SnowflakeError{
			Number:         ErrQueryStatus,
			Message:        errors.ErrMsgQueryStatus,
			MessageArgs:    []any{queryRet.ErrorCode, queryRet.ErrorMessage},
			IncludeQueryID: true,
			QueryID:        qid,
		}, sc)
	}

	// returned errorCode is 0. Now check what is the returned status of the query.
	qStatus := strToQueryStatus(queryRet.Status)
	if qStatus.isError() {
		return &queryRet, exceptionTelemetry(&SnowflakeError{
			Number: ErrQueryReportedError,
			Message: fmt.Sprintf("%s: status from server: [%s]",
				queryRet.ErrorMessage, queryRet.Status),
			IncludeQueryID: true,
			QueryID:        qid,
		}, sc)
	}

	if qStatus.isRunning() {
		return &queryRet, exceptionTelemetry(&SnowflakeError{
			Number: ErrQueryIsRunning,
			Message: fmt.Sprintf("%s: status from server: [%s]",
				queryRet.ErrorMessage, queryRet.Status),
			IncludeQueryID: true,
			QueryID:        qid,
		}, sc)
	}
	//success
	return &queryRet, nil
}

func (sc *snowflakeConn) getQueryResultResp(
	ctx context.Context,
	resultPath string) (
	*execResponse, error) {
	headers := getHeaders()
	if sn, ok := sc.syncParams.get(serviceName); ok {
		headers[httpHeaderServiceName] = *sn
	}
	param := make(url.Values)
	param.Set(requestIDKey, getOrGenerateRequestIDFromContext(ctx).String())
	param.Set("clientStartTime", strconv.FormatInt(sc.currentTimeProvider.currentTime(), 10))
	param.Set(requestGUIDKey, NewUUID().String())
	token, _, _ := sc.rest.TokenAccessor.GetTokens()
	if token != "" {
		headers[headerAuthorizationKey] = fmt.Sprintf(headerSnowflakeToken, token)
	}
	url := sc.rest.getFullURL(resultPath, &param)

	respd, err := getQueryResultWithRetriesForAsyncMode(ctx, sc.rest, url, headers, sc.rest.RequestTimeout)
	if err != nil {
		logger.WithContext(ctx).Errorf("error: %v", err)
		return nil, err
	}
	return respd, nil
}

// Fetch query result for a query id from /queries/<qid>/result endpoint.
func (sc *snowflakeConn) rowsForRunningQuery(
	ctx context.Context, qid string,
	rows *snowflakeRows) error {
	resultPath := fmt.Sprintf(urlQueriesResultFmt, qid)
	resp, err := sc.getQueryResultResp(ctx, resultPath)
	if err != nil {
		logger.WithContext(ctx).Errorf("error: %v", err)
		return err
	}

	if !resp.Success {
		code, err := strconv.Atoi(resp.Code)
		if err != nil {
			return err
		}
		return exceptionTelemetry(&SnowflakeError{
			Number:   code,
			SQLState: resp.Data.SQLState,
			Message:  resp.Message,
			QueryID:  resp.Data.QueryID,
		}, sc)
	}
	rows.addDownloader(populateChunkDownloader(ctx, sc, resp.Data))
	return nil
}

// prepare a Rows object to return for query of 'qid'
func (sc *snowflakeConn) buildRowsForRunningQuery(
	ctx context.Context,
	qid string) (
	driver.Rows, error) {
	rows := new(snowflakeRows)
	rows.sc = sc
	rows.queryID = qid
	rows.ctx = ctx
	if err := sc.rowsForRunningQuery(ctx, qid, rows); err != nil {
		return nil, err
	}
	err := rows.ChunkDownloader.start()
	return rows, err
}
