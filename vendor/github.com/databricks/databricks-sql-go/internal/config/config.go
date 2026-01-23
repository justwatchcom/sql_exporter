package config

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	dbsqlerr "github.com/databricks/databricks-sql-go/errors"
	"github.com/pkg/errors"

	"github.com/databricks/databricks-sql-go/auth"
	"github.com/databricks/databricks-sql-go/auth/noop"
	"github.com/databricks/databricks-sql-go/auth/oauth/m2m"
	"github.com/databricks/databricks-sql-go/auth/oauth/u2m"
	"github.com/databricks/databricks-sql-go/auth/pat"
	"github.com/databricks/databricks-sql-go/internal/cli_service"
	dbsqlerrint "github.com/databricks/databricks-sql-go/internal/errors"
	"github.com/databricks/databricks-sql-go/logger"
)

// Driver Configurations.
// Only UserConfig are currently exposed to users
type Config struct {
	UserConfig
	TLSConfig *tls.Config // nil disables TLS
	ArrowConfig
	PollInterval              time.Duration
	ClientTimeout             time.Duration // max time the http request can last
	PingTimeout               time.Duration // max time allowed for ping
	CanUseMultipleCatalogs    bool
	DriverName                string
	DriverVersion             string
	ThriftProtocol            string
	ThriftTransport           string
	ThriftProtocolVersion     cli_service.TProtocolVersion
	ThriftDebugClientProtocol bool
}

// ToEndpointURL generates the endpoint URL from Config that a Thrift client will connect to
func (c *Config) ToEndpointURL() (string, error) {
	var userInfo string
	endpointUrl := fmt.Sprintf("%s://%s%s:%d%s", c.Protocol, userInfo, c.Host, c.Port, c.HTTPPath)
	if c.Host == "" {
		return endpointUrl, errors.New("databricks: missing Hostname")
	}
	if c.Port == 0 {
		return endpointUrl, errors.New("databricks: missing Port")
	}
	if c.HTTPPath == "" && c.Host != "localhost" {
		return endpointUrl, errors.New("databricks: missing HTTP Path")
	}
	return endpointUrl, nil
}

// DeepCopy returns a true deep copy of Config
func (c *Config) DeepCopy() *Config {
	if c == nil {
		return nil
	}

	return &Config{
		UserConfig:                c.UserConfig.DeepCopy(),
		TLSConfig:                 c.TLSConfig.Clone(),
		ArrowConfig:               c.ArrowConfig.DeepCopy(),
		PollInterval:              c.PollInterval,
		ClientTimeout:             c.ClientTimeout,
		PingTimeout:               c.PingTimeout,
		CanUseMultipleCatalogs:    c.CanUseMultipleCatalogs,
		DriverName:                c.DriverName,
		DriverVersion:             c.DriverVersion,
		ThriftProtocol:            c.ThriftProtocol,
		ThriftTransport:           c.ThriftTransport,
		ThriftProtocolVersion:     c.ThriftProtocolVersion,
		ThriftDebugClientProtocol: c.ThriftDebugClientProtocol,
	}
}

// UserConfig is the set of configurations exposed to users
type UserConfig struct {
	Protocol          string
	Host              string // from databricks UI
	Port              int    // from databricks UI
	HTTPPath          string // from databricks UI
	Catalog           string
	Schema            string
	Authenticator     auth.Authenticator
	AccessToken       string        // from databricks UI
	MaxRows           int           // max rows per page
	QueryTimeout      time.Duration // Timeout passed to server for query processing
	UserAgentEntry    string
	Location          *time.Location
	SessionParams     map[string]string
	RetryWaitMin      time.Duration
	RetryWaitMax      time.Duration
	RetryMax          int
	Transport         http.RoundTripper
	UseLz4Compression bool
	CloudFetchConfig
}

