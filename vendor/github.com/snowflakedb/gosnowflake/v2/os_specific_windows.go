package gosnowflake

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

var osVersion = getWindowsOSVersion()

func getWindowsOSVersion() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		errString := fmt.Sprintf("cannot open Windows registry key: %v", err)
		logger.Debugf(errString)
		return errString
	}
	defer k.Close()

	cv, _, err := k.GetStringValue("CurrentVersion")
	if err != nil {
		logger.Debugf("cannot find Windows current version: %v", err)
		cv = "CurrentVersion=unknown"
	}

	pn, _, err := k.GetStringValue("ProductName")
	if err != nil {
		logger.Debugf("cannot find Windows product name: %v", err)
		pn = "ProductName=unknown"
	}

	maj, _, err := k.GetIntegerValue("CurrentMajorVersionNumber")
	if err != nil {
		logger.Debugf("cannot find Windows major version number: %v", err)
	}

	min, _, err := k.GetIntegerValue("CurrentMinorVersionNumber")
	if err != nil {
		logger.Debugf("cannot find Windows minor version number: %v", err)
	}

	cb, _, err := k.GetStringValue("CurrentBuild")
	if err != nil {
		logger.Debugf("cannot find Windows current build: %v", err)
		cb = "CurrentBuild=unknown"
	}
	return fmt.Sprintf("CurrentVersion=%s; ProductName=%s; MajorVersion=%d; MinorVersion=%d; CurrentBuild=%s", cv, pn, maj, min, cb)
}

func provideFileOwner(file *os.File) (uint32, error) {
	return 0, errors.New("provideFileOwner is unsupported on windows")
}

func getFileContents(filePath string, expectedPerm os.FileMode) ([]byte, error) {
	fileContents, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	return fileContents, nil
}

// The file-based token cache is Linux-only; Windows uses Credential Manager.
func openExistingNoFollow(string) (*os.File, error) {
	return nil, errors.ErrUnsupported
}

func createExclusiveNoFollow(string, os.FileMode) (*os.File, error) {
	return nil, errors.ErrUnsupported
}

func openLockFile(string) (*os.File, error) {
	return nil, errors.ErrUnsupported
}

func tryFlockExclusive(*os.File) error {
	return errors.ErrUnsupported
}

func unlockFlock(*os.File) error {
	return errors.ErrUnsupported
}

func isLockContention(error) bool {
	return false
}
