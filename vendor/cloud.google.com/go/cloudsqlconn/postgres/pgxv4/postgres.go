// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package pgxv4 provides a Cloud SQL Postgres driver that uses pgx v4 and works
// with the database/sql package.
//
// Deprecated: pgxv4 is no longer maintained because pgproto3 has reached end-of-life.
// This package has been rewired to use pgxv5 internally for security reasons.
// Please use pgxv5 directly instead.
package pgxv4

import (
	"cloud.google.com/go/cloudsqlconn"
	"cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
)

// RegisterDriver registers a Postgres driver that uses the cloudsqlconn.Dialer
// configured with the provided options. The choice of name is entirely up to
// the caller and may be used to distinguish between multiple registrations of
// differently configured Dialers. The driver now uses pgx/v5 internally.
// RegisterDriver returns a cleanup function that should be called one the
// database connection is no longer needed.
//
// Deprecated: pgxv4 is no longer maintained because pgproto3 has reached end-of-life.
// This function now uses pgxv5 internally for security reasons.
// Please use pgxv5.RegisterDriver instead.
func RegisterDriver(name string, opts ...cloudsqlconn.Option) (func() error, error) {
	return pgxv5.RegisterDriver(name, opts...)
}