// DeepCopy returns a true deep copy of UserConfig
func (ucfg UserConfig) DeepCopy() UserConfig {
	var sessionParams map[string]string
	if ucfg.SessionParams != nil {
		sessionParams = make(map[string]string)
		for k, v := range ucfg.SessionParams {
			sessionParams[k] = v
		}
	}
	var loccp *time.Location
	if ucfg.Location != nil {
		var err error
		loccp, err = time.LoadLocation(ucfg.Location.String())
		if err != nil {
			logger.Warn().Msg("could not copy location")
		}

	}

	return UserConfig{
		Protocol:          ucfg.Protocol,
		Host:              ucfg.Host,
		Port:              ucfg.Port,
		HTTPPath:          ucfg.HTTPPath,
		Catalog:           ucfg.Catalog,
		Schema:            ucfg.Schema,
		Authenticator:     ucfg.Authenticator,
		AccessToken:       ucfg.AccessToken,
		MaxRows:           ucfg.MaxRows,
		QueryTimeout:      ucfg.QueryTimeout,
		UserAgentEntry:    ucfg.UserAgentEntry,
		Location:          loccp,
		SessionParams:     sessionParams,
		RetryWaitMin:      ucfg.RetryWaitMin,
		RetryWaitMax:      ucfg.RetryWaitMax,
		RetryMax:          ucfg.RetryMax,
		Transport:         ucfg.Transport,
		UseLz4Compression: ucfg.UseLz4Compression,
		CloudFetchConfig:  ucfg.CloudFetchConfig,
	}
}

var defaultMaxRows = 100000

// WithDefaults provides default settings for optional fields in UserConfig
func (ucfg UserConfig) WithDefaults() UserConfig {
	if ucfg.MaxRows <= 0 {
		ucfg.MaxRows = defaultMaxRows
	}
	if ucfg.Protocol == "" {
		ucfg.Protocol = "https"
		ucfg.Port = 443
	}
	if ucfg.Port == 0 {
		ucfg.Port = 443
	}
	if ucfg.Authenticator == nil {
		ucfg.Authenticator = &noop.NoopAuth{}
	}
	if ucfg.SessionParams == nil {
		ucfg.SessionParams = make(map[string]string)
	}
	if ucfg.RetryMax == 0 {
		ucfg.RetryMax = 4
	}
	if ucfg.RetryWaitMin == 0 {
		ucfg.RetryWaitMin = 1 * time.Second
	}
	if ucfg.RetryWaitMax == 0 {
		ucfg.RetryWaitMax = 30 * time.Second
	}
	ucfg.UseLz4Compression = false
	ucfg.CloudFetchConfig = CloudFetchConfig{}.WithDefaults()

	return ucfg
}

// WithDefaults provides default settings for Config
func WithDefaults() *Config {
	return &Config{
		UserConfig:                UserConfig{}.WithDefaults(),
		TLSConfig:                 &tls.Config{MinVersion: tls.VersionTLS12},
		ArrowConfig:               ArrowConfig{}.WithDefaults(),
		PollInterval:              1 * time.Second,
		ClientTimeout:             900 * time.Second,
		PingTimeout:               60 * time.Second,
		CanUseMultipleCatalogs:    true,
		DriverName:                "godatabrickssqlconnector", // important. Do not change
		ThriftProtocol:            "binary",
		ThriftTransport:           "http",
		ThriftProtocolVersion:     cli_service.TProtocolVersion_SPARK_CLI_SERVICE_PROTOCOL_V8,
		ThriftDebugClientProtocol: false,
	}

}

