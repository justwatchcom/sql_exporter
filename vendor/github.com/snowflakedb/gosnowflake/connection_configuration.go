package gosnowflake

import (
	"encoding/base64"
	"errors"
	"os"
	path "path/filepath"
	"strconv"
	"strings"
	"time"

	toml "github.com/BurntSushi/toml"
)

const (
	snowflakeConnectionName = "SNOWFLAKE_DEFAULT_CONNECTION_NAME"
	snowflakeHome           = "SNOWFLAKE_HOME"
	defaultTokenPath        = "/snowflake/session/token"

	othersCanReadFilePermission  = os.FileMode(0044)
	othersCanWriteFilePermission = os.FileMode(0022)
	executableFilePermission     = os.FileMode(0111)

	skipWarningForReadPermissionsEnv = "SF_SKIP_WARNING_FOR_READ_PERMISSIONS_ON_CONFIG_FILE"
)

// LoadConnectionConfig returns connection configs loaded from the toml file.
// By default, SNOWFLAKE_HOME(toml file path) is os.snowflakeHome/.snowflake
// and SNOWFLAKE_DEFAULT_CONNECTION_NAME(DSN) is 'default'
func loadConnectionConfig() (*Config, error) {
	logger.Trace("Loading connection configuration from the local files.")
	cfg := &Config{
		Params:        make(map[string]*string),
		Authenticator: AuthTypeSnowflake, // Default to snowflake
	}
	dsn := getConnectionDSN(os.Getenv(snowflakeConnectionName))
	snowflakeConfigDir, err := getTomlFilePath(os.Getenv(snowflakeHome))
	if err != nil {
		return nil, err
	}
	logger.Debugf("Looking for connection file in directory %v", snowflakeConfigDir)
	tomlFilePath := path.Join(snowflakeConfigDir, "connections.toml")
	err = validateFilePermission(tomlFilePath)
	if err != nil {
		return nil, err
	}
	tomlInfo := make(map[string]interface{})
	_, err = toml.DecodeFile(tomlFilePath, &tomlInfo)
	if err != nil {
		return nil, err
	}
	dsnMap, exist := tomlInfo[dsn]
	if !exist {
		return nil, &SnowflakeError{
			Number:  ErrCodeFailedToFindDSNInToml,
			Message: errMsgFailedToFindDSNInTomlFile,
		}
	}
	connectionConfig, ok := dsnMap.(map[string]interface{})
	if !ok {
		return nil, err
	}
	logger.Trace("Trying to parse the config file")
	err = parseToml(cfg, connectionConfig)
	if err != nil {
		return nil, err
	}
	err = fillMissingConfigParameters(cfg)
	if err != nil {
		return nil, err
	}
	return cfg, err
}

func parseToml(cfg *Config, connectionMap map[string]interface{}) error {
	for key, value := range connectionMap {
		if err := handleSingleParam(cfg, key, value); err != nil {
			return err
		}
	}
	return nil
}

