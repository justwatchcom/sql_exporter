package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	sferrors "github.com/snowflakedb/gosnowflake/v2/internal/errors"
	loggerinternal "github.com/snowflakedb/gosnowflake/v2/internal/logger"
)

var logger = loggerinternal.NewLoggerProxy()

const (
	// DefaultClientTimeout is the timeout for network round trip + read out http response
	DefaultClientTimeout = 900 * time.Second
	// DefaultJWTClientTimeout is the timeout for network round trip + read out http response but used for JWT auth
	DefaultJWTClientTimeout = 10 * time.Second
	// DefaultLoginTimeout is the timeout for retry for login EXCLUDING clientTimeout
	DefaultLoginTimeout = 300 * time.Second
	// DefaultRequestTimeout is the timeout for retry for request EXCLUDING clientTimeout
	DefaultRequestTimeout = 0 * time.Second
	// DefaultJWTTimeout is the timeout for JWT token expiration
	DefaultJWTTimeout = 60 * time.Second
	// DefaultExternalBrowserTimeout is the timeout for external browser login
	DefaultExternalBrowserTimeout = 120 * time.Second
	defaultCloudStorageTimeout    = -1 // Timeout for calling cloud storage.
	defaultMaxRetryCount          = 7  // specifies maximum number of subsequent retries
	// DefaultDomain is the default domain for Snowflake accounts
	DefaultDomain = ".snowflakecomputing.com"
	// CnDomain is the domain for Snowflake accounts in China
	CnDomain             = ".snowflakecomputing.cn"
	topLevelDomainPrefix = ".snowflakecomputing." // used to extract the domain from host
)

const clientType = "Go"

// GetFromEnv retrieves the value of an environment variable.
// If failOnMissing is true and the variable is not set, an error is returned.
func GetFromEnv(name string, failOnMissing bool) (string, error) {
	if value := os.Getenv(name); value != "" {
		return value, nil
	}
	if failOnMissing {
		return "", fmt.Errorf("%v environment variable is not set", name)
	}
	return "", nil
}

