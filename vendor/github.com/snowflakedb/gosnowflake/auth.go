package gosnowflake

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/snowflakedb/gosnowflake/internal/compilation"
	internalos "github.com/snowflakedb/gosnowflake/internal/os"
)

const (
	clientType = "Go"
)

const (
	clientStoreTemporaryCredential = "CLIENT_STORE_TEMPORARY_CREDENTIAL"
	clientRequestMfaToken          = "CLIENT_REQUEST_MFA_TOKEN"
	idTokenAuthenticator           = "ID_TOKEN"
)

// AuthType indicates the type of authentication in Snowflake
type AuthType int

const (
	// AuthTypeSnowflake is the general username password authentication
	AuthTypeSnowflake AuthType = iota
	// AuthTypeOAuth is the OAuth authentication
	AuthTypeOAuth
	// AuthTypeExternalBrowser is to use a browser to access an Fed and perform SSO authentication
	AuthTypeExternalBrowser
	// AuthTypeOkta is to use a native okta URL to perform SSO authentication on Okta
	AuthTypeOkta
	// AuthTypeJwt is to use Jwt to perform authentication
	AuthTypeJwt
	// AuthTypeTokenAccessor is to use the provided token accessor and bypass authentication
	AuthTypeTokenAccessor
	// AuthTypeUsernamePasswordMFA is to use username and password with mfa
	AuthTypeUsernamePasswordMFA
	// AuthTypePat is to use programmatic access token
	AuthTypePat
	// AuthTypeOAuthAuthorizationCode is to use browser-based OAuth2 flow
	AuthTypeOAuthAuthorizationCode
	// AuthTypeOAuthClientCredentials is to use non-interactive OAuth2 flow
	AuthTypeOAuthClientCredentials
	// AuthTypeWorkloadIdentityFederation is to use CSP identity for authentication
	AuthTypeWorkloadIdentityFederation
)

func (authType AuthType) isOauthNativeFlow() bool {
	return authType == AuthTypeOAuthAuthorizationCode || authType == AuthTypeOAuthClientCredentials
}

var refreshOAuthTokenErrorCodes = []string{
	strconv.Itoa(ErrMissingAccessATokenButRefreshTokenPresent),
	invalidOAuthAccessTokenCode,
	expiredOAuthAccessTokenCode,
}

func determineAuthenticatorType(cfg *Config, value string) error {
	upperCaseValue := strings.ToUpper(value)
	lowerCaseValue := strings.ToLower(value)
	if strings.Trim(value, " ") == "" || upperCaseValue == AuthTypeSnowflake.String() {
		cfg.Authenticator = AuthTypeSnowflake
		return nil
	} else if upperCaseValue == AuthTypeOAuth.String() {
		cfg.Authenticator = AuthTypeOAuth
		return nil
	} else if upperCaseValue == AuthTypeJwt.String() {
		cfg.Authenticator = AuthTypeJwt
		return nil
	} else if upperCaseValue == AuthTypeExternalBrowser.String() {
		cfg.Authenticator = AuthTypeExternalBrowser
		return nil
	} else if upperCaseValue == AuthTypeUsernamePasswordMFA.String() {
		cfg.Authenticator = AuthTypeUsernamePasswordMFA
		return nil
	} else if upperCaseValue == AuthTypeTokenAccessor.String() {
		cfg.Authenticator = AuthTypeTokenAccessor
		return nil
	} else if upperCaseValue == AuthTypePat.String() {
		cfg.Authenticator = AuthTypePat
		return nil
	} else if upperCaseValue == AuthTypeOAuthAuthorizationCode.String() {
		cfg.Authenticator = AuthTypeOAuthAuthorizationCode
		return nil
	} else if upperCaseValue == AuthTypeOAuthClientCredentials.String() {
		cfg.Authenticator = AuthTypeOAuthClientCredentials
		return nil
	} else if upperCaseValue == AuthTypeWorkloadIdentityFederation.String() {
		cfg.Authenticator = AuthTypeWorkloadIdentityFederation
		return nil
	} else {
		// possibly Okta case
		oktaURLString, err := url.QueryUnescape(lowerCaseValue)
		if err != nil {
			return &SnowflakeError{
				Number:      ErrCodeFailedToParseAuthenticator,
				Message:     errMsgFailedToParseAuthenticator,
				MessageArgs: []interface{}{lowerCaseValue},
			}
		}

		oktaURL, err := url.Parse(oktaURLString)
		if err != nil {
			return &SnowflakeError{
				Number:      ErrCodeFailedToParseAuthenticator,
				Message:     errMsgFailedToParseAuthenticator,
				MessageArgs: []interface{}{oktaURLString},
			}
		}

		if oktaURL.Scheme != "https" {
			return &SnowflakeError{
				Number:      ErrCodeFailedToParseAuthenticator,
				Message:     errMsgFailedToParseAuthenticator,
				MessageArgs: []interface{}{oktaURLString},
			}
		}
		cfg.OktaURL = oktaURL
		cfg.Authenticator = AuthTypeOkta
	}
	return nil
}

