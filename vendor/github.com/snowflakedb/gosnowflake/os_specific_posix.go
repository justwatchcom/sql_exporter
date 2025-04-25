//go:build darwin || linux

package gosnowflake

import (
	"fmt"
	"os"
	"syscall"
)

func provideFileOwner(file *os.File) (uint32, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	return provideOwnerFromStat(info, file.Name())
}

func provideOwnerFromStat(info os.FileInfo, filepath string) (uint32, error) {
	nativeStat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("cannot cast file info for %v to *syscall.Stat_t", filepath)
	}
	return nativeStat.Uid, nil
}