// DSN constructs a DSN for Snowflake db.
func DSN(cfg *Config) (dsn string, err error) {
	if strings.ToLower(cfg.Region) == "us-west-2" {
		cfg.Region = ""
	}
	// in case account includes region
	region, posDot := extractRegionFromAccount(cfg.Account)
	if strings.ToLower(region) == "us-west-2" {
		region = ""
		cfg.Account = cfg.Account[:posDot]
		logger.Info("Ignoring default region .us-west-2 in DSN from Account configuration.")
	}
	if region != "" {
		if cfg.Region != "" {
			return "", sferrors.ErrRegionConflict()
		}
		cfg.Region = region
		cfg.Account = cfg.Account[:posDot]
	}
	hasHost := true
	if cfg.Host == "" {
		hasHost = false
		if cfg.Region == "" {
			cfg.Host = cfg.Account + DefaultDomain
		} else {
			cfg.Host = buildHostFromAccountAndRegion(cfg.Account, cfg.Region)
		}
	}
	err = FillMissingConfigParameters(cfg)
	if err != nil {
		return "", err
	}
	params := &url.Values{}
	if hasHost && cfg.Account != "" {
		// account may not be included in a Host string
		params.Add("account", cfg.Account)
	}
	if cfg.Database != "" {
		params.Add("database", cfg.Database)
	}
	if cfg.Schema != "" {
		params.Add("schema", cfg.Schema)
	}
	if cfg.Warehouse != "" {
		params.Add("warehouse", cfg.Warehouse)
	}
	if cfg.Role != "" {
		params.Add("role", cfg.Role)
	}
	if cfg.Region != "" {
		params.Add("region", cfg.Region)
	}
	if cfg.OauthClientID != "" {
		params.Add("oauthClientId", cfg.OauthClientID)
	}
	if cfg.OauthClientSecret != "" {
		params.Add("oauthClientSecret", cfg.OauthClientSecret)
	}
	if cfg.OauthAuthorizationURL != "" {
		params.Add("oauthAuthorizationUrl", cfg.OauthAuthorizationURL)
	}
	if cfg.OauthTokenRequestURL != "" {
		params.Add("oauthTokenRequestUrl", cfg.OauthTokenRequestURL)
	}
	if cfg.OauthRedirectURI != "" {
		params.Add("oauthRedirectUri", cfg.OauthRedirectURI)
	}
	if cfg.OauthScope != "" {
		params.Add("oauthScope", cfg.OauthScope)
	}
	if cfg.EnableSingleUseRefreshTokens {
		params.Add("enableSingleUseRefreshTokens", strconv.FormatBool(cfg.EnableSingleUseRefreshTokens))
	}
	if cfg.WorkloadIdentityProvider != "" {
		params.Add("workloadIdentityProvider", cfg.WorkloadIdentityProvider)
	}
	if cfg.WorkloadIdentityEntraResource != "" {
		params.Add("workloadIdentityEntraResource", cfg.WorkloadIdentityEntraResource)
	}
	if cfg.WorkloadIdentityAzureClientID != "" {
		params.Add("workloadIdentityAzureClientId", cfg.WorkloadIdentityAzureClientID)
	}
	if len(cfg.WorkloadIdentityImpersonationPath) > 0 {
		params.Add("workloadIdentityImpersonationPath", strings.Join(cfg.WorkloadIdentityImpersonationPath, ","))
	}
	if cfg.WorkloadIdentityAwsUseOutboundToken != BoolNotSet {
		params.Add("workloadIdentityAwsUseOutboundToken", strconv.FormatBool(cfg.WorkloadIdentityAwsUseOutboundToken != BoolFalse))
	}
	if cfg.WorkloadIdentityHost != "" {
		params.Add("workloadIdentityHost", cfg.WorkloadIdentityHost)
	}
	if cfg.Authenticator != AuthTypeSnowflake {
		if cfg.Authenticator == AuthTypeOkta {
			params.Add("authenticator", strings.ToLower(cfg.OktaURL.String()))
		} else {
			params.Add("authenticator", strings.ToLower(cfg.Authenticator.String()))
		}
	}
	if cfg.SingleAuthenticationPrompt != BoolNotSet {
		if cfg.SingleAuthenticationPrompt == BoolTrue {
			params.Add("singleAuthenticationPrompt", "true")
		} else {
			params.Add("singleAuthenticationPrompt", "false")
		}
	}
	if cfg.Passcode != "" {
		params.Add("passcode", cfg.Passcode)
	}
	if cfg.PasscodeInPassword {
		params.Add("passcodeInPassword", strconv.FormatBool(cfg.PasscodeInPassword))
	}
	if cfg.ClientTimeout != DefaultClientTimeout {
		params.Add("clientTimeout", strconv.FormatInt(int64(cfg.ClientTimeout/time.Second), 10))
	}
	if cfg.JWTClientTimeout != DefaultJWTClientTimeout {
		params.Add("jwtClientTimeout", strconv.FormatInt(int64(cfg.JWTClientTimeout/time.Second), 10))
	}
	if cfg.LoginTimeout != DefaultLoginTimeout {
		params.Add("loginTimeout", strconv.FormatInt(int64(cfg.LoginTimeout/time.Second), 10))
	}
	if cfg.RequestTimeout != DefaultRequestTimeout {
		params.Add("requestTimeout", strconv.FormatInt(int64(cfg.RequestTimeout/time.Second), 10))
	}
	if cfg.JWTExpireTimeout != DefaultJWTTimeout {
		params.Add("jwtTimeout", strconv.FormatInt(int64(cfg.JWTExpireTimeout/time.Second), 10))
	}
	if cfg.ExternalBrowserTimeout != DefaultExternalBrowserTimeout {
		params.Add("externalBrowserTimeout", strconv.FormatInt(int64(cfg.ExternalBrowserTimeout/time.Second), 10))
	}
	if cfg.CloudStorageTimeout != defaultCloudStorageTimeout {
		params.Add("cloudStorageTimeout", strconv.FormatInt(int64(cfg.CloudStorageTimeout/time.Second), 10))
	}
	if cfg.CleanupTimeout != 0 {
		params.Add("cleanupTimeout", strconv.FormatInt(int64(cfg.CleanupTimeout/time.Second), 10))
	}
	if cfg.MaxRetryCount != defaultMaxRetryCount {
		params.Add("maxRetryCount", strconv.Itoa(cfg.MaxRetryCount))
	}
	if cfg.Application != clientType {
		params.Add("application", cfg.Application)
	}
	if cfg.Protocol != "" && cfg.Protocol != "https" {
		params.Add("protocol", cfg.Protocol)
	}
	if cfg.Token != "" {
		params.Add("token", cfg.Token)
	}
	if cfg.TokenFilePath != "" {
		params.Add("tokenFilePath", cfg.TokenFilePath)
	}
	if cfg.CertRevocationCheckMode != CertRevocationCheckDisabled {
		params.Add("certRevocationCheckMode", cfg.CertRevocationCheckMode.String())
	}
	if cfg.CrlAllowCertificatesWithoutCrlURL == BoolTrue {
		params.Add("crlAllowCertificatesWithoutCrlURL", "true")
	}
	if cfg.CrlInMemoryCacheDisabled {
		params.Add("crlInMemoryCacheDisabled", "true")
	}
	if cfg.CrlOnDiskCacheDisabled {
		params.Add("crlOnDiskCacheDisabled", "true")
	}
	if cfg.CrlDownloadMaxSize != 0 {
		params.Add("crlDownloadMaxSize", strconv.Itoa(cfg.CrlDownloadMaxSize))
	}
	if cfg.CrlHTTPClientTimeout != 0 {
		params.Add("crlHttpClientTimeout", strconv.FormatInt(int64(cfg.CrlHTTPClientTimeout/time.Second), 10))
	}
	if cfg.Params != nil {
		for k, v := range cfg.Params {
			params.Add(k, *v)
		}
	}
	if cfg.PrivateKey != nil {
		privateKeyInBytes, err := MarshalPKCS8PrivateKey(cfg.PrivateKey)
		if err != nil {
			return "", err
		}
		keyBase64 := base64.URLEncoding.EncodeToString(privateKeyInBytes)
		params.Add("privateKey", keyBase64)
	}
	if cfg.DisableOCSPChecks {
		params.Add("disableOCSPChecks", strconv.FormatBool(cfg.DisableOCSPChecks))
	}
	if cfg.Tracing != "" {
		params.Add("tracing", cfg.Tracing)
	}
	if cfg.LogQueryText {
		params.Add("logQueryText", strconv.FormatBool(cfg.LogQueryText))
	}
	if cfg.LogQueryParameters {
		params.Add("logQueryParameters", strconv.FormatBool(cfg.LogQueryParameters))
	}
	if cfg.TmpDirPath != "" {
		params.Add("tmpDirPath", cfg.TmpDirPath)
	}
	if cfg.DisableQueryContextCache {
		params.Add("disableQueryContextCache", "true")
	}
	if cfg.IncludeRetryReason == BoolFalse {
		params.Add("includeRetryReason", "false")
	}
	if cfg.ServerSessionKeepAlive {
		params.Add("serverSessionKeepAlive", "true")
	}

	if cfg.OCSPFailOpen != OCSPFailOpenNotSet {
		params.Add("ocspFailOpen", strconv.FormatBool(cfg.OCSPFailOpen == OCSPFailOpenTrue))
	}

	params.Add("validateDefaultParameters", strconv.FormatBool(cfg.ValidateDefaultParameters != BoolFalse))

	if cfg.ClientRequestMfaToken != BoolNotSet {
		params.Add("clientRequestMfaToken", strconv.FormatBool(cfg.ClientRequestMfaToken != BoolFalse))
	}

	if cfg.ClientStoreTemporaryCredential != BoolNotSet {
		params.Add("clientStoreTemporaryCredential", strconv.FormatBool(cfg.ClientStoreTemporaryCredential != BoolFalse))
	}
	if cfg.ClientConfigFile != "" {
		params.Add("clientConfigFile", cfg.ClientConfigFile)
	}
	if cfg.DisableConsoleLogin != BoolNotSet {
		params.Add("disableConsoleLogin", strconv.FormatBool(cfg.DisableConsoleLogin != BoolFalse))
	}
	if cfg.DisableSamlURLCheck != BoolNotSet {
		params.Add("disableSamlURLCheck", strconv.FormatBool(cfg.DisableSamlURLCheck != BoolFalse))
	}
	if cfg.ConnectionDiagnosticsEnabled {
		params.Add("connectionDiagnosticsEnabled", strconv.FormatBool(cfg.ConnectionDiagnosticsEnabled))
	}
	if cfg.ConnectionDiagnosticsAllowlistFile != "" {
		params.Add("connectionDiagnosticsAllowlistFile", cfg.ConnectionDiagnosticsAllowlistFile)
	}
	if cfg.TLSConfigName != "" {
		params.Add("tlsConfigName", cfg.TLSConfigName)
	}
	if cfg.ProxyHost != "" {
		params.Add("proxyHost", cfg.ProxyHost)
	}
	if cfg.ProxyPort != 0 {
		params.Add("proxyPort", strconv.Itoa(cfg.ProxyPort))
	}
	if cfg.ProxyProtocol != "" {
		params.Add("proxyProtocol", cfg.ProxyProtocol)
	}
	if cfg.ProxyUser != "" {
		params.Add("proxyUser", cfg.ProxyUser)
	}
	if cfg.ProxyPassword != "" {
		params.Add("proxyPassword", cfg.ProxyPassword)
	}
	if cfg.NoProxy != "" {
		params.Add("noProxy", cfg.NoProxy)
	}

	dsn = fmt.Sprintf("%v:%v@%v:%v", url.QueryEscape(cfg.User), url.QueryEscape(cfg.Password), cfg.Host, cfg.Port)
	if params.Encode() != "" {
		dsn += "?" + params.Encode()
	}
	return
}