func (authType AuthType) String() string {
	switch authType {
	case AuthTypeSnowflake:
		return "SNOWFLAKE"
	case AuthTypeOAuth:
		return "OAUTH"
	case AuthTypeExternalBrowser:
		return "EXTERNALBROWSER"
	case AuthTypeOkta:
		return "OKTA"
	case AuthTypeJwt:
		return "SNOWFLAKE_JWT"
	case AuthTypeTokenAccessor:
		return "TOKENACCESSOR"
	case AuthTypeUsernamePasswordMFA:
		return "USERNAME_PASSWORD_MFA"
	case AuthTypePat:
		return "PROGRAMMATIC_ACCESS_TOKEN"
	case AuthTypeOAuthAuthorizationCode:
		return "OAUTH_AUTHORIZATION_CODE"
	case AuthTypeOAuthClientCredentials:
		return "OAUTH_CLIENT_CREDENTIALS"
	case AuthTypeWorkloadIdentityFederation:
		return "WORKLOAD_IDENTITY"
	default:
		return "UNKNOWN"
	}
}

// userAgent shows up in User-Agent HTTP header
var userAgent = fmt.Sprintf("%v/%v (%v-%v) %v/%v",
	clientType,
	SnowflakeGoDriverVersion,
	runtime.GOOS,
	runtime.GOARCH,
	runtime.Compiler,
	runtime.Version())

type authRequestClientEnvironment struct {
	Application             string            `json:"APPLICATION"`
	ApplicationPath         string            `json:"APPLICATION_PATH"`
	Os                      string            `json:"OS"`
	OsVersion               string            `json:"OS_VERSION"`
	OsDetails               map[string]string `json:"OS_DETAILS,omitempty"`
	Isa                     string            `json:"ISA,omitempty"`
	OCSPMode                string            `json:"OCSP_MODE"`
	GoVersion               string            `json:"GO_VERSION"`
	OAuthType               string            `json:"OAUTH_TYPE,omitempty"`
	CertRevocationCheckMode string            `json:"CERT_REVOCATION_CHECK_MODE,omitempty"`
	Platform                []string          `json:"PLATFORM,omitempty"`
	CoreVersion             string            `json:"CORE_VERSION,omitempty"`
	CoreLoadError           string            `json:"CORE_LOAD_ERROR,omitempty"`
	CoreFileName            string            `json:"CORE_FILE_NAME,omitempty"`
	CgoEnabled              bool              `json:"CGO_ENABLED,omitempty"`
	LinkingMode             string            `json:"LINKING_MODE,omitempty"`
	LibcFamily              string            `json:"LIBC_FAMILY,omitempty"`
	LibcVersion             string            `json:"LIBC_VERSION,omitempty"`
}

type authRequestData struct {
	ClientAppID             string                       `json:"CLIENT_APP_ID"`
	ClientAppVersion        string                       `json:"CLIENT_APP_VERSION"`
	SvnRevision             string                       `json:"SVN_REVISION"`
	AccountName             string                       `json:"ACCOUNT_NAME"`
	LoginName               string                       `json:"LOGIN_NAME,omitempty"`
	Password                string                       `json:"PASSWORD,omitempty"`
	RawSAMLResponse         string                       `json:"RAW_SAML_RESPONSE,omitempty"`
	ExtAuthnDuoMethod       string                       `json:"EXT_AUTHN_DUO_METHOD,omitempty"`
	Passcode                string                       `json:"PASSCODE,omitempty"`
	Authenticator           string                       `json:"AUTHENTICATOR,omitempty"`
	SessionParameters       map[string]interface{}       `json:"SESSION_PARAMETERS,omitempty"`
	ClientEnvironment       authRequestClientEnvironment `json:"CLIENT_ENVIRONMENT"`
	BrowserModeRedirectPort string                       `json:"BROWSER_MODE_REDIRECT_PORT,omitempty"`
	ProofKey                string                       `json:"PROOF_KEY,omitempty"`
	Token                   string                       `json:"TOKEN,omitempty"`
	Provider                string                       `json:"PROVIDER,omitempty"`
}
type authRequest struct {
	Data authRequestData `json:"data"`
}

type nameValueParameter struct {
	Name  string      `json:"name"`
	Value interface{} `json:"value"`
}

type authResponseSessionInfo struct {
	DatabaseName  string `json:"databaseName"`
	SchemaName    string `json:"schemaName"`
	WarehouseName string `json:"warehouseName"`
	RoleName      string `json:"roleName"`
}

