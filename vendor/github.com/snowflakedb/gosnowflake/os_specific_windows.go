// go:build windows

package gosnowflake

import (
	"errors"
	"os"
)

func provideFileOwner(file *os.File) (uint32, error) {
	return 0, errors.New("provideFileOwner is unsupported on windows")
}