// recordAccountShape marks AccountProvided on shape and records whether the
// raw account string carries a dot (region embedded) or a dash in its
// account portion (org-prefixed). shape may be nil. rawAccount is the
// user-supplied string before any normalization (truncation at the first
// dot, dash stripping for .global, etc.).
//
// The org-account dash heuristic intentionally looks only at the substring
// before the first dot, because a dash in the region portion (e.g.
// "myacct.us-east-1") is not an org separator and would otherwise be
// misclassified.
func recordAccountShape(shape *ConnectionIdentifierShape, rawAccount string) {
	if shape == nil || rawAccount == "" {
		return
	}
	shape.AccountProvided = true
	accountPortion := rawAccount
	if i := strings.Index(rawAccount, "."); i > 0 {
		shape.AccountWithRegion = true
		accountPortion = rawAccount[:i]
	}
	if strings.Contains(accountPortion, "-") {
		shape.AccountOrgProvided = true
	}
}

// inferInputShapeIfMissing populates cfg.inputShape from the current Config
// fields when shape capture has not already run (i.e. cfg was constructed
// programmatically rather than via ParseDSN or LoadConnectionConfig). It is
// a no-op when the shape is already non-nil.
//
// FillMissingConfigParameters runs this as its first step, before any field
// mutation, so callers do not need to invoke it explicitly. The function
// stays unexported because shape capture is an internal telemetry concern.
func inferInputShapeIfMissing(cfg *Config) {
	if cfg == nil || cfg.inputShape != nil {
		return
	}
	shape := &ConnectionIdentifierShape{}
	if cfg.Account != "" {
		recordAccountShape(shape, cfg.Account)
	}
	if cfg.Region != "" {
		shape.RegionProvided = true
	}
	if cfg.Host != "" {
		shape.HostProvided = true
	}
	cfg.inputShape = shape
}

// ParseDSN parses the DSN string to a Config.
func ParseDSN(dsn string) (cfg *Config, err error) {
	// New config with some default values
	cfg = &Config{
		Params:        make(map[string]*string),
		Authenticator: AuthTypeSnowflake, // Default to snowflake
		inputShape:    &ConnectionIdentifierShape{},
	}

	// user[:password]@account/database/schema[?param1=value1&paramN=valueN]
	// or
	// user[:password]@account/database[?param1=value1&paramN=valueN]
	// or
	// user[:password]@host:port/database/schema?account=user_account[?param1=value1&paramN=valueN]
	// or
	// host:port/database/schema?account=user_account[?param1=value1&paramN=valueN]

	foundSlash := false
	secondSlash := false
	done := false
	var i int
	posQuestion := len(dsn)
	for i = len(dsn) - 1; i >= 0; i-- {
		switch dsn[i] {
		case '/':
			foundSlash = true

			// left part is empty if i <= 0
			var j int
			posSecondSlash := i
			if i > 0 {
				for j = i - 1; j >= 0; j-- {
					switch dsn[j] {
					case '/':
						// second slash
						secondSlash = true
						posSecondSlash = j
					case '@':
						// username[:password]@...
						cfg.User, cfg.Password = parseUserPassword(j, dsn)
					}
					if dsn[j] == '@' {
						break
					}
				}

				// account or host:port
				err = parseAccountHostPort(cfg, j, posSecondSlash, dsn)
				if err != nil {
					return nil, err
				}
			}
			// [?param1=value1&...&paramN=valueN]
			// Find the first '?' in dsn[i+1:]
			err = parseParams(cfg, i, dsn)
			if err != nil {
				return
			}
			if secondSlash {
				cfg.Database = dsn[posSecondSlash+1 : i]
				cfg.Schema = dsn[i+1 : posQuestion]
			} else {
				cfg.Database = dsn[posSecondSlash+1 : posQuestion]
			}
			done = true
		case '?':
			posQuestion = i
		}
		if done {
			break
		}
	}
	if !foundSlash {
		// no db or schema is specified
		var j int
		for j = len(dsn) - 1; j >= 0; j-- {
			switch dsn[j] {
			case '@':
				cfg.User, cfg.Password = parseUserPassword(j, dsn)
			case '?':
				posQuestion = j
			}
			if dsn[j] == '@' {
				break
			}
		}
		err = parseAccountHostPort(cfg, j, posQuestion, dsn)
		if err != nil {
			return nil, err
		}
		err = parseParams(cfg, posQuestion-1, dsn)
		if err != nil {
			return
		}
	}
	if posDot := strings.Index(cfg.Account, "."); posDot >= 0 {
		cfg.Account = cfg.Account[:posDot]
	}

	err = FillMissingConfigParameters(cfg)
	if err != nil {
		return nil, err
	}

	// unescape parameters
	var s string
	s, err = url.QueryUnescape(cfg.User)
	if err != nil {
		return nil, err
	}
	cfg.User = s
	s, err = url.QueryUnescape(cfg.Password)
	if err != nil {
		return nil, err
	}
	cfg.Password = s
	s, err = url.QueryUnescape(cfg.Database)
	if err != nil {
		return nil, err
	}
	cfg.Database = s
	s, err = url.QueryUnescape(cfg.Schema)
	if err != nil {
		return nil, err
	}
	cfg.Schema = s
	s, err = url.QueryUnescape(cfg.Role)
	if err != nil {
		return nil, err
	}
	cfg.Role = s
	s, err = url.QueryUnescape(cfg.Warehouse)
	if err != nil {
		return nil, err
	}
	cfg.Warehouse = s
	return cfg, nil
}

