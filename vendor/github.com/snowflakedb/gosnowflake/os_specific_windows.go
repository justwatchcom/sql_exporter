// go:build windows

package gosnowflake

import (
	"errors"
	"os"
)

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
