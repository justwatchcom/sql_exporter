//go:build !linux

package gosnowflake

import (
	"github.com/99designs/keyring"
	"runtime"
	"strings"
	"sync"
)

func defaultOsSpecificSecureStorageManager() secureStorageManager {
	switch runtime.GOOS {
	case "darwin", "windows":
		logger.Debugf("OS is %v, using keyring based secure storage manager.", runtime.GOOS)
		return &threadSafeSecureStorageManager{&sync.Mutex{}, newKeyringBasedSecureStorageManager()}
	default:
		logger.Debugf("OS %v does not support credentials cache", runtime.GOOS)
		return newNoopSecureStorageManager()
	}
}

type keyringSecureStorageManager struct {
}

func newKeyringBasedSecureStorageManager() *keyringSecureStorageManager {
	return &keyringSecureStorageManager{}
}

func (ssm *keyringSecureStorageManager) setCredential(tokenSpec *secureTokenSpec, value string) {
	if value == "" {
		logger.Debug("no token provided")
	} else {
		credentialsKey, err := tokenSpec.buildKey()
		if err != nil {
			logger.Warn(err)
			return
		}
		switch runtime.GOOS {
		case "windows":
			ring, _ := keyring.Open(keyring.Config{
				WinCredPrefix: strings.ToUpper(tokenSpec.host),
				ServiceName:   strings.ToUpper(tokenSpec.user),
			})
			item := keyring.Item{
				Key:  credentialsKey,
				Data: []byte(value),
			}
			if err := ring.Set(item); err != nil {
				logger.Debugf("Failed to write to Windows credential manager. Err: %v", err)
			}
		case "darwin":
			ring, _ := keyring.Open(keyring.Config{
				ServiceName: credentialsKey,
			})
			account := strings.ToUpper(tokenSpec.user)
			item := keyring.Item{
				Key:  account,
				Data: []byte(value),
			}
			if err := ring.Set(item); err != nil {
				logger.Debugf("Failed to write to keychain. Err: %v", err)
			}
		}
	}
}

func (ssm *keyringSecureStorageManager) getCredential(tokenSpec *secureTokenSpec) string {
	cred := ""
	credentialsKey, err := tokenSpec.buildKey()
	if err != nil {
		logger.Warn(err)
		return ""
	}
	switch runtime.GOOS {
	case "windows":
		ring, _ := keyring.Open(keyring.Config{
			WinCredPrefix: strings.ToUpper(tokenSpec.host),
			ServiceName:   strings.ToUpper(tokenSpec.user),
		})
		i, err := ring.Get(credentialsKey)
		if err != nil {
			logger.Debugf("Failed to read credentialsKey or could not find it in Windows Credential Manager. Error: %v", err)
		}
		cred = string(i.Data)
	case "darwin":
		ring, _ := keyring.Open(keyring.Config{
			ServiceName: credentialsKey,
		})
		account := strings.ToUpper(tokenSpec.user)
		i, err := ring.Get(account)
		if err != nil {
			logger.Debugf("Failed to find the item in keychain or item does not exist. Error: %v", err)
		}
		cred = string(i.Data)
		if cred == "" {
			logger.Debug("Returned credential is empty")
		} else {
			logger.Debug("Successfully read token. Returning as string")
		}
	}
	return cred
}

func (ssm *keyringSecureStorageManager) deleteCredential(tokenSpec *secureTokenSpec) {
	credentialsKey, err := tokenSpec.buildKey()
	if err != nil {
		logger.Warn(err)
		return
	}
	switch runtime.GOOS {
	case "windows":
		ring, _ := keyring.Open(keyring.Config{
			WinCredPrefix: strings.ToUpper(tokenSpec.host),
			ServiceName:   strings.ToUpper(tokenSpec.user),
		})
		err := ring.Remove(string(credentialsKey))
		if err != nil {
			logger.Debugf("Failed to delete credentialsKey in Windows Credential Manager. Error: %v", err)
		}
	case "darwin":
		ring, _ := keyring.Open(keyring.Config{
			ServiceName: credentialsKey,
		})
		account := strings.ToUpper(tokenSpec.user)
		err := ring.Remove(account)
		if err != nil {
			logger.Debugf("Failed to delete credentialsKey in keychain. Error: %v", err)
		}
	}
}
