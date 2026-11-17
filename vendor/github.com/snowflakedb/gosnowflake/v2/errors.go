package gosnowflake

import (
	"fmt"
	"runtime/debug"
	"strconv"
	"time"

	sferrors "github.com/snowflakedb/gosnowflake/v2/internal/errors"
)

// SnowflakeError is a error type including various Snowflake specific information.
type SnowflakeError = sferrors.SnowflakeError

func generateTelemetryExceptionData(se *SnowflakeError) *telemetryData {
	data := &telemetryData{
		Message: map[string]string{
			typeKey:          sqlException,
			sourceKey:        telemetrySource,
			driverTypeKey:    "Go",
			driverVersionKey: SnowflakeGoDriverVersion,
			stacktraceKey:    maskSecrets(string(debug.Stack())),
		},
		Timestamp: time.Now().UnixNano() / int64(time.Millisecond),
	}
	if se.QueryID != "" {
		data.Message[queryIDKey] = se.QueryID
	}
	if se.SQLState != "" {
		data.Message[sqlStateKey] = se.SQLState
	}
	if se.Message != "" {
		data.Message[reasonKey] = se.Message
	}
	if len(se.MessageArgs) > 0 {
		data.Message[reasonKey] = fmt.Sprintf(se.Message, se.MessageArgs...)
	}
	if se.Number != 0 {
		data.Message[errorNumberKey] = strconv.Itoa(se.Number)
	}
	return data
}

// exceptionTelemetry generates telemetry data from the error and adds it to the telemetry queue.
func exceptionTelemetry(se *SnowflakeError, sc *snowflakeConn) *SnowflakeError {
	if sc == nil || sc.telemetry == nil || !sc.telemetry.enabled {
		return se // skip expensive stacktrace generation below if telemetry is disabled
	}
	data := generateTelemetryExceptionData(se)
	if err := sc.telemetry.addLog(data); err != nil {
		logger.WithContext(sc.ctx).Debugf("failed to log to telemetry: %v", data)
	}
	return se
}

// return populated error fields replacing the default response
func populateErrorFields(code int, data *execResponse) *SnowflakeError {
	err := sferrors.ErrUnknownError()
	if code != -1 {
		err.Number = code
	}
	if data.Data.SQLState != "" {
		err.SQLState = data.Data.SQLState
	}
	if data.Message != "" {
		err.Message = data.Message
	}
	if data.Data.QueryID != "" {
		err.QueryID = data.Data.QueryID
	}
	return err
}

// Snowflake Server Error code
const (
	queryNotExecutingCode       = "000605"
	queryInProgressCode         = "333333"
	queryInProgressAsyncCode    = "333334"
	sessionExpiredCode          = "390112"
	invalidOAuthAccessTokenCode = "390303"
	expiredOAuthAccessTokenCode = "390318"
)

