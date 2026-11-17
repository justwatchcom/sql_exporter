package gosnowflake

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/snowflakedb/gosnowflake/internal/compilation"
	internalos "github.com/snowflakedb/gosnowflake/internal/os"
)

const disableMinicoreEnv = "SF_DISABLE_MINICORE"

var miniCoreOnce sync.Once
var miniCoreMutex sync.RWMutex

var miniCoreInstance miniCore

var minicoreLoadLogs = struct {
	mu        sync.Mutex
	logs      []string
	startTime time.Time
}{}

type minicoreDirCandidate struct {
	dirType    string
	path       string
	preUseFunc func() error
}

func newMinicoreDirCandidate(dirType, path string) minicoreDirCandidate {
	return minicoreDirCandidate{
		dirType: dirType,
		path:    path,
	}
}

func (m minicoreDirCandidate) String() string {
	return m.dirType
}

// getMiniCoreFileName returns the filename of the loaded minicore library
func getMiniCoreFileName() string {
	miniCoreMutex.RLock()
	defer miniCoreMutex.RUnlock()
	return corePlatformConfig.coreLibFileName
}

// miniCoreErrorType represents the category of minicore error that occurred.
type miniCoreErrorType int

// Error type constants for categorizing minicore failures.
const (
	miniCoreErrorTypeLoad   miniCoreErrorType = iota // Library loading failed
	miniCoreErrorTypeSymbol                          // Symbol lookup failed
	miniCoreErrorTypeCall                            // Function call failed
	miniCoreErrorTypeInit                            // Initialization failed
	miniCoreErrorTypeWrite                           // File write failed
)

// String returns a human-readable string representation of the error type.
func (et miniCoreErrorType) String() string {
	switch et {
	case miniCoreErrorTypeLoad:
		return "load"
	case miniCoreErrorTypeSymbol:
		return "symbol"
	case miniCoreErrorTypeCall:
		return "call"
	case miniCoreErrorTypeInit:
		return "init"
	case miniCoreErrorTypeWrite:
		return "write"
	default:
		return "unknown"
	}
}

// miniCoreError represents a structured error from minicore operations.
// It provides detailed context about what went wrong, where, and why.
type miniCoreError struct {
	errorType miniCoreErrorType // errorType categorizes the kind of error
	platform  string            // platform identifies the OS where error occurred
	path      string            // path to the library file, if applicable
	err       error             // err wraps the underlying error cause
}

// Error returns a formatted error message with context about the failure.
func (e *miniCoreError) Error() string {
	if e.path != "" {
		return fmt.Sprintf("minicore %s on %s (path: %s): %v", e.errorType, e.platform, e.path, e.err)
	}
	return fmt.Sprintf("minicore %s on %s: %v", e.errorType, e.platform, e.err)
}

// Unwrap returns the underlying error for error chain inspection.
func (e *miniCoreError) Unwrap() error {
	return e.err
}

// newMiniCoreError creates a new structured minicore error with full context.
func newMiniCoreError(errType miniCoreErrorType, platform, path string, err error) *miniCoreError {
	return &miniCoreError{
		errorType: errType,
		platform:  platform,
		path:      path,
		err:       err,
	}
}

// corePlatformConfigType holds platform-specific minicore configuration.
type corePlatformConfigType struct {
	initialized     bool   // initialized indicates if the platform is supported
	coreLib         []byte // coreLib contains the embedded native library
	coreLibFileName string // coreLibFileName is the filename from the go:embed directive
}

// corePlatformConfig holds platform-specific configuration. If not initialized, minicore is unsupported.
var corePlatformConfig = corePlatformConfigType{}

type miniCore interface {
	// FullVersion returns the version string from the native library.
	FullVersion() (string, error)
}

// erroredMiniCore implements miniCore but always returns an error.
// It's used when minicore initialization fails.
type erroredMiniCore struct {
	err error
}

// newErroredMiniCore creates a miniCore implementation that always returns the given error.
func newErroredMiniCore(err error) *erroredMiniCore {
	minicoreDebugf("minicore error: %v", err)
	return &erroredMiniCore{err: err}
}

// FullVersion always returns an empty string and the stored error.
func (emc erroredMiniCore) FullVersion() (string, error) {
	return "", emc.err
}

// miniCoreLoaderType manages the loading and initialization of the minicore native library.
type miniCoreLoaderType struct {
	searchDirs []minicoreDirCandidate // searchDirs contains directories to search for the library
}

