package config

import (
	"encoding/base64"
	"errors"
	"os"
	path "path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	sferrors "github.com/snowflakedb/gosnowflake/v2/internal/errors"
)

const (
	snowflakeConnectionName = "SNOWFLAKE_DEFAULT_CONNECTION_NAME"
	snowflakeHome           = "SNOWFLAKE_HOME"
	defaultTokenPath        = "/snowflake/session/token"

	othersCanReadFilePermission  = os.FileMode(0044)
	othersCanWriteFilePermission = os.FileMode(0022)
	executableFilePermission     = os.FileMode(0111)

	skipWarningForReadPermissionsEnv = "SF_SKIP_WARNING_FOR_READ_PERMISSIONS_ON_CONFIG_FILE"
	// SkipTokenFilePermissionsVerificationEnv keeps an env name for skipping token file permission and user verification
	SkipTokenFilePermissionsVerificationEnv = "SKIP_TOKEN_FILE_PERMISSIONS_VERIFICATION"
)

// LoadConnectionConfig returns connection configs loaded from the toml file.
// By default, SNOWFLAKE_HOME(toml file path) is os.snowflakeHome/.snowflake
// and SNOWFLAKE_DEFAULT_CONNECTION_NAME(DSN) is 'default'
func LoadConnectionConfig() (*Config, error) {
	logger.Trace("Loading connection configuration from the local files.")
	cfg := &Config{
		Params:        make(map[string]*string),
		Authenticator: AuthTypeSnowflake, // Default to snowflake
		inputShape:    &ConnectionIdentifierShape{},
	}
	dsn := getConnectionDSN(os.Getenv(snowflakeConnectionName))
	snowflakeConfigDir, err := GetTomlFilePath(os.Getenv(snowflakeHome))
	if err != nil {
		return nil, err
	}
	logger.Debugf("Looking for connection file in directory %v", snowflakeConfigDir)
	tomlFilePath := path.Join(snowflakeConfigDir, "connections.toml")
	if ShouldSkipTokenFilePermissionsVerification() {
		logger.Debugf("Skipping permission verification for %v because %v=true", tomlFilePath, SkipTokenFilePermissionsVerificationEnv)
	} else {
		err = ValidateFilePermission(tomlFilePath)
		if err != nil {
			return nil, err
		}
	}
	tomlInfo := make(map[string]any)
	_, err = toml.DecodeFile(tomlFilePath, &tomlInfo)
	if err != nil {
		return nil, err
	}
	dsnMap, exist := tomlInfo[dsn]
	if !exist {
		return nil, &sferrors.SnowflakeError{
			Number:  sferrors.ErrCodeFailedToFindDSNInToml,
			Message: sferrors.ErrMsgFailedToFindDSNInTomlFile,
		}
	}
	connectionConfig, ok := dsnMap.(map[string]any)
	if !ok {
		return nil, err
	}
	logger.Trace("Trying to parse the config file")
	err = ParseToml(cfg, connectionConfig)
	if err != nil {
		return nil, err
	}
	err = FillMissingConfigParameters(cfg)
	if err != nil {
		return nil, err
	}
	return cfg, err
}

// ParseToml parses a TOML connection map into a Config.
func ParseToml(cfg *Config, connectionMap map[string]any) error {
	for key, value := range connectionMap {
		if err := HandleSingleParam(cfg, key, value); err != nil {
			return err
		}
	}
	return nil
}

// HandleSingleParam processes a single TOML parameter into a Config.
func HandleSingleParam(cfg *Config, key string, value any) error {
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
		if err == nil && cfg.Host != "" && cfg.inputShape != nil {
			cfg.inputShape.HostProvided = true
		}
	case "account":
		cfg.Account, err = parseString(value)
		if err == nil {
			recordAccountShape(cfg.inputShape, cfg.Account)
		}
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
		if err == nil && cfg.Region != "" && cfg.inputShape != nil {
			cfg.inputShape.RegionProvided = true
		}
	case "protocol":
		cfg.Protocol, err = parseString(value)
	case "passcode":
		cfg.Passcode, err = parseString(value)
	case "port":
		cfg.Port, err = ParseInt(value)
	case "passcodeinpassword":
		cfg.PasscodeInPassword, err = ParseBool(value)
	case "clienttimeout":
		cfg.ClientTimeout, err = ParseDuration(value)
	case "jwtclienttimeout":
		cfg.JWTClientTimeout, err = ParseDuration(value)
	case "logintimeout":
		cfg.LoginTimeout, err = ParseDuration(value)
	case "requesttimeout":
		cfg.RequestTimeout, err = ParseDuration(value)
	case "jwttimeout":
		cfg.JWTExpireTimeout, err = ParseDuration(value)
	case "externalbrowsertimeout":
		cfg.ExternalBrowserTimeout, err = ParseDuration(value)
	case "maxretrycount":
		cfg.MaxRetryCount, err = ParseInt(value)
	case "application":
		cfg.Application, err = parseString(value)
	case "authenticator":
		var v string
		v, err = parseString(value)
		if err = checkParsingError(err, key, value); err != nil {
			return err
		}
		err = DetermineAuthenticatorType(cfg, v)
	case "disableocspchecks":
		cfg.DisableOCSPChecks, err = ParseBool(value)
	case "ocspfailopen":
		var vv Bool
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
			return &sferrors.SnowflakeError{
				Number:  sferrors.ErrCodePrivateKeyParseError,
				Message: "Base64 decode failed",
			}
		}
		cfg.PrivateKey, err = ParsePKCS8PrivateKey(block)
	case "privatekeypath", "privatekeyfile":
		var keyPath string
		keyPath, err = parseString(value)
		if err = checkParsingError(err, key, value); err != nil {
			return err
		}
		cfg.PrivateKey, err = parsePrivateKeyFromFile(keyPath)
	case "validatedefaultparameters":
		cfg.ValidateDefaultParameters, err = parseConfigBool(value)
	case "clientrequestmfatoken":
		cfg.ClientRequestMfaToken, err = parseConfigBool(value)
	case "clientstoretemporarycredential":
		cfg.ClientStoreTemporaryCredential, err = parseConfigBool(value)
	case "tracing":
		cfg.Tracing, err = parseString(value)
	case "logquerytext":
		cfg.LogQueryText, err = ParseBool(value)
	case "logqueryparameters":
		cfg.LogQueryParameters, err = ParseBool(value)
	case "tmpdirpath":
		cfg.TmpDirPath, err = parseString(value)
	case "disablequerycontextcache":
		cfg.DisableQueryContextCache, err = ParseBool(value)
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
	case "workloadidentityazureclientid":
		cfg.WorkloadIdentityAzureClientID, err = parseString(value)
	case "workloadidentityimpersonatinpath":
		cfg.WorkloadIdentityImpersonationPath, err = parseStrings(value)
	case "workloadidentityawsuseoutboundtoken":
		cfg.WorkloadIdentityAwsUseOutboundToken, err = parseConfigBool(value)
	case "workloadidentityhost":
		cfg.WorkloadIdentityHost, err = parseString(value)
	case "tokenfilepath":
		cfg.TokenFilePath, err = parseString(value)
		if err = checkParsingError(err, key, value); err != nil {
			return err
		}
	case "connectiondiagnosticsenabled":
		cfg.ConnectionDiagnosticsEnabled, err = ParseBool(value)
	case "connectiondiagnosticsallowlistfile":
		cfg.ConnectionDiagnosticsAllowlistFile, err = parseString(value)
	case "proxyhost":
		cfg.ProxyHost, err = parseString(value)
	case "proxyport":
		cfg.ProxyPort, err = ParseInt(value)
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

