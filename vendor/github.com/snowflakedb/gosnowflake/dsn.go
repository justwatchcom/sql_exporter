package gosnowflake

import (
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultClientTimeout          = 900 * time.Second // Timeout for network round trip + read out http response
	defaultJWTClientTimeout       = 10 * time.Second  // Timeout for network round trip + read out http response but used for JWT auth
	defaultLoginTimeout           = 300 * time.Second // Timeout for retry for login EXCLUDING clientTimeout
	defaultRequestTimeout         = 0 * time.Second   // Timeout for retry for request EXCLUDING clientTimeout
	defaultJWTTimeout             = 60 * time.Second
	defaultExternalBrowserTimeout = 120 * time.Second // Timeout for external browser login
	defaultCloudStorageTimeout    = -1                // Timeout for calling cloud storage.
	defaultMaxRetryCount          = 7                 // specifies maximum number of subsequent retries
	defaultDomain                 = ".snowflakecomputing.com"
	cnDomain                      = ".snowflakecomputing.cn"
	topLevelDomainPrefix          = ".snowflakecomputing." // used to extract the domain from host
)

// ConfigBool is a type to represent true or false in the Config
type ConfigBool uint8

const (
	configBoolNotSet ConfigBool = iota // Reserved for unset to let default value fall into this category
	// ConfigBoolTrue represents true for the config field
	ConfigBoolTrue
	// ConfigBoolFalse represents false for the config field
	ConfigBoolFalse
)

func (cb ConfigBool) String() string {
	switch cb {
	case ConfigBoolTrue:
		return "true"
	case ConfigBoolFalse:
		return "false"
	default:
		return "not set"
	}
}

