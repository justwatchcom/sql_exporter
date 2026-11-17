package gosnowflake

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	sfconfig "github.com/snowflakedb/gosnowflake/v2/internal/config"
)

type tokenType string

const (
	idToken           tokenType = "IdToken"
	mfaToken          tokenType = "MfaToken"
	oauthAccessToken  tokenType = "OauthAccessToken"
	oauthRefreshToken tokenType = "OauthRefreshToken"
)

const (
	credCacheDirEnv          = "SF_TEMPORARY_CREDENTIAL_CACHE_DIR"
	credCacheFileNameEnv     = "SF_TEMPORARY_CREDENTIAL_CACHE_FILE_NAME"
	defaultCredCacheFileName = "credential_cache_v2.json"
	credCacheLockRetries     = 20
	credCacheLockRetryBase   = 100 * time.Millisecond
)

type cacheDirConf struct {
	envVar       string
	pathSegments []string
}

var defaultLinuxCacheDirConf = []cacheDirConf{
	{envVar: credCacheDirEnv, pathSegments: []string{}},
	{envVar: "XDG_CACHE_HOME", pathSegments: []string{"snowflake"}},
	{envVar: "HOME", pathSegments: []string{".cache", "snowflake"}},
}

func credCacheFileName() string {
	if name := os.Getenv(credCacheFileNameEnv); name != "" {
		return name
	}
	return defaultCredCacheFileName
}

type secureTokenSpec interface {
	buildKey() (string, error)
}

// Fields must remain in lexicographic order to match the cross-driver hash contract.
type mfaIDKeyData struct {
	Snowflake string `json:"snowflake"`
	Username  string `json:"username"`
}

// Fields must remain in lexicographic order to match the cross-driver hash contract.
type oauthKeyData struct {
	Idp       string `json:"idp"`
	Role      string `json:"role"`
	Snowflake string `json:"snowflake"`
	Username  string `json:"username"`
}

type hostUserTokenSpec struct {
	tokenType tokenType
	snowflake string
	username  string
}

func (s *hostUserTokenSpec) buildKey() (string, error) {
	snowflake := normalizeURL(s.snowflake)
	username := normalizeIdentifier(s.username)
	if snowflake == "" {
		return "", errors.New("snowflake URL is required for token cache key")
	}
	if username == "" {
		return "", errors.New("username is required for token cache key")
	}
	return marshalAndFinalize(s.tokenType, mfaIDKeyData{
		Snowflake: snowflake,
		Username:  username,
	})
}

type oauthTokenSpec struct {
	tokenType tokenType
	idp       string
	snowflake string
	username  string
	role      string
}

func (s *oauthTokenSpec) buildKey() (string, error) {
	idp := normalizeURL(s.idp)
	snowflake := normalizeURL(s.snowflake)
	username := normalizeIdentifier(s.username)
	if snowflake == "" {
		return "", errors.New("snowflake URL is required for token cache key")
	}
	if username == "" {
		return "", errors.New("username is required for token cache key")
	}
	if idp == "" {
		return "", errors.New("idp URL is required for OAuth token cache key")
	}
	return marshalAndFinalize(s.tokenType, oauthKeyData{
		Idp:       idp,
		Role:      normalizeIdentifier(s.role),
		Snowflake: snowflake,
		Username:  username,
	})
}

// The token type appears only in the key prefix, never in keyData.
func finalizeCacheKey(tt tokenType, jsonBytes []byte) string {
	sum := sha256.Sum256(jsonBytes)
	hexHash := hex.EncodeToString(sum[:])
	return "SnowflakeTokenCache.v2." + string(tt) + "." + hexHash
}

