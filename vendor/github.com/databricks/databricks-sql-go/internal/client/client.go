package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	dbsqlerr "github.com/databricks/databricks-sql-go/errors"
	dbsqlerrint "github.com/databricks/databricks-sql-go/internal/errors"

	"github.com/apache/thrift/lib/go/thrift"
	"github.com/databricks/databricks-sql-go/auth"
	"github.com/databricks/databricks-sql-go/driverctx"
	"github.com/databricks/databricks-sql-go/internal/cli_service"
	"github.com/databricks/databricks-sql-go/internal/config"
	"github.com/databricks/databricks-sql-go/logger"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/pkg/errors"
)

// RecordResults is used to generate test data. Developer should change this manually
var RecordResults bool
var resultIndex int

type ThriftServiceClient struct {
	*cli_service.TCLIServiceClient
}

type contextKey int

const (
	ClientMethod contextKey = iota
)

type clientMethod int

//go:generate go run golang.org/x/tools/cmd/stringer -type=clientMethod -trimprefix=clientMethod

const (
	clientMethodUnknown clientMethod = iota
	clientMethodOpenSession
	clientMethodCloseSession
	clientMethodFetchResults
	clientMethodGetResultSetMetadata
	clientMethodExecuteStatement
	clientMethodGetOperationStatus
	clientMethodCloseOperation
	clientMethodCancelOperation
)

var nonRetryableClientMethods map[clientMethod]any = map[clientMethod]any{
	clientMethodExecuteStatement: struct{}{},
	clientMethodUnknown:          struct{}{},
}

var clientMethodRequestErrorMsgs map[clientMethod]string = map[clientMethod]string{
	clientMethodOpenSession:          "open session request error",
	clientMethodCloseSession:         "close session request error",
	clientMethodFetchResults:         "fetch results request error",
	clientMethodGetResultSetMetadata: "get result set metadata request error",
	clientMethodExecuteStatement:     "execute statement request error",
	clientMethodGetOperationStatus:   "get operation status request error",
	clientMethodCloseOperation:       "close operation request error",
	clientMethodCancelOperation:      "cancel operation request error",
}

// OpenSession is a wrapper around the thrift operation OpenSession
// If RecordResults is true, the results will be marshalled to JSON format and written to OpenSession<index>.json
func (tsc *ThriftServiceClient) OpenSession(ctx context.Context, req *cli_service.TOpenSessionReq) (*cli_service.TOpenSessionResp, error) {
	ctx = startClientMethod(ctx, clientMethodOpenSession)
	var log *logger.DBSQLLogger
	msg, start := logger.Track("OpenSession")

	resp, err := tsc.TCLIServiceClient.OpenSession(ctx, req)
	log, ctx = LoggerAndContext(ctx, resp)
	logDisplayMessage(resp, log)
	defer log.Duration(msg, start)
	if err != nil {
		err = handleClientMethodError(ctx, err)
		return resp, err
	}

	recordResult(ctx, resp)

	return resp, CheckStatus(resp)
}

// CloseSession is a wrapper around the thrift operation CloseSession
// If RecordResults is true, the results will be marshalled to JSON format and written to CloseSession<index>.json
func (tsc *ThriftServiceClient) CloseSession(ctx context.Context, req *cli_service.TCloseSessionReq) (*cli_service.TCloseSessionResp, error) {
	ctx = startClientMethod(ctx, clientMethodCloseSession)
	var log *logger.DBSQLLogger
	log, ctx = LoggerAndContext(ctx, req)
	defer log.Duration(logger.Track("CloseSession"))

	resp, err := tsc.TCLIServiceClient.CloseSession(ctx, req)
	logDisplayMessage(resp, log)
	if err != nil {
		err = handleClientMethodError(ctx, err)
		return resp, err
	}

	recordResult(ctx, resp)

	return resp, CheckStatus(resp)
}

// FetchResults is a wrapper around the thrift operation FetchResults
// If RecordResults is true, the results will be marshalled to JSON format and written to FetchResults<index>.json
func (tsc *ThriftServiceClient) FetchResults(ctx context.Context, req *cli_service.TFetchResultsReq) (*cli_service.TFetchResultsResp, error) {
	ctx = startClientMethod(ctx, clientMethodFetchResults)
	var log *logger.DBSQLLogger
	log, ctx = LoggerAndContext(ctx, req)
	defer log.Duration(logger.Track("FetchResults"))

	resp, err := tsc.TCLIServiceClient.FetchResults(ctx, req)
	logDisplayMessage(resp, log)
	if err != nil {
		err = handleClientMethodError(ctx, err)
		return resp, err
	}

	recordResult(ctx, resp)

	return resp, CheckStatus(resp)
}