// Config is a set of configuration parameters
type Config struct {
	Account   string // Account name
	User      string // Username
	Password  string // Password (requires User)
	Database  string // Database name
	Schema    string // Schema
	Warehouse string // Warehouse
	Role      string // Role
	Region    string // Region

	OauthClientID                string // Client id for OAuth2 external IdP
	OauthClientSecret            string // Client secret for OAuth2 external IdP
	OauthAuthorizationURL        string // Authorization URL of Auth2 external IdP
	OauthTokenRequestURL         string // Token request URL of Auth2 external IdP
	OauthRedirectURI             string // Redirect URI registered in IdP. The default is http://127.0.0.1:<random port>
	OauthScope                   string // Comma separated list of scopes. If empty it is derived from role.
	EnableSingleUseRefreshTokens bool   // Enables single use refresh tokens for Snowflake IdP

	// ValidateDefaultParameters disable the validation checks for Database, Schema, Warehouse and Role
	// at the time a connection is established
	ValidateDefaultParameters ConfigBool

	Params map[string]*string // other connection parameters

	// Deprecated: will be removed in a future release.
	ClientIP net.IP // IP address for network check
	Protocol string // http or https (optional)
	Host     string // hostname (optional)
	Port     int    // port (optional)

	Authenticator              AuthType   // The authenticator type
	SingleAuthenticationPrompt ConfigBool // If enabled prompting for authentication will only occur for the first authentication challenge

	Passcode           string
	PasscodeInPassword bool

	OktaURL *url.URL

	// Deprecated: timeouts may be reorganized in a future release.
	LoginTimeout time.Duration // Login retry timeout EXCLUDING network roundtrip and read out http response
	// Deprecated: timeouts may be reorganized in a future release.
	RequestTimeout time.Duration // request retry timeout EXCLUDING network roundtrip and read out http response
	// Deprecated: timeouts may be reorganized in a future release.
	JWTExpireTimeout time.Duration // JWT expire after timeout
	// Deprecated: timeouts may be reorganized in a future release.
	ClientTimeout time.Duration // Timeout for network round trip + read out http response
	// Deprecated: timeouts may be reorganized in a future release.
	JWTClientTimeout time.Duration // Timeout for network round trip + read out http response used when JWT token auth is taking place
	// Deprecated: timeouts may be reorganized in a future release.
	ExternalBrowserTimeout time.Duration // Timeout for external browser login
	// Deprecated: timeouts may be reorganized in a future release.
	CloudStorageTimeout time.Duration // Timeout for a single call to a cloud storage provider
	MaxRetryCount       int           // Specifies how many times non-periodic HTTP request can be retried

	Application       string // application name.
	DisableOCSPChecks bool   // driver doesn't check certificate revocation status
	// Deprecated: InsecureMode use DisableOCSPChecks instead. Will be removed in a future release.
	InsecureMode bool             // driver doesn't check certificate revocation status
	OCSPFailOpen OCSPFailOpenMode // OCSP Fail Open

	Token         string        // Token to use for OAuth other forms of token based auth
	TokenFilePath string        // TokenFilePath defines a file where to read token from
	TokenAccessor TokenAccessor // Optional token accessor to use
	// Deprecated: will be removed in a future release.
	KeepSessionAlive bool // Enables the session to persist even after the connection is closed

	PrivateKey *rsa.PrivateKey // Private key used to sign JWT

	Transporter http.RoundTripper // RoundTripper to intercept HTTP requests and responses

	tlsConfig     *tls.Config // Custom TLS configuration
	TLSConfigName string      // Name of the TLS config to use

	// Deprecated: will be removed in a future release and replaced with a session parameter.
	DisableTelemetry bool // indicates whether to disable telemetry

	// Deprecated: may be removed in a future release with logging reorganization.
	Tracing            string // sets logging level
	LogQueryText       bool   // indicates whether query text should be logged.
	LogQueryParameters bool   // indicates whether query parameters should be logged.

	TmpDirPath string // sets temporary directory used by a driver for operations like encrypting, compressing etc

	// Deprecated: will be unexported in a future release.
	MfaToken string // Internally used to cache the MFA token
	// Deprecated: will be unexported in a future release.
	IDToken string // Internally used to cache the Id Token for external browser
	// Deprecated: may be unexported in a future release.
	ClientRequestMfaToken ConfigBool // When true the MFA token is cached in the credential manager. True by default in Windows/OSX. False for Linux.
	// Deprecated: may be unexported in a future release.
	ClientStoreTemporaryCredential ConfigBool // When true the ID token is cached in the credential manager. True by default in Windows/OSX. False for Linux.

	DisableQueryContextCache bool // Should HTAP query context cache be disabled

	IncludeRetryReason ConfigBool // Should retried request contain retry reason

	ClientConfigFile string // File path to the client configuration json file

	DisableConsoleLogin ConfigBool // Indicates whether console login should be disabled

	DisableSamlURLCheck ConfigBool // Indicates whether the SAML URL check should be disabled

	WorkloadIdentityProvider          string   // The workload identity provider to use for WIF authentication
	WorkloadIdentityEntraResource     string   // The resource to use for WIF authentication on Azure environment
	WorkloadIdentityImpersonationPath []string // The components to use for WIF impersonation.

	CertRevocationCheckMode           CertRevocationCheckMode // revocation check mode for CRLs
	CrlAllowCertificatesWithoutCrlURL ConfigBool              // Allow certificates (not short-lived) without CRL DP included to be treated as correct ones
	CrlInMemoryCacheDisabled          bool                    // Should the in-memory cache be disabled
	CrlOnDiskCacheDisabled            bool                    // Should the on-disk cache be disabled
	CrlDownloadMaxSize                int                     // Max size in bytes of CRL to download. 0 means no limit. Default is 0.
	CrlHTTPClientTimeout              time.Duration           // Timeout for HTTP client used to download CRL

	ConnectionDiagnosticsEnabled       bool   // Indicates whether connection diagnostics should be enabled
	ConnectionDiagnosticsAllowlistFile string // File path to the allowlist file for connection diagnostics. If not specified, the allowlist.json file in the current directory will be used.

	ProxyHost     string // Proxy host
	ProxyPort     int    // Proxy port
	ProxyUser     string // Proxy user
	ProxyPassword string // Proxy password
	ProxyProtocol string // Proxy protocol (http or https)
	NoProxy       string // No proxy for this host list
}

func (c *Config) getToken() (string, error) {
	if c.TokenFilePath != "" {
		return readToken(c.TokenFilePath)
	}
	return c.Token, nil
}

