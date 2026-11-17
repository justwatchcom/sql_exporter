// Copyright 2020 Google LLC
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
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/cloudsqlconn/debug"
	"cloud.google.com/go/cloudsqlconn/errtype"
	"cloud.google.com/go/cloudsqlconn/instance"
	"github.com/jackc/pgx/v5/pgproto3"
	"golang.org/x/time/rate"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"
)

const (
	// the refresh bufferPool is the amount of time before a refresh operation's
	// certificate expires that a new refresh operation begins.
	refreshBuffer = 4 * time.Minute

	// refreshInterval is the amount of time between refresh attempts as
	// enforced by the rate limiter.
	refreshInterval = 30 * time.Second

	// RefreshTimeout is the maximum amount of time to wait for a refresh
	// cycle to complete. This value should be greater than the
	// refreshInterval.
	RefreshTimeout = 60 * time.Second

	// FailoverPeriod is the frequency with which the dialer will check
	// if the DNS record has changed for connections configured using
	// a DNS name.
	FailoverPeriod = 30 * time.Second

	// refreshBurst is the initial burst allowed by the rate limiter.
	refreshBurst = 2
)

// refreshOperation is a pending result of a refresh operation of data used to
// connect securely. It should only be initialized by the Instance struct as
// part of a refresh cycle.
type refreshOperation struct {
	// indicates the struct is ready to read from
	ready chan struct{}
	// timer that triggers refresh, can be used to cancel.
	timer  *time.Timer
	result ConnectionInfo
	err    error
}

// cancel prevents the the refresh operation from starting, if it hasn't
// already started. Returns true if timer was stopped successfully, or false if
// it has already started.
func (r *refreshOperation) cancel() bool {
	return r.timer.Stop()
}

// isValid returns true if this result is complete, successful, and is still
// valid.
func (r *refreshOperation) isValid() bool {
	// verify the refreshOperation has finished running
	select {
	default:
		return false
	case <-r.ready:
		if r.err != nil || time.Now().After(r.result.Expiration.Round(0)) {
			return false
		}
		return true
	}
}

// RefreshAheadCache manages the information used to connect to the Cloud SQL
// instance by periodically calling the Cloud SQL Admin API. It automatically
// refreshes the required information approximately 4 minutes before the
// previous certificate expires (every ~56 minutes).
type RefreshAheadCache struct {
	// openConns is the number of open connections to the instance.
	openConns uint64

	connName instance.ConnName
	logger   debug.ContextLogger

	// refreshTimeout sets the maximum duration a refresh cycle can run
	// for.
	refreshTimeout time.Duration
	// l controls the rate at which refresh cycles are run.
	l *rate.Limiter
	r adminAPIClient

	mu              sync.RWMutex
	useIAMAuthNDial bool
	dialFunc        func(ctx context.Context, ci ConnectionInfo, ipType string) (net.Conn, error)
	iamPrincipals   []pgPrincipal
	// cur represents the current refreshOperation that will be used to
	// create connections. If a valid complete refreshOperation isn't
	// available it's possible for cur to be equal to next.
	cur *refreshOperation
	// next represents a future or ongoing refreshOperation. Once complete,
	// it will replace cur and schedule a replacement to occur.
	next *refreshOperation

	// ctx is the default ctx for refresh operations. Canceling it prevents
	// new refresh operations from being triggered.
	ctx    context.Context
	cancel context.CancelFunc
}

type pgPrincipal struct {
	user     string
	database string
	ipType   string
}

func appendIAMPrincipal(principals []pgPrincipal, user, database, ipType string) []pgPrincipal {
	if user == "" {
		return principals
	}
	if database == "" {
		database = user
	}
	principal := pgPrincipal{user: user, database: database, ipType: ipType}
	for _, existing := range principals {
		if existing == principal {
			return principals
		}
	}
	return append(principals, principal)
}