// ParseDSN constructs UserConfig and CloudFetchConfig by parsing DSN string supplied to `sql.Open()`
func ParseDSN(dsn string) (UserConfig, error) {
	fullDSN := dsn
	if !strings.HasPrefix(dsn, "https://") && !strings.HasPrefix(dsn, "http://") {
		fullDSN = "https://" + dsn
	}
	parsedURL, err := url.Parse(fullDSN)
	if err != nil {
		return UserConfig{}, dbsqlerrint.NewRequestError(context.TODO(), dbsqlerr.ErrInvalidDSNFormat, err)
	}
	ucfg := UserConfig{}.WithDefaults()
	ucfg.Protocol = parsedURL.Scheme
	ucfg.Host = parsedURL.Hostname()
	port, err := strconv.Atoi(parsedURL.Port())
	if err != nil {
		return UserConfig{}, dbsqlerrint.NewRequestError(context.TODO(), dbsqlerr.ErrInvalidDSNPort, err)
	}
	ucfg.Port = port

	ucfg.HTTPPath = parsedURL.Path

	// Any params that are not specifically handled are assumed to be session params.
	// Use extractableParams so that the processed values are deleted as we go.
	params := &extractableParams{Values: parsedURL.Query()}

	// Create an authenticator based on the url and params
	err = makeAuthenticator(parsedURL, params, &ucfg)
	if err != nil {
		return UserConfig{}, err
	}

	if maxRows, ok, err := params.extractAsInt("maxRows"); ok {
		if err != nil {
			return UserConfig{}, err
		}
		if maxRows > 0 {
			ucfg.MaxRows = maxRows
		}
	}

	if timeoutSeconds, ok, err := params.extractAsInt("timeout"); ok {
		if err != nil {
			return UserConfig{}, err
		}
		ucfg.QueryTimeout = time.Duration(timeoutSeconds) * time.Second
	}

	if catalog, ok := params.extract("catalog"); ok {
		ucfg.Catalog = catalog
	}
	if userAgent, ok := params.extract("userAgentEntry"); ok {
		ucfg.UserAgentEntry = userAgent
		params.Del("userAgentEntry")
	}
	if schema, ok := params.extract("schema"); ok {
		ucfg.Schema = schema
	}

	// Cloud Fetch parameters
	if useCloudFetch, ok, err := params.extractAsBool("useCloudFetch"); ok {
		if err != nil {
			return UserConfig{}, err
		}
		ucfg.UseCloudFetch = useCloudFetch
	}

	if numThreads, ok, err := params.extractAsInt("maxDownloadThreads"); ok {
		if err != nil {
			return UserConfig{}, err
		}
		ucfg.MaxDownloadThreads = numThreads
	}

	// for timezone we do a case insensitive key match.
	// We use getNoCase because we want to leave timezone in the params so that it will also
	// be used as a session param.
	if timezone, ok := params.getNoCase("timezone"); ok {
		ucfg.Location, err = time.LoadLocation(timezone)
	}

	// any left over params are treated as session params
	if len(params.Values) > 0 {
		sessionParams := make(map[string]string)
		for k := range params.Values {
			sessionParams[k] = params.Get(k)
		}
		ucfg.SessionParams = sessionParams
	}

	return ucfg, err
}

// update the config with an authenticator based on the value from the parsed DSN
func makeAuthenticator(parsedURL *url.URL, params *extractableParams, config *UserConfig) error {
	name := parsedURL.User.Username()
	// if the user name is set to 'token' we will interpret the password as an acess token
	if name == "token" {
		pass, _ := parsedURL.User.Password()
		return addPatAuthenticator(pass, config)
	} else if name != "" {
		// Currently don't support user name/password authentication
		return dbsqlerrint.NewRequestError(context.TODO(), dbsqlerr.ErrBasicAuthNotSupported, nil)
	} else {
		// Process parameters that specify the authentication type.  They are removed from params
		// Get the optional authentication type param
		authTypeS, _ := params.extract("authType")
		authType := auth.ParseAuthType(authTypeS)

		// Get optional parameters for creating an authenticator
		clientId, hasClientId := params.extract("clientId")
		if !hasClientId {
			clientId, hasClientId = params.extract("clientID")
		}

		clientSecret, hasClientSecret := params.extract("clientSecret")
		accessToken, hasAccessToken := params.extract("accessToken")

		switch authType {
		case auth.AuthTypeUnknown:
			// if no authentication type is specified create an authenticator based on which
			// params have values
			if hasAccessToken {
				return addPatAuthenticator(accessToken, config)
			}

			if hasClientId || hasClientSecret {
				return addOauthM2MAuthenticator(clientId, clientSecret, config)
			}
		case auth.AuthTypePat:
			return addPatAuthenticator(accessToken, config)
		case auth.AuthTypeOauthM2M:
			return addOauthM2MAuthenticator(clientId, clientSecret, config)
		case auth.AuthTypeOauthU2M:
			return addOauthU2MAuthenticator(config)
		}

	}

	return nil
}

func addPatAuthenticator(accessToken string, config *UserConfig) error {
	if accessToken == "" {
		return dbsqlerrint.NewRequestError(context.TODO(), dbsqlerr.ErrInvalidDSNPATIsEmpty, nil)
	}
	config.AccessToken = accessToken
	pat := &pat.PATAuth{
		AccessToken: accessToken,
	}
	config.Authenticator = pat
	return nil
}

func addOauthM2MAuthenticator(clientId, clientSecret string, config *UserConfig) error {
	if clientId == "" || clientSecret == "" {
		return dbsqlerrint.NewRequestError(context.TODO(), dbsqlerr.ErrInvalidDSNM2m, nil)
	}

	m2m := m2m.NewAuthenticator(clientId, clientSecret, config.Host)
	config.Authenticator = m2m
	return nil
}