// Validate enables testing if config is correct.
// A driver client may call it manually, but it is also called during opening first connection.
func (c *Config) Validate() error {
	if c.TmpDirPath != "" {
		if _, err := os.Stat(c.TmpDirPath); err != nil {
			return err
		}
	}
	if strings.EqualFold(c.WorkloadIdentityProvider, "azure") && len(c.WorkloadIdentityImpersonationPath) > 0 {
		return errors.New("WorkloadIdentityImpersonationPath is not supported for Azure")
	}
	if c.Token != "" && c.TokenFilePath != "" {
		return errors.New("token and tokenFilePath cannot be specified at the same time")
	}
	return nil
}

// ocspMode returns the OCSP mode in string INSECURE, FAIL_OPEN, FAIL_CLOSED
func (c *Config) ocspMode() string {
	if c.DisableOCSPChecks || c.InsecureMode {
		return ocspModeInsecure
	} else if c.OCSPFailOpen == ocspFailOpenNotSet || c.OCSPFailOpen == OCSPFailOpenTrue {
		// by default or set to true
		return ocspModeFailOpen
	}
	return ocspModeFailClosed
}

func (c *Config) transportConfigFor(transportType transportType) *transportConfig {
	return defaultTransportConfigs.forTransportType(transportType)
}

func (c *Config) describeIdentityAttributes() string {
	return fmt.Sprintf("host: %v, account: %v, user: %v, password existed: %v, role: %v, database: %v, schema: %v, warehouse: %v, %v",
		c.Host, c.Account, c.User, (c.Password != ""), c.Role, c.Database, c.Schema, c.Warehouse, c.describeProxy())
}

