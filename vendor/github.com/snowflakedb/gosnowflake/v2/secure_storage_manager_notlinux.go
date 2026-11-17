//go:build !linux

package gosnowflake

import (
	"runtime"
	"sync"

	"github.com/99designs/keyring"
)

const keyringServiceName = "snowflake_credential_cache"

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
	openKeyring func() (keyring.Keyring, error)
}

func newKeyringBasedSecureStorageManager() *keyringSecureStorageManager {
	return &keyringSecureStorageManager{openKeyring: openTokenKeyring}
}

func tokenKeyringConfig() keyring.Config {
	return keyring.Config{
		ServiceName:   keyringServiceName,
		WinCredPrefix: keyringServiceName,
	}
}

func openTokenKeyring() (keyring.Keyring, error) {
	return keyring.Open(tokenKeyringConfig())
}

func (ssm *keyringSecureStorageManager) setCredential(tokenSpec secureTokenSpec, value string) {
	if value == "" {
		logger.Debug("no token provided")
		return
	}
	credentialsKey, err := tokenSpec.buildKey()
	if err != nil {
		logger.Warnf("cannot build token spec: %v", err)
		return
	}
	ring, err := ssm.openKeyring()
	if err != nil {
		logger.Debugf("Failed to open credential store. Err: %v", err)
		return
	}
	item := keyring.Item{
		Key:  credentialsKey,
		Data: []byte(value),
	}
	if err := ring.Set(item); err != nil {
		logger.Debugf("Failed to write to credential store. Err: %v", err)
	}
}

func (ssm *keyringSecureStorageManager) getCredential(tokenSpec secureTokenSpec) string {
	credentialsKey, err := tokenSpec.buildKey()
	if err != nil {
		logger.Warnf("cannot build token spec: %v", err)
		return ""
	}
	ring, err := ssm.openKeyring()
	if err != nil {
		logger.Debugf("Failed to open credential store. Error: %v", err)
		return ""
	}
	i, err := ring.Get(credentialsKey)
	if err != nil {
		logger.Debugf("Failed to find the item in credential store or item does not exist. Error: %v", err)
	}
	cred := string(i.Data)
	if cred == "" {
		logger.Debug("Returned credential is empty")
	} else {
		logger.Debug("Successfully read token. Returning as string")
	}
	return cred
}

func (ssm *keyringSecureStorageManager) deleteCredential(tokenSpec secureTokenSpec) {
	credentialsKey, err := tokenSpec.buildKey()
	if err != nil {
		logger.Warnf("cannot build token spec: %v", err)
		return
	}
	ring, err := ssm.openKeyring()
	if err != nil {
		logger.Debugf("Failed to open credential store. Error: %v", err)
		return
	}
	err = ring.Remove(credentialsKey)
	if err != nil {
		logger.Debugf("Failed to delete credentialsKey in credential store. Error: %v", err)
	}
}