// RecordIAMPrincipal records a PostgreSQL (user, database, ipType) tuple
// observed on an active Auto-IAM connection so that subsequent background
// refresh probes can send a StartupMessage over the same IP preference and
// update PgBouncer's cached pool credentials.
func (i *RefreshAheadCache) RecordIAMPrincipal(user, database, ipType string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.iamPrincipals = appendIAMPrincipal(i.iamPrincipals, user, database, ipType)
}

// NewRefreshAheadCache initializes a new Instance given an instance connection name
func NewRefreshAheadCache(
	cn instance.ConnName,
	l debug.ContextLogger,
	client *sqladmin.Service,
	key *rsa.PrivateKey,
	refreshTimeout time.Duration,
	tp auth.TokenProvider,
	dialerID string,
	useIAMAuthNDial bool,
	dialFunc func(ctx context.Context, ci ConnectionInfo, ipType string) (net.Conn, error),
) *RefreshAheadCache {
	ctx, cancel := context.WithCancel(context.Background())
	i := &RefreshAheadCache{
		connName: cn,
		logger:   l,
		l:        rate.NewLimiter(rate.Every(refreshInterval), refreshBurst),
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
		ctx:             ctx,
		cancel:          cancel,
	}
	// For the initial refresh operation, set cur = next so that connection
	// requests block until the first refresh is complete.
	i.mu.Lock()
	i.cur = i.scheduleRefresh(0)
	i.next = i.cur
	i.mu.Unlock()
	return i
}

// Close closes the instance; it stops the refresh cycle and prevents it from
// making additional calls to the Cloud SQL Admin API.
func (i *RefreshAheadCache) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.cancel()
	i.cur.cancel()
	i.next.cancel()
	return nil
}

// ConnectionInfo contains all necessary information to connect securely to the
// server-side Proxy running on a Cloud SQL instance.
type ConnectionInfo struct {
	ConnectionName    instance.ConnName
	ClientCertificate tls.Certificate
	ServerCACert      []*x509.Certificate
	ServerCAMode      string
	DBVersion         string
	// The DNSName is from the ConnectSettings API.
	// It is used to validate the server identity of the CAS instances.
	DNSName    string
	Expiration time.Time
	// Features of the MDX protocol supported by this database
	MdxProtocolSupport []string

	addrs map[string][]string
}

// NewConnectionInfo initializes a ConnectionInfo struct.
func NewConnectionInfo(
	cn instance.ConnName,
	dnsName string,
	serverCAMode string,
	version string,
	ipAddrs map[string][]string,
	serverCACert []*x509.Certificate,
	clientCert tls.Certificate,
	mdxProtocolSupport []string,
) ConnectionInfo {
	return ConnectionInfo{
		addrs:              ipAddrs,
		ClientCertificate:  clientCert,
		ServerCACert:       serverCACert,
		ServerCAMode:       serverCAMode,
		Expiration:         clientCert.Leaf.NotAfter,
		DBVersion:          version,
		ConnectionName:     cn,
		DNSName:            dnsName,
		MdxProtocolSupport: mdxProtocolSupport,
	}
}

// Addr returns the first IP address or DNS name for the given IP type.
func (c ConnectionInfo) Addr(ipType string) (string, error) {
	var (
		addrs []string
		ok    bool
	)
	switch ipType {
	case AutoIP:
		// Try Public first
		addrs, ok = c.addrs[PublicIP]
		if !ok || len(addrs) == 0 {
			// Try Private second
			addrs, ok = c.addrs[PrivateIP]
		}
	case SQLData:
		// Fall back to direct IP in preference order: PRIVATE, PSC, PUBLIC
		addrs, ok = c.addrs[PrivateIP]
		if !ok || len(addrs) == 0 {
			addrs, ok = c.addrs[PSC]
		}
		if !ok || len(addrs) == 0 {
			addrs, ok = c.addrs[PublicIP]
		}
	default:
		addrs, ok = c.addrs[ipType]
	}
	if !ok || len(addrs) == 0 {
		err := errtype.NewConfigError(
			fmt.Sprintf("instance does not have IP of type %q", ipType),
			c.ConnectionName.String(),
		)
		return "", err
	}
	return addrs[0], nil
}