func (c *Config) describeProxy() string {
	if c.ProxyHost != "" {
		return fmt.Sprintf("proxyHost: %v, proxyPort: %v proxyUser: %v, proxyPassword %v, proxyProtocol: %v, noProxy: %v", c.ProxyHost, c.ProxyPort, c.ProxyUser, c.ProxyPassword != "", c.ProxyProtocol, c.NoProxy)
	}
	return "proxy was not configured"
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
			return "", errRegionConflict()
		}
		cfg.Region = region
		cfg.Account = cfg.Account[:posDot]
	}
	hasHost := true
	if cfg.Host == "" {
		hasHost = false
		if cfg.Region == "" {
			cfg.Host = cfg.Account + defaultDomain
		} else {
			cfg.Host = buildHostFromAccountAndRegion(cfg.Account, cfg.Region)
		}
	}
	err = fillMissingConfigParameters(cfg)
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
	if len(cfg.WorkloadIdentityImpersonationPath) > 0 {
		params.Add("workloadIdentityImpersonationPath", strings.Join(cfg.WorkloadIdentityImpersonationPath, ","))
	}
	if cfg.Authenticator != AuthTypeSnowflake {
		if cfg.Authenticator == AuthTypeOkta {
			params.Add("authenticator", strings.ToLower(cfg.OktaURL.String()))
		} else {
			params.Add("authenticator", strings.ToLower(cfg.Authenticator.String()))
		}
	}
	if cfg.SingleAuthenticationPrompt != configBoolNotSet {
		if cfg.SingleAuthenticationPrompt == ConfigBoolTrue {
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
	if cfg.ClientTimeout != defaultClientTimeout {
		params.Add("clientTimeout", strconv.FormatInt(int64(cfg.ClientTimeout/time.Second), 10))
	}
	if cfg.JWTClientTimeout != defaultJWTClientTimeout {
		params.Add("jwtClientTimeout", strconv.FormatInt(int64(cfg.JWTClientTimeout/time.Second), 10))
	}
	if cfg.LoginTimeout != defaultLoginTimeout {
		params.Add("loginTimeout", strconv.FormatInt(int64(cfg.LoginTimeout/time.Second), 10))
	}
	if cfg.RequestTimeout != defaultRequestTimeout {
		params.Add("requestTimeout", strconv.FormatInt(int64(cfg.RequestTimeout/time.Second), 10))
	}
	if cfg.JWTExpireTimeout != defaultJWTTimeout {
		params.Add("jwtTimeout", strconv.FormatInt(int64(cfg.JWTExpireTimeout/time.Second), 10))
	}
	if cfg.ExternalBrowserTimeout != defaultExternalBrowserTimeout {
		params.Add("externalBrowserTimeout", strconv.FormatInt(int64(cfg.ExternalBrowserTimeout/time.Second), 10))
	}
	if cfg.CloudStorageTimeout != defaultCloudStorageTimeout {
		params.Add("cloudStorageTimeout", strconv.FormatInt(int64(cfg.CloudStorageTimeout/time.Second), 10))
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
	if cfg.CrlAllowCertificatesWithoutCrlURL == ConfigBoolTrue {
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
		privateKeyInBytes, err := marshalPKCS8PrivateKey(cfg.PrivateKey)
		if err != nil {
			return "", err
		}
		keyBase64 := base64.URLEncoding.EncodeToString(privateKeyInBytes)
		params.Add("privateKey", keyBase64)
	}
	if cfg.InsecureMode {
		params.Add("insecureMode", strconv.FormatBool(cfg.InsecureMode))
	}
	if cfg.DisableOCSPChecks {
		params.Add("disableOCSPChecks", strconv.FormatBool(cfg.DisableOCSPChecks))
	}
	if cfg.DisableTelemetry {
		params.Add("disableTelemetry", strconv.FormatBool(cfg.DisableTelemetry))
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
	if cfg.IncludeRetryReason == ConfigBoolFalse {
		params.Add("includeRetryReason", "false")
	}

	params.Add("ocspFailOpen", strconv.FormatBool(cfg.OCSPFailOpen != OCSPFailOpenFalse))

	params.Add("validateDefaultParameters", strconv.FormatBool(cfg.ValidateDefaultParameters != ConfigBoolFalse))

	if cfg.ClientRequestMfaToken != configBoolNotSet {
		params.Add("clientRequestMfaToken", strconv.FormatBool(cfg.ClientRequestMfaToken != ConfigBoolFalse))
	}

	if cfg.ClientStoreTemporaryCredential != configBoolNotSet {
		params.Add("clientStoreTemporaryCredential", strconv.FormatBool(cfg.ClientStoreTemporaryCredential != ConfigBoolFalse))
	}
	if cfg.ClientConfigFile != "" {
		params.Add("clientConfigFile", cfg.ClientConfigFile)
	}
	if cfg.DisableConsoleLogin != configBoolNotSet {
		params.Add("disableConsoleLogin", strconv.FormatBool(cfg.DisableConsoleLogin != ConfigBoolFalse))
	}
	if cfg.DisableSamlURLCheck != configBoolNotSet {
		params.Add("disableSamlURLCheck", strconv.FormatBool(cfg.DisableSamlURLCheck != ConfigBoolFalse))
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

// ParseDSN parses the DSN string to a Config.
func ParseDSN(dsn string) (cfg *Config, err error) {
	// New config with some default values
	cfg = &Config{
		Params:        make(map[string]*string),
		Authenticator: AuthTypeSnowflake, // Default to snowflake
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
	if cfg.Account == "" && hostIncludesTopLevelDomain(cfg.Host) {
		posDot := strings.Index(cfg.Host, ".")
		if posDot > 0 {
			cfg.Account = cfg.Host[:posDot]
		}
	}
	posDot := strings.Index(cfg.Account, ".")
	if posDot >= 0 {
		cfg.Account = cfg.Account[:posDot]
	}

	err = fillMissingConfigParameters(cfg)
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

func fillMissingConfigParameters(cfg *Config) error {
	posDash := strings.LastIndex(cfg.Account, "-")
	if posDash > 0 {
		if strings.Contains(strings.ToLower(cfg.Host), ".global.") {
			cfg.Account = cfg.Account[:posDash]
		}
	}
	if strings.Trim(cfg.Account, " ") == "" {
		return errEmptyAccount()
	}

	if authRequiresUser(cfg) && strings.TrimSpace(cfg.User) == "" {
		return errEmptyUsername()
	}

	if authRequiresPassword(cfg) && strings.TrimSpace(cfg.Password) == "" {
		return errEmptyPassword()
	}

	if authRequiresEitherPasswordOrToken(cfg) && strings.TrimSpace(cfg.Password) == "" && strings.TrimSpace(cfg.Token) == "" {
		return errEmptyPasswordAndToken()
	}

	if authRequiresClientIDAndSecret(cfg) && (strings.TrimSpace(cfg.OauthClientID) == "" || strings.TrimSpace(cfg.OauthClientSecret) == "") {
		return errEmptyOAuthParameters()
	}
	if strings.Trim(cfg.Protocol, " ") == "" {
		cfg.Protocol = "https"
	}
	if cfg.Port == 0 {
		cfg.Port = 443
	}

	cfg.Region = strings.Trim(cfg.Region, " ")
	if cfg.Region != "" {
		// region is specified but not included in Host
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
				cfg.Host = cfg.Account + defaultDomain
			}
		}
	}
	if cfg.LoginTimeout == 0 {
		cfg.LoginTimeout = defaultLoginTimeout
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	if cfg.JWTExpireTimeout == 0 {
		cfg.JWTExpireTimeout = defaultJWTTimeout
	}
	if cfg.ClientTimeout == 0 {
		cfg.ClientTimeout = defaultClientTimeout
	}
	if cfg.JWTClientTimeout == 0 {
		cfg.JWTClientTimeout = defaultJWTClientTimeout
	}
	if cfg.ExternalBrowserTimeout == 0 {
		cfg.ExternalBrowserTimeout = defaultExternalBrowserTimeout
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

	if cfg.OCSPFailOpen == ocspFailOpenNotSet {
		cfg.OCSPFailOpen = OCSPFailOpenTrue
	}

	if cfg.ValidateDefaultParameters == configBoolNotSet {
		cfg.ValidateDefaultParameters = ConfigBoolTrue
	}

	if cfg.IncludeRetryReason == configBoolNotSet {
		cfg.IncludeRetryReason = ConfigBoolTrue
	}

	if cfg.ProxyHost != "" && cfg.ProxyProtocol == "" {
		cfg.ProxyProtocol = "http" // Default to http if not specified
	}

	domain, _ := extractDomainFromHost(cfg.Host)
	if len(cfg.Host) == len(domain) {
		return &SnowflakeError{
			Number:      ErrCodeFailedToParseHost,
			Message:     errMsgFailedToParseHost,
			MessageArgs: []interface{}{cfg.Host},
		}
	}
	if cfg.TLSConfigName != "" {
		if tlsConfig, ok := getTLSConfig(cfg.TLSConfigName); ok {
			cfg.tlsConfig = tlsConfig
		} else {
			return &SnowflakeError{
				Number:  ErrCodeMissingTLSConfig,
				Message: fmt.Sprintf(errMsgMissingTLSConfig, cfg.TLSConfigName),
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
		return cnDomain
	}
	return defaultDomain
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

func authRequiresEitherPasswordOrToken(cfg *Config) bool {
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
			cfg.Host = cfg.Account + defaultDomain
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
				err = &SnowflakeError{
					Number:      ErrCodeFailedToParsePort,
					Message:     errMsgFailedToParsePort,
					MessageArgs: []interface{}{dsn[k+1 : posSlash]},
				}
				return
			}
			break
		}
	}
	cfg.Host = dsn[posAt+1 : k]
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
	logger.Infof("Query String: %v\n", maskSecrets(params))
	paramsSlice := strings.Split(params, "&")
	insecureModeIdx := findByPrefix(paramsSlice, "insecureMode")
	disableOCSPChecksIdx := findByPrefix(paramsSlice, "disableOCSPChecks")
	if insecureModeIdx > -1 && disableOCSPChecksIdx > -1 {
		logger.Warn("duplicated insecureMode and disableOCSPChecks. disableOCSPChecks takes precedence")
		paramsSlice = append(paramsSlice[:insecureModeIdx-1], paramsSlice[insecureModeIdx+1:]...)
	}
	if slices.Contains(paramsSlice, "token") && slices.Contains(paramsSlice, "tokenFilePath") {
		return errors.New("token and tokenFilePath cannot be specified at the same time")
	}
	for _, v := range paramsSlice {
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
		case "protocol":
			cfg.Protocol = value
		case "singleAuthenticationPrompt":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			if vv {
				cfg.SingleAuthenticationPrompt = ConfigBoolTrue
			} else {
				cfg.SingleAuthenticationPrompt = ConfigBoolFalse
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
		case "maxRetryCount":
			cfg.MaxRetryCount, err = strconv.Atoi(value)
			if err != nil {
				return err
			}
		case "application":
			cfg.Application = value
		case "authenticator":
			err := determineAuthenticatorType(cfg, value)
			if err != nil {
				return err
			}
		case "insecureMode":
			logInsecureModeDeprecationInfo()
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			cfg.InsecureMode = vv
		case "disableOCSPChecks":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			cfg.DisableOCSPChecks = vv
		case "disableTelemetry":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			cfg.DisableTelemetry = vv
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
			cfg.Token, err = readToken(value)
			if err != nil {
				return
			}
		case "tlsConfigName":
			cfg.TLSConfigName = value
		case "workloadIdentityProvider":
			cfg.WorkloadIdentityProvider = value
		case "workloadIdentityEntraResource":
			cfg.WorkloadIdentityEntraResource = value
		case "workloadIdentityImpersonationPath":
			cfg.WorkloadIdentityImpersonationPath = strings.Split(value, ",")
		case "privateKey":
			var decodeErr error
			block, decodeErr := base64.URLEncoding.DecodeString(value)
			if decodeErr != nil {
				err = &SnowflakeError{
					Number:  ErrCodePrivateKeyParseError,
					Message: "Base64 decode failed",
				}
				return
			}
			cfg.PrivateKey, err = parsePKCS8PrivateKey(block)
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
				cfg.ValidateDefaultParameters = ConfigBoolTrue
			} else {
				cfg.ValidateDefaultParameters = ConfigBoolFalse
			}
		case "clientRequestMfaToken":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			if vv {
				cfg.ClientRequestMfaToken = ConfigBoolTrue
			} else {
				cfg.ClientRequestMfaToken = ConfigBoolFalse
			}
		case "clientStoreTemporaryCredential":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			if vv {
				cfg.ClientStoreTemporaryCredential = ConfigBoolTrue
			} else {
				cfg.ClientStoreTemporaryCredential = ConfigBoolFalse
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
				cfg.IncludeRetryReason = ConfigBoolTrue
			} else {
				cfg.IncludeRetryReason = ConfigBoolFalse
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
				cfg.DisableConsoleLogin = ConfigBoolTrue
			} else {
				cfg.DisableConsoleLogin = ConfigBoolFalse
			}
		case "disableSamlURLCheck":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if err != nil {
				return
			}
			if vv {
				cfg.DisableSamlURLCheck = ConfigBoolTrue
			} else {
				cfg.DisableSamlURLCheck = ConfigBoolFalse
			}
		case "certRevocationCheckMode":
			var certRevocationCheckMode CertRevocationCheckMode
			certRevocationCheckMode, err = parseCertRevocationCheckMode(value)
			if err != nil {
				return
			}
			cfg.CertRevocationCheckMode = certRevocationCheckMode
		case "crlAllowCertificatesWithoutCrlURL":
			var vv bool
			vv, err = strconv.ParseBool(value)
			if vv {
				cfg.CrlAllowCertificatesWithoutCrlURL = ConfigBoolTrue
			} else {
				cfg.CrlAllowCertificatesWithoutCrlURL = ConfigBoolFalse
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
			cfg.ProxyPort, err = parseInt(value)
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

func logInsecureModeDeprecationInfo() {
	logger.Warn("insecureMode is deprecated. Use disableOCSPChecks instead.")
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

// ConfigParam is used to bind the name of the Config field with the environment variable and set the requirement for it
type ConfigParam struct {
	Name          string
	EnvName       string
	FailOnMissing bool
}

// GetConfigFromEnv is used to parse the environment variable values to specific fields of the Config
func GetConfigFromEnv(properties []*ConfigParam) (*Config, error) {
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

func extractAccountName(rawAccount string) string {
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
