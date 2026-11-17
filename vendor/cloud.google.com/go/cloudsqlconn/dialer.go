// Copyright 2020 Google LLC
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

package cloudsqlconn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/auth/httptransport"
	"cloud.google.com/go/cloudsqlconn/debug"
	"cloud.google.com/go/cloudsqlconn/errtype"
	"cloud.google.com/go/cloudsqlconn/instance"
	"cloud.google.com/go/cloudsqlconn/internal/cloudsql"
	"cloud.google.com/go/cloudsqlconn/internal/mdx"
	"cloud.google.com/go/cloudsqlconn/internal/sqldataclient"
	"cloud.google.com/go/cloudsqlconn/internal/trace"
	"cloud.google.com/go/compute/metadata"
	"github.com/google/uuid"
	"golang.org/x/net/proxy"
	"google.golang.org/api/option"

	tel "cloud.google.com/go/cloudsqlconn/internal/tel"
	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// defaultTCPKeepAlive is the default keep alive value used on connections to a Cloud SQL instance.
	defaultTCPKeepAlive = 30 * time.Second
	// metricShutdownTimeout bounds how long Dialer.Close waits for the
	// built-in metrics exporters to flush.
	metricShutdownTimeout = 3 * time.Second
	// serverProxyPort is the port the server-side proxy receives connections on.
	serverProxyPort = "3307"
	// iamLoginScope is the OAuth2 scope used for tokens embedded in the ephemeral
	// certificate.
	iamLoginScope = "https://www.googleapis.com/auth/sqlservice.login"
	// monitoringWriteScope is the OAuth2 scope required to write time series
	// to Cloud Monitoring. It is used for the built-in metrics export.
	monitoringWriteScope = tel.MonitoringWriteScope
	// universeDomainEnvVar is the environment variable for setting the default
	// service domain for a given Cloud universe.
	universeDomainEnvVar = "GOOGLE_CLOUD_UNIVERSE_DOMAIN"
	// defaultUniverseDomain is the default value for universe domain.
	// Universe domain is the default service domain for a given Cloud universe.
	defaultUniverseDomain = "googleapis.com"
)

var (
	// ErrDialerClosed is used when a caller invokes Dial after closing the
	// Dialer.
	ErrDialerClosed = errors.New("cloudsqlconn: dialer is closed")
	// versionString indicates the version of this library.
	//go:embed version.txt
	versionString string
	userAgent     = "cloud-sql-go-connector/" + strings.TrimSpace(versionString)

	// builtInMetricsDefault is the default for WithBuiltInMetrics. Built-in
	// metrics are on by default; this is a var so the unit tests can turn
	// them off process-wide without touching the real Cloud Monitoring API.
	builtInMetricsDefault = true
)

// keyGenerator encapsulates the details of RSA key generation to provide lazy
// generation, custom keys, or a default RSA generator.
type keyGenerator struct {
	once    sync.Once
	key     *rsa.PrivateKey
	err     error
	genFunc func() (*rsa.PrivateKey, error)
}

// newKeyGenerator initializes a keyGenerator that will (in order):
// - always return the RSA key if one is provided, or
// - generate an RSA key lazily when it's requested, or
// - (default) immediately generate an RSA key as part of the initializer.
func newKeyGenerator(
	k *rsa.PrivateKey, lazy bool, genFunc func() (*rsa.PrivateKey, error),
) (*keyGenerator, error) {
	g := &keyGenerator{genFunc: genFunc}
	switch {
	case k != nil:
		// If the caller has provided a key, initialize the key and consume the
		// sync.Once now.
		g.once.Do(func() { g.key, g.err = k, nil })
	case lazy:
		// If lazy refresh is enabled, do nothing and wait for the call to
		// rsaKey.
	default:
		// If no key has been provided and lazy refresh isn't enabled, generate
		// the key and consume the sync.Once now.
		g.once.Do(func() { g.key, g.err = g.genFunc() })
	}
	return g, g.err
}

// rsaKey will generate an RSA key if one is not already cached. Otherwise, it
// will return the cached key.
func (g *keyGenerator) rsaKey() (*rsa.PrivateKey, error) {
	g.once.Do(func() { g.key, g.err = g.genFunc() })

	return g.key, g.err
}

type connectionInfoCache interface {
	ConnectionInfo(context.Context) (cloudsql.ConnectionInfo, error)
	UpdateRefresh(*bool)
	ForceRefresh()
	RecordIAMPrincipal(user, database, ipType string)
	io.Closer
}

type cacheKey struct {
	domainName string
	project    string
	region     string
	name       string
}

// getClientUniverseDomain returns the default service domain for a given Cloud
// universe, with the following precedence:
//
// 1. A non-empty option.WithUniverseDomain or similar client option.
// 2. A non-empty environment variable GOOGLE_CLOUD_UNIVERSE_DOMAIN.
// 3. The default value "googleapis.com".
//
// This is the universe domain configured for the client, which will be compared
// to the universe domain that is separately configured for the credentials.
func (c *dialerConfig) getClientUniverseDomain() string {
	if c.clientUniverseDomain != "" {
		return c.clientUniverseDomain
	}
	if envUD := os.Getenv(universeDomainEnvVar); envUD != "" {
		return envUD
	}
	return defaultUniverseDomain
}

type sqlDataConnState struct {
	mu             sync.Mutex
	allowed        bool
	cooldownUntil  time.Time
	lastErr        error
	backoffCounter int
}

// A Dialer is used to create connections to Cloud SQL instances.
//
// Use NewDialer to initialize a Dialer.
type Dialer struct {
	lock             sync.RWMutex
	cache            map[cacheKey]*monitoredCache
	sqlDataConnState map[cacheKey]*sqlDataConnState
	keyGenerator     *keyGenerator
	refreshTimeout   time.Duration
	// closing is set by the first call to Close, which then closes closed.
	closing atomic.Bool
	// closed reports if the dialer has been closed.
	closed chan struct{}

	sqladmin *sqladmin.Service
	logger   debug.ContextLogger

	// lazyRefresh determines what kind of caching is used for ephemeral
	// certificates. When lazyRefresh is true, the dialer will use a lazy
	// cache, refresh certificates only when a connection attempt needs a fresh
	// certificate. Otherwise, a refresh ahead cache will be used. The refresh
	// ahead cache assumes a background goroutine may run consistently.
	lazyRefresh bool

	// defaultDialConfig holds the constructor level DialOptions, so that it
	// can be copied and mutated by the Dial function.
	defaultDialConfig dialConfig

	// dialerID uniquely identifies a Dialer. It is generated once per Dialer
	// and used for monitoring purposes: as the dialer_id tag on the
	// OpenCensus metrics (when a client has configured exporters) and as
	// the client_uid label of the built-in metrics. Each Dialer owns its
	// own metric counters and export loop, so the ID must be unique per
	// Dialer: two Dialers in one process sharing an ID would write
	// conflicting cumulative points to the same Cloud Monitoring series.
	dialerID        string
	metricsMu       sync.Mutex
	metricRecorders map[metricRecorderKey]tel.MetricRecorder
	// metricsWG tracks background shutdowns of replaced metric recorders so
	// that Close can wait for them before closing the shared metric client.
	metricsWG sync.WaitGroup

	// dialFunc is the function used to connect to the address on the named
	// network. By default, it is golang.org/x/net/proxy#Dial.
	dialFunc func(cxt context.Context, network, addr string) (net.Conn, error)

	// iamTokenProvider supplies the OAuth2 token used for IAM DB Authn.
	iamTokenProvider auth.TokenProvider

	// resolver converts instance names into DNS names.
	resolver       instance.ConnectionNameResolver
	dnsResolver    cloudsql.NetResolver
	failoverPeriod time.Duration

	// metadataExchangeDisabled true when the dialer should never
	// send MDX mdx requests.
	metadataExchangeDisabled bool

	sqlDataDialer sqldataclient.Dialer

	tokenProvider                   auth.TokenProvider
	sqlDataEndpoint                 string
	resourceExhaustedCooldownPeriod time.Duration

	// applicationName is the name of the application using the dialer.
	applicationName string

	// computePlatform is the compute_platform metric label. It is either
	// detected from the environment or set explicitly via WithComputePlatform.
	computePlatform string

	// clientRegion is the client_region metric label, detected once at
	// construction when metrics are enabled.
	clientRegion string

	// metricsPrincipal is the best-effort identity used for the metric
	// export, for error messages only. May be empty.
	metricsPrincipal string

	// enableBuiltInMetrics controls whether the Dialer exports built-in
	// metrics (on by default). When false, the metric export is a no-op.
	enableBuiltInMetrics bool

	// mClient is used for built-in system metrics. It is nil when built-in
	// metrics are disabled or the client failed to initialize.
	mClient *monitoring.MetricClient

	// userAgent is the combined user agent string.
	userAgent string
}