// Addrs returns all IP addresses or DNS names for the given IP type.
func (c ConnectionInfo) Addrs(ipType string) ([]string, error) {
	addrs, _, err := c.ResolveAddrs(ipType)
	return addrs, err
}

// ResolveAddrs returns the IP addresses (or DNS names) to dial for the given
// IP type, along with the concrete IP type that was selected. For the
// strategy values AutoIP and SQLData the returned type is the type actually
// chosen (e.g. PublicIP or PrivateIP), which is what should be reported in
// telemetry; for concrete types it is ipType itself.
func (c ConnectionInfo) ResolveAddrs(ipType string) ([]string, string, error) {
	var (
		addrs    []string
		ok       bool
		resolved = ipType
	)
	// lookup returns the first candidate type that has at least one address.
	lookup := func(candidates ...string) {
		for _, t := range candidates {
			addrs, ok = c.addrs[t]
			if ok && len(addrs) > 0 {
				resolved = t
				return
			}
		}
		ok = false
	}
	switch ipType {
	case AutoIP:
		// Try Public first, then Private.
		lookup(PublicIP, PrivateIP)
	case SQLData:
		// Fall back to direct IP in preference order: PRIVATE, PSC, PUBLIC.
		lookup(PrivateIP, PSC, PublicIP)
	default:
		lookup(ipType)
	}
	if !ok || len(addrs) == 0 {
		err := errtype.NewConfigError(
			fmt.Sprintf("instance does not have IP of type %q", ipType),
			c.ConnectionName.String(),
		)
		return nil, "", err
	}
	cp := make([]string, len(addrs))
	copy(cp, addrs)
	return cp, resolved, nil
}

// TLSConfig constructs a ClientProtocolTLS configuration for the given connection info.
func (c ConnectionInfo) TLSConfig() *tls.Config {
	pool := x509.NewCertPool()
	for _, caCert := range c.ServerCACert {
		pool.AddCert(caCert)
	}

	var serverName string
	if c.ConnectionName.HasDomainName() {
		// If the connector was configured with a DNS name, use the DNS name from
		// the configuration to validate the server certificate.
		serverName = c.ConnectionName.DomainName()
	} else {
		// If the connector was configured with an Instance Connection Name,
		// use the DNS name from the instance metadata.
		serverName = c.DNSName
	}

	return &tls.Config{
		ServerName:   serverName,
		Certificates: []tls.Certificate{c.ClientCertificate},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
		// Replace entire default TLS verification with our custom TLS
		// verification defined in verifyPeerCertificateFunc(). This allows the
		// connector to gracefully and securely handle deviations from standard TLS
		// hostname validation in some existing Cloud SQL certificates.
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: verifyPeerCertificateFunc(serverName, c.ConnectionName, pool),
	}
}

// ConnectionInfo returns an IP address specified by ipType (i.e., public or
// private) and a ClientProtocolTLS config that can be used to connect to a Cloud SQL
// instance.
func (i *RefreshAheadCache) ConnectionInfo(ctx context.Context) (ConnectionInfo, error) {
	op, err := i.refreshOperation(ctx)
	if err != nil {
		return ConnectionInfo{}, err
	}
	return op.result, nil
}

// UpdateRefresh cancels all existing refresh attempts and schedules new
// attempts with the provided config only if it differs from the current
// configuration.
func (i *RefreshAheadCache) UpdateRefresh(useIAMAuthNDial *bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if useIAMAuthNDial != nil && *useIAMAuthNDial != i.useIAMAuthNDial {
		// Cancel any pending refreshes
		i.cur.cancel()
		i.next.cancel()

		i.useIAMAuthNDial = *useIAMAuthNDial
		// reschedule a new refresh immediately
		i.cur = i.scheduleRefresh(0)
		i.next = i.cur
	}
}