func marshalAndFinalize(tt tokenType, keyData any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// The other drivers' serializers write <, > and & literally today; escaping them would change the hash.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(keyData); err != nil {
		return "", fmt.Errorf("failed to serialize cache key: %w", err)
	}
	return finalizeCacheKey(tt, bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

func includeExplicitPort(cfg *Config) bool {
	if cfg.Port == 0 {
		return false
	}
	if strings.EqualFold(cfg.Protocol, "http") {
		return cfg.Port != 80
	}
	return cfg.Port != 443
}

func hostWithOptionalPort(cfg *Config) string {
	if includeExplicitPort(cfg) {
		return net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	}
	return cfg.Host
}

func newMfaTokenSpec(cfg *Config) *hostUserTokenSpec {
	return &hostUserTokenSpec{
		tokenType: mfaToken,
		snowflake: hostWithOptionalPort(cfg),
		username:  cfg.User,
	}
}

func newIDTokenSpec(cfg *Config) *hostUserTokenSpec {
	return &hostUserTokenSpec{
		tokenType: idToken,
		snowflake: hostWithOptionalPort(cfg),
		username:  cfg.User,
	}
}

func newOAuthAccessTokenSpec(cfg *Config) *oauthTokenSpec {
	return &oauthTokenSpec{
		tokenType: oauthAccessToken,
		idp:       oauthTokenRequestURL(cfg),
		snowflake: hostWithOptionalPort(cfg),
		username:  cfg.User,
		role:      cfg.Role,
	}
}

func newOAuthRefreshTokenSpec(cfg *Config) *oauthTokenSpec {
	return &oauthTokenSpec{
		tokenType: oauthRefreshToken,
		idp:       oauthTokenRequestURL(cfg),
		snowflake: hostWithOptionalPort(cfg),
		username:  cfg.User,
		role:      cfg.Role,
	}
}

type secureStorageManager interface {
	setCredential(tokenSpec secureTokenSpec, value string)
	getCredential(tokenSpec secureTokenSpec) string
	deleteCredential(tokenSpec secureTokenSpec)
}

var credentialsStorage = newSecureStorageManager()

func newSecureStorageManager() secureStorageManager {
	return defaultOsSpecificSecureStorageManager()
}

type fileBasedSecureStorageManager struct {
	credDirPath  string
	credFilePath string
	lockPath     string
}

func newFileBasedSecureStorageManager() (*fileBasedSecureStorageManager, error) {
	credDirPath, err := buildCredCacheDirPath(defaultLinuxCacheDirConf)
	if err != nil {
		return nil, err
	}
	credFilePath := filepath.Join(credDirPath, credCacheFileName())
	ssm := &fileBasedSecureStorageManager{
		credDirPath:  credDirPath,
		credFilePath: credFilePath,
		lockPath:     credFilePath + ".lck",
	}
	return ssm, nil
}

func lookupCacheDir(envVar string, pathSegments ...string) (string, error) {
	envVal := os.Getenv(envVar)
	if envVal == "" {
		return "", fmt.Errorf("environment variable %s not set", envVar)
	}

	fileInfo, err := os.Stat(envVal)
	if err != nil {
		return "", fmt.Errorf("failed to stat %s=%s, due to %v", envVar, envVal, err)
	}

	if !fileInfo.IsDir() {
		return "", fmt.Errorf("environment variable %s=%s is not a directory", envVar, envVal)
	}

	cacheDir := filepath.Join(envVal, filepath.Join(pathSegments...))
	parentOfCacheDir := filepath.Dir(cacheDir)

	if err = os.MkdirAll(parentOfCacheDir, os.FileMode(0755)); err != nil {
		return "", err
	}

	// We don't check if permissions are incorrect here if a directory exists, because we check it later.
	if err = os.Mkdir(cacheDir, os.FileMode(0700)); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}

	return cacheDir, nil
}

func buildCredCacheDirPath(confs []cacheDirConf) (string, error) {
	for _, conf := range confs {
		path, err := lookupCacheDir(conf.envVar, conf.pathSegments...)
		if err != nil {
			if conf.envVar == credCacheDirEnv && os.Getenv(credCacheDirEnv) != "" {
				logger.Warnf("%s is set but unusable; token caching is disabled. %v", credCacheDirEnv, err)
				return "", err
			}
			logger.Debugf("Skipping %s in cache directory lookup due to %v", conf.envVar, err)
			continue
		}
		logger.Debugf("Using %s as cache directory", path)
		return path, nil
	}

	return "", errors.New("no credentials cache directory found")
}