var (
	errUseTokenSource    = errors.New("use WithTokenSource when IAM AuthN is not enabled")
	errUseIAMTokenSource = errors.New("use WithIAMAuthNTokenSources instead of WithTokenSource be used when IAM AuthN is enabled")
)

type nullLogger struct{}

func (nullLogger) Debugf(_ context.Context, _ string, _ ...interface{}) {}

// NewDialer creates a new Dialer.
//
// Initial calls to NewDialer make take longer than normal because generation of an
// RSA keypair is performed. Calls with a WithRSAKeyPair DialOption or after a default
// RSA keypair is generated will be faster.
func NewDialer(ctx context.Context, opts ...Option) (*Dialer, error) {
	cfg := &dialerConfig{
		refreshTimeout:                  cloudsql.RefreshTimeout,
		dialFunc:                        proxy.Dial,
		logger:                          nullLogger{},
		useragents:                      []string{userAgent},
		failoverPeriod:                  cloudsql.FailoverPeriod,
		dnsResolver:                     net.DefaultResolver,
		sqlDataEndpoint:                 "sqladmin.googleapis.com:443",
		sqlDataStreamTimeout:            2 * time.Hour,
		resourceExhaustedCooldownPeriod: 5 * time.Second,
		applicationName:                 "", // user-declared only; see WithApplicationName
		enableBuiltInMetrics:            builtInMetricsDefault,
	}
	for _, opt := range opts {
		opt(cfg)
		if cfg.err != nil {
			return nil, cfg.err
		}
	}

	if cfg.useIAMAuthN && cfg.setTokenSource && !cfg.setIAMAuthNTokenSource {
		return nil, errUseIAMTokenSource
	}
	if cfg.setIAMAuthNTokenSource && !cfg.useIAMAuthN {
		return nil, errUseTokenSource
	}

	// If callers have not provided a credential source, either explicitly with
	// WithTokenSource or implicitly with WithCredentialsJSON etc., then use
	// default credentials
	if !cfg.setCredentials {
		c, err := credentials.DetectDefault(&credentials.DetectOptions{
			Scopes: []string{sqladmin.SqlserviceAdminScope},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create default credentials: %w", err)
		}
		cfg.authCredentials = c
		// create second set of credentials, scoped for IAM AuthN login only
		scoped, err := credentials.DetectDefault(&credentials.DetectOptions{
			Scopes: []string{iamLoginScope},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create scoped credentials: %w", err)
		}
		cfg.iamLoginTokenProvider = scoped.TokenProvider
	}

	// For all credential paths, use auth library's built-in
	// httptransport.NewClient
	if cfg.authCredentials != nil {
		// Set headers for auth client as below WithHTTPClient will ignore
		// WithQuotaProject and WithUserAgent Options
		headers := http.Header{}
		headers.Set("User-Agent", strings.Join(cfg.useragents, " "))
		if cfg.quotaProject != "" {
			headers.Set("X-Goog-User-Project", cfg.quotaProject)
		}

		authClient, err := httptransport.NewClient(&httptransport.Options{
			Headers:        headers,
			Credentials:    cfg.authCredentials,
			UniverseDomain: cfg.getClientUniverseDomain(),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create auth client: %w", err)
		}
		// If callers have not provided an HTTPClient explicitly with
		// WithHTTPClient, then use auth client
		if !cfg.setHTTPClient {
			cfg.sqladminOpts = append(cfg.sqladminOpts, option.WithHTTPClient(authClient))
		}
	} else {
		// Add this to the end to make sure it's not overridden
		cfg.sqladminOpts = append(cfg.sqladminOpts, option.WithUserAgent(strings.Join(cfg.useragents, " ")))
		if cfg.quotaProject != "" {
			cfg.sqladminOpts = append(cfg.sqladminOpts, option.WithQuotaProject(cfg.quotaProject))
		}
	}

	client, err := sqladmin.NewService(ctx, cfg.sqladminOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create sqladmin client: %w", err)
	}

	dc := dialConfig{
		connectionType: cloudsql.PublicIP,
		tcpKeepAlive:   defaultTCPKeepAlive,
		useIAMAuthN:    cfg.useIAMAuthN,
	}
	for _, opt := range cfg.dialOpts {
		opt(&dc)
	}
	if dc.setSQLData && dc.setConnectTunnel {
		return nil, errtype.NewConfigError("cannot specify WithSQLData and WithConnectTunnel at the same time", "")
	}

	if err := trace.InitMetrics(); err != nil {
		return nil, err
	}
	g, err := newKeyGenerator(cfg.rsaKey, cfg.lazyRefresh,
		func() (*rsa.PrivateKey, error) {
			return rsa.GenerateKey(rand.Reader, 2048)
		})
	if err != nil {
		return nil, err
	}
	var r instance.ConnectionNameResolver = cloudsql.DefaultResolver
	if cfg.resolver != nil {
		r = cfg.resolver
	} else if cfg.useDNSNameResolver {
		r = cloudsql.NewDNSResolver(cfg.dnsResolver, client)
	}

	tp := cfg.apiTokenProvider
	if tp == nil && cfg.authCredentials != nil {
		tp = cfg.authCredentials.TokenProvider
	}

	// Only create the Cloud Monitoring client when built-in metrics are
	// enabled, so that callers who do not opt in pay no cost for it.
	//
	// This must run here, after credentials have been resolved above (ADC
	// detection, authCredentials, token providers) and tp is known, so that
	// the metric export uses the same identity, user agent, quota project,
	// and universe domain as the Dialer's other API clients. Creating the
	// client earlier would silently fall back to Application Default
	// Credentials regardless of the options the caller passed.
	var mClient *monitoring.MetricClient
	if cfg.enableBuiltInMetrics && !cfg.builtInMetricsSet && cfg.getClientUniverseDomain() != defaultUniverseDomain {
		// The on-by-default behaviour is for the default universe, where
		// the cloudsql.googleapis.com metric descriptors exist. Elsewhere
		// (Trusted Partner Cloud etc.) the export would most likely be
		// rejected, so require an explicit opt-in there.
		cfg.logger.Debugf(ctx, "built-in metrics are off by default in universe %q; pass WithBuiltInMetrics(true) to enable them", cfg.getClientUniverseDomain())
		cfg.enableBuiltInMetrics = false
	}
	if cfg.enableBuiltInMetrics {
		mClient, err = newMetricClient(ctx, cfg, tp)
		if err != nil {
			// Don't fail dialer initialization on metric client errors.
			// Just disable metric collection below.
			cfg.logger.Debugf(ctx, "built-in metrics exporter failed to initialize: %v", err)
			mClient = nil
		}
	}

	var sqlDataDialer sqldataclient.Dialer
	if cfg.sqlDataDialer != nil {
		sqlDataDialer = cfg.sqlDataDialer
	} else {
		sqlDataDialer = sqldataclient.NewGrpcDialer(cfg.sqlDataEndpoint, tp, cfg.quotaProject, cfg.logger, false, cfg.sqlDataStreamTimeout, strings.Join(cfg.useragents, " "))
	}

	// Environment detection for the metric resource labels. All of it is
	// skipped when metrics are off (or the metric client could not be
	// created): DetectComputePlatform and DetectClientRegion may contact the
	// metadata server, and detectPrincipal may too. Detecting once here,
	// rather than per recorder, keeps metadata-server round trips off the
	// dial path. An explicit WithComputePlatform always wins over detection.
	computePlatform := cfg.computePlatform
	clientRegion := tel.UnknownValue
	var metricsPrincipal string
	if mClient != nil {
		if computePlatform == "" {
			computePlatform = tel.DetectComputePlatform()
		}
		clientRegion = tel.DetectClientRegion()
		metricsPrincipal = detectPrincipal(ctx, cfg)
	}

	d := &Dialer{
		closed:                          make(chan struct{}),
		sqlDataConnState:                make(map[cacheKey]*sqlDataConnState),
		cache:                           make(map[cacheKey]*monitoredCache),
		lazyRefresh:                     cfg.lazyRefresh,
		enableBuiltInMetrics:            cfg.enableBuiltInMetrics,
		keyGenerator:                    g,
		refreshTimeout:                  cfg.refreshTimeout,
		sqladmin:                        client,
		mClient:                         mClient,
		logger:                          cfg.logger,
		defaultDialConfig:               dc,
		dialerID:                        uuid.New().String(),
		iamTokenProvider:                cfg.iamLoginTokenProvider,
		metricRecorders:                 map[metricRecorderKey]tel.MetricRecorder{},
		dialFunc:                        cfg.dialFunc,
		dnsResolver:                     cfg.dnsResolver,
		resolver:                        r,
		failoverPeriod:                  cfg.failoverPeriod,
		metadataExchangeDisabled:        cfg.metadataExchangeDisabled,
		tokenProvider:                   tp,
		sqlDataEndpoint:                 cfg.sqlDataEndpoint,
		sqlDataDialer:                   sqlDataDialer,
		resourceExhaustedCooldownPeriod: cfg.resourceExhaustedCooldownPeriod,
		userAgent:                       strings.Join(cfg.useragents, " "),
		applicationName:                 cfg.applicationName,
		computePlatform:                 computePlatform,
		clientRegion:                    clientRegion,
		metricsPrincipal:                metricsPrincipal,
	}

	// log the Dialer ID
	d.logger.Debugf(ctx, "Cloud SQL Go Connector Dialer ID: %s", d.dialerID)

	return d, nil
}

// newMetricClient creates the Cloud Monitoring client used for built-in
// metrics. The client is configured with the same user agent, quota project,
// and universe domain as the Dialer's other API clients, and authenticates
// with the identity returned by metricTokenProvider.
func newMetricClient(ctx context.Context, cfg *dialerConfig, apiTP auth.TokenProvider) (*monitoring.MetricClient, error) {
	opts := []option.ClientOption{
		option.WithUserAgent(strings.Join(cfg.useragents, " ")),
		option.WithUniverseDomain(cfg.getClientUniverseDomain()),
	}
	if cfg.quotaProject != "" {
		opts = append(opts, option.WithQuotaProject(cfg.quotaProject))
	}
	creds, err := metricCredentials(cfg, apiTP)
	if err != nil {
		return nil, err
	}
	if creds != nil {
		opts = append(opts, option.WithAuthCredentials(creds))
	}
	// Caller-provided client options go last so they take precedence.
	opts = append(opts, cfg.clientOpts...)
	return monitoring.NewMetricClient(ctx, opts...)
}

// metricCredentials returns the credentials for the built-in metrics export.
//
// When the Dialer detected credentials itself (Application Default
// Credentials, WithCredentialsFile, or WithCredentialsJSON), the credentials
// are re-detected with the monitoring.write scope so that the resulting
// tokens are accepted by the Cloud Monitoring API. This yields the same
// identity as the Dialer's API calls, with the scope the export needs.
//
// When the caller supplied a token source or credentials directly, that
// token provider is used as-is because its scopes cannot be changed.
//
// In both cases the credentials carry the Dialer's configured universe
// domain: the Monitoring client validates that the credentials' universe
// matches the client's, and credentials without one default to
// googleapis.com, which would fail that check for every export in a
// non-default universe.
func metricCredentials(cfg *dialerConfig, apiTP auth.TokenProvider) (*auth.Credentials, error) {
	if !cfg.setCredentials || cfg.credentialsJSON != nil {
		c, err := credentials.DetectDefault(&credentials.DetectOptions{
			Scopes:          []string{monitoringWriteScope},
			CredentialsJSON: cfg.credentialsJSON,
			// Only an explicit WithUniverseDomain is passed through. The
			// option overrides the universe_domain found in the credential
			// file, so passing the googleapis.com default here would strip a
			// TPC credential of its universe and send its tokens to the
			// default universe's endpoint. Left empty, the file's value (or
			// googleapis.com) is used, as for the Admin API credentials.
			UniverseDomain: cfg.clientUniverseDomain,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create credentials for built-in metrics: %w", err)
		}
		return c, nil
	}
	if apiTP == nil {
		return nil, nil
	}
	ud := cfg.getClientUniverseDomain()
	return auth.NewCredentials(&auth.CredentialsOptions{
		TokenProvider: apiTP,
		UniverseDomainProvider: auth.CredentialsPropertyFunc(func(context.Context) (string, error) {
			return ud, nil
		}),
	}), nil
}

// authProxyMetricsFlag is the Cloud SQL Auth Proxy flag that controls
// built-in metrics. It is referenced in error messages so proxy users learn
// how to turn metrics off; the proxy must use this exact flag name.
const authProxyMetricsFlag = "--enable-builtin-metrics"

// metricsDisableHint returns how to turn built-in metrics off in the client
// that embeds this connector.
func metricsDisableHint(connectorType string) string {
	if connectorType == tel.ConnectorTypeAuthProxy {
		return "run the proxy with " + authProxyMetricsFlag + "=false"
	}
	return "pass cloudsqlconn.WithBuiltInMetrics(false) to NewDialer"
}

// detectPrincipal returns the identity the Dialer authenticates with, when
// it can be determined cheaply: the client_email of service account JSON
// credentials, or the default service account on GCE/Cloud Run/GKE when
// using Application Default Credentials. Returns "" otherwise (for example
// caller-supplied token sources, whose principal is opaque). Used only to
// make permission errors actionable.
func detectPrincipal(ctx context.Context, cfg *dialerConfig) string {
	if len(cfg.credentialsJSON) > 0 {
		var sa struct {
			ClientEmail string `json:"client_email"`
		}
		if err := json.Unmarshal(cfg.credentialsJSON, &sa); err == nil && sa.ClientEmail != "" {
			return sa.ClientEmail
		}
		return ""
	}
	if cfg.setCredentials {
		// Token source / credentials supplied by the caller: principal unknown.
		return ""
	}
	// Application Default Credentials.
	if f := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); f != "" {
		if b, err := os.ReadFile(f); err == nil {
			var sa struct {
				ClientEmail string `json:"client_email"`
			}
			if err := json.Unmarshal(b, &sa); err == nil && sa.ClientEmail != "" {
				return sa.ClientEmail
			}
		}
		return ""
	}
	mctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if metadata.OnGCEWithContext(mctx) {
		if email, err := metadata.EmailWithContext(mctx, "default"); err == nil {
			return email
		}
	}
	return ""
}

// metricRecorder returns the built-in metric recorder for inst, creating it
// on first use. A recorder created before the database engine is known
// (engine "unknown") is replaced once the engine is learned; the replaced
// recorder is flushed and stopped in the background so the dial that
// learned the engine is not held up by an export.
//
// The lock is held only for the map lookup/insert and the (in-memory)
// recorder construction; nothing under it touches the network.
func (d *Dialer) metricRecorder(ctx context.Context, inst instance.ConnName, dbEngineType string) tel.MetricRecorder {
	key := newMetricRecorderKey(inst)
	d.metricsMu.Lock()
	defer d.metricsMu.Unlock()
	old, ok := d.metricRecorders[key]
	if ok {
		if dbEngineType == tel.UnknownValue || dbEngineType == "" || old.DatabaseEngine() == dbEngineType {
			return old
		}
	}
	// Metrics off (by option, by universe default, or because the metric
	// client could not be created, which was logged once in NewDialer):
	// nothing to build, store or log.
	if !d.enableBuiltInMetrics || d.mClient == nil {
		return tel.NullMetricRecorder{}
	}
	// A Dial that raced with Close must not create a recorder after Close
	// has shut the others down and closed the metric client: it would leak
	// an export goroutine writing to a closed client.
	select {
	case <-d.closed:
		return tel.NullMetricRecorder{}
	default:
	}
	cfg := tel.Config{
		Enabled:           d.enableBuiltInMetrics,
		Version:           strings.TrimSpace(versionString),
		ResourceContainer: inst.Project(),
		// ResourceID is the Cloud SQL instance identifier formatted as [project_name:instance_name].
		ResourceID:         inst.Project() + ":" + inst.Name(),
		ClientUID:          d.dialerID,
		ApplicationName:    d.applicationName,
		Region:             inst.Region(),
		ClientRegion:       d.clientRegion,
		ComputePlatform:    d.computePlatform,
		ConnectorType:      tel.ConnectorTypeValue(d.userAgent),
		Principal:          d.metricsPrincipal,
		DisableHint:        metricsDisableHint(tel.ConnectorTypeValue(d.userAgent)),
		ConnectorVersion:   tel.ConnectorVersionValue(d.userAgent, strings.TrimSpace(versionString)),
		DatabaseEngineType: dbEngineType,
	}
	mr := tel.NewMetricRecorder(ctx, d.logger, d.mClient, cfg)
	d.metricRecorders[key] = mr
	if ok {
		// Flush and stop the previous recorder so it does not leak a
		// goroutine or double-report, off the dial path. Close waits on
		// metricsWG before closing the shared metric client.
		d.metricsWG.Add(1)
		go func() {
			defer d.metricsWG.Done()
			sctx, cancel := context.WithTimeout(context.Background(), metricShutdownTimeout)
			defer cancel()
			if err := old.Shutdown(sctx); err != nil {
				d.logger.Debugf(sctx, "[%v] failed to shut down replaced metric recorder: %v", inst.String(), err)
			}
		}()
	}
	return mr
}

// Dial returns a net.Conn connected to the specified Cloud SQL instance. The
// icn argument may be the instance's connection name in the format
// "project-name:region:instance-name" or a DNS name that resolves to an
// instance connection name.
func (d *Dialer) Dial(ctx context.Context, icn string, opts ...DialOption) (conn net.Conn, err error) {
	select {
	case <-d.closed:
		return nil, ErrDialerClosed
	default:
	}
	cfg := d.defaultDialConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	// Start the dial span before resolving the instance name so that
	// resolution failures (e.g. DNS) are attributed to the dial, both in the
	// span and in the dial-error metric, as they were before built-in
	// metrics were added.
	startTime := time.Now()
	var endDial trace.EndSpanFunc
	ctx, endDial = trace.StartSpan(ctx, "cloud.google.com/go/cloudsqlconn.Dial",
		trace.AddInstanceName(icn),
		trace.AddDialerID(d.dialerID),
	)
	defer func() {
		trace.RecordDialError(context.Background(), icn, d.dialerID, err)
		endDial(err)
	}()

	// Resolve the instance connection name to a ConnName struct.
	// Note: icn may be a domain name that resolves to an instance connection name.
	cn, err := d.resolver.Resolve(ctx, icn)
	if err != nil {
		return nil, err
	}
	// The built-in metric recorder is keyed by instance, so it can only be
	// looked up once the name has resolved.
	mr := d.metricRecorder(ctx, cn, tel.UnknownValue)

	attrs := tel.Attributes{
		IAMAuthN: cfg.useIAMAuthN,
		IPType:   cfg.connectionType,
	}

	// Log if resolver changed the instance name input string.
	if cn.DomainName() != "" {
		// icn is a domain name, which resolves to a actual icn
		d.logger.Debugf(ctx, "resolved domain name %s to %s", icn, cn.String())
	} else if cn.String() != icn {
		// icn was not a domain name, but the resolver changed it and cn != icn
		d.logger.Debugf(ctx, "resolved instance connection string %s to %s", icn, cn.String())
	}

	if cfg.setSQLData && cfg.setConnectTunnel {
		return nil, errtype.NewConfigError("cannot specify WithSQLData and WithConnectTunnel at the same time", cn.String())
	}
	// When using NoIP connections, there is no need to retrieve connection
	// info and instead we may proceed immediately to connecting.
	if cfg.connectionType == cloudsql.SQLData || cfg.connectionType == cloudsql.ConnectTunnel {
		key := createKey(cn)
		d.lock.RLock()
		state, ok := d.sqlDataConnState[key]
		d.lock.RUnlock()
		if !ok {
			d.lock.Lock()
			state, ok = d.sqlDataConnState[key]
			if !ok {
				state = &sqlDataConnState{allowed: true}
				d.sqlDataConnState[key] = state
			}
			d.lock.Unlock()
		}

		state.mu.Lock()
		if !state.allowed {
			state.mu.Unlock()
		} else {
			cooldownUntil := state.cooldownUntil
			lastErr := state.lastErr
			state.mu.Unlock()

			if time.Now().Before(cooldownUntil) {
				err := errtype.NewResourceExhaustedError("cooldown active", cn.String(), lastErr)
				recordDialFailure(ctx, mr, attrs, startTime, err)
				return nil, err
			}

			// Attempt to connect SqlDataService
			sdcConn, sdcErr := d.connectSQLDataService(ctx, cn, cfg, startTime, mr, attrs)
			if sdcErr == nil {
				// Connection succeeded, return the connection.
				return sdcConn, nil
			}
			// Connection failed. Record the SQL Data attempt as such
			// regardless of whether a direct-IP fallback follows: the
			// fallback is a separate attempt with its own IP type.
			recordDialFailure(ctx, mr, attrs, startTime, sdcErr)

			if cfg.connectionType == cloudsql.ConnectTunnel || !isPreconditionFailed(sdcErr) {
				return nil, sdcErr
			}
			// sdcErr is a PreconditionFailed error. Fall back to direct IP connection.
			// This is a streaming gRPC. The PreconditionFailed error usually occurs on the
			// first read from the stream, not here.
			startTime = time.Now()
		}
	}

	return d.connectInstanceIP(ctx, cn, cfg, startTime, mr, attrs)
}

// recordDialFailure records a failed connection attempt in the built-in
// metrics: one connect_latencies sample and one open_connection_count
// increment, both labelled with the classified error status.
func recordDialFailure(ctx context.Context, mr tel.MetricRecorder, attrs tel.Attributes, startTime time.Time, err error) {
	attrs.DialStatus = tel.ConnectError
	attrs.ConnectionPart, attrs.AttemptStatus = tel.ClassifyDialError(err)
	mr.RecordConnectLatencies(ctx, attrs, time.Since(startTime).Milliseconds())
	mr.RecordOpenConnectionCount(ctx, attrs)
}

// connectSQLDataService dials a connection through the SqlDataService.
func (d *Dialer) connectSQLDataService(ctx context.Context, cn instance.ConnName, cfg dialConfig, startTime time.Time, mr tel.MetricRecorder, attrs tel.Attributes) (net.Conn, error) {
	key := createKey(cn)
	onResourceExhausted := func(err error) {
		d.lock.RLock()
		state, ok := d.sqlDataConnState[key]
		d.lock.RUnlock()
		if !ok {
			return
		}
		state.mu.Lock()
		// Maximum backup counter is 5.
		if state.backoffCounter < 5 {
			state.backoffCounter++
		}
		backoff := cooldownBackoff(d.resourceExhaustedCooldownPeriod, state.backoffCounter)
		state.cooldownUntil = time.Now().Add(backoff)
		state.lastErr = err
		state.mu.Unlock()
	}

	onSuccess := func() {
		d.lock.RLock()
		state, ok := d.sqlDataConnState[key]
		d.lock.RUnlock()
		if !ok {
			return
		}
		state.mu.Lock()
		state.backoffCounter = 0
		state.cooldownUntil = time.Time{}
		state.mu.Unlock()
	}

	conn, err := d.sqlDataDialer.ConnectSQLDataService(ctx, cn, cfg.useIAMAuthN)
	if err != nil {
		if isResourceExhaustedError(err) {
			onResourceExhausted(err)
		}
		return nil, err
	}

	// In gRPC bidirectional streaming, StreamSqlData and Send(StartSession)
	// buffer frames locally and return before GFE or SFEv3 sends response
	// headers, and PostgreSQL is client-speaks-first so Dial cannot block on
	// Recv. Defer recording the sql_data attempt outcome until the first Read
	// result (or Close if closed before reading) so GFE rejections such as
	// ResourceExhausted or InvalidArgument are recorded with their true error
	// status rather than as successful dials. The database engine is not known
	// on this path (no connection info is fetched), so the recorder's engine
	// stays unknown.
	var (
		attemptOnce sync.Once
		opened      atomic.Bool
		openTime    time.Time
		closeOnce   sync.Once
	)
	recordSuccess := func() {
		attemptOnce.Do(func() {
			openTime = time.Now()
			opened.Store(true)
			latency := openTime.Sub(startTime).Milliseconds()
			okAttrs := attrs
			okAttrs.DialStatus = tel.ConnectSuccess
			okAttrs.ConnectionPart = ""
			okAttrs.AttemptStatus = tel.ConnectSuccess
			mr.RecordOpenConnection(context.Background(), okAttrs)
			mr.RecordConnectLatencies(context.Background(), okAttrs, latency)
			mr.RecordOpenConnectionCount(context.Background(), okAttrs)
		})
	}
	recordFailure := func(err error) {
		attemptOnce.Do(func() {
			recordDialFailure(context.Background(), mr, attrs, startTime, err)
		})
	}
	recordClosed := func() {
		// If the caller closes the connection before any Read completed and no
		// failure occurred, account for the dial and its closure once.
		recordSuccess()
		if !opened.Load() {
			return
		}
		closeOnce.Do(func() {
			okAttrs := attrs
			okAttrs.DialStatus = tel.ConnectSuccess
			okAttrs.ConnectionPart = ""
			okAttrs.AttemptStatus = tel.ConnectSuccess
			mr.RecordClosedConnection(context.Background(), okAttrs)
			mr.RecordClosedConnectionCount(context.Background(), okAttrs)
			mr.RecordConnectionDuration(context.Background(), okAttrs, time.Since(openTime).Seconds())
		})
	}

	if cfg.connectionType != cloudsql.ConnectTunnel {
		// This creates a fallback connection, and marks the connection
		// as permanently needing to fallback to direct IP.
		var lastFallbackErr atomic.Value
		fb := func() (net.Conn, error) {
			d.lock.RLock()
			state, ok := d.sqlDataConnState[key]
			d.lock.RUnlock()
			if ok {
				state.mu.Lock()
				state.allowed = false
				state.mu.Unlock()
			}
			// The sql_data stream was rejected as unsupported and is being
			// replaced by a direct-IP connection, which records its own metrics
			// (connectInstanceIP returns an instrumentedConn). Record the failed
			// sql_data attempt and time the fallback as its own attempt.
			if v := lastFallbackErr.Load(); v != nil {
				recordFailure(v.(error))
			}
			return d.connectInstanceIP(ctx, cn, cfg, time.Now(), mr, attrs)
		}

		isUnsupported := func(err error) bool {
			if isSQLDataUnsupportedError(err) {
				lastFallbackErr.Store(err)
				return true
			}
			return false
		}

		conn = newFallbackConn(conn, isUnsupported, fb)
	}
	return &metricsConn{
		Conn: &resourceExhaustedTrackingConn{
			Conn:                conn,
			onResourceExhausted: onResourceExhausted,
			onSuccess: func() {
				onSuccess()
				recordSuccess()
			},
			onInitialError: recordFailure,
		},
		onClose: recordClosed,
	}, nil
}

// metricsConn runs onClose when the connection is closed. onClose is
// expected to be idempotent; Close may be called more than once.
type metricsConn struct {
	net.Conn
	onClose func()
}

func (c *metricsConn) Close() error {
	err := c.Conn.Close()
	c.onClose()
	return err
}

// connectInstanceIP dials the IP address of the instance using a normal net.Conn.
func (d *Dialer) connectInstanceIP(ctx context.Context, cn instance.ConnName, cfg dialConfig, startTime time.Time, mr tel.MetricRecorder, attrs tel.Attributes) (conn net.Conn, err error) {
	var endInfo trace.EndSpanFunc
	ctx, endInfo = trace.StartSpan(ctx, "cloud.google.com/go/cloudsqlconn/internal.InstanceInfo")
	c, err := d.connectionInfoCache(ctx, cn, &cfg.useIAMAuthN)
	if err != nil {
		attrs.DialStatus = tel.ConnectError
		attrs.ConnectionPart, attrs.AttemptStatus = tel.ClassifyDialError(err)
		mr.RecordConnectLatencies(ctx, attrs, time.Since(startTime).Milliseconds())
		mr.RecordOpenConnectionCount(ctx, attrs)
		endInfo(err)
		return nil, err
	}
	ci, err := c.ConnectionInfo(ctx)
	if err != nil {
		d.removeCached(ctx, cn, c, err)
		attrs.DialStatus = tel.ConnectError
		attrs.ConnectionPart, attrs.AttemptStatus = tel.ClassifyDialError(err)
		mr.RecordConnectLatencies(ctx, attrs, time.Since(startTime).Milliseconds())
		mr.RecordOpenConnectionCount(ctx, attrs)
		endInfo(err)
		return nil, err
	}
	mr = d.metricRecorder(ctx, cn, tel.DetectDatabaseEngineType(ci.DBVersion))
	endInfo(nil)

	// If the client certificate has expired (as when the computer goes to
	// sleep, and the refresh cycle cannot run), force a refresh immediately.
	// The TLS handshake will not fail on an expired client certificate. It's
	// not until the first read where the client cert error will be surfaced.
	// So check that the certificate is valid before proceeding.
	if !validClientCert(ctx, cn, d.logger, ci.Expiration) {
		d.logger.Debugf(ctx, "[%v] Refreshing certificate now", cn.String())
		c.ForceRefresh()
		// Block on refreshed connection info
		ci, err = c.ConnectionInfo(ctx)
		if err != nil {
			d.removeCached(ctx, cn, c, err)
			attrs.DialStatus = tel.ConnectError
			attrs.ConnectionPart, attrs.AttemptStatus = tel.ClassifyDialError(err)
			mr.RecordConnectLatencies(ctx, attrs, time.Since(startTime).Milliseconds())
			mr.RecordOpenConnectionCount(ctx, attrs)
			return nil, err
		}
	}

	var connectEnd trace.EndSpanFunc
	ctx, connectEnd = trace.StartSpan(ctx, "cloud.google.com/go/cloudsqlconn/internal.Connect")
	defer func() { connectEnd(err) }()

	tlsConn, resolvedIPType, err := d.connect(ctx, cn, ci, cfg)
	if resolvedIPType != "" {
		// Report the IP type actually used rather than the requested strategy
		// (e.g. "public" instead of "auto").
		attrs.IPType = resolvedIPType
	}
	if err != nil {
		var dialErr *errtype.DialError
		if errors.As(err, &dialErr) && dialErr.Message == "failed to dial" {
			// refresh the instance info in case it caused the connection failure
			c.ForceRefresh()
		} else if !errors.As(err, &dialErr) || dialErr.Message == "handshake failed" {
			// TLS handshake or address configuration errors are fatal and require a refresh.
			// Remove the instance from the cache so that future calls to Dial() will block
			// until the certificate is refreshed successfully.
			d.logger.Debugf(ctx, "[%v] Connection failed: %v", cn.String(), err)
			d.removeCached(ctx, cn, c, err)
		}
		attrs.DialStatus = tel.ConnectError
		attrs.ConnectionPart, attrs.AttemptStatus = tel.ClassifyDialError(err)
		mr.RecordConnectLatencies(ctx, attrs, time.Since(startTime).Milliseconds())
		mr.RecordOpenConnectionCount(ctx, attrs)
		return nil, err
	}

	// Use tlsConn as the official connection
	netConn := tlsConn
	// Send MDX if the client protocol type was set.
	mdxReq := newMDXRequest(ci, cfg, d.metadataExchangeDisabled)
	if mdxReq != nil {
		netConn = cloudsql.NewMDXConn(tlsConn, cn.String(), mdxReq, d.logger)
	}
	if cfg.useIAMAuthN && strings.HasPrefix(ci.DBVersion, "POSTGRES") {
		ipType := resolvedIPType
		if ipType == "" {
			ipType = cfg.connectionType
		}
		netConn = cloudsql.NewPostgresConn(netConn, func(user, database string) {
			c.RecordIAMPrincipal(user, database, ipType)
		})
	}

	latency := time.Since(startTime).Milliseconds()
	openTime := time.Now()
	n := c.openConnsCount.Add(1)
	trace.RecordOpenConnections(ctx, int64(n), d.dialerID, cn.String())
	trace.RecordDialLatency(ctx, cn.String(), d.dialerID, latency)
	attrs.DialStatus = tel.ConnectSuccess
	attrs.ConnectionPart = ""
	attrs.AttemptStatus = tel.ConnectSuccess
	mr.RecordOpenConnection(ctx, attrs)
	mr.RecordConnectLatencies(ctx, attrs, latency)
	mr.RecordOpenConnectionCount(ctx, attrs)

	closeFunc := func() {
		durationSec := time.Since(openTime).Seconds()
		n := c.openConnsCount.Add(^uint64(0)) // c.openConnsCount = c.openConnsCount - 1
		trace.RecordOpenConnections(context.Background(), int64(n), d.dialerID, cn.String())
		mr.RecordClosedConnection(context.Background(), attrs)
		mr.RecordClosedConnectionCount(context.Background(), attrs)
		mr.RecordConnectionDuration(context.Background(), attrs, durationSec)
	}
	errFunc := func(err error) {
		// io.EOF occurs when the server closes the connection. This is safe to
		// ignore.
		if err == io.EOF {
			return
		}
		d.logger.Debugf(ctx, "[%v] IO Error on Read or Write: %v", cn.String(), err)
		if d.isTLSError(err) {
			// CLIENT_PROTOCOL_TLS handshake errors are fatal. Remove the instance from the cache
			// so that future calls to Dial() will block until the certificate
			// is refreshed successfully.
			d.removeCached(ctx, cn, c, err)
			_ = netConn.Close() // best effort close attempt
		}
	}
	iConn := newInstrumentedConn(netConn, closeFunc, errFunc, d.dialerID, cn.String())

	// If this connection was opened using a Domain Name, then store it for later
	// in case it needs to be forcibly closed.
	if cn.HasDomainName() {
		c.mu.Lock()
		c.openConns = append(c.openConns, iConn)
		c.mu.Unlock()
	}
	d.logger.Debugf(ctx, "dial successful")
	return iConn, nil
}

// connect establishes a TLS connection to the instance specified by cn and ci,
// using the provided dialConfig.
func (d *Dialer) connect(ctx context.Context, cn instance.ConnName, ci cloudsql.ConnectionInfo, cfg dialConfig) (net.Conn, string, error) {
	addrs, resolvedIPType, err := ci.ResolveAddrs(cfg.connectionType)
	if err != nil {
		return nil, "", err
	}

	var targets []string

	// If the connector is configured with a custom DNS name, attempt to use
	// that DNS name to connect to the instance. Fall back to the metadata IP
	// address if the DNS name does not resolve to an IP address.
	if cn.HasDomainName() {
		resolved, err := d.dnsResolver.LookupHost(ctx, cn.DomainName())
		if err != nil {
			d.logger.Debugf(ctx,
				"[%v] custom DNS name %q did not resolve to an IP address: %v, using %s from instance metadata",
				cn.String(), cn.DomainName(), err, addrs)
			targets = addrs
		} else if len(resolved) == 0 {
			d.logger.Debugf(ctx,
				"[%v] custom DNS name %q resolved but returned no entries, using %s from instance metadata",
				cn.String(), cn.DomainName(), addrs)
			targets = addrs
		} else {
			d.logger.Debugf(ctx,
				"[%v] custom DNS name %q resolved to %q, using it to connect",
				cn.String(), cn.DomainName(), resolved)
			targets = resolved
		}
	} else {
		targets = addrs
	}

	f := d.dialFunc
	if cfg.dialFunc != nil {
		f = cfg.dialFunc
	}
	// Attempt to dial each of the targets in turn. Use the first one that succeeds.
	var dialErrs []error
	var conn net.Conn
	for _, target := range targets {
		dialAddr := net.JoinHostPort(target, serverProxyPort)
		d.logger.Debugf(ctx, "[%v] Dialing %v", cn.String(), dialAddr)
		var dialErr error
		conn, dialErr = f(ctx, "tcp", dialAddr)
		if dialErr == nil {
			dialErrs = nil
			break
		}
		dialErrs = append(dialErrs, dialErr)
		d.logger.Debugf(ctx, "[%v] Dialing %v failed: %v", cn.String(), dialAddr, dialErr)
	}
	if len(dialErrs) > 0 || conn == nil {
		return nil, resolvedIPType, errtype.NewDialError("failed to dial", cn.String(), errors.Join(dialErrs...))
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		if err := tc.SetKeepAlive(true); err != nil {
			_ = conn.Close()
			return nil, resolvedIPType, errtype.NewDialError("failed to set keep-alive", cn.String(), err)
		}
		if err := tc.SetKeepAlivePeriod(cfg.tcpKeepAlive); err != nil {
			_ = conn.Close()
			return nil, resolvedIPType, errtype.NewDialError("failed to set keep-alive period", cn.String(), err)
		}
	}

	tlsConn := tls.Client(conn, ci.TLSConfig())
	err = tlsConn.HandshakeContext(ctx)
	if err != nil {
		_ = tlsConn.Close() // best effort close attempt
		return nil, resolvedIPType, errtype.NewDialError("handshake failed", cn.String(), err)
	}

	return tlsConn, resolvedIPType, nil
}

func isSQLDataUnsupportedError(err error) bool {
	if s, ok := status.FromError(err); ok {
		return s.Code() == codes.FailedPrecondition &&
			strings.HasPrefix(s.Message(), "unsupported instance edition")
	}
	return false
}

func (d *Dialer) isTLSError(err error) bool {
	if nErr, ok := err.(net.Error); ok {
		return !nErr.Timeout() && // it's a permanent net error
			strings.Contains(nErr.Error(), "tls") // it's a TLS-related error
	}
	return false
}

// removeCached stops all background refreshes, closes open sockets, and deletes
// the cache entry.
func (d *Dialer) removeCached(
	ctx context.Context,
	i instance.ConnName, c *monitoredCache, err error,
) {
	d.logger.Debugf(
		ctx,
		"[%v] Removing connection info from cache: %v",
		i.String(),
		err,
	)

	// If this instance of monitoredCache is still in the cache, remove it.
	// If this instance was already removed from the cache or
	// if *a separate goroutine* replaced it with a new instance, do nothing.
	key := createKey(i)
	d.lock.Lock()
	if cachedC, ok := d.cache[key]; ok && cachedC == c {
		delete(d.cache, key)
	}
	d.lock.Unlock()

	// Close the monitoredCache, this call is idempotent.
	c.Close()
}

// validClientCert checks that the ephemeral client certificate retrieved from
// the cache is unexpired. The time comparisons strip the monotonic clock value
// to ensure an accurate result, even after laptop sleep.
func validClientCert(
	ctx context.Context, cn instance.ConnName,
	l debug.ContextLogger, expiration time.Time,
) bool {
	// Use UTC() to strip monotonic clock value to guard against inaccurate
	// comparisons, especially after laptop sleep.
	// See the comments on the monotonic clock in the Go documentation for
	// details: https://pkg.go.dev/time#hdr-Monotonic_Clocks
	now := time.Now().UTC()
	valid := expiration.UTC().After(now)
	l.Debugf(
		ctx,
		"[%v] Now = %v, Current cert expiration = %v",
		cn.String(),
		now.Format(time.RFC3339),
		expiration.UTC().Format(time.RFC3339),
	)
	l.Debugf(ctx, "[%v] Cert is valid = %v", cn.String(), valid)
	return valid
}

// EngineVersion returns the engine type and version for the instance
// connection name. The value will correspond to one of the following types for
// the instance:
// https://cloud.google.com/sql/docs/mysql/admin-api/rest/v1beta4/SqlDatabaseVersion
func (d *Dialer) EngineVersion(ctx context.Context, icn string) (string, error) {
	cn, err := d.resolver.Resolve(ctx, icn)
	if err != nil {
		return "", err
	}
	c, err := d.connectionInfoCache(ctx, cn, &d.defaultDialConfig.useIAMAuthN)
	if err != nil {
		return "", err
	}
	ci, err := c.ConnectionInfo(ctx)
	if err != nil {
		d.removeCached(ctx, cn, c, err)
		return "", err
	}
	return ci.DBVersion, nil
}

// Warmup starts the background refresh necessary to connect to the instance.
// Use Warmup to start the refresh process early if you don't know when you'll
// need to call "Dial".
func (d *Dialer) Warmup(ctx context.Context, icn string, opts ...DialOption) error {
	cn, err := d.resolver.Resolve(ctx, icn)
	if err != nil {
		return err
	}
	cfg := d.defaultDialConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	c, err := d.connectionInfoCache(ctx, cn, &cfg.useIAMAuthN)
	if err != nil {
		return err
	}
	_, err = c.ConnectionInfo(ctx)
	if err != nil {
		d.removeCached(ctx, cn, c, err)
	}
	return err
}

// newInstrumentedConn initializes an instrumentedConn that on closing will
// decrement the number of open connects and record the result.
func newInstrumentedConn(conn net.Conn, closeFunc func(), errFunc func(error), dialerID, connName string) *instrumentedConn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &instrumentedConn{
		Conn:         conn,
		closeFunc:    closeFunc,
		errFunc:      errFunc,
		dialerID:     dialerID,
		connName:     connName,
		reportTicker: time.NewTicker(5 * time.Second),
		stopReporter: cancel,
	}

	go c.report(ctx)

	return c
}

// instrumentedConn wraps a net.Conn and invokes closeFunc when the connection
// is closed.
type instrumentedConn struct {
	net.Conn
	closeFunc    func()
	errFunc      func(error)
	mu           sync.RWMutex
	closed       bool
	dialerID     string
	connName     string
	bytesRead    atomic.Int64
	bytesWritten atomic.Int64
	reportTicker *time.Ticker
	stopReporter func()
}

// Read delegates to the underlying net.Conn interface and records number of
// bytes read
func (i *instrumentedConn) Read(b []byte) (int, error) {
	bytesRead, err := i.Conn.Read(b)
	if err == nil {
		i.bytesRead.Add(int64(bytesRead))
	} else {
		i.errFunc(err)
	}
	return bytesRead, err
}

// Write delegates to the underlying net.Conn interface and records number of
// bytes written
func (i *instrumentedConn) Write(b []byte) (int, error) {
	bytesWritten, err := i.Conn.Write(b)
	if err == nil {
		i.bytesWritten.Add(int64(bytesWritten))
	} else {
		i.errFunc(err)
	}
	return bytesWritten, err
}

// isClosed returns true if this connection is closing or is already closed.
func (i *instrumentedConn) isClosed() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.closed
}

// Close delegates to the underlying net.Conn interface and reports the close
// to the provided closeFunc. Close is idempotent: only the first call
// reports, so a connection that is closed twice (which database/sql drivers
// and connection pools may do) is counted once in open_connections,
// closed_connection_count and connection_durations.
func (i *instrumentedConn) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return i.Conn.Close()
	}
	i.closed = true
	i.stopReporter()
	i.reportCounters()
	i.closeFunc()
	return i.Conn.Close()
}

