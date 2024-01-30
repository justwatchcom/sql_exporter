// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package instance

import (
	"fmt"
	"regexp"

	"cloud.google.com/go/cloudsqlconn/errtype"
)

var (
	// Instance connection name is the format <PROJECT>:<REGION>:<INSTANCE>
	// Additionally, we have to support legacy "domain-scoped" projects
	// (e.g. "google.com:PROJECT")
	connNameRegex = regexp.MustCompile("([^:]+(:[^:]+)?):([^:]+):([^:]+)")
)

// ConnName represents the "instance connection name", in the format
// "project:region:name".
type ConnName struct {
	project string
	region  string
	name    string
}

func (c *ConnName) String() string {
	return fmt.Sprintf("%s:%s:%s", c.project, c.region, c.name)
}

// Project returns the project within which the Cloud SQL instance runs.
func (c *ConnName) Project() string {
	return c.project
}

// Region returns the region where the Cloud SQL instance runs.
func (c *ConnName) Region() string {
	return c.region
}

// Name returns the Cloud SQL instance name
func (c *ConnName) Name() string {
	return c.name
}

// ParseConnName initializes a new ConnName struct.
func ParseConnName(cn string) (ConnName, error) {
	b := []byte(cn)
	m := connNameRegex.FindSubmatch(b)
	if m == nil {
		err := errtype.NewConfigError(
			"invalid instance connection name, expected PROJECT:REGION:INSTANCE",
			cn,
		)
		return ConnName{}, err
	}

	c := ConnName{
		project: string(m[1]),
		region:  string(m[3]),
		name:    string(m[4]),
	}
	return c, nil
}