func addOauthU2MAuthenticator(config *UserConfig) error {
	u2m, err := u2m.NewAuthenticator(config.Host, 0)
	if err == nil {
		config.Authenticator = u2m
	}
	return err
}

type extractableParams struct {
	url.Values
}

// returns the value corresponding to the key, if any, and a bool flag indicating if
// there was a set value and it is not the empty string
// deletes the key/value from params
func (params *extractableParams) extract(key string) (string, bool) {
	return extractParam(key, params, false, true)
}

func (params *extractableParams) extractAsInt(key string) (int, bool, error) {
	if intString, ok := extractParam(key, params, false, true); ok {
		i, err := strconv.Atoi(intString)
		if err != nil {
			return 0, true, dbsqlerrint.NewRequestError(context.TODO(), dbsqlerr.InvalidDSNFormat(key, intString, "int"), err)
		}

		return i, true, nil
	}

	return 0, false, nil
}

func (params *extractableParams) extractAsBool(key string) (bool, bool, error) {
	if boolString, ok := extractParam(key, params, false, true); ok {
		b, err := strconv.ParseBool(boolString)
		if err != nil {
			return false, true, dbsqlerrint.NewRequestError(context.TODO(), dbsqlerr.InvalidDSNFormat(key, boolString, "bool"), err)
		}

		return b, true, nil
	}
	return false, false, nil
}

// returns the value corresponding to the key using case insensitive key matching and a bool flag
// indicating if the value was set and is not the empty string
func (params *extractableParams) getNoCase(key string) (string, bool) {
	return extractParam(key, params, true, false)
}

func extractParam(key string, params *extractableParams, ignoreCase bool, delValue bool) (string, bool) {
	if ignoreCase {
		key = strings.ToLower(key)
	}

	for k := range params.Values {
		kc := k
		if ignoreCase {
			kc = strings.ToLower(k)
		}
		if kc == key {
			val := params.Get(k)
			if delValue {
				params.Del(k)
			}
			return val, val != ""
		}
	}

	return "", false
}

type ArrowConfig struct {
	UseArrowBatches         bool
	UseArrowNativeDecimal   bool
	UseArrowNativeTimestamp bool

	// the following are currently not supported
	UseArrowNativeComplexTypes  bool
	UseArrowNativeIntervalTypes bool
}

func (ucfg ArrowConfig) WithDefaults() ArrowConfig {
	ucfg.UseArrowBatches = true
	ucfg.UseArrowNativeTimestamp = true
	ucfg.UseArrowNativeComplexTypes = true

	return ucfg
}

// DeepCopy returns a true deep copy of UserConfig
func (arrowConfig ArrowConfig) DeepCopy() ArrowConfig {
	return ArrowConfig{
		UseArrowBatches:             arrowConfig.UseArrowBatches,
		UseArrowNativeDecimal:       arrowConfig.UseArrowNativeDecimal,
		UseArrowNativeTimestamp:     arrowConfig.UseArrowNativeTimestamp,
		UseArrowNativeComplexTypes:  arrowConfig.UseArrowNativeComplexTypes,
		UseArrowNativeIntervalTypes: arrowConfig.UseArrowNativeIntervalTypes,
	}
}

type CloudFetchConfig struct {
	UseCloudFetch      bool
	MaxDownloadThreads int
	MaxFilesInMemory   int
	MinTimeToExpiry    time.Duration
}

func (cfg CloudFetchConfig) WithDefaults() CloudFetchConfig {
	cfg.UseCloudFetch = false

	if cfg.MaxDownloadThreads <= 0 {
		cfg.MaxDownloadThreads = 10
	}

	if cfg.MaxFilesInMemory < 1 {
		cfg.MaxFilesInMemory = 10
	}

	if cfg.MinTimeToExpiry < 0 {
		cfg.MinTimeToExpiry = 0 * time.Second
	}

	return cfg
}

func (cfg CloudFetchConfig) DeepCopy() CloudFetchConfig {
	return CloudFetchConfig{
		UseCloudFetch:      cfg.UseCloudFetch,
		MaxDownloadThreads: cfg.MaxDownloadThreads,
		MaxFilesInMemory:   cfg.MaxFilesInMemory,
		MinTimeToExpiry:    cfg.MinTimeToExpiry,
	}
}
