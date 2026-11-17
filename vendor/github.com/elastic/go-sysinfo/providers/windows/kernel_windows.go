// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package windows

import (
	"os"
	"path/filepath"

	windows "github.com/elastic/go-windows"
	"golang.org/x/sys/windows/registry"
)

// fallbackSystemRoot is the last-resort default when the registry query and
// both environment variables are unavailable.
const fallbackSystemRoot = `C:\Windows`

// systemRootFromRegistry reads the SystemRoot value from
// HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion, which reflects the
// actual Windows directory regardless of the process environment. Returns ""
// on any error so the caller can fall back gracefully.
func systemRootFromRegistry() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`,
		registry.READ|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer k.Close()
	val, _, err := k.GetStringValue("SystemRoot")
	if err != nil {
		return ""
	}
	return val
}

// kernelExePath returns the absolute path to the running kernel image.
// It prefers the registry (immune to a stripped process environment), then
// falls back to %SystemRoot% / %WINDIR%, then to the hardcoded default.
// See #287.
func kernelExePath() string {
	root := systemRootFromRegistry()
	if root == "" {
		root = os.Getenv("SystemRoot")
	}
	if root == "" {
		root = os.Getenv("WINDIR")
	}
	if root == "" {
		root = fallbackSystemRoot
	}
	return filepath.Join(root, "System32", "ntoskrnl.exe")
}

func KernelVersion() (string, error) {
	versionData, err := windows.GetFileVersionInfo(kernelExePath())
	if err != nil {
		return "", err
	}

	fileVersion, err := versionData.QueryValue("FileVersion")
	if err == nil {
		return fileVersion, nil
	}

	// Make a second attempt through the fixed version info.
	info, err := versionData.FixedFileInfo()
	if err != nil {
		return "", err
	}
	return info.ProductVersion(), nil
}