// ForceRefresh triggers an immediate refresh operation to be scheduled and
// used for future connection attempts. Until the refresh completes, the
// existing connection info will be available for use if valid.
func (i *RefreshAheadCache) ForceRefresh() {
	i.mu.Lock()
	defer i.mu.Unlock()
	// If the next refresh hasn't started yet, we can cancel it and start an
	// immediate one
	if i.next.cancel() {
		i.next = i.scheduleRefresh(0)
	}
	// block all sequential connection attempts on the next refresh operation
	// if current is invalid
	if !i.cur.isValid() {
		i.cur = i.next
	}
}

// refreshOperation returns the most recent refresh operation
// waiting for it to complete if necessary
func (i *RefreshAheadCache) refreshOperation(ctx context.Context) (*refreshOperation, error) {
	i.mu.RLock()
	cur := i.cur
	i.mu.RUnlock()
	var err error
	select {
	case <-cur.ready:
		err = cur.err
	case <-ctx.Done():
		err = ctx.Err()
	case <-i.ctx.Done():
		err = i.ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return cur, nil
}

// refreshDuration returns the duration to wait before starting the next
// refresh. Usually that duration will be half of the time until certificate
// expiration.
func refreshDuration(now, certExpiry time.Time) time.Duration {
	d := certExpiry.Sub(now.Round(0))
	if d < time.Hour {
		// Something is wrong with the certificate, refresh now.
		if d < refreshBuffer {
			return 0
		}
		// Otherwise wait until 4 minutes before expiration for next
		// refresh cycle.
		return d - refreshBuffer
	}
	return d / 2
}

// scheduleRefresh schedules a refresh operation to be triggered after a given
// duration. The returned refreshOperation can be used to either Cancel or Wait
// for the operation's completion.
func (i *RefreshAheadCache) scheduleRefresh(d time.Duration) *refreshOperation {
	r := &refreshOperation{}
	r.ready = make(chan struct{})
	r.timer = time.AfterFunc(d, func() {
		// instance has been closed, don't schedule anything
		if err := i.ctx.Err(); err != nil {
			i.logger.Debugf(
				context.Background(),
				"[%v] Instance is closed, stopping refresh operations",
				i.connName.String(),
			)
			r.err = err
			close(r.ready)
			return
		}
		i.logger.Debugf(
			context.Background(),
			"[%v] Connection info refresh operation started",
			i.connName.String(),
		)

		ctx, cancel := context.WithTimeout(i.ctx, i.refreshTimeout)
		defer cancel()

		// avoid refreshing too often to try not to tax the SQL Admin
		// API quotas
		var useIAMAuthN bool
		err := i.l.Wait(ctx)
		if err != nil {
			r.err = errtype.NewDialError(
				"context was canceled or expired before refresh completed",
				i.connName.String(),
				nil,
			)
		} else {
			i.mu.RLock()
			useIAMAuthN = i.useIAMAuthNDial
			i.mu.RUnlock()
			r.result, r.err = i.r.ConnectionInfo(
				ctx, i.connName, useIAMAuthN,
			)
		}
		switch r.err {
		case nil:
			if useIAMAuthN {
				if probeErr := i.probeConnection(ctx, r.result); probeErr != nil {
					i.logger.Debugf(
						ctx,
						"[%v] Proactive IAM token refresh probe encountered error: %v",
						i.connName.String(),
						probeErr,
					)
				}
			}
			i.logger.Debugf(
				ctx,
				"[%v] Connection info refresh operation complete",
				i.connName.String(),
			)
			i.logger.Debugf(
				ctx,
				"[%v] Current certificate expiration = %v",
				i.connName.String(),
				r.result.Expiration.UTC().Format(time.RFC3339),
			)
		default:
			i.logger.Debugf(
				ctx,
				"[%v] Connection info refresh operation failed, err = %v",
				i.connName.String(),
				r.err,
			)
		}

		// Once the refresh is complete, update "current" with working
		// refreshOperation and schedule a new refresh
		i.mu.Lock()
		defer i.mu.Unlock()

		close(r.ready)

		// if failed, scheduled the next refresh immediately
		if r.err != nil {
			i.logger.Debugf(
				ctx,
				"[%v] Connection info refresh operation scheduled immediately",
				i.connName.String(),
			)
			i.next = i.scheduleRefresh(0)
			// If the latest refreshOperation is bad, avoid replacing the
			// used refreshOperation while it's still valid and potentially
			// able to provide successful connections. TODO: This
			// means that errors while the current refreshOperation is still
			// valid are suppressed. We should try to surface
			// errors in a more meaningful way.
			if !i.cur.isValid() {
				i.cur = r
			}
			return
		}

		// Update the current results, and schedule the next refresh in
		// the future
		i.cur = r
		t := refreshDuration(time.Now(), i.cur.result.Expiration)
		i.logger.Debugf(
			ctx,
			"[%v] Connection info refresh operation scheduled at %v (now + %v)",
			i.connName.String(),
			time.Now().Add(t).UTC().Format(time.RFC3339),
			t.Round(time.Minute),
		)
		i.next = i.scheduleRefresh(t)
	})
	return r
}

const (
	maxPgStartupPacketLen = 10000
	probeEOFDrainTimeout  = 500 * time.Millisecond
)

func (i *RefreshAheadCache) probeConnection(ctx context.Context, ci ConnectionInfo) error {
	if !strings.HasPrefix(ci.DBVersion, "POSTGRES") {
		return nil
	}
	i.mu.RLock()
	if len(i.iamPrincipals) == 0 {
		i.mu.RUnlock()
		return nil
	}
	principals := append([]pgPrincipal(nil), i.iamPrincipals...)
	i.mu.RUnlock()
	return probeInstanceConnection(
		ctx, ci, i.refreshTimeout, i.dialFunc, principals, i.logger,
	)
}

func probeInstanceConnection(
	ctx context.Context,
	ci ConnectionInfo,
	refreshTimeout time.Duration,
	dialFunc func(ctx context.Context, ci ConnectionInfo, ipType string) (net.Conn, error),
	principals []pgPrincipal,
	logger debug.ContextLogger,
) error {
	if dialFunc == nil || !strings.HasPrefix(ci.DBVersion, "POSTGRES") || len(principals) == 0 {
		return nil
	}
	if refreshTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, refreshTimeout)
		defer cancel()
	}

	var lastErr error
	for _, principal := range principals {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		if err := probeSinglePrincipal(ctx, ci, dialFunc, principal, logger); err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return lastErr
	}

	logger.Debugf(ctx, "[%v] Proactive IAM token refresh probe successful", ci.ConnectionName.String())
	return nil
}

