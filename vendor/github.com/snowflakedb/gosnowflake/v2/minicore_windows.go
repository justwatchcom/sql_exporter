//go:build windows && !minicore_disabled

package gosnowflake

import (
	_ "embed"
	"fmt"
	"golang.org/x/sys/windows"
	"syscall"
	"unsafe"
)

type windowsMiniCore struct {
	// fullVersion holds the version string returned from the library
	fullVersion string
	// coreInitError holds any error that occurred during initialization
	coreInitError error
}

var _ = func() any {
	osSpecificLoadFromPath = loadFromPath
	return nil
}()

func (wmc *windowsMiniCore) FullVersion() (string, error) {
	return wmc.fullVersion, wmc.coreInitError
}

func loadFromPath(libPath string) miniCore {
	minicoreDebugf("Calling LoadLibrary")
	dllHandle, err := windows.LoadLibrary(libPath)
	minicoreDebugf("Calling LoadLibrary finished")
	if err != nil {
		mcErr := newMiniCoreError(miniCoreErrorTypeLoad, "windows", libPath, fmt.Errorf("failed to load shared library: %v", err))
		return newErroredMiniCore(mcErr)
	}

	// Release the DLL handle, because we cache minicore fullVersion result.
	defer windows.FreeLibrary(dllHandle)

	// Get the address of the function
	minicoreDebugf("getting procedure address")
	procAddr, err := windows.GetProcAddress(dllHandle, "sf_core_full_version")
	if err != nil {
		mcErr := newMiniCoreError(miniCoreErrorTypeSymbol, "windows", libPath, fmt.Errorf("procedure sf_core_full_version not found: %v", err))
		return newErroredMiniCore(mcErr)
	}

	minicoreDebugf("Invoking system call")
	// Second return value - omitted, required for syscalls that returns more values
	ret, _, callErr := syscall.Syscall(
		procAddr,
		0, // nargs: Number of arguments is ZERO
		0, // a1: Argument 1 (unused)
		0, // a2: Argument 2 (unused)
		0, // a3: Argument 3 (unused)
	)
	minicoreDebugf("Invoking system call finished")

	if callErr != 0 {
		mcErr := newMiniCoreError(miniCoreErrorTypeCall, "windows", libPath, fmt.Errorf("system call failed with error code: %v", callErr))
		return newErroredMiniCore(mcErr)
	}

	cStrPtr := (*byte)(unsafe.Pointer(ret))
	if cStrPtr == nil {
		mcErr := newMiniCoreError(miniCoreErrorTypeCall, "windows", libPath, fmt.Errorf("native function returned null pointer (error code: %v)", callErr))
		return newErroredMiniCore(mcErr)
	}

	goStr := windows.BytePtrToString(cStrPtr)
	return &windowsMiniCore{
		fullVersion: goStr,
	}
}