func (i *instrumentedConn) reportCounters() {
	bytesRead := i.bytesRead.Swap(0)
	bytesWritten := i.bytesWritten.Swap(0)
	trace.RecordBytesReceived(context.Background(), bytesRead, i.connName, i.dialerID)
	trace.RecordBytesSent(context.Background(), bytesWritten, i.connName, i.dialerID)
}

func (i *instrumentedConn) report(ctx context.Context) {
	defer i.reportTicker.Stop()
	for {
		select {
		case <-i.reportTicker.C:
			i.reportCounters()
		case <-ctx.Done():
			return
		}
	}
}

// Close closes the Dialer; it prevents the Dialer from refreshing the information
// needed to connect.
func (d *Dialer) Close() error {
	// Only the first caller proceeds; concurrent and repeated calls return
	// immediately (the first call may still be flushing).
	if !d.closing.CompareAndSwap(false, true) {
		return nil
	}
	close(d.closed)
	d.lock.Lock()
	for _, i := range d.cache {
		i.Close()
	}
	if err := d.sqlDataDialer.Close(); err != nil {
		d.logger.Debugf(context.Background(),
			"failed to close SQL data connection: %v", err)
	}
	d.lock.Unlock()

	// Flush and stop the built-in metrics exporters. Without this, metrics
	// recorded since the last periodic export (up to 60s worth) are lost
	// when the process exits, which is the common case on Cloud Run.
	//
	// The flush does network I/O (bounded by metricShutdownTimeout), so no
	// Dialer lock is held while it runs: d.lock is needed by connections'
	// error paths (removeCached) and metricsMu by any Dial that raced
	// Close. d.closed is already closed, so metricRecorder cannot add to the
	// map after this snapshot. Recorders are shut down concurrently so that a
	// process with many instances (e.g. the Auth Proxy) flushes all of them
	// within the budget rather than running out of time partway through.
	d.metricsMu.Lock()
	recorders := d.metricRecorders
	d.metricRecorders = map[metricRecorderKey]tel.MetricRecorder{}
	d.metricsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), metricShutdownTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for key, mr := range recorders {
		wg.Add(1)
		go func(key metricRecorderKey, mr tel.MetricRecorder) {
			defer wg.Done()
			// Best effort: an error here isn't actionable by the caller.
			if err := mr.Shutdown(ctx); err != nil {
				d.logger.Debugf(ctx, "[%v] failed to shut down metric recorder: %v", key, err)
			}
		}(key, mr)
	}
	wg.Wait()
	// Also wait for any replaced recorders still flushing in the background
	// before closing the client they export through.
	d.metricsWG.Wait()
	if d.mClient != nil {
		if err := d.mClient.Close(); err != nil {
			d.logger.Debugf(ctx, "failed to close metric client: %v", err)
		}
	}
	return nil
}