// applyAccountFromHostIfMissing sets Account to the first DNS label of Host when Account is empty
// and Host matches the Snowflake hostname heuristic (hostIncludesTopLevelDomain). FillMissingConfigParameters
// invokes this so programmatic Config (e.g. database/sql.Connector) matches behavior that DSN users
// already got via ParseDSN plus FillMissingConfigParameters. ParseDSN still truncates dotted account
// values from parameters before FillMissingConfigParameters; that step does not apply to non-empty
// Account set directly on a programmatic Config.
func applyAccountFromHostIfMissing(cfg *Config) {
	if strings.TrimSpace(cfg.Account) != "" {
		return
	}
	if !hostIncludesTopLevelDomain(cfg.Host) {
		return
	}
	posDot := strings.Index(cfg.Host, ".")
	if posDot <= 0 {
		return
	}
	cfg.Account = cfg.Host[:posDot]
}

// FillMissingConfigParameters fills in default values for missing config parameters.
func FillMissingConfigParameters(cfg *Config) error {
	// Capture user-supplied provenance before any field below normalizes the
	// Config. Idempotent: ParseDSN and LoadConnectionConfig have already set
	// cfg.inputShape, so this only matters for programmatic Configs handed
	// to FillMissingConfigParameters directly (e.g. via database/sql
	// Connector or driver.OpenWithConfig).
	inferInputShapeIfMissing(cfg)

	// Single choke point for the account/region hostname-component invariant.
	// It must stay here — above applyAccountFromHostIfMissing and above every
	// cfg.Host assignment this function makes — for two reasons:
	//   1. cfg.Account is still the value the *user* supplied. Once
	//      applyAccountFromHostIfMissing has run, Account may instead be the
	//      first DNS label of an operator-supplied Host, and rejecting that
	//      synthesized value would break working explicit-host connections.
	//   2. It precedes the derivations that concatenate Account and Region into
	//      a hostname (below, and in DSN/transformAccountToHost via the error
	//      this returns), so no invalid identifier can reach the network.
	// An empty account is not this function's business: the existing
	// ErrEmptyAccount check below still owns that case and keeps its code.
	if err := validateAccountAndRegion(cfg); err != nil {
		return err
	}

	applyAccountFromHostIfMissing(cfg)

	if cfg.Host != "" {
		lower := strings.ToLower(cfg.Host)
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			return sferrors.ErrHostWithScheme(cfg.Host)
		}
	}

	posDash := strings.LastIndex(cfg.Account, "-")
	if posDash > 0 {
		if strings.Contains(strings.ToLower(cfg.Host), ".global.") {
			cfg.Account = cfg.Account[:posDash]
		}
	}
	if strings.Trim(cfg.Account, " ") == "" {
		return sferrors.ErrEmptyAccount()
	}

	if authRequiresUser(cfg) && strings.TrimSpace(cfg.User) == "" {
		return sferrors.ErrEmptyUsername()
	}

	if authRequiresPassword(cfg) && strings.TrimSpace(cfg.Password) == "" {
		return sferrors.ErrEmptyPassword()
	}

	if authRequiresToken(cfg) && strings.TrimSpace(cfg.Token) == "" && strings.TrimSpace(cfg.TokenFilePath) == "" {
		return sferrors.ErrEmptyToken()
	}

	if authRequiresClientIDAndSecret(cfg) && (strings.TrimSpace(cfg.OauthClientID) == "" || strings.TrimSpace(cfg.OauthClientSecret) == "") {
		return sferrors.ErrEmptyOAuthParameters()
	}
	if strings.Trim(cfg.Protocol, " ") == "" {
		cfg.Protocol = "https"
	}
	if cfg.Port == 0 {
		cfg.Port = 443
	}

	cfg.Region = strings.Trim(cfg.Region, " ")
	if cfg.Region != "" && !hostProvidedByUser(cfg) {
		// Region is specified but not included in Host. This only patches up a
		// Host the driver itself synthesized from Account: transformAccountToHost
		// builds Host before parseDSNParams reads the region parameter, so the
		// region would otherwise be dropped. It must not run when the user gave
		// Host explicitly — Host takes precedence over Region.
		domain, i := extractDomainFromHost(cfg.Host)
		if i >= 1 {
			hostPrefix := cfg.Host[0:i]
			if !strings.HasSuffix(hostPrefix, cfg.Region) {
				cfg.Host = fmt.Sprintf("%v.%v%v", hostPrefix, cfg.Region, domain)
			}
		}
	}
	if cfg.Host == "" {
		if cfg.Region != "" {
			cfg.Host = cfg.Account + "." + cfg.Region + getDomainBasedOnRegion(cfg.Region)
		} else {
			region, _ := extractRegionFromAccount(cfg.Account)
			if region != "" {
				cfg.Host = cfg.Account + getDomainBasedOnRegion(region)
			} else {
				cfg.Host = cfg.Account + DefaultDomain
			}
		}
	}
	// Verify the host is a recognized Snowflake endpoint before fetching cloud credentials.
	// Deliberately uses isSnowflakeHostForWorkloadIdentity (a label-boundary suffix match), not
	// hostIncludesTopLevelDomain (a bare substring test used elsewhere for account/port inference),
	// which has different, looser correctness requirements.
	if cfg.Authenticator == AuthTypeWorkloadIdentityFederation && !isSnowflakeHostForWorkloadIdentity(cfg.Host) {
		return sferrors.ErrWifNonSnowflakeHost(cfg.Host)
	}
	if cfg.LoginTimeout == 0 {
		cfg.LoginTimeout = DefaultLoginTimeout
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	if cfg.JWTExpireTimeout == 0 {
		cfg.JWTExpireTimeout = DefaultJWTTimeout
	}
	if cfg.ClientTimeout == 0 {
		cfg.ClientTimeout = DefaultClientTimeout
	}
	if cfg.JWTClientTimeout == 0 {
		cfg.JWTClientTimeout = DefaultJWTClientTimeout
	}
	if cfg.ExternalBrowserTimeout == 0 {
		cfg.ExternalBrowserTimeout = DefaultExternalBrowserTimeout
	}
	if cfg.CloudStorageTimeout == 0 {
		cfg.CloudStorageTimeout = defaultCloudStorageTimeout
	}
	if cfg.MaxRetryCount == 0 {
		cfg.MaxRetryCount = defaultMaxRetryCount
	}
	if strings.Trim(cfg.Application, " ") == "" {
		cfg.Application = clientType
	}

	if cfg.ValidateDefaultParameters == BoolNotSet {
		cfg.ValidateDefaultParameters = BoolTrue
	}

	if cfg.IncludeRetryReason == BoolNotSet {
		cfg.IncludeRetryReason = BoolTrue
	}

	if cfg.ProxyHost != "" && cfg.ProxyProtocol == "" {
		cfg.ProxyProtocol = "http" // Default to http if not specified
	}

	domain, _ := extractDomainFromHost(cfg.Host)
	if len(cfg.Host) == len(domain) {
		return &sferrors.SnowflakeError{
			Number:      sferrors.ErrCodeFailedToParseHost,
			Message:     sferrors.ErrMsgFailedToParseHost,
			MessageArgs: []any{cfg.Host},
		}
	}
	if cfg.TLSConfigName != "" {
		if _, ok := GetTLSConfig(cfg.TLSConfigName); !ok {
			return &sferrors.SnowflakeError{
				Number:  sferrors.ErrCodeMissingTLSConfig,
				Message: fmt.Sprintf(sferrors.ErrMsgMissingTLSConfig, cfg.TLSConfigName),
			}
		}
	}
	return nil
}

