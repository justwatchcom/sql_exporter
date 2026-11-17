//go:build !windows && !minicore_disabled

package gosnowflake

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>
#include <string.h>

static void* dlOpen(const char* path) {
    return dlopen(path, RTLD_LAZY);
}

static void* dlSym(void* handle, const char* name) {
    return dlsym(handle, name);
}

static int dlClose(void* handle) {
	return dlclose(handle);
}

static char* dlError() {
	return dlerror();
}

typedef const char* (*coreFullVersion)();

static const char* callCoreFullVersion(coreFullVersion f) {
    return f();
}
*/
import "C"
import (
	"errors"
	"fmt"
	"unsafe"
)

type posixMiniCore struct {
	// fullVersion holds the version string returned from Rust, just to not invoke it multiple times.
	fullVersion string
	// coreInitError holds any error that occurred during initialization.
	coreInitError error
}

func newPosixMiniCore(fullVersion string) *posixMiniCore {
	return &posixMiniCore{
		fullVersion: fullVersion,
	}
}

func (pmc *posixMiniCore) FullVersion() (string, error) {
	return pmc.fullVersion, pmc.coreInitError
}

var _ = func() any {
	osSpecificLoadFromPath = loadFromPath
	return nil
}()

func loadFromPath(libPath string) miniCore {
	cLibPath := C.CString(libPath)
	defer C.free(unsafe.Pointer(cLibPath))

	// Loading library
	minicoreDebugf("Calling dlOpen")
	handle := C.dlOpen(cLibPath)
	minicoreDebugf("Calling dlOpen finished")
	if handle == nil {
		err := C.dlError()
		mcErr := newMiniCoreError(miniCoreErrorTypeLoad, "posix", libPath, fmt.Errorf("failed to load shared library: %v", C.GoString(err)))
		return newErroredMiniCore(mcErr)
	}

	// Unloading library at the end
	defer func() {
		minicoreDebugf("Calling dlClose")
		defer minicoreDebugf("Calling dlClose finished")
		if ret := C.dlClose(handle); ret != 0 {
			err := C.dlError()
			minicoreDebugf("Error when closing dynamic library: %v", C.GoString(err))
		}
	}()

	// Loading symbol
	symbolName := C.CString("sf_core_full_version")
	defer C.free(unsafe.Pointer(symbolName))
	minicoreDebugf("Loading sf_core_full_version symbol")
	coreFullVersionSymbol := C.dlSym(handle, symbolName)
	minicoreDebugf("Loading sf_core_full_version symbol finished")
	if coreFullVersionSymbol == nil {
		err := C.dlError()
		mcErr := newMiniCoreError(miniCoreErrorTypeSymbol, "posix", libPath, fmt.Errorf("symbol 'sf_core_full_version' not found: %v", C.GoString(err)))
		return newErroredMiniCore(mcErr)
	}

	// Calling minicore
	var coreFullVersionFunc C.coreFullVersion = (C.coreFullVersion)(coreFullVersionSymbol)
	minicoreDebugf("Calling sf_core_full_version")
	fullVersion := C.GoString(C.callCoreFullVersion(coreFullVersionFunc))
	minicoreDebugf("Calling sf_core_full_version finished")
	if fullVersion == "" {
		return newErroredMiniCore(newMiniCoreError(miniCoreErrorTypeCall, "posix", libPath, errors.New("failed to get version from core library function")))
	}
	return newPosixMiniCore(fullVersion)
}