type authResponseMain struct {
	Token               string                  `json:"token,omitempty"`
	Validity            time.Duration           `json:"validityInSeconds,omitempty"`
	MasterToken         string                  `json:"masterToken,omitempty"`
	MasterValidity      time.Duration           `json:"masterValidityInSeconds"`
	MfaToken            string                  `json:"mfaToken,omitempty"`
	MfaTokenValidity    time.Duration           `json:"mfaTokenValidityInSeconds"`
	IDToken             string                  `json:"idToken,omitempty"`
	IDTokenValidity     time.Duration           `json:"idTokenValidityInSeconds"`
	DisplayUserName     string                  `json:"displayUserName"`
	ServerVersion       string                  `json:"serverVersion"`
	FirstLogin          bool                    `json:"firstLogin"`
	RemMeToken          string                  `json:"remMeToken"`
	RemMeValidity       time.Duration           `json:"remMeValidityInSeconds"`
	HealthCheckInterval time.Duration           `json:"healthCheckInterval"`
	NewClientForUpgrade string                  `json:"newClientForUpgrade"`
	SessionID           int64                   `json:"sessionId"`
	Parameters          []nameValueParameter    `json:"parameters"`
	SessionInfo         authResponseSessionInfo `json:"sessionInfo"`
	TokenURL            string                  `json:"tokenUrl,omitempty"`
	SSOURL              string                  `json:"ssoUrl,omitempty"`
	ProofKey            string                  `json:"proofKey,omitempty"`
}

type authResponse struct {
	Data    authResponseMain `json:"data"`
	Message string           `json:"message"`
	Code    string           `json:"code"`
	Success bool             `json:"success"`
}

func postAuth(
	ctx context.Context,
	sr *snowflakeRestful,
	client *http.Client,
	params *url.Values,
	headers map[string]string,
	bodyCreator bodyCreatorType,
	timeout time.Duration) (
	data *authResponse, err error) {
	params.Set(requestIDKey, getOrGenerateRequestIDFromContext(ctx).String())
	params.Set(requestGUIDKey, NewUUID().String())

	fullURL := sr.getFullURL(loginRequestPath, params)
	logger.WithContext(ctx).Infof("full URL: %v", fullURL)
	resp, err := sr.FuncAuthPost(ctx, client, fullURL, headers, bodyCreator, timeout, sr.MaxRetryCount)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.WithContext(ctx).Errorf("failed to close HTTP response body for %v. err: %v", fullURL, closeErr)
		}
	}()
	if resp.StatusCode == http.StatusOK {
		var respd authResponse
		err = json.NewDecoder(resp.Body).Decode(&respd)
		if err != nil {
			logger.WithContext(ctx).Errorf("failed to decode JSON. err: %v", err)
			return nil, err
		}
		return &respd, nil
	}
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		// service availability or connectivity issue. Most likely server side issue.
		return nil, &SnowflakeError{
			Number:      ErrCodeServiceUnavailable,
			SQLState:    SQLStateConnectionWasNotEstablished,
			Message:     errMsgServiceUnavailable,
			MessageArgs: []interface{}{resp.StatusCode, fullURL},
		}
	case http.StatusUnauthorized, http.StatusForbidden:
		// failed to connect to db. account name may be wrong
		return nil, &SnowflakeError{
			Number:      ErrCodeFailedToConnect,
			SQLState:    SQLStateConnectionRejected,
			Message:     errMsgFailedToConnect,
			MessageArgs: []interface{}{resp.StatusCode, fullURL},
		}
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.WithContext(ctx).Errorf("failed to extract HTTP response body. err: %v", err)
		return nil, err
	}
	logger.WithContext(ctx).Infof("HTTP: %v, URL: %v, Body: %v", resp.StatusCode, fullURL, b)
	logger.WithContext(ctx).Infof("Header: %v", resp.Header)
	return nil, &SnowflakeError{
		Number:      ErrFailedToAuth,
		SQLState:    SQLStateConnectionRejected,
		Message:     errMsgFailedToAuth,
		MessageArgs: []interface{}{resp.StatusCode, fullURL},
	}
}

// Generates a map of headers needed to authenticate
// with Snowflake.
func getHeaders() map[string]string {
	headers := make(map[string]string)
	headers[httpHeaderContentType] = headerContentTypeApplicationJSON
	headers[httpHeaderAccept] = headerAcceptTypeApplicationSnowflake
	headers[httpClientAppID] = clientType
	headers[httpClientAppVersion] = SnowflakeGoDriverVersion
	headers[httpHeaderUserAgent] = userAgent
	return headers
}

