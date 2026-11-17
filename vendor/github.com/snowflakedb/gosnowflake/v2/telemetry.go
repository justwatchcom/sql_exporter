package gosnowflake

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	telemetryPath           = "/telemetry/send"
	defaultTelemetryTimeout = 10 * time.Second
	defaultFlushSize        = 100
)

const (
	typeKey          = "type"
	sourceKey        = "source"
	queryIDKey       = "QueryID"
	driverTypeKey    = "DriverType"
	driverVersionKey = "DriverVersion"
	golangVersionKey = "GolangVersion"
	sqlStateKey      = "SQLState"
	reasonKey        = "reason"
	errorNumberKey   = "ErrorNumber"
	stacktraceKey    = "Stacktrace"
)

const (
	telemetrySource           = "golang_driver"
	sqlException              = "client_sql_exception"
	connectionParameters      = "client_connection_parameters"
	connectionIdentifierShape = "client_connection_identifier_shape"
	disableConnectionShapeEnv = "SF_TELEMETRY_DISABLE_CONNECTION_SHAPE"
)

// Wire-format keys for the client_connection_identifier_shape telemetry
// event. Values are stringified booleans ("true" / "false") to match the
// existing client_connection_parameters telemetry style. All fields describe
// what the user supplied at the moment of input — they reflect intent, not
// the final post-normalization state of Config.
//
//   - account_provided:     the user explicitly set Account (via DSN authority,
//     "?account=", TOML, or programmatic Config).
//   - account_with_region:  the raw account string the user typed contained a
//     dot (e.g. "myacct.us-east-1"), signaling the deprecated
//     "account.region" embedded form. Set only on the raw input.
//   - account_org_provided: the raw account string carried a dash in its
//     account portion (e.g. "myorg-myacct"), signaling the org-prefixed form.
//     Region-portion dashes (e.g. the "-east-" in "myacct.us-east-1") are
//     intentionally not counted; see recordAccountShape.
//   - region_provided:      the user explicitly set Region as a distinct field
//     (via "?region=", TOML, or programmatic Config). Note: a region embedded
//     inside a dotted account string is NOT region_provided; that's
//     account_with_region.
//   - host_provided:        the user explicitly set Host. True when the DSN
//     authority is a full hostname (Snowflake TLD) or carries an explicit
//     port, when TOML sets host=, or when a programmatic Config sets Host.
//
// TODO(SNOW-3548350): remove these keys with the telemetry emission
// (target: 2026-11-30).
const (
	accountProvidedKey    = "account_provided"
	accountWithRegionKey  = "account_with_region"
	accountOrgProvidedKey = "account_org_provided"
	regionProvidedKey     = "region_provided"
	hostProvidedKey       = "host_provided"
)

type telemetryData struct {
	Timestamp int64             `json:"timestamp,omitempty"`
	Message   map[string]string `json:"message,omitempty"`
}

type snowflakeTelemetry struct {
	logs      []*telemetryData
	flushSize int
	sr        *snowflakeRestful
	mutex     *sync.Mutex
	enabled   bool
}

func (st *snowflakeTelemetry) addLog(data *telemetryData) error {
	if !st.enabled {
		logger.Debug("telemetry disabled; not adding log")
		return nil
	}
	st.mutex.Lock()
	st.logs = append(st.logs, data)
	shouldFlush := len(st.logs) >= st.flushSize
	st.mutex.Unlock()
	if shouldFlush {
		if err := st.sendBatch(); err != nil {
			return err
		}
	}
	return nil
}

func (st *snowflakeTelemetry) sendBatch() error {
	if !st.enabled {
		logger.Debug("telemetry disabled; not sending log")
		return nil
	}
	type telemetry struct {
		Logs []*telemetryData `json:"logs"`
	}

	st.mutex.Lock()
	logsToSend := st.logs
	minicoreLoadLogs.mu.Lock()
	if mcLogs := minicoreLoadLogs.logs; len(mcLogs) > 0 {
		logsToSend = append(logsToSend, &telemetryData{
			Timestamp: time.Now().UnixMilli(),
			Message: map[string]string{
				"minicoreLogs": strings.Join(mcLogs, "; "),
			},
		})
		minicoreLoadLogs.logs = make([]string, 0)
	}
	minicoreLoadLogs.mu.Unlock()
	st.logs = make([]*telemetryData, 0)
	st.mutex.Unlock()

	if len(logsToSend) == 0 {
		logger.Debug("nothing to send to telemetry")
		return nil
	}

	s := &telemetry{logsToSend}
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	logger.Debugf("sending %v logs to telemetry.", len(logsToSend))
	logger.Debugf("telemetry payload being sent: %v", string(body))

	headers := getHeaders()
	if token, _, _ := st.sr.TokenAccessor.GetTokens(); token != "" {
		headers[headerAuthorizationKey] = fmt.Sprintf(headerSnowflakeToken, token)
	}
	fullURL := st.sr.getFullURL(telemetryPath, nil)
	resp, err := st.sr.FuncPost(context.Background(), st.sr,
		fullURL, headers, body,
		defaultTelemetryTimeout, defaultTimeProvider, nil)
	if err != nil {
		logger.Errorf("failed to upload metrics to telemetry. err: %v", err)
		return err
	}
	defer func() {
		if err = resp.Body.Close(); err != nil {
			logger.Errorf("failed to close response body for %v. err: %v", fullURL, err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("non-successful response from telemetry server: %v. "+
			"disabling telemetry", resp.StatusCode)
		logger.Error(err.Error())
		st.enabled = false
		return err
	}
	var respd telemetryResponse
	if err = json.NewDecoder(resp.Body).Decode(&respd); err != nil {
		logger.Errorf("cannot decode telemetry response body: %v", err)
		st.enabled = false
		return err
	}
	if !respd.Success {
		err = fmt.Errorf("telemetry send failed with error code: %v, message: %v",
			respd.Code, respd.Message)
		logger.Error(err.Error())
		st.enabled = false
		return err
	}
	logger.Debug("successfully uploaded metrics to telemetry")
	return nil
}