// GetResultSetMetadata is a wrapper around the thrift operation GetResultSetMetadata
// If RecordResults is true, the results will be marshalled to JSON format and written to GetResultSetMetadata<index>.json
func (tsc *ThriftServiceClient) GetResultSetMetadata(ctx context.Context, req *cli_service.TGetResultSetMetadataReq) (*cli_service.TGetResultSetMetadataResp, error) {
	ctx = startClientMethod(ctx, clientMethodGetResultSetMetadata)
	var log *logger.DBSQLLogger
	log, ctx = LoggerAndContext(ctx, req)
	defer log.Duration(logger.Track("GetResultSetMetadata"))

	resp, err := tsc.TCLIServiceClient.GetResultSetMetadata(ctx, req)
	logDisplayMessage(resp, log)
	if err != nil {
		err = handleClientMethodError(ctx, err)
		return resp, err
	}

	recordResult(ctx, resp)

	return resp, CheckStatus(resp)
}

// ExecuteStatement is a wrapper around the thrift operation ExecuteStatement
// If RecordResults is true, the results will be marshalled to JSON format and written to ExecuteStatement<index>.json
func (tsc *ThriftServiceClient) ExecuteStatement(ctx context.Context, req *cli_service.TExecuteStatementReq) (*cli_service.TExecuteStatementResp, error) {
	ctx = startClientMethod(ctx, clientMethodExecuteStatement)
	var log *logger.DBSQLLogger
	log, ctx = LoggerAndContext(ctx, req)
	msg, start := log.Track("ExecuteStatement")

	// We use context.Background to fix a problem where on context done the query would not be cancelled.
	resp, err := tsc.TCLIServiceClient.ExecuteStatement(context.Background(), req)
	log, ctx = LoggerAndContext(ctx, resp)
	logDisplayMessage(resp, log)
	logExecStatementState(resp, log)

	defer log.Duration(msg, start)
	if err != nil {
		err = handleClientMethodError(ctx, err)
		return resp, err
	}

	return resp, CheckStatus(resp)
}

// GetOperationStatus is a wrapper around the thrift operation GetOperationStatus
// If RecordResults is true, the results will be marshalled to JSON format and written to GetOperationStatus<index>.json
func (tsc *ThriftServiceClient) GetOperationStatus(ctx context.Context, req *cli_service.TGetOperationStatusReq) (*cli_service.TGetOperationStatusResp, error) {
	ctx = startClientMethod(ctx, clientMethodGetOperationStatus)
	var log *logger.DBSQLLogger
	log, ctx = LoggerAndContext(ctx, req)
	defer log.Duration(logger.Track("GetOperationStatus"))

	resp, err := tsc.TCLIServiceClient.GetOperationStatus(ctx, req)
	logDisplayMessage(resp, log)
	if err != nil {
		err = handleClientMethodError(driverctx.NewContextWithQueryId(ctx, SprintGuid(req.OperationHandle.OperationId.GUID)), err)
		return resp, err
	}

	recordResult(ctx, resp)

	return resp, CheckStatus(resp)
}

// CloseOperation is a wrapper around the thrift operation CloseOperation
// If RecordResults is true, the results will be marshalled to JSON format and written to CloseOperation<index>.json
func (tsc *ThriftServiceClient) CloseOperation(ctx context.Context, req *cli_service.TCloseOperationReq) (*cli_service.TCloseOperationResp, error) {
	ctx = startClientMethod(ctx, clientMethodCloseOperation)
	var log *logger.DBSQLLogger
	log, ctx = LoggerAndContext(ctx, req)
	defer log.Duration(logger.Track("CloseOperation"))

	resp, err := tsc.TCLIServiceClient.CloseOperation(ctx, req)
	logDisplayMessage(resp, log)
	if err != nil {
		err = handleClientMethodError(ctx, err)
		return resp, err
	}

	recordResult(ctx, resp)

	return resp, CheckStatus(resp)
}