func (ssm *fileBasedSecureStorageManager) getTokens(data map[string]any) map[string]any {
	val, ok := data["tokens"]
	if !ok {
		return map[string]any{}
	}

	tokens, ok := val.(map[string]any)
	if !ok {
		return map[string]any{}
	}

	return tokens
}

func (ssm *fileBasedSecureStorageManager) withLock(action func(cacheFile *os.File)) {
	cacheDir, err := os.Open(ssm.credDirPath)
	if err != nil {
		logger.Warnf("cannot access %v. %v", ssm.credDirPath, err)
		return
	}
	defer func() {
		if err := cacheDir.Close(); err != nil {
			logger.Warnf("cannot release file descriptor for %v. %v", ssm.credDirPath, err)
		}
	}()

	verifyPermissions := !sfconfig.ShouldSkipTokenFilePermissionsVerification()
	if verifyPermissions {
		// Checked before the lock file is opened: another user who can write to the
		// directory could otherwise create and hold the lock file.
		if err := ensureFileOwner(cacheDir); err != nil {
			logger.Warnf("failed to ensure owner for temporary cache dir. %v", err)
			return
		}
		if err := ensureFilePermissions(cacheDir, 0700|os.ModeDir); err != nil {
			logger.Warnf("failed to ensure permission for temporary cache dir. %v", err)
			return
		}
	} else {
		logger.Debugf("Skipping credential cache permission verification because SKIP_TOKEN_FILE_PERMISSIONS_VERIFICATION=true")
	}

	lockFile, err := ssm.lockFile(verifyPermissions)
	if err != nil {
		logger.Warnf("Unable to lock cache. %v", err)
		return
	}
	defer ssm.unlockFile(lockFile)

	ssm.withCacheFile(verifyPermissions, action)
}

func (ssm *fileBasedSecureStorageManager) withCacheFile(verifyPermissions bool, action func(*os.File)) {
	cacheFile, err := ssm.openCacheFile()
	if err != nil {
		logger.Warnf("cannot access %v. %v", ssm.credFilePath, err)
		return
	}
	defer func() {
		if err := cacheFile.Close(); err != nil {
			logger.Warnf("cannot release file descriptor for %v. %v", ssm.credFilePath, err)
		}
	}()

	if verifyPermissions {
		if err := ensureFileOwner(cacheFile); err != nil {
			logger.Warnf("failed to ensure owner for temporary cache file. %v", err)
			return
		}
		if err := ensureFilePermissions(cacheFile, 0600); err != nil {
			logger.Warnf("failed to ensure permission for temporary cache file. %v", err)
			return
		}
	}

	action(cacheFile)
}

func (ssm *fileBasedSecureStorageManager) setCredential(tokenSpec secureTokenSpec, value string) {
	if value == "" {
		logger.Debug("no token provided")
		return
	}
	credentialsKey, err := tokenSpec.buildKey()
	if err != nil {
		logger.Warnf("cannot build token spec: %v", err)
		return
	}

	ssm.withLock(func(cacheFile *os.File) {
		credCache, err := ssm.readTemporaryCacheFile(cacheFile)
		if err != nil {
			logger.Warnf("Error while reading cache file. %v", err)
			credCache = map[string]any{}
		}
		tokens := ssm.getTokens(credCache)
		tokens[credentialsKey] = value
		credCache["tokens"] = tokens
		err = ssm.writeTemporaryCacheFile(credCache, cacheFile)
		if err != nil {
			logger.Warnf("Set credential failed. Unable to write cache. %v", err)
		} else {
			logger.Debugf("Set credential succeeded. Key: %v, file location: %v", credentialsKey, ssm.credFilePath)
		}
	})
}