// metricRecorderKey identifies the Cloud SQL instance a metric recorder
// belongs to. It deliberately excludes the domain name: an instance dialed
// both by DNS name and by connection name is one monitored resource and must
// have one recorder, otherwise two cumulative series with identical labels
// would be written for it and the Monitoring API would reject them as
// conflicting.
type metricRecorderKey struct {
	project, region, name string
}

func newMetricRecorderKey(cn instance.ConnName) metricRecorderKey {
	return metricRecorderKey{project: cn.Project(), region: cn.Region(), name: cn.Name()}
}

func (k metricRecorderKey) String() string {
	return k.project + ":" + k.region + ":" + k.name
}

// createKey creates a key for the cache from an instance.ConnName.
// An instance.ConnName uniquely identifies a connection using
// project:region:instance + domainName. However, in the dialer cache,
// we want to to identify entries either by project:region:instance, or
// by domainName, but not the combination of the two.
func createKey(cn instance.ConnName) cacheKey {
	if cn.HasDomainName() {
		return cacheKey{domainName: cn.DomainName()}
	}
	return cacheKey{
		name:    cn.Name(),
		project: cn.Project(),
		region:  cn.Region(),
	}
}

// connectionInfoCache is a helper function for returning the appropriate
// connection info Cache in a threadsafe way. It will create a new cache,
// modify the existing one, or leave it unchanged as needed.
//
// It returns a *monitoredCache, a bool indicating a cache hit, and an error.
func (d *Dialer) connectionInfoCache(
	ctx context.Context, cn instance.ConnName, useIAMAuthN *bool,
) (*monitoredCache, error) {
	k := createKey(cn)

	d.lock.RLock()
	c, ok := d.cache[k]
	d.lock.RUnlock()

	if ok && !c.isClosed() {
		c.UpdateRefresh(useIAMAuthN)
		return c, nil
	}

	d.lock.Lock()
	defer d.lock.Unlock()

	// Recheck to ensure instance wasn't created or changed between locks
	c, ok = d.cache[k]

	// c exists and is not closed
	if ok && !c.isClosed() {
		c.UpdateRefresh(useIAMAuthN)
		return c, nil
	}

	// Create a new instance of monitoredCache
	var useIAMAuthNDial bool
	if useIAMAuthN != nil {
		useIAMAuthNDial = *useIAMAuthN
	}
	d.logger.Debugf(ctx, "[%v] Connection info added to cache", cn.String())
	rsaKey, err := d.keyGenerator.rsaKey()
	if err != nil {
		return nil, err
	}
	dialFunc := func(ctx context.Context, ci cloudsql.ConnectionInfo, ipType string) (net.Conn, error) {
		probeCfg := d.defaultDialConfig
		if ipType != "" {
			probeCfg.connectionType = ipType
		}
		conn, _, err := d.connect(ctx, cn, ci, probeCfg)
		return conn, err
	}
	var cache connectionInfoCache
	if d.lazyRefresh {
		cache = cloudsql.NewLazyRefreshCache(
			cn,
			d.logger,
			d.sqladmin, rsaKey,
			d.refreshTimeout, d.iamTokenProvider,
			d.dialerID, useIAMAuthNDial,
			dialFunc,
		)
	} else {
		cache = cloudsql.NewRefreshAheadCache(
			cn,
			d.logger,
			d.sqladmin, rsaKey,
			d.refreshTimeout, d.iamTokenProvider,
			d.dialerID, useIAMAuthNDial,
			dialFunc,
		)
	}
	c = newMonitoredCache(cache, cn, d.failoverPeriod, d.resolver, d.logger)
	d.cache[k] = c

	return c, nil
}