// Used to authenticate the user with Snowflake.
func authenticate(
	ctx context.Context,
	sc *snowflakeConn,
	samlResponse []byte,
	proofKey []byte,
) (resp *authResponseMain, err error) {
	if sc.cfg.Authenticator == AuthTypeTokenAccessor {
		logger.WithContext(ctx).Info("Bypass authentication using existing token from token accessor")
		sessionInfo := authResponseSessionInfo{
			DatabaseName:  sc.cfg.Database,
			SchemaName:    sc.cfg.Schema,
			WarehouseName: sc.cfg.Warehouse,
			RoleName:      sc.cfg.Role,
		}
		token, masterToken, sessionID := sc.cfg.TokenAccessor.GetTokens()
		return &authResponseMain{
			Token:       token,
			MasterToken: masterToken,
			SessionID:   sessionID,
			SessionInfo: sessionInfo,
		}, nil
	}

	headers := getHeaders()
	// Get the current application path
	applicationPath, err := os.Executable()
	if err != nil {
		logger.WithContext(ctx).Warnf("Failed to get executable path: %v", err)
		applicationPath = "unknown"
	}

	oauthType := ""
	switch sc.cfg.Authenticator {
	case AuthTypeOAuthAuthorizationCode:
		oauthType = "OAUTH_AUTHORIZATION_CODE"
	case AuthTypeOAuthClientCredentials:
		oauthType = "OAUTH_CLIENT_CREDENTIALS"
	}

	clientEnvironment := newAuthRequestClientEnvironment()
	clientEnvironment.Application = sc.cfg.Application
	clientEnvironment.ApplicationPath = applicationPath
	clientEnvironment.OAuthType = oauthType
	clientEnvironment.CertRevocationCheckMode = sc.cfg.CertRevocationCheckMode.String()
	clientEnvironment.Platform = getDetectedPlatforms()

	sessionParameters := make(map[string]interface{})
	paramsMutex.Lock()
	for k, v := range sc.cfg.Params {
		// upper casing to normalize keys
		sessionParameters[strings.ToUpper(k)] = *v
	}
	paramsMutex.Unlock()

	sessionParameters[sessionClientValidateDefaultParameters] = sc.cfg.ValidateDefaultParameters != ConfigBoolFalse
	if sc.cfg.ClientRequestMfaToken == ConfigBoolTrue {
		sessionParameters[clientRequestMfaToken] = true
	}
	if sc.cfg.ClientStoreTemporaryCredential == ConfigBoolTrue {
		sessionParameters[clientStoreTemporaryCredential] = true
	}
	bodyCreator := func() ([]byte, error) {
		return createRequestBody(sc, sessionParameters, clientEnvironment, proofKey, samlResponse)
	}

	params := &url.Values{}
	if sc.cfg.Database != "" {
		params.Add("databaseName", sc.cfg.Database)
	}
	if sc.cfg.Schema != "" {
		params.Add("schemaName", sc.cfg.Schema)
	}
	if sc.cfg.Warehouse != "" {
		params.Add("warehouse", sc.cfg.Warehouse)
	}
	if sc.cfg.Role != "" {
		params.Add("roleName", sc.cfg.Role)
	}

	logger.WithContext(ctx).Infof("Information for Auth: Host: %v, User: %v, Authenticator: %v, Params: %v, Protocol: %v, Port: %v, LoginTimeout: %v",
		sc.rest.Host, sc.cfg.User, sc.cfg.Authenticator.String(), params, sc.rest.Protocol, sc.rest.Port, sc.rest.LoginTimeout)

	respd, err := sc.rest.FuncPostAuth(ctx, sc.rest, sc.rest.getClientFor(sc.cfg.Authenticator), params, headers, bodyCreator, sc.rest.LoginTimeout)
	if err != nil {
		return nil, err
	}
	if !respd.Success {
		logger.WithContext(ctx).Errorln("Authentication FAILED")
		sc.rest.TokenAccessor.SetTokens("", "", -1)
		if sessionParameters[clientRequestMfaToken] == true {
			credentialsStorage.deleteCredential(newMfaTokenSpec(sc.cfg.Host, sc.cfg.User))
		}
		if sessionParameters[clientStoreTemporaryCredential] == true && sc.cfg.Authenticator == AuthTypeExternalBrowser {
			credentialsStorage.deleteCredential(newIDTokenSpec(sc.cfg.Host, sc.cfg.User))
		}
		if sessionParameters[clientStoreTemporaryCredential] == true && sc.cfg.Authenticator.isOauthNativeFlow() {
			credentialsStorage.deleteCredential(newOAuthAccessTokenSpec(sc.cfg.OauthTokenRequestURL, sc.cfg.User))
		}
		code, err := strconv.Atoi(respd.Code)
		if err != nil {
			return nil, err
		}
		return nil, (&SnowflakeError{
			Number:   code,
			SQLState: SQLStateConnectionRejected,
			Message:  respd.Message,
		}).exceptionTelemetry(sc)
	}
	logger.WithContext(ctx).Info("Authentication SUCCESS")
	sc.rest.TokenAccessor.SetTokens(respd.Data.Token, respd.Data.MasterToken, respd.Data.SessionID)
	if sessionParameters[clientRequestMfaToken] == true {
		token := respd.Data.MfaToken
		credentialsStorage.setCredential(newMfaTokenSpec(sc.cfg.Host, sc.cfg.User), token)
	}
	if sessionParameters[clientStoreTemporaryCredential] == true {
		token := respd.Data.IDToken
		credentialsStorage.setCredential(newIDTokenSpec(sc.cfg.Host, sc.cfg.User), token)
	}
	return &respd.Data, nil
}