func extractDomainFromHost(host string) (domain string, index int) {
	i := strings.LastIndex(strings.ToLower(host), topLevelDomainPrefix)
	if i >= 1 {
		domain = host[i:]
		return domain, i
	}
	return "", i
}

func getDomainBasedOnRegion(region string) string {
	if strings.HasPrefix(strings.ToLower(region), "cn-") {
		return CnDomain
	}
	return DefaultDomain
}

func extractRegionFromAccount(account string) (region string, posDot int) {
	posDot = strings.Index(strings.ToLower(account), ".")
	if posDot > 0 {
		return account[posDot+1:], posDot
	}
	return "", posDot
}

func hostIncludesTopLevelDomain(host string) bool {
	return strings.Contains(strings.ToLower(host), topLevelDomainPrefix)
}

// hostProvidedByUser reports whether Host came from the user rather than from
// driver-side synthesis out of Account and Region. Callers must run after
// inferInputShapeIfMissing, which guarantees a non-nil shape.
func hostProvidedByUser(cfg *Config) bool {
	return cfg.inputShape != nil && cfg.inputShape.HostProvided
}

// wifAllowedHostSuffixesEnvVar is the ONLY escape hatch for the
// isSnowflakeHostForWorkloadIdentity allowlist. Read only from the process
// environment - never from the DSN, connection parameters or configuration
// files - so connection configuration cannot influence the allowlist.
// Entries are additive: they extend the recognized-host list and cannot
// disable it.
const wifAllowedHostSuffixesEnvVar = "SNOWFLAKE_WIF_ALLOWED_HOST_SUFFIXES"

// wifAllowedHostSuffixes are the Snowflake-owned domain suffixes that the
// WORKLOAD_IDENTITY authenticator is permitted to send ambient cloud
// credentials to. Apex (no subdomain) is accepted for parity with the other
// drivers' equivalent check.
var wifAllowedHostSuffixes = []string{
	"snowflakecomputing.com",
	"snowflakecomputing.cn",
	"snowflakecomputing.mil",
}

// NormalizeHost normalizes a host string for allow-list matching and URL
// construction. It trims whitespace, lowercases ASCII A–Z only (to avoid
// Unicode case-folding surprises such as the Kelvin sign U+212A folding to
// 'k'), strips everything from the first ':' onward (port), then strips
// exactly one trailing '.'. Non-ASCII characters are left unchanged so that
// IsLdhHost can reject them.
//
// The port must be stripped before the trailing dot: a host in FQDN form
// with an explicit port (e.g. "acct.snowflakecomputing.com.:443") carries
// the dot immediately before the colon, so removing the colon-and-tail
// first leaves the trailing dot to be stripped in the next step.
func NormalizeHost(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}, s)
	if idx := strings.IndexByte(s, ':'); idx >= 0 {
		s = s[:idx]
	}
	s = strings.TrimSuffix(s, ".")
	return s
}

// IsLdhHost returns true if the already-normalized host contains only
// LDH characters ([a-z0-9_-] per label, separated by '.'). Each label must
// be non-empty.
//
// This is an allow-list, not a block-list. Enumerating forbidden characters
// does not work: URL parsers variously terminate the authority at a space,
// semicolon, quote, percent-escape, or a full-width look-alike of '.', '#',
// or '?'. A host whose trailing labels are a recognized Snowflake domain can
// still begin with an unrelated name, so a parser stopping early resolves
// that unrelated name instead.
func IsLdhHost(normalized string) bool {
	if normalized == "" {
		return false
	}
	labelLen := 0
	for _, c := range normalized {
		switch {
		case c == '.':
			if labelLen == 0 {
				return false
			}
			labelLen = 0
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_':
			labelLen++
		default:
			return false
		}
	}
	return labelLen > 0
}

// IsSnowflakeHost returns true if the already-normalized host ends at a
// recognized Snowflake domain suffix, anchored at a label boundary. Unlike
// isSnowflakeHostForWorkloadIdentity it does not consult the
// SNOWFLAKE_WIF_ALLOWED_HOST_SUFFIXES escape hatch.
func IsSnowflakeHost(normalized string) bool {
	for _, s := range wifAllowedHostSuffixes {
		if normalized == s || strings.HasSuffix(normalized, "."+s) {
			return true
		}
	}
	return false
}

// extraWifAllowedHostSuffixesFromEnv parses the additive escape-hatch
// suffixes from SNOWFLAKE_WIF_ALLOWED_HOST_SUFFIXES (comma-separated), normalizing
// each entry and dropping empty ones. It never disables the check - it can
// only widen the allowlist.
func extraWifAllowedHostSuffixesFromEnv() []string {
	raw := os.Getenv(wifAllowedHostSuffixesEnvVar)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var extra []string
	for part := range strings.SplitSeq(raw, ",") {
		s := NormalizeHost(part)
		if s != "" {
			extra = append(extra, s)
		}
	}
	if len(extra) > 0 {
		logger.Infof("WORKLOAD_IDENTITY host allowlist extended via %v with additional suffixes: %v", wifAllowedHostSuffixesEnvVar, extra)
	}
	return extra
}