// CancelOperation is a wrapper around the thrift operation CancelOperation
// If RecordResults is true, the results will be marshalled to JSON format and written to CancelOperation<index>.json
func (tsc *ThriftServiceClient) CancelOperation(ctx context.Context, req *cli_service.TCancelOperationReq) (*cli_service.TCancelOperationResp, error) {
	ctx = startClientMethod(ctx, clientMethodCancelOperation)
	var log *logger.DBSQLLogger
	log, ctx = LoggerAndContext(ctx, req)
	defer log.Duration(logger.Track("CancelOperation"))

	resp, err := tsc.TCLIServiceClient.CancelOperation(ctx, req)
	logDisplayMessage(resp, log)
	if err != nil {
		err = handleClientMethodError(ctx, err)
		return resp, err
	}

	recordResult(ctx, resp)

	return resp, CheckStatus(resp)
}

// InitThriftClient is a wrapper of the http transport, so we can have access to response code and headers.
// It is important to know the code and headers to know if we need to retry or not
func InitThriftClient(cfg *config.Config, httpclient *http.Client) (*ThriftServiceClient, error) {
	var err error
	endpoint, err := cfg.ToEndpointURL()
	if err != nil {
		return nil, err
	}
	tcfg := &thrift.TConfiguration{
		TLSConfig: cfg.TLSConfig,
	}

	var protocolFactory thrift.TProtocolFactory
	switch cfg.ThriftProtocol {
	case "compact":
		protocolFactory = thrift.NewTCompactProtocolFactoryConf(tcfg)
	case "simplejson":
		protocolFactory = thrift.NewTSimpleJSONProtocolFactoryConf(tcfg)
	case "json":
		protocolFactory = thrift.NewTJSONProtocolFactory()
	case "binary":
		protocolFactory = thrift.NewTBinaryProtocolFactoryConf(tcfg)
	case "header":
		protocolFactory = thrift.NewTHeaderProtocolFactoryConf(tcfg)
	default:
		return nil, dbsqlerrint.NewRequestError(context.TODO(), fmt.Sprintf("invalid protocol specified %s", cfg.ThriftProtocol), nil)
	}
	if cfg.ThriftDebugClientProtocol {
		protocolFactory = thrift.NewTDebugProtocolFactoryWithLogger(protocolFactory, "client:", thrift.StdLogger(nil))
	}

	var tTrans thrift.TTransport

	switch cfg.ThriftTransport {
	case "http":
		if httpclient == nil {
			if cfg.Authenticator == nil {
				return nil, dbsqlerrint.NewRequestError(context.TODO(), dbsqlerr.ErrNoDefaultAuthenticator, nil)
			}
			httpclient = RetryableClient(cfg)
		}

		tTrans, err = thrift.NewTHttpClientWithOptions(endpoint, thrift.THttpClientOptions{Client: httpclient})

		thriftHttpClient := tTrans.(*thrift.THttpClient)
		userAgent := fmt.Sprintf("%s/%s", cfg.DriverName, cfg.DriverVersion)
		if cfg.UserAgentEntry != "" {
			userAgent = fmt.Sprintf("%s/%s (%s)", cfg.DriverName, cfg.DriverVersion, cfg.UserAgentEntry)
		}
		thriftHttpClient.SetHeader("User-Agent", userAgent)

	default:
		return nil, dbsqlerrint.NewDriverError(context.TODO(), fmt.Sprintf("unsupported transport `%s`", cfg.ThriftTransport), nil)
	}
	if err != nil {
		return nil, dbsqlerrint.NewRequestError(context.TODO(), dbsqlerr.ErrInvalidURL, err)
	}
	if err = tTrans.Open(); err != nil {
		return nil, dbsqlerrint.NewRequestError(context.TODO(), fmt.Sprintf("failed to open http transport for endpoint %s", endpoint), err)
	}
	iprot := protocolFactory.GetProtocol(tTrans)
	oprot := protocolFactory.GetProtocol(tTrans)
	tclient := cli_service.NewTCLIServiceClient(thrift.NewTStandardClient(iprot, oprot))
	tsClient := &ThriftServiceClient{tclient}
	return tsClient, nil
}

func startClientMethod(ctx context.Context, method clientMethod) context.Context {
	ctx = context.WithValue(ctx, ClientMethod, method)
	log, _ := LoggerAndContext(ctx, nil)
	log.Debug().Msgf("client.%s", method.String())
	return ctx
}