func newAuthRequestClientEnvironment() authRequestClientEnvironment {
	var coreVersion string
	var coreLoadError string

	// Try to get minicore version, but don't block if it's not loaded yet
	if !compilation.MinicoreEnabled {
		logger.Trace("minicore disabled at compile time")
		coreLoadError = "Minicore is disabled at compile time (built with -tags minicore_disabled)"
	} else if strings.EqualFold(os.Getenv(disableMinicoreEnv), "true") {
		logger.Trace("minicore loading disabled")
		coreLoadError = "Minicore is disabled with SF_DISABLE_MINICORE env variable"
	} else if mc := getMiniCore(); mc != nil {
		var err error
		coreVersion, err = mc.FullVersion()
		if err != nil {
			logger.Debugf("Minicore loading failed. %v", err)
			var mcErr *miniCoreError
			if errors.As(err, &mcErr) {
				coreLoadError = fmt.Sprintf("Failed to load binary: %v", mcErr.errorType)
			} else {
				coreLoadError = "Failed to load binary: unknown"
			}
		}
	} else {
		// Minicore not loaded yet - this is expected during startup
		coreVersion = ""
		coreLoadError = "Minicore is still loading"
		logger.Debugf("Minicore not yet loaded for client environment telemetry")
	}
	libcInfo := internalos.GetLibcInfo()
	linkingMode, err := compilation.CheckDynamicLinking()
	if err != nil {
		logger.Debugf("cannot determine if app is dynamically linked: %v", err)
	}
	return authRequestClientEnvironment{
		Os:            runtime.GOOS,
		OsVersion:     osVersion,
		OsDetails:     internalos.GetOsDetails(),
		Isa:           runtime.GOARCH,
		GoVersion:     runtime.Version(),
		CoreVersion:   coreVersion,
		CoreFileName:  getMiniCoreFileName(),
		CoreLoadError: coreLoadError,
		CgoEnabled:    compilation.CgoEnabled,
		LinkingMode:   linkingMode.String(),
		LibcFamily:    libcInfo.Family,
		LibcVersion:   libcInfo.Version,
	}
}