// newMiniCoreLoader creates a new minicore miniCoreLoaderType with platform-appropriate search directories.
func newMiniCoreLoader() *miniCoreLoaderType {
	return &miniCoreLoaderType{
		searchDirs: buildMiniCoreSearchDirs(),
	}
}

// buildMiniCoreSearchDirs constructs the list of directories to search for the minicore library.
func buildMiniCoreSearchDirs() []minicoreDirCandidate {
	var dirs []minicoreDirCandidate

	// Add temp directory
	if tempDir, err := os.MkdirTemp("", "gosnowflake-cgo"); err == nil && tempDir != "" {
		minicoreDebugf("created temp directory for minicore loading")
		switch runtime.GOOS {
		case "linux", "darwin":
			if err = os.Chmod(tempDir, 0700); err == nil {
				minicoreDebugf("configured permissions to temp as 0700")
				dirs = append(dirs, newMinicoreDirCandidate("temp", tempDir))
			} else {
				minicoreDebugf("cannot change minicore directory permissions to 0700")
			}
		default:
			dirs = append(dirs, newMinicoreDirCandidate("temp", tempDir))
		}
	} else {
		minicoreDebugf("cannot create temp directory for gosnowflakecore: %v", err)
	}

	// Add platform-specific cache directory
	if cacheDir := getMiniCoreCacheDirInHome(); cacheDir != "" {
		dirCandidate := newMinicoreDirCandidate("home", cacheDir)
		dirCandidate.preUseFunc = func() error {
			minicoreDebugf("using cache directory: %v", cacheDir)
			if err := os.MkdirAll(cacheDir, 0700); err != nil {
				minicoreDebugf("cannot create %v: %v", cacheDir, err)
				return err
			}
			minicoreDebugf("created cache directory: %v, configured permissions to 0700", cacheDir)
			if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
				if err := os.Chmod(cacheDir, 0700); err != nil {
					minicoreDebugf("cannot change minicore cache directory permissions to 0700. %v", err)
					return err
				}
			}
			minicoreDebugf("configured permissions to cache directory as 0700")
			return nil
		}
		dirs = append(dirs, dirCandidate)
	}

	// Add current working directory
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, newMinicoreDirCandidate("cwd", cwd))
	} else {
		minicoreDebugf("cannot get current working directory: %v", err)
	}

	minicoreDebugf("candidate directories for minicore loading: %v", dirs)
	return dirs
}

// getMiniCoreCacheDirInHome returns the platform-specific cache directory for storing the minicore library.
func getMiniCoreCacheDirInHome() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		minicoreDebugf("cannot get user home directory: %v", err)
		return ""
	}

	switch runtime.GOOS {
	case "windows":
		return filepath.Join(homeDir, "AppData", "Local", "Snowflake", "Caches", "minicore")
	case "darwin":
		return filepath.Join(homeDir, "Library", "Caches", "Snowflake", "minicore")
	default:
		return filepath.Join(homeDir, ".cache", "snowflake", "minicore")
	}
}

// loadCore loads and initializes the minicore native library.
func (l *miniCoreLoaderType) loadCore() miniCore {
	if !corePlatformConfig.initialized {
		return newErroredMiniCore(newMiniCoreError(miniCoreErrorTypeInit, runtime.GOOS, "",
			fmt.Errorf("minicore is not supported on %v/%v platform", runtime.GOOS, runtime.GOARCH)))
	}

	if linkingMode, err := compilation.CheckDynamicLinking(); err != nil || linkingMode == compilation.UnknownLinking {
		minicoreDebugf("cannot determine linking mode: %v, proceeding anyway", err)
	} else if linkingMode == compilation.StaticLinking {
		return newErroredMiniCore(newMiniCoreError(miniCoreErrorTypeLoad, runtime.GOOS, "",
			fmt.Errorf("binary is statically linked (no dynamic linker); dlopen is unavailable")))
	}

	libDir, libPath, err := l.writeLibraryToFile()
	if err != nil {
		return newErroredMiniCore(err)
	}
	defer func(libDir minicoreDirCandidate, libPath string) {
		if err = os.Remove(libPath); err != nil {
			minicoreDebugf("cannot remove library. %v", err)
		}
		if libDir.dirType == "temp" {
			if err = os.Remove(libDir.path); err != nil {
				minicoreDebugf("cannot remove temp directory. %v", err)
			}
		}
	}(libDir, libPath)

	minicoreDebugf("Loading minicore library from: %s", libDir)
	return osSpecificLoadFromPath(libPath)
}