func probeSinglePrincipal(
	ctx context.Context,
	ci ConnectionInfo,
	dialFunc func(ctx context.Context, ci ConnectionInfo, ipType string) (net.Conn, error),
	principal pgPrincipal,
	logger debug.ContextLogger,
) error {
	if principal.user == "" {
		return nil
	}
	logger.Debugf(ctx, "[%v] Probing IAM token refresh", ci.ConnectionName.String())
	conn, err := dialFunc(ctx, ci, principal.ipType)
	if err != nil {
		return fmt.Errorf("failed to dial probe connection: %w", err)
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	database := principal.database
	if database == "" {
		database = principal.user
	}
	frontend := pgproto3.NewFrontend(conn, conn)
	frontend.Send(&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersion30,
		Parameters: map[string]string{
			"user":     principal.user,
			"database": database,
		},
	})
	if err := frontend.Flush(); err == nil {
		// Read server response (AuthenticationOk / ErrorResponse) so that
		// PgBouncer completes auth_query and finish_set_pool, updating
		// pool->user_credentials->passwd in db->user_tree before disconnect.
		_, _ = frontend.Receive()
		frontend.Send(&pgproto3.Terminate{})
		_ = frontend.Flush()
		// Wait briefly for server EOF so PgBouncer terminates cleanly without a
		// server-side unexpected EOF warning, without letting a slow-to-FIN peer
		// exhaust the refresh budget for remaining principals.
		drainDeadline := time.Now().Add(probeEOFDrainTimeout)
		if dl, ok := ctx.Deadline(); ok && dl.Before(drainDeadline) {
			drainDeadline = dl
		}
		_ = conn.SetReadDeadline(drainDeadline)
		var discard [128]byte
		for {
			if _, err := conn.Read(discard[:]); err != nil {
				break
			}
		}
	}

	return nil
}