func createRequestBody(sc *snowflakeConn, sessionParameters map[string]interface{},
	clientEnvironment authRequestClientEnvironment, proofKey []byte, samlResponse []byte,
) ([]byte, error) {
	requestMain := authRequestData{
		ClientAppID:       clientType,
		ClientAppVersion:  SnowflakeGoDriverVersion,
		AccountName:       sc.cfg.Account,
		SessionParameters: sessionParameters,
		ClientEnvironment: clientEnvironment,
	}

	switch sc.cfg.Authenticator {
	case AuthTypeExternalBrowser:
		if sc.cfg.IDToken != "" {
			requestMain.Authenticator = idTokenAuthenticator
			requestMain.Token = sc.cfg.IDToken
			requestMain.LoginName = sc.cfg.User
		} else {
			requestMain.ProofKey = string(proofKey)
			requestMain.Token = string(samlResponse)
			requestMain.LoginName = sc.cfg.User
			requestMain.Authenticator = AuthTypeExternalBrowser.String()
		}
	case AuthTypeOAuth:
		requestMain.LoginName = sc.cfg.User
		requestMain.Authenticator = AuthTypeOAuth.String()
		var err error
		if requestMain.Token, err = sc.cfg.getToken(); err != nil {
			return nil, fmt.Errorf("failed to get OAuth token: %w", err)
		}
	case AuthTypeOkta:
		samlResponse, err := authenticateBySAML(
			sc.ctx,
			sc.rest,
			sc.cfg.OktaURL,
			sc.cfg.Application,
			sc.cfg.Account,
			sc.cfg.User,
			sc.cfg.Password,
			sc.cfg.DisableSamlURLCheck)
		if err != nil {
			return nil, err
		}
		requestMain.RawSAMLResponse = string(samlResponse)
	case AuthTypeJwt:
		requestMain.Authenticator = AuthTypeJwt.String()

		jwtTokenString, err := prepareJWTToken(sc.cfg)
		if err != nil {
			return nil, err
		}
		requestMain.Token = jwtTokenString
	case AuthTypePat:
		logger.WithContext(sc.ctx).Info("Programmatic access token")
		requestMain.Authenticator = AuthTypePat.String()
		requestMain.LoginName = sc.cfg.User
		var err error
		if requestMain.Token, err = sc.cfg.getToken(); err != nil {
			return nil, fmt.Errorf("failed to get PAT token: %w", err)
		}
	case AuthTypeSnowflake:
		logger.WithContext(sc.ctx).Debug("Username and password")
		requestMain.LoginName = sc.cfg.User
		requestMain.Password = sc.cfg.Password
		switch {
		case sc.cfg.PasscodeInPassword:
			requestMain.ExtAuthnDuoMethod = "passcode"
		case sc.cfg.Passcode != "":
			requestMain.Passcode = sc.cfg.Passcode
			requestMain.ExtAuthnDuoMethod = "passcode"
		}
	case AuthTypeUsernamePasswordMFA:
		logger.WithContext(sc.ctx).Debug("Username and password MFA")
		requestMain.LoginName = sc.cfg.User
		requestMain.Password = sc.cfg.Password
		switch {
		case sc.cfg.MfaToken != "":
			requestMain.Token = sc.cfg.MfaToken
		case sc.cfg.PasscodeInPassword:
			requestMain.ExtAuthnDuoMethod = "passcode"
		case sc.cfg.Passcode != "":
			requestMain.Passcode = sc.cfg.Passcode
			requestMain.ExtAuthnDuoMethod = "passcode"
		}
	case AuthTypeOAuthAuthorizationCode:
		logger.WithContext(sc.ctx).Debug("OAuth authorization code")
		token, err := authenticateByAuthorizationCode(sc)
		if err != nil {
			return nil, err
		}
		requestMain.LoginName = sc.cfg.User
		requestMain.Token = token
	case AuthTypeOAuthClientCredentials:
		logger.WithContext(sc.ctx).Debug("OAuth client credentials")
		oauthClient, err := newOauthClient(sc.ctx, sc.cfg, sc)
		if err != nil {
			return nil, err
		}
		token, err := oauthClient.authenticateByOAuthClientCredentials()
		if err != nil {
			return nil, err
		}
		requestMain.LoginName = sc.cfg.User
		requestMain.Token = token
	case AuthTypeWorkloadIdentityFederation:
		logger.WithContext(sc.ctx).Debug("Workload Identity Federation")
		wifAttestationProvider := createWifAttestationProvider(sc.ctx, sc.cfg, sc.telemetry)
		wifAttestation, err := wifAttestationProvider.getAttestation(sc.cfg.WorkloadIdentityProvider)
		if err != nil {
			return nil, err
		}
		if wifAttestation == nil {
			return nil, errors.New("workload identity federation attestation is not available, please check your configuration")
		}
		requestMain.Authenticator = AuthTypeWorkloadIdentityFederation.String()
		requestMain.Token = wifAttestation.Credential
		requestMain.Provider = wifAttestation.ProviderType
	}

	logger.WithContext(sc.ctx).Debugf("Request body is created for the authentication. Authenticator: %s, User: %s, Account: %s", sc.cfg.Authenticator.String(), sc.cfg.User, sc.cfg.Account)

	authRequest := authRequest{
		Data: requestMain,
	}
	jsonBody, err := json.Marshal(authRequest)
	if err != nil {
		logger.WithContext(sc.ctx).Errorf("Failed to marshal JSON. err: %v", err)
		return nil, err
	}
	return jsonBody, nil
}

type oauthLockKey struct {
	tokenRequestURL string
	user            string
	flowType        string
}

func newOAuthAuthorizationCodeLockKey(tokenRequestURL, user string) *oauthLockKey {
	return &oauthLockKey{
		tokenRequestURL: tokenRequestURL,
		user:            user,
		flowType:        "authorization_code",
	}
}

func newRefreshTokenLockKey(tokenRequestURL, user string) *oauthLockKey {
	return &oauthLockKey{
		tokenRequestURL: tokenRequestURL,
		user:            user,
		flowType:        "refresh_token",
	}
}

func (o *oauthLockKey) lockID() string {
	return o.tokenRequestURL + "|" + o.user + "|" + o.flowType
}

