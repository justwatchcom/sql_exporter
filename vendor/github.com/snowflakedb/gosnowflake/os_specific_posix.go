//go:build !windows

package gosnowflake

import (
	"fmt"
	"io"
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

func getFileContents(filePath string, expectedPerm os.FileMode) ([]byte, error) {
	// open the file with read only and no symlink flags
	file, err := os.OpenFile(filePath, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// validate file permissions and owner
	if err = validateFilePermissionBits(file, expectedPerm); err != nil {
		return nil, err
	}
	if err = ensureFileOwner(file); err != nil {
		return nil, err
	}

	// read the file
	fileContents, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}

	return fileContents, nil
}

func validateFilePermissionBits(f *os.File, expectedPerm os.FileMode) error {
	fileInfo, err := f.Stat()
	if err != nil {
		return err
	}
	filePerm := fileInfo.Mode()
	if filePerm&expectedPerm != 0 {
		return fmt.Errorf("incorrect permissions of %s", f.Name())
	}
	return nil
}