func (ssm *fileBasedSecureStorageManager) lockFile(verifyOwner bool) (*os.File, error) {
	lockPath := ssm.lockPath
	lockFile, err := openLockFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open %v. err: %v", lockPath, err)
	}
	if err := ensureRegularFile(lockFile); err != nil {
		_ = lockFile.Close()
		return nil, err
	}
	if verifyOwner {
		if err := ensureFileOwner(lockFile); err != nil {
			_ = lockFile.Close()
			return nil, err
		}
	}

	sleep := credCacheLockRetryBase
	for attempt := range credCacheLockRetries {
		err := tryFlockExclusive(lockFile)
		if err == nil {
			return lockFile, nil
		}
		if !isLockContention(err) {
			_ = lockFile.Close()
			return nil, fmt.Errorf("failed to lock cache. lockPath: %v, err: %v", lockPath, err)
		}
		if attempt < credCacheLockRetries-1 {
			upper := min(sleep*3, credCacheLockRetryBase*16)
			sleep = time.Duration(chooseRandomFromRange(float64(credCacheLockRetryBase), float64(upper)))
			time.Sleep(sleep)
		}
	}
	_ = lockFile.Close()
	return nil, fmt.Errorf("failed to lock cache. lockPath: %v", lockPath)
}

func (ssm *fileBasedSecureStorageManager) unlockFile(lockFile *os.File) {
	if err := unlockFlock(lockFile); err != nil {
		logger.Warnf("Failed to unlock cache lock: %v. %v", ssm.lockPath, err)
	}
	if err := lockFile.Close(); err != nil {
		logger.Warnf("cannot release file descriptor for %v. %v", ssm.lockPath, err)
	}
}

func (ssm *fileBasedSecureStorageManager) getCredential(tokenSpec secureTokenSpec) string {
	credentialsKey, err := tokenSpec.buildKey()
	if err != nil {
		logger.Warnf("cannot build token spec: %v", err)
		return ""
	}

	ret := ""
	ssm.withLock(func(cacheFile *os.File) {
		credCache, err := ssm.readTemporaryCacheFile(cacheFile)
		if err != nil {
			logger.Warnf("Error while reading cache file. %v", err)
			return
		}
		cred, ok := ssm.getTokens(credCache)[credentialsKey]
		if !ok {
			return
		}

		credStr, ok := cred.(string)
		if !ok {
			return
		}

		ret = credStr
	})
	return ret
}

func (ssm *fileBasedSecureStorageManager) openCacheFile() (*os.File, error) {
	path := ssm.credFilePath
	f, err := openExistingNoFollow(path)
	if err == nil {
		if err := ensureRegularFile(f); err != nil {
			_ = f.Close()
			return nil, err
		}
		return f, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err = createExclusiveNoFollow(path, 0600)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	f, err = openExistingNoFollow(path)
	if err != nil {
		return nil, err
	}
	if err := ensureRegularFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func ensureRegularFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%v is not a regular file", f.Name())
	}
	return nil
}