func authenticateByAuthorizationCode(sc *snowflakeConn) (string, error) {
	oauthClient, err := newOauthClient(sc.ctx, sc.cfg, sc)
	if err != nil {
		return "", err
	}
	if !isEligibleForParallelLogin(sc.cfg, sc.cfg.ClientStoreTemporaryCredential) {
		return oauthClient.authenticateByOAuthAuthorizationCode()
	}

	lockKey := newOAuthAuthorizationCodeLockKey(oauthClient.tokenURL(), sc.cfg.User)
	valueAwaiter := valueAwaitHolder.get(lockKey)
	defer valueAwaiter.resumeOne()
	token, err := awaitValue(valueAwaiter, func() (string, error) {
		return credentialsStorage.getCredential(newOAuthAccessTokenSpec(oauthClient.tokenURL(), sc.cfg.User)), nil
	}, func(s string, err error) bool {
		return s != ""
	}, func() string {
		return ""
	})
	if err != nil || token != "" {
		return token, err
	}
	token, err = oauthClient.authenticateByOAuthAuthorizationCode()
	if err != nil {
		return "", err
	}
	valueAwaiter.done()
	return token, err
}

// Generate a JWT token in string given the configuration
func prepareJWTToken(config *Config) (string, error) {
	if config.PrivateKey == nil {
		return "", errors.New("trying to use keypair authentication, but PrivateKey was not provided in the driver config")
	}
	logger.Debug("preparing JWT for keypair authentication")
	pubBytes, err := x509.MarshalPKIXPublicKey(config.PrivateKey.Public())
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(pubBytes)

	accountName := extractAccountName(config.Account)
	userName := strings.ToUpper(config.User)

	issueAtTime := time.Now().UTC()
	jwtClaims := jwt.MapClaims{
		"iss": fmt.Sprintf("%s.%s.%s", accountName, userName, "SHA256:"+base64.StdEncoding.EncodeToString(hash[:])),
		"sub": fmt.Sprintf("%s.%s", accountName, userName),
		"iat": issueAtTime.Unix(),
		"nbf": time.Date(2015, 10, 10, 12, 0, 0, 0, time.UTC).Unix(),
		"exp": issueAtTime.Add(config.JWTExpireTimeout).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwtClaims)

	tokenString, err := token.SignedString(config.PrivateKey)

	if err != nil {
		return "", err
	}

	logger.Debugf("successfully generated JWT with following claims: %v", jwtClaims)
	return tokenString, err
}

type tokenLockKey struct {
	snowflakeHost string
	user          string
	tokenType     string
}

func newMfaTokenLockKey(snowflakeHost, user string) *tokenLockKey {
	return &tokenLockKey{
		snowflakeHost: snowflakeHost,
		user:          user,
		tokenType:     "MFA",
	}
}

func newIDTokenLockKey(snowflakeHost, user string) *tokenLockKey {
	return &tokenLockKey{
		snowflakeHost: snowflakeHost,
		user:          user,
		tokenType:     "ID",
	}
}

func (m *tokenLockKey) lockID() string {
	return m.snowflakeHost + "|" + m.user + "|" + m.tokenType
}