// newMDXRequest builds a metadata exchange request based on the connection
// info and dialer configuration. It returns nil if metadata exchange is
// disabled, not supported by the instance, or if the client protocol is not
// specified or supported.
func newMDXRequest(ci cloudsql.ConnectionInfo, cfg dialConfig, metadataExchangeDisabled bool) *mdx.MetadataExchangeRequest {
	if metadataExchangeDisabled ||
		len(ci.MdxProtocolSupport) == 0 ||
		cfg.mdxClientProtocolType == "" {
		return nil
	}

	var cpt mdx.MetadataExchangeRequest_ClientProtocolType
	if slices.Contains(ci.MdxProtocolSupport, "CLIENT_PROTOCOL_TYPE") {
		switch cfg.mdxClientProtocolType {
		case cloudsql.ClientProtocolTCP:
			cpt = mdx.MetadataExchangeRequest_TCP
		case cloudsql.ClientProtocolUDS:
			cpt = mdx.MetadataExchangeRequest_UDS
		case cloudsql.ClientProtocolTLS:
			cpt = mdx.MetadataExchangeRequest_TLS
		default:
			cpt = mdx.MetadataExchangeRequest_CLIENT_PROTOCOL_TYPE_UNSPECIFIED
		}
	}
	if cpt == mdx.MetadataExchangeRequest_CLIENT_PROTOCOL_TYPE_UNSPECIFIED {
		return nil
	}

	return mdx.MetadataExchangeRequest_builder{ClientProtocolType: &cpt}.Build()
}