var osSpecificLoadFromPath = func(libPath string) miniCore {
	return newErroredMiniCore(fmt.Errorf("minicore loader is not available on %v/%v", runtime.GOOS, runtime.GOARCH))
}

// writeLibraryToFile writes the embedded library to the first available directory
func (l *miniCoreLoaderType) writeLibraryToFile() (minicoreDirCandidate, string, error) {
	var errs []error

	for _, dir := range l.searchDirs {
		if dir.preUseFunc != nil {
			if err := dir.preUseFunc(); err != nil {
				minicoreDebugf("Failed to prepare directory %q: %v", dir.path, err)
				errs = append(errs, fmt.Errorf("failed to prepare directory %q: %v", dir.path, err))
				continue
			}
		}
		libPath := filepath.Join(dir.path, corePlatformConfig.coreLibFileName)
		if err := os.WriteFile(libPath, corePlatformConfig.coreLib, 0600); err != nil {
			minicoreDebugf("Failed to write embedded library to %q: %v", libPath, err)
			errs = append(errs, fmt.Errorf("failed to write to %q: %v", libPath, err))
			continue
		}
		minicoreDebugf("Successfully wrote embedded library to %s", dir)
		return dir, libPath, nil
	}

	return minicoreDirCandidate{}, "", newMiniCoreError(miniCoreErrorTypeWrite, runtime.GOOS, "",
		fmt.Errorf("failed to write embedded library to any directory (errors: %v)", errs))
}

// getMiniCore returns the minicore instance, loading it asynchronously if needed.
func getMiniCore() miniCore {
	miniCoreOnce.Do(func() {
		minicoreDebugf("minicore enabled at compile time: %v", compilation.MinicoreEnabled)
		minicoreDebugf("cgo enabled: %v", compilation.CgoEnabled)
		if !compilation.MinicoreEnabled {
			logger.Debugf("minicore disabled at compile time (built with -tags minicore_disabled)")
			return
		}
		if strings.EqualFold(os.Getenv(disableMinicoreEnv), "true") {
			logger.Debugf("minicore loading disabled")
			return
		}
		go func() {
			minicoreLoadLogs.mu.Lock()
			minicoreLoadLogs.startTime = time.Now()
			minicoreLoadLogs.mu.Unlock()

			minicoreDebugf("Starting asynchronous minicore loading")
			miniCoreLoader := newMiniCoreLoader()
			core := miniCoreLoader.loadCore()
			miniCoreMutex.Lock()
			miniCoreInstance = core
			miniCoreMutex.Unlock()
			if v, err := core.FullVersion(); err != nil {
				minicoreDebugf("Minicore version not available: %v", err)
			} else {
				minicoreDebugf("Minicore loading completed, version: %s", v)
			}
		}()
	})

	// Return current instance (may be nil initially)
	miniCoreMutex.RLock()
	defer miniCoreMutex.RUnlock()
	return miniCoreInstance
}

func init() {
	// Start async minicore loading but don't block initialization.
	// This allows the application to start quickly while minicore loads in the background.
	getMiniCore()
}

func minicoreDebugf(format string, args ...any) {
	minicoreLoadLogs.mu.Lock()
	defer minicoreLoadLogs.mu.Unlock()
	var finalArgs []any
	finalArgs = append(finalArgs, time.Since(minicoreLoadLogs.startTime))
	finalArgs = append(finalArgs, args...)
	finalFormat := "[%v] " + format
	logger.Debugf(finalFormat, finalArgs...)
	minicoreLoadLogs.logs = append(minicoreLoadLogs.logs, maskSecrets(fmt.Sprintf(finalFormat, finalArgs...)))
}

// libcType represents the type of C library in use
type libcType string

const (
	libcTypeGlibc   libcType = "glibc"
	libcTypeMusl    libcType = "musl"
	libcTypeIgnored libcType = ""
)

// detectLibc detects whether glibc or musl is in use
func detectLibc() libcType {
	if runtime.GOOS != "linux" {
		return libcTypeIgnored
	}

	info := internalos.GetLibcInfo()

	switch info.Family {
	case "glibc":
		minicoreDebugf("detected glibc environment")
		if info.Version != "" {
			minicoreDebugf("glibc version: %s", info.Version)
		}
		return libcTypeGlibc
	case "musl":
		minicoreDebugf("detected musl environment")
		if info.Version != "" {
			minicoreDebugf("musl version: %s", info.Version)
		}
		return libcTypeMusl
	default:
		minicoreDebugf("Could not detect libc type, assuming glibc")
		return libcTypeGlibc
	}
}
