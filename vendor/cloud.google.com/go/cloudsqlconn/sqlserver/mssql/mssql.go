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

// Package mssql provides a Cloud SQL SQL Server driver that works with the
// database/sql package.
package mssql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net"

	"cloud.google.com/go/cloudsqlconn"
	mssqldb "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// RegisterDriver registers a SQL Server driver that uses the
// cloudsqlconn.Dialer configured with the provided options. The choice of name
// is entirely up to the caller and may be used to distinguish between multiple
// registrations of differently configured Dialers.
func RegisterDriver(name string, opts ...cloudsqlconn.Option) (func() error, error) {
	d, err := cloudsqlconn.NewDialer(context.Background(), opts...)
	if err != nil {
		return func() error { return nil }, err
	}
	sql.Register(name, &sqlserverDriver{
		d: d,
	})
	return func() error { return d.Close() }, nil
}

type csqlDialer struct {
	driver.Conn

	d        *cloudsqlconn.Dialer
	connName string
}

// DialContext adheres to the mssql.Dialer interface.
func (c *csqlDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	return c.d.Dial(ctx, c.connName)
}

// Close ensures the cloudsqlconn.Dialer is closed before the conncetion is
// closed.
func (c *csqlDialer) Close() error {
	c.d.Close()
	return c.Conn.Close()
}

type sqlserverDriver struct {
	d *cloudsqlconn.Dialer
}

// Open accepts a URL, ADO, or ODBC style connection string and returns a
// connection to the database using cloudsqlconn.Dialer. The Cloud SQL instance
// connection name should be specified in a "cloudsql" parameter. For example:
//
// "sqlserver://user:password@localhost?database=mydb&cloudsql=my-proj:us-central1:my-inst"
//
// For details, see
// https://github.com/microsoft/go-mssqldb#the-connection-string-can-be-specified-in-one-of-three-formats
func (s *sqlserverDriver) Open(name string) (driver.Conn, error) {
	res, err := msdsn.Parse(name)
	if err != nil {
		return nil, err
	}
	c, err := mssqldb.NewConnector(name)
	if err != nil {
		return nil, err
	}
	connName := res.Parameters["cloudsql"]
	c.Dialer = &csqlDialer{
		d:        s.d,
		connName: connName,
	}
	return c.Connect(context.Background())
}