// isSnowflakeHostForWorkloadIdentity is a suffix-anchored allowlist that
// restricts Workload Identity attestation to recognized Snowflake hosts
// before any cloud credential is fetched. Unlike hostIncludesTopLevelDomain
// (a substring test used elsewhere for account/port inference), matching
// here is anchored to a label boundary at the end of the host, so only the
// listed suffixes and their subdomains are recognized.
func isSnowflakeHostForWorkloadIdentity(host string) bool {
	h := NormalizeHost(host)
	if h == "" {
		return false
	}
	if IsSnowflakeHost(h) {
		return true
	}
	for _, s := range extraWifAllowedHostSuffixesFromEnv() {
		if h == s || strings.HasSuffix(h, "."+s) {
			return true
		}
	}
	return false
}

func buildHostFromAccountAndRegion(account, region string) string {
	return account + "." + region + getDomainBasedOnRegion(region)
}

func authRequiresUser(cfg *Config) bool {
	return cfg.Authenticator != AuthTypeOAuth &&
		cfg.Authenticator != AuthTypeTokenAccessor &&
		cfg.Authenticator != AuthTypeExternalBrowser &&
		cfg.Authenticator != AuthTypePat &&
		cfg.Authenticator != AuthTypeOAuthAuthorizationCode &&
		cfg.Authenticator != AuthTypeOAuthClientCredentials &&
		cfg.Authenticator != AuthTypeWorkloadIdentityFederation
}

func authRequiresPassword(cfg *Config) bool {
	return cfg.Authenticator != AuthTypeOAuth &&
		cfg.Authenticator != AuthTypeTokenAccessor &&
		cfg.Authenticator != AuthTypeExternalBrowser &&
		cfg.Authenticator != AuthTypeJwt &&
		cfg.Authenticator != AuthTypePat &&
		cfg.Authenticator != AuthTypeOAuthAuthorizationCode &&
		cfg.Authenticator != AuthTypeOAuthClientCredentials &&
		cfg.Authenticator != AuthTypeWorkloadIdentityFederation
}

func authRequiresToken(cfg *Config) bool {
	return cfg.Authenticator == AuthTypePat
}

func authRequiresClientIDAndSecret(cfg *Config) bool {
	return cfg.Authenticator == AuthTypeOAuthAuthorizationCode
}

// transformAccountToHost transforms account to host
func transformAccountToHost(cfg *Config) (err error) {
	if cfg.Port == 0 && cfg.Host != "" && !hostIncludesTopLevelDomain(cfg.Host) {
		// account name is specified instead of host:port
		cfg.Account = cfg.Host
		// Capture the user-supplied account shape before normalization
		// truncates/rewrites it. A dot signals "account.region" form; a dash
		// signals "org-account" form.
		recordAccountShape(cfg.inputShape, cfg.Account)
		region, posDot := extractRegionFromAccount(cfg.Account)
		if strings.ToLower(region) == "us-west-2" {
			region = ""
			cfg.Account = cfg.Account[:posDot]
			logger.Info("Ignoring default region .us-west-2 from Account configuration.")
		}
		if region != "" {
			cfg.Region = region
			cfg.Account = cfg.Account[:posDot]
			cfg.Host = buildHostFromAccountAndRegion(cfg.Account, cfg.Region)
		} else {
			cfg.Host = cfg.Account + DefaultDomain
		}
		cfg.Port = 443
	}
	return nil
}

// parseAccountHostPort parses the DSN string to attempt to get account or host and port.
func parseAccountHostPort(cfg *Config, posAt, posSlash int, dsn string) (err error) {
	// account or host:port
	var k int
	for k = posAt + 1; k < posSlash; k++ {
		if dsn[k] == ':' {
			cfg.Port, err = strconv.Atoi(dsn[k+1 : posSlash])
			if err != nil {
				err = &sferrors.SnowflakeError{
					Number:      sferrors.ErrCodeFailedToParsePort,
					Message:     sferrors.ErrMsgFailedToParsePort,
					MessageArgs: []any{dsn[k+1 : posSlash]},
				}
				return
			}
			break
		}
	}
	cfg.Host = dsn[posAt+1 : k]
	// The DSN carries a real host (not an account-form authority that
	// transformAccountToHost will rewrite) when cfg.Host is non-empty AND
	// either of:
	//   1) the authority contains the Snowflake TLD, e.g. "myacct.snowflakecomputing.com", or
	//   2) the authority carries an explicit port, e.g. "localhost:443" — the
	//      transformAccountToHost guard only fires when Port == 0, so any
	//      explicit port means we never rewrite Host out from under the user.
	// The Host-non-empty check guards against degenerate forms like
	// "u:p@:8080/db" where the port is set but the host slice is empty.
	// Capture HostProvided in both real cases so non-Snowflake-TLD targets
	// (localhost, IP, private DNS) aren't silently misclassified as host-not-provided.
	if cfg.inputShape != nil && cfg.Host != "" && (hostIncludesTopLevelDomain(cfg.Host) || cfg.Port != 0) {
		cfg.inputShape.HostProvided = true
	}
	return transformAccountToHost(cfg)
}

// parseUserPassword parses the DSN string for username and password
func parseUserPassword(posAt int, dsn string) (user, password string) {
	var k int
	for k = 0; k < posAt; k++ {
		if dsn[k] == ':' {
			password = dsn[k+1 : posAt]
			break
		}
	}
	user = dsn[:k]
	return
}

// parseParams parse parameters
func parseParams(cfg *Config, posQuestion int, dsn string) (err error) {
	for j := posQuestion + 1; j < len(dsn); j++ {
		if dsn[j] == '?' {
			if err = parseDSNParams(cfg, dsn[j+1:]); err != nil {
				return
			}
			break
		}
	}
	return
}