func handleSingleParam(cfg *Config, key string, value interface{}) error {
	var err error

	// We normalize the key to handle both snake_case and camelCase.
	normalizedKey := strings.ReplaceAll(strings.ToLower(key), "_", "")

	// the cases in switch statement should be in lower case and no _
	switch normalizedKey {
	case "user", "username":
		cfg.User, err = parseString(value)
	case "password":
		cfg.Password, err = parseString(value)
	case "host":
		cfg.Host, err = parseString(value)
	case "account":
		cfg.Account, err = parseString(value)
	case "warehouse":
		cfg.Warehouse, err = parseString(value)
	case "database":
		cfg.Database, err = parseString(value)
	case "schema":
		cfg.Schema, err = parseString(value)
	case "role":
		cfg.Role, err = parseString(value)
	case "region":
		cfg.Region, err = parseString(value)
	case "protocol":
		cfg.Protocol, err = parseString(value)
	case "passcode":
		cfg.Passcode, err = parseString(value)
	case "port":
		cfg.Port, err = parseInt(value)
	case "passcodeinpassword":
		cfg.PasscodeInPassword, err = parseBool(value)
	case "clienttimeout":
		cfg.ClientTimeout, err = parseDuration(value)
	case "jwtclienttimeout":
		cfg.JWTClientTimeout, err = parseDuration(value)
	case "logintimeout":
		cfg.LoginTimeout, err = parseDuration(value)
	case "requesttimeout":
		cfg.RequestTimeout, err = parseDuration(value)
	case "jwttimeout":
		cfg.JWTExpireTimeout, err = parseDuration(value)
	case "externalbrowsertimeout":
		cfg.ExternalBrowserTimeout, err = parseDuration(value)
	case "maxretrycount":
		cfg.MaxRetryCount, err = parseInt(value)
	case "application":
		cfg.Application, err = parseString(value)
	case "authenticator":
		var v string
		v, err = parseString(value)
		if err = checkParsingError(err, key, value); err != nil {
			return err
		}
		err = determineAuthenticatorType(cfg, v)
	case "disableocspchecks":
		cfg.DisableOCSPChecks, err = parseBool(value)
	case "insecuremode":
		logInsecureModeDeprecationInfo()
		cfg.InsecureMode, err = parseBool(value)
	case "ocspfailopen":
		var vv ConfigBool
		vv, err = parseConfigBool(value)
		if err := checkParsingError(err, key, value); err != nil {
			return err
		}
		cfg.OCSPFailOpen = OCSPFailOpenMode(vv)
	case "token":
		cfg.Token, err = parseString(value)
	case "privatekey":
		var v string
		v, err = parseString(value)
		if err = checkParsingError(err, key, value); err != nil {
			return err
		}
		block, decodeErr := base64.URLEncoding.DecodeString(v)
		if decodeErr != nil {
			return &SnowflakeError{
				Number:  ErrCodePrivateKeyParseError,
				Message: "Base64 decode failed",
			}
		}
		cfg.PrivateKey, err = parsePKCS8PrivateKey(block)
	case "validatedefaultparameters":
		cfg.ValidateDefaultParameters, err = parseConfigBool(value)
	case "clientrequestmfatoken":
		cfg.ClientRequestMfaToken, err = parseConfigBool(value)
	case "clientstoretemporarycredential":
		cfg.ClientStoreTemporaryCredential, err = parseConfigBool(value)
	case "tracing":
		cfg.Tracing, err = parseString(value)
	case "logquerytext":
		cfg.LogQueryText, err = parseBool(value)
	case "logqueryparameters":
		cfg.LogQueryParameters, err = parseBool(value)
	case "tmpdirpath":
		cfg.TmpDirPath, err = parseString(value)
	case "disablequerycontextcache":
		cfg.DisableQueryContextCache, err = parseBool(value)
	case "includeretryreason":
		cfg.IncludeRetryReason, err = parseConfigBool(value)
	case "clientconfigfile":
		cfg.ClientConfigFile, err = parseString(value)
	case "disableconsolelogin":
		cfg.DisableConsoleLogin, err = parseConfigBool(value)
	case "disablesamlurlcheck":
		cfg.DisableSamlURLCheck, err = parseConfigBool(value)
	case "oauthauthorizationurl":
		cfg.OauthAuthorizationURL, err = parseString(value)
	case "oauthclientid":
		cfg.OauthClientID, err = parseString(value)
	case "oauthclientsecret":
		cfg.OauthClientSecret, err = parseString(value)
	case "oauthtokenrequesturl":
		cfg.OauthTokenRequestURL, err = parseString(value)
	case "oauthredirecturi":
		cfg.OauthRedirectURI, err = parseString(value)
	case "oauthscope":
		cfg.OauthScope, err = parseString(value)
	case "workloadidentityprovider":
		cfg.WorkloadIdentityProvider, err = parseString(value)
	case "workloadidentityentraresource":
		cfg.WorkloadIdentityEntraResource, err = parseString(value)
	case "workloadidentityimpersonatinpath":
		cfg.WorkloadIdentityImpersonationPath, err = parseStrings(value)
	case "tokenfilepath":
		cfg.TokenFilePath, err = parseString(value)
		if err = checkParsingError(err, key, value); err != nil {
			return err
		}
	case "connectiondiagnosticsenabled":
		cfg.ConnectionDiagnosticsEnabled, err = parseBool(value)
	case "connectiondiagnosticsallowlistfile":
		cfg.ConnectionDiagnosticsAllowlistFile, err = parseString(value)
	case "proxyhost":
		cfg.ProxyHost, err = parseString(value)
	case "proxyport":
		cfg.ProxyPort, err = parseInt(value)
	case "proxyuser":
		cfg.ProxyUser, err = parseString(value)
	case "proxypassword":
		cfg.ProxyPassword, err = parseString(value)
	case "proxyprotocol":
		cfg.ProxyProtocol, err = parseString(value)
	case "noproxy":
		cfg.NoProxy, err = parseString(value)
	default:
		param, err := parseString(value)
		if err = checkParsingError(err, key, value); err != nil {
			return err
		}
		cfg.Params[urlDecodeIfNeeded(key)] = &param
	}
	return checkParsingError(err, key, value)
}