// handler function for errors returned by the thrift client methods
func handleClientMethodError(ctx context.Context, err error) dbsqlerr.DBRequestError {
	if err == nil {
		return nil
	}

	// If the passed error indicates an invalid session we inject a bad connection error
	// into the error stack. This allows the for retrying with a new connection.
	s := err.Error()
	if strings.Contains(s, "Invalid SessionHandle") {
		err = dbsqlerrint.NewBadConnectionError(err)
	}

	// the passed error will be wrapped in a DBRequestError
	method := getClientMethod(ctx)
	msg := clientMethodRequestErrorMsgs[method]

	dbErr := dbsqlerrint.NewRequestError(ctx, msg, err)

	log, _ := LoggerAndContext(ctx, nil)

	log.Err(err).Msg("")

	return dbErr
}

// Extract a clientMethod value from the given Context.
func getClientMethod(ctx context.Context) clientMethod {
	v, _ := ctx.Value(ClientMethod).(clientMethod)
	return v
}

// Write the result
func recordResult(ctx context.Context, resp any) {
	if RecordResults && resp != nil {
		method := getClientMethod(ctx)
		j, _ := json.MarshalIndent(resp, "", " ")
		_ = os.WriteFile(fmt.Sprintf("%s%d.json", method, resultIndex), j, 0600)
		resultIndex++
	}
}

// ThriftResponse represents the thrift rpc response
type ThriftResponse interface {
	GetStatus() *cli_service.TStatus
}

// CheckStatus checks the status code after a thrift operation.
// Returns nil if the operation is successful or still executing, otherwise returns an error.
func CheckStatus(resp interface{}) error {
	rpcresp, ok := resp.(ThriftResponse)
	if ok {
		status := rpcresp.GetStatus()
		if status.StatusCode == cli_service.TStatusCode_ERROR_STATUS {
			return errors.New(status.GetErrorMessage())
		}
		if status.StatusCode == cli_service.TStatusCode_INVALID_HANDLE_STATUS {
			return errors.New("thrift: invalid handle")
		}

		// SUCCESS, SUCCESS_WITH_INFO, STILL_EXECUTING are ok
		return nil
	}
	return errors.New("thrift: invalid response")
}

// SprintGuid is a convenience function to format a byte array into GUID.
func SprintGuid(bts []byte) string {
	if len(bts) == 16 {
		return fmt.Sprintf("%x-%x-%x-%x-%x", bts[0:4], bts[4:6], bts[6:8], bts[8:10], bts[10:16])
	}
	logger.Warn().Msgf("GUID not valid: %x", bts)
	return fmt.Sprintf("%x", bts)
}

// Create an updated context and a logger that includes query and connection id
func LoggerAndContext(ctx context.Context, c any) (*logger.DBSQLLogger, context.Context) {
	connId := driverctx.ConnIdFromContext(ctx)
	corrId := driverctx.CorrelationIdFromContext(ctx)
	queryId := driverctx.QueryIdFromContext(ctx)
	if connId == "" {
		connId = guidFromHasSessionHandle(c)
		ctx = driverctx.NewContextWithConnId(ctx, connId)
	}
	if queryId == "" {
		queryId = guidFromHasOpHandle(c)
		ctx = driverctx.NewContextWithQueryId(ctx, queryId)
	}
	log := logger.WithContext(connId, corrId, queryId)

	return log, ctx
}

type hasOpHandle interface {
	GetOperationHandle() *cli_service.TOperationHandle
}
type hasSessionHandle interface {
	GetSessionHandle() *cli_service.TSessionHandle
}

func guidFromHasOpHandle(c any) (guid string) {
	if c == nil || reflect.ValueOf(c).IsNil() {
		return
	}
	if ho, ok := c.(hasOpHandle); ok {
		opHandle := ho.GetOperationHandle()
		if opHandle != nil && opHandle.OperationId != nil && opHandle.OperationId.GUID != nil {
			guid = SprintGuid(opHandle.OperationId.GUID)
		}
	}
	return
}

func guidFromHasSessionHandle(c any) (guid string) {
	if c == nil || reflect.ValueOf(c).IsNil() {
		return
	}
	if ho, ok := c.(hasSessionHandle); ok {
		sessionHandle := ho.GetSessionHandle()
		if sessionHandle != nil && sessionHandle.SessionId != nil && sessionHandle.SessionId.GUID != nil {
			guid = SprintGuid(sessionHandle.SessionId.GUID)
		}
	}
	return
}

