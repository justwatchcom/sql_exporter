// Copyright (c) 2023 Snowflake Computing Inc. All rights reserved.

package gosnowflake

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
)

// log levels for easy logging
const (
	levelOff   string = "OFF"   // log level for logging switched off
	levelError string = "ERROR" // error log level
	levelWarn  string = "WARN"  // warn log level
	levelInfo  string = "INFO"  // info log level
	levelDebug string = "DEBUG" // debug log level
	levelTrace string = "TRACE" // trace log level
)

const (
	defaultConfigName = "sf_client_config.json"
	clientConfEnvName = "SF_CLIENT_CONFIG_FILE"
)

func getClientConfig(filePathFromConnectionString string) (*ClientConfig, error) {
	configPredefinedFilePaths := clientConfigPredefinedDirs()
	filePath := findClientConfigFilePath(filePathFromConnectionString, configPredefinedFilePaths)
	if filePath == "" { // we did not find a config file
		return nil, nil
	}
	return parseClientConfiguration(filePath)
}

func findClientConfigFilePath(filePathFromConnectionString string, configPredefinedDirs []string) string {
	if filePathFromConnectionString != "" {
		return filePathFromConnectionString
	}
	envConfigFilePath := os.Getenv(clientConfEnvName)
	if envConfigFilePath != "" {
		return envConfigFilePath
	}
	return searchForConfigFile(configPredefinedDirs)
}

func searchForConfigFile(directories []string) string {
	for _, dir := range directories {
		filePath := path.Join(dir, defaultConfigName)
		exists, err := existsFile(filePath)
		if err != nil {
			logger.Errorf("Error while searching for the client config in %s directory: %s", dir, err)
			continue
		}
		if exists {
			return filePath
		}
	}
	return ""
}

func existsFile(filePath string) (bool, error) {
	_, err := os.Stat(filePath)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func clientConfigPredefinedDirs() []string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		logger.Warnf("Home dir could not be determined: %w", err)
		return []string{".", os.TempDir()}
	}
	return []string{".", homeDir, os.TempDir()}
}

// ClientConfig config root
type ClientConfig struct {
	Common *ClientConfigCommonProps `json:"common"`
}

// ClientConfigCommonProps properties from "common" section
type ClientConfigCommonProps struct {
	LogLevel string `json:"log_level,omitempty"`
	LogPath  string `json:"log_path,omitempty"`
}

func parseClientConfiguration(filePath string) (*ClientConfig, error) {
	if filePath == "" {
		return nil, nil
	}
	fileContents, err := os.ReadFile(filePath)
	if err != nil {
		return nil, parsingClientConfigError(err)
	}
	var clientConfig ClientConfig
	err = json.Unmarshal(fileContents, &clientConfig)
	if err != nil {
		return nil, parsingClientConfigError(err)
	}
	err = validateClientConfiguration(&clientConfig)
	if err != nil {
		return nil, parsingClientConfigError(err)
	}
	return &clientConfig, nil
}

func parsingClientConfigError(err error) error {
	return fmt.Errorf("parsing client config failed: %w", err)
}

func validateClientConfiguration(clientConfig *ClientConfig) error {
	if clientConfig == nil {
		return errors.New("client config not found")
	}
	if clientConfig.Common == nil {
		return errors.New("common section in client config not found")
	}
	return validateLogLevel(*clientConfig)
}

func validateLogLevel(clientConfig ClientConfig) error {
	var logLevel = clientConfig.Common.LogLevel
	if logLevel != "" {
		_, error := toLogLevel(logLevel)
		if error != nil {
			return error
		}
	}
	return nil
}

func toLogLevel(logLevelString string) (string, error) {
	var logLevel = strings.ToUpper(logLevelString)
	switch logLevel {
	case levelOff, levelError, levelWarn, levelInfo, levelDebug, levelTrace:
		return logLevel, nil
	default:
		return "", errors.New("unknown log level: " + logLevelString)
	}
}