func checkParsingError(err error, key string, value interface{}) error {
	if err != nil {
		err = &SnowflakeError{
			Number:      ErrCodeTomlFileParsingFailed,
			Message:     errMsgFailedToParseTomlFile,
			MessageArgs: []interface{}{key, value},
		}
		logger.Errorf("Parsed key: %s, value: %v is not an option for the connection config", key, value)
		return err
	}
	logger.Warnf("Parsed key: %s, value: %v — cannot be parsed as string", key, value)
	return nil
}

func parseInt(i interface{}) (int, error) {
	v, ok := i.(string)
	if !ok {
		num, ok := i.(int)
		if !ok {
			return 0, errors.New("failed to parse the value to integer")
		}
		return num, nil
	}
	return strconv.Atoi(v)
}

func parseBool(i interface{}) (bool, error) {
	v, ok := i.(string)
	if !ok {
		vv, ok := i.(bool)
		if !ok {
			return false, errors.New("failed to parse the value to boolean")
		}
		return vv, nil
	}
	return strconv.ParseBool(v)
}

func parseConfigBool(i interface{}) (ConfigBool, error) {
	vv, err := parseBool(i)
	if err != nil {
		return ConfigBoolFalse, err
	}
	if vv {
		return ConfigBoolTrue, nil
	}
	return ConfigBoolFalse, nil
}

func parseDuration(i interface{}) (time.Duration, error) {
	v, ok := i.(string)
	if !ok {
		num, err := parseInt(i)
		if err != nil {
			return time.Duration(0), err
		}
		t := int64(num)
		return time.Duration(t * int64(time.Second)), nil
	}
	return parseTimeout(v)
}

func readToken(tokenPath string) (string, error) {
	if tokenPath == "" {
		tokenPath = defaultTokenPath
	}
	if !path.IsAbs(tokenPath) {
		var err error
		tokenPath, err = path.Abs(tokenPath)
		if err != nil {
			return "", err
		}
	}
	err := validateFilePermission(tokenPath)
	if err != nil {
		return "", err
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return "", err
	}
	return string(token), nil
}

func parseString(i interface{}) (string, error) {
	v, ok := i.(string)
	if !ok {
		return "", errors.New("failed to convert the value to string")
	}
	return v, nil
}

func parseStrings(i interface{}) ([]string, error) {
	s, ok := i.(string)
	if !ok {
		return nil, errors.New("failed to convert the value to string")
	}
	return strings.Split(s, ","), nil
}

func getTomlFilePath(filePath string) (string, error) {
	if len(filePath) == 0 {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		filePath = path.Join(homeDir, ".snowflake")
	}
	absDir, err := path.Abs(filePath)
	if err != nil {
		return "", err
	}
	return absDir, nil
}

func getConnectionDSN(dsn string) string {
	if len(dsn) != 0 {
		return dsn
	}
	return "default"
}

func validateFilePermission(filePath string) error {
	if isWindows {
		return nil
	}

	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return err
	}

	permission := fileInfo.Mode().Perm()

	if !shouldSkipWarningForReadPermissions() && permission&othersCanReadFilePermission != 0 {
		logger.Warnf("file '%v' is readable by someone other than the owner. Your Permission: %v. If you want "+
			"to disable this warning, either remove read permissions from group and others or set the environment "+
			"variable %v to true", filePath, permission, skipWarningForReadPermissionsEnv)
	}

	if permission&executableFilePermission != 0 {
		return &SnowflakeError{
			Number:      ErrCodeInvalidFilePermission,
			Message:     errMsgInvalidExecutablePermissionToFile,
			MessageArgs: []interface{}{filePath, permission},
		}
	}

	if permission&othersCanWriteFilePermission != 0 {
		return &SnowflakeError{
			Number:      ErrCodeInvalidFilePermission,
			Message:     errMsgInvalidWritablePermissionToFile,
			MessageArgs: []interface{}{filePath, permission},
		}
	}

	return nil
}

func shouldSkipWarningForReadPermissions() bool {
	return os.Getenv(skipWarningForReadPermissionsEnv) != ""
}