func logExecStatementState(resp *cli_service.TExecuteStatementResp, log *logger.DBSQLLogger) {
	if resp != nil {
		if resp.DirectResults != nil {
			state := resp.DirectResults.GetOperationStatus().GetOperationState()
			log.Debug().Msgf("execute statement state: %s", state)
			status := resp.DirectResults.GetOperationStatus().GetStatus().StatusCode
			log.Debug().Msgf("execute statement status: %s", status)
			logDisplayMessage(resp.DirectResults, log)
		} else {
			status := resp.GetStatus().StatusCode
			log.Debug().Msgf("execute statement status: %s", status)
		}
	}
}

type hasGetStatus interface{ GetStatus() *cli_service.TStatus }
type hasGetDisplayMessage interface{ GetDisplayMessage() string }
type hasGetOperationStatus interface {
	GetOperationStatus() *cli_service.TGetOperationStatusResp
}

func logDisplayMessage(c any, log *logger.DBSQLLogger) {
	if c == nil || reflect.ValueOf(c).IsNil() {
		return
	}

	if hd, ok := c.(hasGetDisplayMessage); ok {
		dm := hd.GetDisplayMessage()
		if dm != "" {
			log.Debug().Msg(dm)
		}
	} else if gs, ok := c.(hasGetStatus); ok {
		logDisplayMessage(gs.GetStatus(), log)
	} else if gos, ok := c.(hasGetOperationStatus); ok {
		logDisplayMessage(gos.GetOperationStatus(), log)
	}
}

var retryableStatusCodes = map[int]any{http.StatusTooManyRequests: struct{}{}, http.StatusServiceUnavailable: struct{}{}}

func isRetryableServerResponse(resp *http.Response) bool {
	if resp == nil {
		return false
	}

	_, ok := retryableStatusCodes[resp.StatusCode]
	return ok
}

type Transport struct {
	Base  http.RoundTripper
	Authr auth.Authenticator
	trace bool
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.trace {
		trace := &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { log.Printf("conn was reused: %t", info.Reused) },
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	}

	defer logger.Duration(logger.Track("RoundTrip"))
	// this is inspired by oauth2.Transport
	reqBodyClosed := false
	if req.Body != nil {
		defer func() {
			if !reqBodyClosed {
				req.Body.Close()
			}
		}()
	}

	req2 := cloneRequest(req) // per RoundTripper contract

	err := t.Authr.Authenticate(req2)

	if err != nil {
		return nil, err
	}

	// req.Body is assumed to be closed by the base RoundTripper.
	reqBodyClosed = true
	resp, err := t.Base.RoundTrip(req2)

	return resp, err
}

func RetryableClient(cfg *config.Config) *http.Client {
	httpclient := PooledClient(cfg)
	retryableClient := &retryablehttp.Client{
		HTTPClient:   httpclient,
		Logger:       &leveledLogger{},
		RetryWaitMin: cfg.RetryWaitMin,
		RetryWaitMax: cfg.RetryWaitMax,
		RetryMax:     cfg.RetryMax,
		ErrorHandler: errorHandler,
		CheckRetry:   RetryPolicy,
		Backoff:      backoff,
	}
	return retryableClient.StandardClient()
}

func PooledTransport(cfg *config.Config) *http.Transport {
	var tlsConfig *tls.Config
	if (cfg.TLSConfig != nil) && cfg.TLSConfig.InsecureSkipVerify {
		tlsConfig = cfg.TLSConfig
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		TLSClientConfig:       tlsConfig,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       180 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConnsPerHost:   10, // this client is only used for one host
		MaxConnsPerHost:       100,
	}
	return transport
}

func PooledClient(cfg *config.Config) *http.Client {
	if cfg.Authenticator == nil {
		return nil
	}

	var tr *Transport
	if cfg.Transport != nil {
		tr = &Transport{
			Base:  cfg.Transport,
			Authr: cfg.Authenticator,
		}
	} else {
		tr = &Transport{
			Base:  PooledTransport(cfg),
			Authr: cfg.Authenticator,
		}
	}

	return &http.Client{
		Transport: tr,
		Timeout:   cfg.ClientTimeout,
	}
}

// cloneRequest returns a clone of the provided *http.Request.
// The clone is a shallow copy of the struct and its Header map.
func cloneRequest(r *http.Request) *http.Request {
	// shallow copy of the struct
	r2 := new(http.Request)
	*r2 = *r
	// deep copy of the Header
	r2.Header = make(http.Header, len(r.Header))
	for k, s := range r.Header {
		r2.Header[k] = append([]string(nil), s...)
	}
	return r2
}