// parseDSNParams parses the DSN "query string". Values must be url.QueryEscape'ed
func parseDSNParams(cfg *Config, params string) (err error) {
	logger.Infof("Query String: %v\n", params)
	paramsSlice := strings.SplitSeq(params, "&")
	for v := range paramsSlice {
		param := strings.SplitN(v, "=", 2)
		if len(param) != 2 {
			continue
		}
		var value string
		value, err = url.QueryUnescape(param[1])
		if err != nil {
			return err
		}
		switch param[0] {
		// Disable INFILE whitelist / enable all files
		case "account":
			cfg.Account = value
			recordAccountShape(cfg.inputShape, value)
		case "warehouse":
			cfg.Warehouse = value
		case "database":
			cfg.Database = value
		case "schema":
			cfg.Schema = value
		case "role":
			cfg.Role = value
		case "region":
			cfg.Region = value
			if cfg.inputShape != nil {
				cfg.inputShape.RegionProvided = true
			}
		case "protocol":
			cfg.Protocol = value
		case "singleAuthenticationPrompt":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			if vv {
				cfg.SingleAuthenticationPrompt = BoolTrue
			} else {
				cfg.SingleAuthenticationPrompt = BoolFalse
			}
		case "passcode":
			cfg.Passcode = value
		case "oauthClientId":
			cfg.OauthClientID = value
		case "oauthClientSecret":
			cfg.OauthClientSecret = value
		case "oauthAuthorizationUrl":
			cfg.OauthAuthorizationURL = value
		case "oauthTokenRequestUrl":
			cfg.OauthTokenRequestURL = value
		case "oauthRedirectUri":
			cfg.OauthRedirectURI = value
		case "oauthScope":
			cfg.OauthScope = value
		case "enableSingleUseRefreshTokens":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			cfg.EnableSingleUseRefreshTokens = vv
		case "passcodeInPassword":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			cfg.PasscodeInPassword = vv
		case "clientTimeout":
			cfg.ClientTimeout, err = parseTimeout(value)
			if err != nil {
				return
			}
		case "jwtClientTimeout":
			cfg.JWTClientTimeout, err = parseTimeout(value)
			if err != nil {
				return
			}
		case "loginTimeout":
			cfg.LoginTimeout, err = parseTimeout(value)
			if err != nil {
				return
			}
		case "requestTimeout":
			cfg.RequestTimeout, err = parseTimeout(value)
			if err != nil {
				return
			}
		case "jwtTimeout":
			cfg.JWTExpireTimeout, err = parseTimeout(value)
			if err != nil {
				return err
			}
		case "externalBrowserTimeout":
			cfg.ExternalBrowserTimeout, err = parseTimeout(value)
			if err != nil {
				return err
			}
		case "cloudStorageTimeout":
			cfg.CloudStorageTimeout, err = parseTimeout(value)
			if err != nil {
				return err
			}
		case "cleanupTimeout":
			cfg.CleanupTimeout, err = parseTimeout(value)
			if err != nil {
				return err
			}
		case "maxRetryCount":
			cfg.MaxRetryCount, err = strconv.Atoi(value)
			if err != nil {
				return err
			}
		case "serverSessionKeepAlive":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			cfg.ServerSessionKeepAlive = vv
		case "application":
			cfg.Application = value
		case "authenticator":
			err := DetermineAuthenticatorType(cfg, value)
			if err != nil {
				return err
			}
		case "disableOCSPChecks":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			cfg.DisableOCSPChecks = vv
		case "ocspFailOpen":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			if vv {
				cfg.OCSPFailOpen = OCSPFailOpenTrue
			} else {
				cfg.OCSPFailOpen = OCSPFailOpenFalse
			}

		case "token":
			cfg.Token = value
		case "tokenFilePath":
			cfg.TokenFilePath = value
		case "tlsConfigName":
			cfg.TLSConfigName = value
		case "workloadIdentityProvider":
			cfg.WorkloadIdentityProvider = value
		case "workloadIdentityEntraResource":
			cfg.WorkloadIdentityEntraResource = value
		case "workloadIdentityAzureClientId":
			cfg.WorkloadIdentityAzureClientID = value
		case "workloadIdentityImpersonationPath":
			cfg.WorkloadIdentityImpersonationPath = strings.Split(value, ",")
		case "workloadIdentityAwsUseOutboundToken":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			if vv {
				cfg.WorkloadIdentityAwsUseOutboundToken = BoolTrue
			} else {
				cfg.WorkloadIdentityAwsUseOutboundToken = BoolFalse
			}
		case "workloadIdentityHost":
			cfg.WorkloadIdentityHost = value
		case "privateKey":
			var decodeErr error
			block, decodeErr := base64.URLEncoding.DecodeString(value)
			if decodeErr != nil {
				err = &sferrors.SnowflakeError{
					Number:  sferrors.ErrCodePrivateKeyParseError,
					Message: "Base64 decode failed",
				}
				return
			}
			cfg.PrivateKey, err = ParsePKCS8PrivateKey(block)
			if err != nil {
				return err
			}
		case "validateDefaultParameters":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			if vv {
				cfg.ValidateDefaultParameters = BoolTrue
			} else {
				cfg.ValidateDefaultParameters = BoolFalse
			}
		case "clientRequestMfaToken":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			if vv {
				cfg.ClientRequestMfaToken = BoolTrue
			} else {
				cfg.ClientRequestMfaToken = BoolFalse
			}
		case "clientStoreTemporaryCredential":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			if vv {
				cfg.ClientStoreTemporaryCredential = BoolTrue
			} else {
				cfg.ClientStoreTemporaryCredential = BoolFalse
			}
		case "tracing":
			cfg.Tracing = value
		case "logQueryText":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			cfg.LogQueryText = vv
		case "logQueryParameters":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			cfg.LogQueryParameters = vv
		case "tmpDirPath":
			cfg.TmpDirPath = value
		case "disableQueryContextCache":
			var b bool
			b, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			cfg.DisableQueryContextCache = b
		case "includeRetryReason":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			if vv {
				cfg.IncludeRetryReason = BoolTrue
			} else {
				cfg.IncludeRetryReason = BoolFalse
			}
		case "clientConfigFile":
			cfg.ClientConfigFile = value
		case "disableConsoleLogin":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			if vv {
				cfg.DisableConsoleLogin = BoolTrue
			} else {
				cfg.DisableConsoleLogin = BoolFalse
			}
		case "disableSamlURLCheck":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			if vv {
				cfg.DisableSamlURLCheck = BoolTrue
			} else {
				cfg.DisableSamlURLCheck = BoolFalse
			}
		case "certRevocationCheckMode":
			var certRevocationCheckMode CertRevocationCheckMode
			certRevocationCheckMode, err = ParseCertRevocationCheckMode(value)
			if err != nil {
				return
			}
			cfg.CertRevocationCheckMode = certRevocationCheckMode
		case "crlAllowCertificatesWithoutCrlURL":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if vv {
				cfg.CrlAllowCertificatesWithoutCrlURL = BoolTrue
			} else {
				cfg.CrlAllowCertificatesWithoutCrlURL = BoolFalse
			}
		case "crlInMemoryCacheDisabled":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			if vv {
				cfg.CrlInMemoryCacheDisabled = true
			} else {
				cfg.CrlInMemoryCacheDisabled = false
			}
		case "crlOnDiskCacheDisabled":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			if vv {
				cfg.CrlOnDiskCacheDisabled = true
			} else {
				cfg.CrlOnDiskCacheDisabled = false
			}
		case "crlDownloadMaxSize":
			cfg.CrlDownloadMaxSize, err = strconv.Atoi(value)
			if err != nil {
				return
			}
		case "crlHttpClientTimeout":
			var vv int64
			vv, err = strconv.ParseInt(value, 10, 64)
			if err != nil {
				return
			}
			cfg.CrlHTTPClientTimeout = time.Duration(vv * int64(time.Second))
		case "connectionDiagnosticsEnabled":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			cfg.ConnectionDiagnosticsEnabled = vv
		case "connectionDiagnosticsAllowlistFile":
			cfg.ConnectionDiagnosticsAllowlistFile = value
		case "proxyHost":
			cfg.ProxyHost, err = parseString(value)
		case "proxyPort":
			cfg.ProxyPort, err = ParseInt(value)
		case "proxyUser":
			cfg.ProxyUser, err = parseString(value)
		case "proxyPassword":
			cfg.ProxyPassword, err = parseString(value)
		case "noProxy":
			cfg.NoProxy, err = parseString(value)
		case "proxyProtocol":
			cfg.ProxyProtocol, err = parseString(value)
		default:
			if cfg.Params == nil {
				cfg.Params = make(map[string]*string)
			}
			// handle session variables $variable=value
			cfg.Params[urlDecodeIfNeeded(param[0])] = &value
		}
	}
	return
}