func isPreconditionFailed(err error) bool {
	if s, ok := status.FromError(err); ok {
		return s.Code() == codes.FailedPrecondition
	}
	return false
}

func isResourceExhaustedError(err error) bool {
	if s, ok := status.FromError(err); ok {
		return s.Code() == codes.ResourceExhausted
	}
	return false
}

type resourceExhaustedTrackingConn struct {
	net.Conn
	onResourceExhausted func(error)
	onSuccess           func()
	onInitialError      func(error)
	firstReadDone       bool
	mu                  sync.Mutex
}

func (c *resourceExhaustedTrackingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err == nil || errors.Is(err, io.EOF) {
		c.mu.Lock()
		if !c.firstReadDone {
			c.firstReadDone = true
			c.mu.Unlock()
			c.onSuccess()
		} else {
			c.mu.Unlock()
		}
	} else {
		if isResourceExhaustedError(err) {
			c.onResourceExhausted(err)
		}
		c.mu.Lock()
		first := !c.firstReadDone
		c.firstReadDone = true
		c.mu.Unlock()
		if first && c.onInitialError != nil {
			c.onInitialError(err)
		}
	}
	return n, err
}

func (c *resourceExhaustedTrackingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if err != nil {
		if isResourceExhaustedError(err) {
			c.onResourceExhausted(err)
		}
		c.mu.Lock()
		first := !c.firstReadDone
		c.firstReadDone = true
		c.mu.Unlock()
		if first && c.onInitialError != nil {
			c.onInitialError(err)
		}
	}
	return n, err
}

func cooldownBackoff(base time.Duration, attempt int) time.Duration {
	const multi = 1.618
	exp := float64(attempt-1) + mathrand.Float64()
	return time.Duration(float64(base) * math.Pow(multi, exp))
}