type leveledLogger struct {
}

func (l *leveledLogger) Error(msg string, keysAndValues ...interface{}) {
	logger.Error().Msg(msg)
}
func (l *leveledLogger) Info(msg string, keysAndValues ...interface{}) {
	logger.Info().Msg(msg)
}
func (l *leveledLogger) Debug(msg string, keysAndValues ...interface{}) {
	logger.Debug().Msg(msg)
}
func (l *leveledLogger) Warn(msg string, keysAndValues ...interface{}) {
	logger.Warn().Msg(msg)
}

func errorHandler(resp *http.Response, err error, numTries int) (*http.Response, error) {
	var werr error
	msg := fmt.Sprintf("request error after %d attempt(s)", numTries)
	if err == nil {
		werr = errors.New(msg)
	} else {
		werr = errors.Wrap(err, msg)
	}

	if resp != nil {
		if resp.Header != nil {
			reason := resp.Header.Get("X-Databricks-Reason-Phrase")
			terrmsg := resp.Header.Get("X-Thriftserver-Error-Message")

			if reason != "" {
				werr = dbsqlerrint.WrapErr(werr, reason)
			} else if terrmsg != "" {
				werr = dbsqlerrint.WrapErr(werr, terrmsg)
			}
		}

		logger.Err(werr).Msg(resp.Status)
	}

	return resp, werr
}

var (
	// A regular expression to match the error returned by net/http when the
	// configured number of redirects is exhausted. This error isn't typed
	// specifically so we resort to matching on the error string.
	redirectsErrorRe = regexp.MustCompile(`stopped after \d+ redirects\z`)

	// A regular expression to match the error returned by net/http when the
	// scheme specified in the URL is invalid. This error isn't typed
	// specifically so we resort to matching on the error string.
	schemeErrorRe = regexp.MustCompile(`unsupported protocol scheme`)

	// A regular expression to match the error returned by net/http when the
	// TLS certificate is not trusted. This error isn't typed
	// specifically so we resort to matching on the error string.
	notTrustedErrorRe = regexp.MustCompile(`certificate is not trusted`)

	errorRes = []*regexp.Regexp{redirectsErrorRe, schemeErrorRe, notTrustedErrorRe}
)

func RetryPolicy(ctx context.Context, resp *http.Response, err error) (bool, error) {
	// do not retry on context.Canceled or context.DeadlineExceeded
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	caller := getClientMethod(ctx)
	_, nonRetryableClientMethod := nonRetryableClientMethods[caller]

	if err != nil {
		if isRetryableError(err) && !nonRetryableClientMethod {
			return true, nil
		}

		return false, err
	}

	// shouldn't retry on no response or success
	if resp == nil || resp.StatusCode == http.StatusOK {
		return false, nil
	}

	checkErr := fmt.Errorf("unexpected HTTP status %s", resp.Status)

	// 429 Too Many Requests or 503 service unavailable is recoverable. Sometimes the server puts
	// a Retry-After response header to indicate when the server is
	// available to start processing request from client.
	if isRetryableServerResponse(resp) {
		var retryAfter string
		if resp.Header != nil {
			retryAfter = resp.Header.Get("Retry-After")
		}

		return true, dbsqlerrint.NewRetryableError(checkErr, retryAfter)
	}

	if !nonRetryableClientMethod && (resp.StatusCode == 0 || (resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented)) {
		return true, checkErr
	}

	// checkErr will be non-nil if the response code was not StatusOK.
	// Returning it here ensures that the error handler will be called.
	return false, checkErr
}

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	if v, ok := err.(*url.Error); ok {
		s := v.Error()
		for _, re := range errorRes {
			if re.MatchString(s) {
				return false
			}
		}

		if _, ok := v.Err.(x509.UnknownAuthorityError); ok {
			return false
		}
	}

	// The error is likely recoverable so retry.
	return true
}

func backoff(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
	// honour the Retry-After header
	if resp != nil && resp.Header != nil {
		if s, ok := resp.Header["Retry-After"]; ok {
			if sleep, err := strconv.ParseInt(s[0], 10, 64); err == nil {
				return time.Second * time.Duration(sleep)
			}
		}
	}

	// exponential backoff
	mult := math.Pow(2, float64(attemptNum)) * float64(min)
	sleep := time.Duration(mult)
	if float64(sleep) != mult || sleep > max {
		sleep = max
	}
	return sleep
}