func authenticateWithConfig(sc *snowflakeConn) error {
	var authData *authResponseMain
	var samlResponse []byte
	var proofKey []byte
	var err error

	mfaTokenLockKey := newMfaTokenLockKey(sc.cfg.Host, sc.cfg.User)
	idTokenLockKey := newIDTokenLockKey(sc.cfg.Host, sc.cfg.User)

	if sc.cfg.Authenticator == AuthTypeExternalBrowser || sc.cfg.Authenticator == AuthTypeOAuthAuthorizationCode || sc.cfg.Authenticator == AuthTypeOAuthClientCredentials {
		if (runtime.GOOS == "windows" || runtime.GOOS == "darwin") && sc.cfg.ClientStoreTemporaryCredential == configBoolNotSet {
			sc.cfg.ClientStoreTemporaryCredential = ConfigBoolTrue
		}
		if sc.cfg.Authenticator == AuthTypeExternalBrowser {
			if isEligibleForParallelLogin(sc.cfg, sc.cfg.ClientStoreTemporaryCredential) {
				valueAwaiter := valueAwaitHolder.get(idTokenLockKey)
				defer valueAwaiter.resumeOne()
				sc.cfg.IDToken, _ = awaitValue(valueAwaiter, func() (string, error) {
					credential := credentialsStorage.getCredential(newIDTokenSpec(sc.cfg.Host, sc.cfg.User))
					return credential, nil
				}, func(s string, err error) bool {
					return s != ""
				}, func() string {
					return ""
				})
			} else if sc.cfg.ClientStoreTemporaryCredential == ConfigBoolTrue {
				sc.cfg.IDToken = credentialsStorage.getCredential(newIDTokenSpec(sc.cfg.Host, sc.cfg.User))
			}
		}
		// Disable console login by default
		if sc.cfg.DisableConsoleLogin == configBoolNotSet {
			sc.cfg.DisableConsoleLogin = ConfigBoolTrue
		}
	}

	if sc.cfg.Authenticator == AuthTypeUsernamePasswordMFA {
		if (runtime.GOOS == "windows" || runtime.GOOS == "darwin") && sc.cfg.ClientRequestMfaToken == configBoolNotSet {
			sc.cfg.ClientRequestMfaToken = ConfigBoolTrue
		}
		if isEligibleForParallelLogin(sc.cfg, sc.cfg.ClientRequestMfaToken) {
			valueAwaiter := valueAwaitHolder.get(mfaTokenLockKey)
			defer valueAwaiter.resumeOne()
			sc.cfg.MfaToken, _ = awaitValue(valueAwaiter, func() (string, error) {
				credential := credentialsStorage.getCredential(newMfaTokenSpec(sc.cfg.Host, sc.cfg.User))
				return credential, nil
			}, func(s string, err error) bool {
				return s != ""
			}, func() string {
				return ""
			})
		} else if sc.cfg.ClientRequestMfaToken == ConfigBoolTrue {
			sc.cfg.MfaToken = credentialsStorage.getCredential(newMfaTokenSpec(sc.cfg.Host, sc.cfg.User))
		}
	}

	logger.WithContext(sc.ctx).Infof("Authenticating via %v", sc.cfg.Authenticator.String())
	switch sc.cfg.Authenticator {
	case AuthTypeExternalBrowser:
		if sc.cfg.IDToken == "" {
			samlResponse, proofKey, err = authenticateByExternalBrowser(
				sc.ctx,
				sc.rest,
				sc.cfg.Authenticator.String(),
				sc.cfg.Application,
				sc.cfg.Account,
				sc.cfg.User,
				sc.cfg.ExternalBrowserTimeout,
				sc.cfg.DisableConsoleLogin)
			if err != nil {
				sc.cleanup()
				return err
			}
		}
	}
	authData, err = authenticate(
		sc.ctx,
		sc,
		samlResponse,
		proofKey)
	if err != nil {
		var se *SnowflakeError
		if errors.As(err, &se) && slices.Contains(refreshOAuthTokenErrorCodes, strconv.Itoa(se.Number)) {
			credentialsStorage.deleteCredential(newOAuthAccessTokenSpec(sc.cfg.OauthTokenRequestURL, sc.cfg.User))

			if sc.cfg.Authenticator == AuthTypeOAuthAuthorizationCode {
				doRefreshTokenWithLock(sc)
			}

			// if refreshing succeeds for authorization code, we will take a token from cache
			// if it fails, we will just run the full flow
			authData, err = authenticate(sc.ctx, sc, nil, nil)
		}
		if err != nil {
			sc.cleanup()
			return err
		}
	}
	if sc.cfg.Authenticator == AuthTypeUsernamePasswordMFA && isEligibleForParallelLogin(sc.cfg, sc.cfg.ClientRequestMfaToken) {
		valueAwaiter := valueAwaitHolder.get(mfaTokenLockKey)
		valueAwaiter.done()
	}
	if sc.cfg.Authenticator == AuthTypeExternalBrowser && isEligibleForParallelLogin(sc.cfg, sc.cfg.ClientStoreTemporaryCredential) {
		valueAwaiter := valueAwaitHolder.get(idTokenLockKey)
		valueAwaiter.done()
	}
	sc.populateSessionParameters(authData.Parameters)
	sc.ctx = context.WithValue(sc.ctx, SFSessionIDKey, authData.SessionID)
	return nil
}

func doRefreshTokenWithLock(sc *snowflakeConn) {
	if oauthClient, err := newOauthClient(sc.ctx, sc.cfg, sc); err != nil {
		logger.Warnf("failed to create oauth client. %v", err)
	} else {
		lockKey := newRefreshTokenLockKey(oauthClient.tokenURL(), sc.cfg.User)
		if _, err = getValueWithLock(chooseLockerForAuth(sc.cfg), lockKey, func() (string, error) {
			if err = oauthClient.refreshToken(); err != nil {
				logger.Warnf("cannot refresh token. %v", err)
				credentialsStorage.deleteCredential(newOAuthRefreshTokenSpec(sc.cfg.OauthTokenRequestURL, sc.cfg.User))
				return "", err
			}
			return "", nil
		}); err != nil {
			logger.Warnf("failed to refresh token with lock. %v", err)
		}
	}
}

func chooseLockerForAuth(cfg *Config) locker {
	if cfg.SingleAuthenticationPrompt == ConfigBoolFalse {
		return noopLocker
	}
	if cfg.User == "" {
		return noopLocker
	}
	return exclusiveLocker
}

func isEligibleForParallelLogin(cfg *Config, cacheEnabled ConfigBool) bool {
	return cfg.SingleAuthenticationPrompt != ConfigBoolFalse && cfg.User != "" && cacheEnabled == ConfigBoolTrue
}
