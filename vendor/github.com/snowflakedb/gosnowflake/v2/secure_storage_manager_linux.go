//go:build linux

package gosnowflake

import (
	"runtime"
	"sync"
)

func defaultOsSpecificSecureStorageManager() secureStorageManager {
	logger.Debugf("OS is %v, using file based secure storage manager.", runtime.GOOS)
	ssm, err := newFileBasedSecureStorageManager()
	if err != nil {
		logger.Debugf("failed to create credentials cache dir: %v. Not storing credentials locally.", err)
		return newNoopSecureStorageManager()
	}
	return &threadSafeSecureStorageManager{&sync.Mutex{}, ssm}
}