func parseTimeout(value string) (time.Duration, error) {
	var vv int64
	var err error
	vv, err = strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Duration(0), err
	}
	return time.Duration(vv * int64(time.Second)), nil
}

// GetConfigFromEnv is used to parse the environment variable values to specific fields of the Config
func GetConfigFromEnv(properties []*Param) (*Config, error) {
	var account, user, password, token, tokenFilePath, role, host, portStr, protocol, warehouse, database, schema, region, passcode, application string
	var oauthClientID, oauthClientSecret, oauthAuthorizationURL, oauthTokenRequestURL, oauthRedirectURI, oauthScope string
	var privateKey *rsa.PrivateKey
	var err error
	if len(properties) == 0 || properties == nil {
		return nil, errors.New("missing configuration parameters for the connection")
	}
	for _, prop := range properties {
		value, err := GetFromEnv(prop.EnvName, prop.FailOnMissing)
		if err != nil {
			return nil, err
		}
		switch prop.Name {
		case "Account":
			account = value
		case "User":
			user = value
		case "Password":
			password = value
		case "Token":
			token = value
		case "TokenFilePath":
			tokenFilePath = value
		case "Role":
			role = value
		case "Host":
			host = value
		case "Port":
			portStr = value
		case "Protocol":
			protocol = value
		case "Warehouse":
			warehouse = value
		case "Database":
			database = value
		case "Region":
			region = value
		case "Passcode":
			passcode = value
		case "Schema":
			schema = value
		case "Application":
			application = value
		case "PrivateKey":
			privateKey, err = parsePrivateKeyFromFile(value)
			if err != nil {
				return nil, err
			}
		case "OAuthClientId":
			oauthClientID = value
		case "OAuthClientSecret":
			oauthClientSecret = value
		case "OAuthAuthorizationURL":
			oauthAuthorizationURL = value
		case "OAuthTokenRequestURL":
			oauthTokenRequestURL = value
		case "OAuthRedirectURI":
			oauthRedirectURI = value
		case "OAuthScope":
			oauthScope = value
		default:
			return nil, errors.New("unknown property: " + prop.Name)
		}
	}

	port := 443 // snowflake default port
	if len(portStr) > 0 {
		port, err = strconv.Atoi(portStr)
		if err != nil {
			return nil, err
		}
	}

	cfg := &Config{
		Account:               account,
		User:                  user,
		Password:              password,
		Token:                 token,
		TokenFilePath:         tokenFilePath,
		Role:                  role,
		Host:                  host,
		Port:                  port,
		Protocol:              protocol,
		Warehouse:             warehouse,
		Database:              database,
		Schema:                schema,
		PrivateKey:            privateKey,
		Region:                region,
		Passcode:              passcode,
		Application:           application,
		OauthClientID:         oauthClientID,
		OauthClientSecret:     oauthClientSecret,
		OauthAuthorizationURL: oauthAuthorizationURL,
		OauthTokenRequestURL:  oauthTokenRequestURL,
		OauthRedirectURI:      oauthRedirectURI,
		OauthScope:            oauthScope,
		Params:                map[string]*string{},
	}
	return cfg, nil
}

func parsePrivateKeyFromFile(path string) (*rsa.PrivateKey, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(bytes)
	if block == nil {
		return nil, errors.New("failed to parse PEM block containing the private key")
	}
	privateKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pk, ok := privateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("interface convertion. expected type *rsa.PrivateKey, but got %T", privateKey)
	}
	return pk, nil
}

// ExtractAccountName extract an account name from a raw account.
func ExtractAccountName(rawAccount string) string {
	posDot := strings.Index(rawAccount, ".")
	if posDot > 0 {
		return strings.ToUpper(rawAccount[:posDot])
	}
	return strings.ToUpper(rawAccount)
}

func urlDecodeIfNeeded(param string) (decodedParam string) {
	unescaped, err := url.QueryUnescape(param)
	if err != nil {
		return param
	}
	return unescaped
}

// GetToken retrieves the token from the Config, reading from file if TokenFilePath is set.
func GetToken(c *Config) (string, error) {
	if c.TokenFilePath != "" {
		return ReadToken(c.TokenFilePath)
	}
	return c.Token, nil
}

// DescribeIdentityAttributes returns a string describing the identity attributes of the Config.
func DescribeIdentityAttributes(c *Config) string {
	return fmt.Sprintf("host: %v, account: %v, user: %v, password existed: %v, role: %v, database: %v, schema: %v, warehouse: %v, %v",
		c.Host, c.Account, c.User, (c.Password != ""), c.Role, c.Database, c.Schema, c.Warehouse, DescribeProxy(c))
}

// DescribeProxy returns a string describing the proxy configuration.
func DescribeProxy(c *Config) string {
	if c.ProxyHost != "" {
		return fmt.Sprintf("proxyHost: %v, proxyPort: %v proxyUser: %v, proxyPassword %v, proxyProtocol: %v, noProxy: %v", c.ProxyHost, c.ProxyPort, c.ProxyUser, c.ProxyPassword != "", c.ProxyProtocol, c.NoProxy)
	}
	return "proxy was not configured"
}