func ensureFileOwner(f *os.File) error {
	ownerUID, err := provideFileOwner(f)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	currentUser, err := user.Current()
	if err != nil {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if strconv.Itoa(int(ownerUID)) != currentUser.Uid {
		return errors.New("incorrect owner of " + f.Name())
	}
	return nil
}

func ensureFilePermissions(f *os.File, expectedMode os.FileMode) error {
	fileInfo, err := f.Stat()
	if err != nil {
		return err
	}
	if fileInfo.Mode().Perm() != expectedMode&os.ModePerm {
		return fmt.Errorf("incorrect permissions(%v, expected %v) for credential file", fileInfo.Mode(), expectedMode)
	}
	return nil
}

func (ssm *fileBasedSecureStorageManager) readTemporaryCacheFile(cacheFile *os.File) (map[string]any, error) {

	jsonData, err := io.ReadAll(cacheFile)
	if err != nil {
		logger.Warnf("Failed to read credential cache file. %v.\n", err)
		return map[string]any{}, nil
	}
	if _, err = cacheFile.Seek(0, 0); err != nil {
		return map[string]any{}, fmt.Errorf("cannot seek to the beginning of a cache file. %v", err)
	}

	if len(jsonData) == 0 {
		// Happens when the file didn't exist before.
		return map[string]any{}, nil
	}

	credentialsMap := map[string]any{}
	err = json.Unmarshal(jsonData, &credentialsMap)
	if err != nil {
		return map[string]any{}, fmt.Errorf("failed to unmarshal credential cache file. %v", err)
	}

	return credentialsMap, nil
}

func (ssm *fileBasedSecureStorageManager) deleteCredential(tokenSpec secureTokenSpec) {
	credentialsKey, err := tokenSpec.buildKey()
	if err != nil {
		logger.Warnf("cannot build token spec: %v", err)
		return
	}

	ssm.withLock(func(cacheFile *os.File) {
		credCache, err := ssm.readTemporaryCacheFile(cacheFile)
		if err != nil {
			logger.Warnf("Error while reading cache file. %v", err)
			return
		}
		delete(ssm.getTokens(credCache), credentialsKey)

		err = ssm.writeTemporaryCacheFile(credCache, cacheFile)
		if err != nil {
			logger.Warnf("Set credential failed. Unable to write cache. %v", err)
		} else {
			logger.Debugf("Deleted credential succeeded. Key: %v, file location: %v", credentialsKey, ssm.credFilePath)
		}
	})
}

func (ssm *fileBasedSecureStorageManager) writeTemporaryCacheFile(cache map[string]any, cacheFile *os.File) error {
	bytes, err := json.Marshal(cache)
	if err != nil {
		return fmt.Errorf("failed to marshal credential cache map. %w", err)
	}

	if _, err = cacheFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("cannot seek to the beginning of a cache file. %v", err)
	}
	if err = cacheFile.Truncate(0); err != nil {
		return fmt.Errorf("error while truncating credentials cache. %v", err)
	}
	_, err = cacheFile.Write(bytes)
	if err != nil {
		return fmt.Errorf("failed to write the credential cache file: %w", err)
	}
	return nil
}

// Inputs may be a bare host[:port] with no scheme, which net/url does not parse as a host.
func normalizeURL(rawURL string) string {
	s := rawURL
	if _, after, ok := strings.Cut(s, "://"); ok {
		s = after
	}
	s, _, _ = strings.Cut(s, "?")
	s, _, _ = strings.Cut(s, "#")
	authority, path := s, ""
	if i := strings.IndexByte(s, '/'); i >= 0 {
		authority, path = s[:i], s[i:]
	}
	if _, host, ok := strings.Cut(authority, "@"); ok {
		authority = host
	}
	return strings.ToLower(strings.TrimRight(authority+path, "/"))
}

func normalizeIdentifier(id string) string {
	if strings.Contains(id, `"`) {
		return id
	}
	return strings.ToLower(id)
}

type noopSecureStorageManager struct {
}

func newNoopSecureStorageManager() *noopSecureStorageManager {
	return &noopSecureStorageManager{}
}

func (ssm *noopSecureStorageManager) setCredential(_ secureTokenSpec, _ string) {
}

func (ssm *noopSecureStorageManager) getCredential(_ secureTokenSpec) string {
	return ""
}

func (ssm *noopSecureStorageManager) deleteCredential(_ secureTokenSpec) {
}

type threadSafeSecureStorageManager struct {
	mu       *sync.Mutex
	delegate secureStorageManager
}

func (ssm *threadSafeSecureStorageManager) setCredential(tokenSpec secureTokenSpec, value string) {
	ssm.mu.Lock()
	defer ssm.mu.Unlock()
	ssm.delegate.setCredential(tokenSpec, value)
}

func (ssm *threadSafeSecureStorageManager) getCredential(tokenSpec secureTokenSpec) string {
	ssm.mu.Lock()
	defer ssm.mu.Unlock()
	return ssm.delegate.getCredential(tokenSpec)
}

func (ssm *threadSafeSecureStorageManager) deleteCredential(tokenSpec secureTokenSpec) {
	ssm.mu.Lock()
	defer ssm.mu.Unlock()
	ssm.delegate.deleteCredential(tokenSpec)
}