func checkParsingError(err error, key string, value any) error {
	if err != nil {
		err = &sferrors.SnowflakeError{
			Number:      sferrors.ErrCodeTomlFileParsingFailed,
			Message:     sferrors.ErrMsgFailedToParseTomlFile,
			MessageArgs: []any{key, value},
		}
		// Log the setting name only: TOML values may hold credentials, and the
		// "Parsed key: <k>, value: <v>" layout is not covered by MaskSecrets (SNOW-3649818).
		logger.Errorf("Parsed key: %s is not an option for the connection config", key)
		return err
	}
	logger.Warnf("Parsed key: %s — cannot be parsed as string", key)
	return nil
}

// ParseInt parses an interface value to int.
func ParseInt(i any) (int, error) {
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

// ParseBool parses an interface value to bool.
func ParseBool(i any) (bool, error) {
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

func parseConfigBool(i any) (Bool, error) {
	vv, err := ParseBool(i)
	if err != nil {
		return BoolFalse, err
	}
	if vv {
		return BoolTrue, nil
	}
	return BoolFalse, nil
}

// ParseDuration parses an interface value to time.Duration.
func ParseDuration(i any) (time.Duration, error) {
	v, ok := i.(string)
	if !ok {
		num, err := ParseInt(i)
		if err != nil {
			return time.Duration(0), err
		}
		t := int64(num)
		return time.Duration(t * int64(time.Second)), nil
	}
	return parseTimeout(v)
}

// ReadToken reads a token from the given path (or default path if empty).
func ReadToken(tokenPath string) (string, error) {
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
	err := ValidateFilePermission(tokenPath)
	if err != nil {
		return "", err
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return "", err
	}
	return string(token), nil
}

func parseString(i any) (string, error) {
	v, ok := i.(string)
	if !ok {
		return "", errors.New("failed to convert the value to string")
	}
	return v, nil
}

func parseStrings(i any) ([]string, error) {
	s, ok := i.(string)
	if !ok {
		return nil, errors.New("failed to convert the value to string")
	}
	return strings.Split(s, ","), nil
}

// GetTomlFilePath returns the path to the TOML file directory.
func GetTomlFilePath(filePath string) (string, error) {
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

// ValidateFilePermission checks that a file does not have overly permissive permissions.
func ValidateFilePermission(filePath string) error {
	if runtime.GOOS == "windows" {
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
		return &sferrors.SnowflakeError{
			Number:      sferrors.ErrCodeInvalidFilePermission,
			Message:     sferrors.ErrMsgInvalidExecutablePermissionToFile,
			MessageArgs: []any{filePath, permission},
		}
	}

	if permission&othersCanWriteFilePermission != 0 {
		return &sferrors.SnowflakeError{
			Number:      sferrors.ErrCodeInvalidFilePermission,
			Message:     sferrors.ErrMsgInvalidWritablePermissionToFile,
			MessageArgs: []any{filePath, permission},
		}
	}

	return nil
}

func shouldSkipWarningForReadPermissions() bool {
	return os.Getenv(skipWarningForReadPermissionsEnv) != ""
}

// ShouldSkipTokenFilePermissionsVerification reports whether the user has
// requested bypassing owner/permission checks for connections.toml and the
// credential cache (file, directory, and lock file). Set the
// SF_SKIP_TOKEN_FILE_PERMISSIONS_VERIFICATION environment variable to "true"
// to enable the bypass.
func ShouldSkipTokenFilePermissionsVerification() bool {
	return strings.EqualFold(os.Getenv(SkipTokenFilePermissionsVerificationEnv), "true")
}