// NewPostgresConn wraps conn so that the first PostgreSQL v3
// StartupMessage written by the client is inspected to extract (user, database)
// and passed to onStartup. Once the startup message has been processed,
// subsequent writes pass straight through to conn without buffering overhead.
//
// Like io.Writer, Write is not safe for concurrent use by multiple goroutines;
// callers are responsible for synchronizing concurrent writes.
func NewPostgresConn(conn net.Conn, onStartup func(user, database string)) net.Conn {
	if onStartup == nil {
		return conn
	}
	return &postgresConn{
		Conn:      conn,
		onStartup: onStartup,
	}
}

type parserState int

const (
	stateReadLength parserState = iota
	stateReadBody
	stateDone
)

// postgresConn intercepts initial writes on a PostgreSQL connection to extract
// the user and database from the v3 StartupMessage. Consistent with io.Writer,
// callers must serialize concurrent Write calls.
type postgresConn struct {
	net.Conn
	startupMessageSent bool
	state              parserState
	expectedLen        int
	buf                []byte
	onStartup          func(user, database string)
}

func (p *postgresConn) Write(b []byte) (int, error) {
	if p.startupMessageSent {
		return p.Conn.Write(b)
	}

	if user, db := p.parseStream(b); user != "" {
		p.onStartup(user, db)
	}

	return p.Conn.Write(b)
}

// parseStream feeds incoming chunks into the state machine to identify
// the PostgreSQL v3.0/v3.2 StartupMessage and extract client parameters.
func (p *postgresConn) parseStream(b []byte) (user, database string) {
	p.buf = append(p.buf, b...)

	for {
		switch p.state {
		case stateReadLength:
			// Step 1: Wait for at least 4 bytes to determine total packet length.
			if len(p.buf) < 4 {
				return "", ""
			}
			p.expectedLen = int(binary.BigEndian.Uint32(p.buf[:4]))
			// PostgreSQL packets are at least 8 bytes (4 bytes length + 4 bytes code).
			// If length is malformed or exceeds maximum, stop inspection.
			if p.expectedLen < 8 || p.expectedLen > maxPgStartupPacketLen+8 {
				p.state = stateDone
				p.startupMessageSent = true
				p.buf = nil
				return "", ""
			}
			p.state = stateReadBody
			fallthrough

		case stateReadBody:
			// Step 2: Wait until the complete packet body has arrived.
			if len(p.buf) < p.expectedLen {
				return "", ""
			}
			pkt := p.buf[:p.expectedLen]
			code := binary.BigEndian.Uint32(pkt[4:8])

			switch code {
			case 80877103, 80877104: // SSLRequest or GSSEncRequest (always 8 bytes)
				// Step 3: SSL/GSS negotiation precedes the StartupMessage.
				// Discard this 8-byte request and loop back to read the next packet length.
				p.buf = p.buf[p.expectedLen:]
				p.state = stateReadLength
				continue

			case pgproto3.ProtocolVersion30, pgproto3.ProtocolVersion32:
				// Step 4: Decode v3.0/v3.2 StartupMessage (ProtocolVersion30 / ProtocolVersion32) and extract connection parameters.
				var sm pgproto3.StartupMessage
				if err := sm.Decode(pkt[4:p.expectedLen]); err == nil {
					user = sm.Parameters["user"]
					database = sm.Parameters["database"]
					if user != "" && database == "" {
						database = user
					}
				}
				p.state = stateDone
				p.startupMessageSent = true
				p.buf = nil
				return user, database

			default:
				// Step 5: Unknown protocol code or unexpected packet; stop parsing.
				p.state = stateDone
				p.startupMessageSent = true
				p.buf = nil
				return "", ""
			}

		case stateDone:
			p.buf = nil
			return "", ""
		}
	}
}
