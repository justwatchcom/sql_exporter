// Copyright 2024 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cloudsql

import (
	"context"
	"crypto/rsa"
	"net"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/cloudsqlconn/debug"
	"cloud.google.com/go/cloudsqlconn/instance"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"
)

// LazyRefreshCache caches connection info and refreshes the cache only when
// a caller requests connection info and the current certificate is expired.
type LazyRefreshCache struct {
	connName        instance.ConnName
	logger          debug.ContextLogger
	r               adminAPIClient
	refreshTimeout  time.Duration
	mu              sync.Mutex
	useIAMAuthNDial bool
	dialFunc        func(ctx context.Context, ci ConnectionInfo, ipType string) (net.Conn, error)
	iamPrincipals   []pgPrincipal
	needsRefresh    bool
	cached          ConnectionInfo
}

// NewLazyRefreshCache initializes a new LazyRefreshCache.
func NewLazyRefreshCache(
	cn instance.ConnName,
	l debug.ContextLogger,
	client *sqladmin.Service,
	key *rsa.PrivateKey,
	refreshTimeout time.Duration,
	tp auth.TokenProvider,
	dialerID string,
	useIAMAuthNDial bool,
	dialFunc func(ctx context.Context, ci ConnectionInfo, ipType string) (net.Conn, error),
) *LazyRefreshCache {
	return &LazyRefreshCache{
		connName: cn,
		logger:   l,
		r: newAdminAPIClient(
			l,
			client,
			key,
			tp,
			dialerID,
		),
		refreshTimeout:  refreshTimeout,
		useIAMAuthNDial: useIAMAuthNDial,
		dialFunc:        dialFunc,
	}
}

// RecordIAMPrincipal records a PostgreSQL (user, database, ipType) tuple
// observed on an active Auto-IAM connection so that subsequent refresh probes
// can send a StartupMessage over the same IP preference and update PgBouncer's
// cached pool credentials.
func (c *LazyRefreshCache) RecordIAMPrincipal(user, database, ipType string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.iamPrincipals = appendIAMPrincipal(c.iamPrincipals, user, database, ipType)
}

// ConnectionInfo returns connection info for the associated instance. New
// connection info is retrieved under two conditions:
// - the current connection info's certificate has expired, or
// - a caller has separately called ForceRefresh
func (c *LazyRefreshCache) ConnectionInfo(
	ctx context.Context,
) (ConnectionInfo, error) {
	c.mu.Lock()
	// strip monotonic clock with UTC()
	now := time.Now().UTC()
	// Pad expiration with a bufferPool to give the client plenty of time to
	// establish a connection to the server with the certificate.
	exp := c.cached.Expiration.UTC().Add(-refreshBuffer)
	if !c.needsRefresh && now.Before(exp) {
		ci := c.cached
		c.mu.Unlock()
		c.logger.Debugf(
			ctx,
			"[%v] Connection info is still valid, using cached info",
			c.connName.String(),
		)
		return ci, nil
	}

	c.logger.Debugf(
		ctx,
		"[%v] Connection info refresh operation started",
		c.connName.String(),
	)
	ci, err := c.r.ConnectionInfo(ctx, c.connName, c.useIAMAuthNDial)
	if err != nil {
		c.mu.Unlock()
		c.logger.Debugf(
			ctx,
			"[%v] Connection info refresh operation failed, err = %v",
			c.connName.String(),
			err,
		)
		return ConnectionInfo{}, err
	}
	c.cached = ci
	c.needsRefresh = false
	var principals []pgPrincipal
	if c.useIAMAuthNDial && len(c.iamPrincipals) > 0 && strings.HasPrefix(ci.DBVersion, "POSTGRES") {
		principals = append([]pgPrincipal(nil), c.iamPrincipals...)
	}
	dialFunc := c.dialFunc
	refreshTimeout := c.refreshTimeout
	c.mu.Unlock()

	c.logger.Debugf(
		ctx,
		"[%v] Connection info refresh operation complete",
		c.connName.String(),
	)
	c.logger.Debugf(
		ctx,
		"[%v] Current certificate expiration = %v",
		c.connName.String(),
		ci.Expiration.UTC().Format(time.RFC3339),
	)

	if len(principals) > 0 {
		if probeErr := probeInstanceConnection(
			ctx, ci, refreshTimeout, dialFunc, principals, c.logger,
		); probeErr != nil {
			c.logger.Debugf(
				ctx,
				"[%v] Proactive IAM token refresh probe encountered error: %v",
				c.connName.String(),
				probeErr,
			)
		}
	}
	return ci, nil
}

// UpdateRefresh updates the refresh operation to either enable or disable IAM
// authentication for the cached connection info.
func (c *LazyRefreshCache) UpdateRefresh(useIAMAuthNDial *bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if useIAMAuthNDial != nil && *useIAMAuthNDial != c.useIAMAuthNDial {
		c.useIAMAuthNDial = *useIAMAuthNDial
		c.needsRefresh = true
	}
}

// ForceRefresh invalidates the caches and configures the next call to
// ConnectionInfo to retrieve a fresh connection info.
func (c *LazyRefreshCache) ForceRefresh() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.needsRefresh = true
}

// Close is a no-op and provided purely for a consistent interface with other
// caching types.
func (c *LazyRefreshCache) Close() error {
	return nil
}