// Driver return errors — re-exported from internal/errors
const (
	/* connection */

	// ErrCodeEmptyAccountCode is an error code for the case where a DSN doesn't include account parameter
	ErrCodeEmptyAccountCode = sferrors.ErrCodeEmptyAccountCode
	// ErrCodeEmptyUsernameCode is an error code for the case where a DSN doesn't include user parameter
	ErrCodeEmptyUsernameCode = sferrors.ErrCodeEmptyUsernameCode
	// ErrCodeEmptyPasswordCode is an error code for the case where a DSN doesn't include password parameter
	ErrCodeEmptyPasswordCode = sferrors.ErrCodeEmptyPasswordCode
	// ErrCodeFailedToParseHost is an error code for the case where a DSN includes an invalid host name
	ErrCodeFailedToParseHost = sferrors.ErrCodeFailedToParseHost
	// ErrCodeFailedToParsePort is an error code for the case where a DSN includes an invalid port number
	ErrCodeFailedToParsePort = sferrors.ErrCodeFailedToParsePort
	// ErrCodeIdpConnectionError is an error code for the case where a IDP connection failed
	ErrCodeIdpConnectionError = sferrors.ErrCodeIdpConnectionError
	// ErrCodeSSOURLNotMatch is an error code for the case where a SSO URL doesn't match
	ErrCodeSSOURLNotMatch = sferrors.ErrCodeSSOURLNotMatch
	// ErrCodeServiceUnavailable is an error code for the case where service is unavailable.
	ErrCodeServiceUnavailable = sferrors.ErrCodeServiceUnavailable
	// ErrCodeFailedToConnect is an error code for the case where a DB connection failed due to wrong account name
	ErrCodeFailedToConnect = sferrors.ErrCodeFailedToConnect
	// ErrCodeRegionOverlap is an error code for the case where a region is specified despite an account region present
	ErrCodeRegionOverlap = sferrors.ErrCodeRegionOverlap
	// ErrCodePrivateKeyParseError is an error code for the case where the private key is not parsed correctly
	ErrCodePrivateKeyParseError = sferrors.ErrCodePrivateKeyParseError
	// ErrCodeFailedToParseAuthenticator is an error code for the case where a DNS includes an invalid authenticator
	ErrCodeFailedToParseAuthenticator = sferrors.ErrCodeFailedToParseAuthenticator
	// ErrCodeClientConfigFailed is an error code for the case where clientConfigFile is invalid or applying client configuration fails
	ErrCodeClientConfigFailed = sferrors.ErrCodeClientConfigFailed
	// ErrCodeTomlFileParsingFailed is an error code for the case where parsing the toml file is failed because of invalid value.
	ErrCodeTomlFileParsingFailed = sferrors.ErrCodeTomlFileParsingFailed
	// ErrCodeFailedToFindDSNInToml is an error code for the case where the DSN does not exist in the toml file.
	ErrCodeFailedToFindDSNInToml = sferrors.ErrCodeFailedToFindDSNInToml
	// ErrCodeInvalidFilePermission is an error code for the case where the user does not have 0600 permission to the toml file.
	ErrCodeInvalidFilePermission = sferrors.ErrCodeInvalidFilePermission
	// ErrCodeEmptyPasswordAndToken is an error code for the case where a DSN do includes neither password nor token
	ErrCodeEmptyPasswordAndToken = sferrors.ErrCodeEmptyPasswordAndToken
	// ErrCodeEmptyToken is an error code for the case where token-based auth (e.g. PAT) is used but neither token nor tokenFilePath is provided.
	ErrCodeEmptyToken = sferrors.ErrCodeEmptyToken
	// ErrCodeEmptyOAuthParameters is an error code for the case where the client ID or client secret are not provided for OAuth flows.
	ErrCodeEmptyOAuthParameters = sferrors.ErrCodeEmptyOAuthParameters
	// ErrCodeHostWithScheme is an error code for the case where the host includes a URL scheme (e.g. "https://")
	ErrCodeHostWithScheme = sferrors.ErrCodeHostWithScheme
	// ErrMissingAccessATokenButRefreshTokenPresent is an error code for the case when access token is not found in cache, but the refresh token is present.
	ErrMissingAccessATokenButRefreshTokenPresent = sferrors.ErrMissingAccessATokenButRefreshTokenPresent
	// ErrCodeMissingTLSConfig is an error code for the case where the TLS config is missing.
	ErrCodeMissingTLSConfig = sferrors.ErrCodeMissingTLSConfig

	/* network */

	// ErrFailedToPostQuery is an error code for the case where HTTP POST failed.
	ErrFailedToPostQuery = sferrors.ErrFailedToPostQuery
	// ErrFailedToRenewSession is an error code for the case where session renewal failed.
	ErrFailedToRenewSession = sferrors.ErrFailedToRenewSession
	// ErrFailedToCancelQuery is an error code for the case where cancel query failed.
	ErrFailedToCancelQuery = sferrors.ErrFailedToCancelQuery
	// ErrFailedToCloseSession is an error code for the case where close session failed.
	ErrFailedToCloseSession = sferrors.ErrFailedToCloseSession
	// ErrFailedToAuth is an error code for the case where authentication failed for unknown reason.
	ErrFailedToAuth = sferrors.ErrFailedToAuth
	// ErrFailedToAuthSAML is an error code for the case where authentication via SAML failed for unknown reason.
	ErrFailedToAuthSAML = sferrors.ErrFailedToAuthSAML
	// ErrFailedToAuthOKTA is an error code for the case where authentication via OKTA failed for unknown reason.
	ErrFailedToAuthOKTA = sferrors.ErrFailedToAuthOKTA
	// ErrFailedToGetSSO is an error code for the case where authentication via OKTA failed for unknown reason.
	ErrFailedToGetSSO = sferrors.ErrFailedToGetSSO
	// ErrFailedToParseResponse is an error code for when we cannot parse an external browser response from Snowflake.
	ErrFailedToParseResponse = sferrors.ErrFailedToParseResponse
	// ErrFailedToGetExternalBrowserResponse is an error code for when there's an error reading from the open socket.
	ErrFailedToGetExternalBrowserResponse = sferrors.ErrFailedToGetExternalBrowserResponse
	// ErrFailedToHeartbeat is an error code when a heartbeat fails.
	ErrFailedToHeartbeat = sferrors.ErrFailedToHeartbeat

	/* rows */

	// ErrFailedToGetChunk is an error code for the case where it failed to get chunk of result set
	ErrFailedToGetChunk = sferrors.ErrFailedToGetChunk
	// ErrNonArrowResponseInArrowBatches is an error code for case where ArrowBatches mode is enabled, but response is not Arrow-based
	ErrNonArrowResponseInArrowBatches = sferrors.ErrNonArrowResponseInArrowBatches

	/* transaction*/

	// ErrNoReadOnlyTransaction is an error code for the case where readonly mode is specified.
	ErrNoReadOnlyTransaction = sferrors.ErrNoReadOnlyTransaction
	// ErrNoDefaultTransactionIsolationLevel is an error code for the case where non default isolation level is specified.
	ErrNoDefaultTransactionIsolationLevel = sferrors.ErrNoDefaultTransactionIsolationLevel

	/* file transfer */

	// ErrInvalidStageFs is an error code denoting an invalid stage in the file system
	ErrInvalidStageFs = sferrors.ErrInvalidStageFs
	// ErrFailedToDownloadFromStage is an error code denoting the failure to download a file from the stage
	ErrFailedToDownloadFromStage = sferrors.ErrFailedToDownloadFromStage
	// ErrFailedToUploadToStage is an error code denoting the failure to upload a file to the stage
	ErrFailedToUploadToStage = sferrors.ErrFailedToUploadToStage
	// ErrInvalidStageLocation is an error code denoting an invalid stage location
	ErrInvalidStageLocation = sferrors.ErrInvalidStageLocation
	// ErrLocalPathNotDirectory is an error code denoting a local path that is not a directory
	ErrLocalPathNotDirectory = sferrors.ErrLocalPathNotDirectory
	// ErrFileNotExists is an error code denoting the file to be transferred does not exist
	ErrFileNotExists = sferrors.ErrFileNotExists
	// ErrCompressionNotSupported is an error code denoting the user specified compression type is not supported
	ErrCompressionNotSupported = sferrors.ErrCompressionNotSupported
	// ErrInternalNotMatchEncryptMaterial is an error code denoting the encryption material specified does not match
	ErrInternalNotMatchEncryptMaterial = sferrors.ErrInternalNotMatchEncryptMaterial
	// ErrCommandNotRecognized is an error code denoting the PUT/GET command was not recognized
	ErrCommandNotRecognized = sferrors.ErrCommandNotRecognized
	// ErrFailedToConvertToS3Client is an error code denoting the failure of an interface to s3.Client conversion
	ErrFailedToConvertToS3Client = sferrors.ErrFailedToConvertToS3Client
	// ErrNotImplemented is an error code denoting the file transfer feature is not implemented
	ErrNotImplemented = sferrors.ErrNotImplemented
	// ErrInvalidPadding is an error code denoting the invalid padding of decryption key
	ErrInvalidPadding = sferrors.ErrInvalidPadding
	// ErrGetStreamMultipleFiles is an error code denoting a streaming GET whose stage path
	// matched more than one file, so it cannot be streamed into a single reader.
	ErrGetStreamMultipleFiles = sferrors.ErrGetStreamMultipleFiles
	// ErrDownloadStreamLocalFS is an error code denoting DownloadStream used with a LOCAL_FS stage
	ErrDownloadStreamLocalFS = sferrors.ErrDownloadStreamLocalFS
	// ErrDownloadStreamInvalidArg is an error code denoting a missing stage or file name for DownloadStream
	ErrDownloadStreamInvalidArg = sferrors.ErrDownloadStreamInvalidArg
	// ErrFailedToDecrypt is an error code denoting CSE metadata or decryption failed for a stage download
	ErrFailedToDecrypt = sferrors.ErrFailedToDecrypt
	// ErrDownloadStreamDecompress is an error code denoting gzip decompression failed while reading a DownloadStream
	ErrDownloadStreamDecompress = sferrors.ErrDownloadStreamDecompress
	// ErrUploadStreamInvalidArg is an error code denoting an invalid argument for UploadStream
	ErrUploadStreamInvalidArg = sferrors.ErrUploadStreamInvalidArg
	// ErrUploadStreamLocalFS is an error code denoting UploadStream used with a LOCAL_FS stage
	ErrUploadStreamLocalFS = sferrors.ErrUploadStreamLocalFS
	// ErrUploadStreamSkipped is an error code denoting UploadStream left an existing object in place
	ErrUploadStreamSkipped = sferrors.ErrUploadStreamSkipped

	/* binding */

	// ErrBindSerialization is an error code for a failed serialization of bind variables
	ErrBindSerialization = sferrors.ErrBindSerialization
	// ErrBindUpload is an error code for the uploading process of bind elements to the stage
	ErrBindUpload = sferrors.ErrBindUpload

	/* async */

	// ErrAsync is an error code for an unknown async error
	ErrAsync = sferrors.ErrAsync

	/* multi-statement */

	// ErrNoResultIDs is an error code for empty result IDs for multi statement queries
	ErrNoResultIDs = sferrors.ErrNoResultIDs

	/* converter */

	// ErrInvalidTimestampTz is an error code for the case where a returned TIMESTAMP_TZ internal value is invalid
	ErrInvalidTimestampTz = sferrors.ErrInvalidTimestampTz
	// ErrInvalidOffsetStr is an error code for the case where an offset string is invalid. The input string must
	// consist of sHHMI where one sign character '+'/'-' followed by zero filled hours and minutes
	ErrInvalidOffsetStr = sferrors.ErrInvalidOffsetStr
	// ErrInvalidBinaryHexForm is an error code for the case where a binary data in hex form is invalid.
	ErrInvalidBinaryHexForm = sferrors.ErrInvalidBinaryHexForm
	// ErrTooHighTimestampPrecision is an error code for the case where cannot convert Snowflake timestamp to arrow.Timestamp
	ErrTooHighTimestampPrecision = sferrors.ErrTooHighTimestampPrecision
	// ErrNullValueInArray is an error code for the case where there are null values in an array without arrayValuesNullable set to true
	ErrNullValueInArray = sferrors.ErrNullValueInArray
	// ErrNullValueInMap is an error code for the case where there are null values in a map without mapValuesNullable set to true
	ErrNullValueInMap = sferrors.ErrNullValueInMap

	/* OCSP */

	// ErrOCSPStatusRevoked is an error code for the case where the certificate is revoked.
	ErrOCSPStatusRevoked = sferrors.ErrOCSPStatusRevoked
	// ErrOCSPStatusUnknown is an error code for the case where the certificate revocation status is unknown.
	ErrOCSPStatusUnknown = sferrors.ErrOCSPStatusUnknown
	// ErrOCSPInvalidValidity is an error code for the case where the OCSP response validity is invalid.
	ErrOCSPInvalidValidity = sferrors.ErrOCSPInvalidValidity
	// ErrOCSPNoOCSPResponderURL is an error code for the case where the OCSP responder URL is not attached.
	ErrOCSPNoOCSPResponderURL = sferrors.ErrOCSPNoOCSPResponderURL

	/* query Status*/

	// ErrQueryStatus when check the status of a query, receive error or no status
	ErrQueryStatus = sferrors.ErrQueryStatus
	// ErrQueryIDFormat the query ID given to fetch its result is not valid
	ErrQueryIDFormat = sferrors.ErrQueryIDFormat
	// ErrQueryReportedError server side reports the query failed with error
	ErrQueryReportedError = sferrors.ErrQueryReportedError
	// ErrQueryIsRunning the query is still running
	ErrQueryIsRunning = sferrors.ErrQueryIsRunning

	/* GS error code */

	// ErrSessionGone is an GS error code for the case that session is already closed
	ErrSessionGone = sferrors.ErrSessionGone
	// ErrRoleNotExist is a GS error code for the case that the role specified does not exist
	ErrRoleNotExist = sferrors.ErrRoleNotExist
	// ErrObjectNotExistOrAuthorized is a GS error code for the case that the server-side object specified does not exist
	ErrObjectNotExistOrAuthorized = sferrors.ErrObjectNotExistOrAuthorized
)
