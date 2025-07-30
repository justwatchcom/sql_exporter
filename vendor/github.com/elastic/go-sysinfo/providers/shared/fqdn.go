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

//go:build linux || darwin || aix

package shared

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// FQDN attempts to lookup the host's fully-qualified domain name and returns it.
// It does so using the following algorithm:
//
//  1. It gets the hostname from the OS. If this step fails, it returns an error.
//
//  2. It tries to perform a CNAME DNS lookup for the hostname. If this succeeds, it
//     returns the CNAME (after trimming any trailing period) as the FQDN.
//
//  3. It tries to perform an IP lookup for the hostname. If this succeeds, it tries
//     to perform a reverse DNS lookup on the returned IPs and returns the first
//     successful result (after trimming any trailing period) as the FQDN.
//
//  4. If steps 2 and 3 both fail, an empty string is returned as the FQDN along with
//     errors from those steps.
func FQDN() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("could not get hostname to look for FQDN: %w", err)
	}

	return fqdn(hostname)
}

func fqdn(hostname string) (string, error) {
	var errs error
	cname, err := net.LookupCNAME(hostname)
	if err != nil {
		errs = fmt.Errorf("could not get FQDN, all methods failed: failed looking up CNAME: %w",
			err)
	}
	if cname != "" {
		return strings.ToLower(strings.TrimSuffix(cname, ".")), nil
	}

	ips, err := net.LookupIP(hostname)
	if err != nil {
		errs = fmt.Errorf("%s: failed looking up IP: %w", errs, err)
	}

	for _, ip := range ips {
		names, err := net.LookupAddr(ip.String())
		if err != nil || len(names) == 0 {
			continue
		}
		return strings.ToLower(strings.TrimSuffix(names[0], ".")), nil
	}

	return "", errs
}
