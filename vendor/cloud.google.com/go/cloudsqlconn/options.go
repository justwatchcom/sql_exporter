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
	"crypto/rsa"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/auth/oauth2adapt"
	"cloud.google.com/go/cloudsqlconn/debug"
	"cloud.google.com/go/cloudsqlconn/errtype"
	"cloud.google.com/go/cloudsqlconn/instance"
	"cloud.google.com/go/cloudsqlconn/internal/cloudsql"
	"cloud.google.com/go/cloudsqlconn/internal/sqldataclient"
	"cloud.google.com/go/cloudsqlconn/internal/tel"
	"golang.org/x/oauth2"
	apiopt "google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"
)

// An Option is an option for configuring a Dialer.
type Option func(d *dialerConfig)

type dialerConfig struct {
	rsaKey       *rsa.PrivateKey
	sqladminOpts []apiopt.ClientOption
	// clientOpts are options to configure any Google Cloud API client. They
	// should not include any CloudSQL-specific configuration.
	clientOpts []apiopt.ClientOption
	// credentialsJSON holds the raw credentials provided via
	// WithCredentialsJSON or WithCredentialsFile, so that additional
	// credentials with different scopes can be derived from them.
	credentialsJSON       []byte
	dialOpts              []DialOption
	dialFunc              func(ctx context.Context, network, addr string) (net.Conn, error)
	refreshTimeout        time.Duration
	useIAMAuthN           bool
	logger                debug.ContextLogger
	lazyRefresh           bool
	sqlDataEndpoint       string
	clientUniverseDomain  string
	quotaProject          string
	authCredentials       *auth.Credentials
	iamLoginTokenProvider auth.TokenProvider
	apiTokenProvider      auth.TokenProvider
	useragents            []string
	// applicationName is the application_name metric label. It is
	// user-declared via WithApplicationName and left empty otherwise; it is
	// deliberately never inferred from the environment (VM name, pod name,
	// service name): compute_platform and client_uid already describe where
	// and on which instance the connector runs, and inferred values would be
	// inconsistent across platforms.
	applicationName string
	// computePlatform, when set, overrides the auto-detected compute_platform
	// metric label. It is validated by WithComputePlatform.
	computePlatform                 string
	setAdminAPIEndpoint             bool
	setCredentials                  bool
	setHTTPClient                   bool
	setTokenSource                  bool
	setIAMAuthNTokenSource          bool
	resolver                        instance.ConnectionNameResolver
	failoverPeriod                  time.Duration
	metadataExchangeDisabled        bool
	dnsResolver                     cloudsql.NetResolver
	sqlDataDialer                   sqldataclient.Dialer
	sqlDataStreamTimeout            time.Duration
	resourceExhaustedCooldownPeriod time.Duration
	useDNSNameResolver              bool
	enableBuiltInMetrics            bool
	// builtInMetricsSet records that WithBuiltInMetrics was called, so the
	// default can be adjusted for non-default universes without overriding
	// an explicit choice.
	builtInMetricsSet bool
	// err tracks any dialer options that may have failed.
	err    error
	client *sqladmin.Service
}

// WithOptions turns a list of Option's into a single Option.
func WithOptions(opts ...Option) Option {
	return func(d *dialerConfig) {
		for _, opt := range opts {
			opt(d)
		}
	}
}

// WithSQLDataStreamTimeout sets the maximum time allowed for a stream to SQLDataService
func WithSQLDataStreamTimeout(timeout time.Duration) Option {
	return func(d *dialerConfig) {
		d.sqlDataStreamTimeout = timeout
	}
}

// WithResourceExhaustedCooldownPeriod sets the cooldown period after a ResourceExhausted error.
func WithResourceExhaustedCooldownPeriod(period time.Duration) Option {
	return func(d *dialerConfig) {
		d.resourceExhaustedCooldownPeriod = period
	}
}

// WithCredentialsFile returns an Option that specifies a service account
// or refresh token JSON credentials file to be used as the basis for
// authentication.
func WithCredentialsFile(filename string) Option {
	return func(d *dialerConfig) {
		b, err := os.ReadFile(filename)
		if err != nil {
			d.err = errtype.NewConfigError(err.Error(), "n/a")
			return
		}
		opt := WithCredentialsJSON(b)
		opt(d)
	}
}

// WithCredentialsJSON returns an Option that specifies a service account
// or refresh token JSON credentials to be used as the basis for authentication.
func WithCredentialsJSON(b []byte) Option {
	return func(d *dialerConfig) {
		c, err := credentials.DetectDefault(&credentials.DetectOptions{
			Scopes:          []string{sqladmin.SqlserviceAdminScope},
			CredentialsJSON: b,
		})
		if err != nil {
			d.err = errtype.NewConfigError(err.Error(), "n/a")
			return
		}
		d.authCredentials = c
		// Create another set of credentials scoped to login only
		scoped, err := credentials.DetectDefault(&credentials.DetectOptions{
			Scopes:          []string{iamLoginScope},
			CredentialsJSON: b,
		})
		if err != nil {
			d.err = errtype.NewConfigError(err.Error(), "n/a")
			return
		}
		d.iamLoginTokenProvider = scoped.TokenProvider
		d.credentialsJSON = b
		d.setCredentials = true
	}
}

// WithUserAgent returns an Option that sets the User-Agent.
func WithUserAgent(ua string) Option {
	return func(d *dialerConfig) {
		d.useragents = append(d.useragents, ua)
	}
}

// WithApplicationName returns an Option that sets the application_name label
// of the built-in metrics, which identifies the application and groups its
// metrics across all of its instances (for example "checkout-api").
//
// The label is only ever what the application declares here; when it is not
// set it is left empty. The connector does not infer a value from the
// environment: compute_platform already says where the connector runs and
// client_uid already tells instances apart, and a per-instance name (VM or
// pod) would split a service's metrics into one series per instance.
func WithApplicationName(name string) Option {
	return func(d *dialerConfig) {
		d.applicationName = name
	}
}

// ComputePlatform identifies the platform an application runs on. It is
// reported as the compute_platform label of built-in metrics.
//
// The set of valid values is closed: only the ComputePlatform* constants
// defined in this package are accepted by WithComputePlatform. This keeps the
// cardinality of the metric's monitored resource bounded.
type ComputePlatform string

const (
	// ComputePlatformGCE identifies Compute Engine.
	ComputePlatformGCE ComputePlatform = tel.PlatformGCE
	// ComputePlatformGKE identifies Google Kubernetes Engine.
	ComputePlatformGKE ComputePlatform = tel.PlatformGKE
	// ComputePlatformCloudRun identifies Cloud Run.
	ComputePlatformCloudRun ComputePlatform = tel.PlatformCloudRun
	// ComputePlatformAppEngine identifies App Engine.
	ComputePlatformAppEngine ComputePlatform = tel.PlatformAppEngine
	// ComputePlatformCloudFunctions identifies Cloud Functions.
	ComputePlatformCloudFunctions ComputePlatform = tel.PlatformCloudFunctions
	// ComputePlatformAIStudio identifies applications deployed by Google AI
	// Studio. This platform cannot be auto-detected and must be set
	// explicitly with WithComputePlatform.
	ComputePlatformAIStudio ComputePlatform = tel.PlatformAIStudio
)

// computePlatforms is the ordered list of valid ComputePlatform values. It is
// the single source of truth for ComputePlatforms, ParseComputePlatform, and
// the error messages of WithComputePlatform.
var computePlatforms = []ComputePlatform{
	ComputePlatformGCE,
	ComputePlatformGKE,
	ComputePlatformCloudRun,
	ComputePlatformAppEngine,
	ComputePlatformCloudFunctions,
	ComputePlatformAIStudio,
}

// ComputePlatforms returns every valid ComputePlatform value, in a stable
// order. It is intended for help text and validation in clients that accept
// the platform as a string, such as a command-line flag.
func ComputePlatforms() []ComputePlatform {
	return slices.Clone(computePlatforms)
}

// ParseComputePlatform converts a string, for example a command-line flag
// value, to a ComputePlatform. Leading and trailing whitespace is ignored and
// matching is case-insensitive, so "ai_studio", "AI_STUDIO", and " ai_studio "
// all return ComputePlatformAIStudio. Any other value returns an error that
// lists the valid values.
//
// Use this rather than a type conversion such as ComputePlatform(s): a
// conversion accepts any string and is only rejected later, when NewDialer
// validates the option.
func ParseComputePlatform(s string) (ComputePlatform, error) {
	p := ComputePlatform(strings.ToLower(strings.TrimSpace(s)))
	if slices.Contains(computePlatforms, p) {
		return p, nil
	}
	return "", fmt.Errorf("invalid compute platform %q; must be one of: %s", s, validComputePlatformList())
}

// validComputePlatformList returns the valid values as a comma-separated list.
func validComputePlatformList() string {
	names := make([]string, len(computePlatforms))
	for i, p := range computePlatforms {
		names[i] = string(p)
	}
	return strings.Join(names, ", ")
}

// WithComputePlatform returns an Option that overrides the auto-detected
// compute platform reported in built-in metrics. Use it when the Dialer is
// embedded in a client that knows its deployment environment better than
// runtime heuristics can determine (for example, the Cloud SQL Auth Proxy
// running as a sidecar for an AI Studio application).
//
// Only the ComputePlatform* constants are accepted; any other value causes
// NewDialer to return a configuration error. To accept the platform as a
// string (for example from a command-line flag), convert it with
// ParseComputePlatform first. When this option is not provided, the platform
// is detected from the runtime environment.
func WithComputePlatform(p ComputePlatform) Option {
	return func(d *dialerConfig) {
		if !slices.Contains(computePlatforms, p) {
			d.err = errtype.NewConfigError(
				fmt.Sprintf("invalid compute platform %q; must be one of: %s", p, validComputePlatformList()),
				"n/a")
			return
		}
		d.computePlatform = string(p)
	}
}

// WithDefaultDialOptions returns an Option that specifies the default
// DialOptions used.
func WithDefaultDialOptions(opts ...DialOption) Option {
	return func(d *dialerConfig) {
		d.dialOpts = append(d.dialOpts, opts...)
	}
}

// WithTokenSource returns an Option that specifies an OAuth2 token source to be
// used as the basis for authentication.
//
// When Auth IAM AuthN is enabled, use WithIAMAuthNTokenSources or WithIAMAuthNCredentials to set the token
// source for login tokens separately from the API client token source.
//
// You may only use one of the following options:
// WithIAMAuthNCredentials, WithIAMAuthNTokenSources, WithCredentials, WithTokenSource
func WithTokenSource(s oauth2.TokenSource) Option {
	return func(d *dialerConfig) {
		d.setTokenSource = true
		d.setCredentials = true
		d.apiTokenProvider = oauth2adapt.TokenProviderFromTokenSource(s)
		d.sqladminOpts = append(d.sqladminOpts, apiopt.WithTokenSource(s))
	}
}

// WithIAMAuthNTokenSources sets the oauth2.TokenSource for the API client and a
// second token source for IAM AuthN login tokens. The API client token source
// should have the following scopes:
//
//  1. https://www.googleapis.com/auth/sqlservice.admin, and
//  2. https://www.googleapis.com/auth/cloud-platform
//
// The IAM AuthN token source on the other hand should only have:
//
//  1. https://www.googleapis.com/auth/sqlservice.login.
//
// Prefer this option over WithTokenSource or WithCredentials when using IAM AuthN which does not
// distinguish between the two token sources.
//
// You may only use one of the following options:
// WithIAMAuthNCredentials, WithIAMAuthNTokenSources, WithCredentials, WithTokenSource
func WithIAMAuthNTokenSources(apiTS, iamLoginTS oauth2.TokenSource) Option {
	return func(d *dialerConfig) {
		d.setIAMAuthNTokenSource = true
		d.setCredentials = true
		d.iamLoginTokenProvider = oauth2adapt.TokenProviderFromTokenSource(iamLoginTS)
		d.apiTokenProvider = oauth2adapt.TokenProviderFromTokenSource(apiTS)
		d.sqladminOpts = append(d.sqladminOpts, apiopt.WithTokenSource(apiTS))
	}
}

// WithCredentials returns an Option that specifies an OAuth2 token source to be
// used as the basis for authentication.
//
// When Auth IAM AuthN is enabled, use WithIAMAuthNTokenSources to set the token
// source for login tokens separately from the API client token source.
//
// You may only use one of the following options:
// WithIAMAuthNCredentials, WithIAMAuthNTokenSources, WithCredentials, WithTokenSource
func WithCredentials(c *auth.Credentials) Option {
	return func(d *dialerConfig) {
		d.setTokenSource = true
		d.setCredentials = true
		d.apiTokenProvider = c.TokenProvider
		d.sqladminOpts = append(d.sqladminOpts, apiopt.WithAuthCredentials(c))
	}
}

// WithIAMAuthNCredentials sets the oauth2.TokenSource for the API client and a
// second token source for IAM AuthN login tokens. The API client token source
// should have the following scopes:
//
//  1. https://www.googleapis.com/auth/sqlservice.admin, and
//  2. https://www.googleapis.com/auth/cloud-platform
//
// The IAM AuthN token source on the other hand should only have:
//
//  1. https://www.googleapis.com/auth/sqlservice.login.
//
// Prefer this option over WithTokenSource or WithCredentials when using IAM AuthN which does not
// distinguish between the two token sources.
//
// You may only use one of the following options:
// WithIAMAuthNCredentials, WithIAMAuthNTokenSources, WithCredentials, WithTokenSource
func WithIAMAuthNCredentials(apiCreds, iamCreds *auth.Credentials) Option {
	return func(d *dialerConfig) {
		d.setIAMAuthNTokenSource = true
		d.setCredentials = true
		d.iamLoginTokenProvider = iamCreds
		d.apiTokenProvider = apiCreds.TokenProvider
		d.sqladminOpts = append(d.sqladminOpts, apiopt.WithAuthCredentials(apiCreds))
	}
}

// WithRSAKey returns an Option that specifies a rsa.PrivateKey used to represent the client.
func WithRSAKey(k *rsa.PrivateKey) Option {
	return func(d *dialerConfig) {
		d.rsaKey = k
	}
}

// WithRefreshTimeout returns an Option that sets a timeout on refresh
// operations. Defaults to 60s.
func WithRefreshTimeout(t time.Duration) Option {
	return func(d *dialerConfig) {
		d.refreshTimeout = t
	}
}

// WithHTTPClient configures the underlying SQL Admin API client with the
// provided HTTP client. This option is generally unnecessary except for
// advanced use-cases.
func WithHTTPClient(client *http.Client) Option {
	return func(d *dialerConfig) {
		d.sqladminOpts = append(d.sqladminOpts, apiopt.WithHTTPClient(client))
		d.setHTTPClient = true
	}
}

// WithAdminAPIEndpoint configures the underlying SQL Admin API client to use
// the provided URL.
func WithAdminAPIEndpoint(url string) Option {
	return func(d *dialerConfig) {
		d.sqladminOpts = append(d.sqladminOpts, apiopt.WithEndpoint(url))
		d.setAdminAPIEndpoint = true
	}
}

// WithUniverseDomain configures the underlying SQL Admin API client to use
// the provided universe domain. Enables Trusted Partner Cloud (TPC).
func WithUniverseDomain(ud string) Option {
	return func(d *dialerConfig) {
		d.sqladminOpts = append(d.sqladminOpts, apiopt.WithUniverseDomain(ud))
		d.clientUniverseDomain = ud
	}
}

// WithQuotaProject returns an Option that specifies the project used for quota and billing purposes.
func WithQuotaProject(p string) Option {
	return func(cfg *dialerConfig) {
		cfg.quotaProject = p
	}
}

// WithDialFunc configures the function used to connect to the address on the
// named network. This option is generally unnecessary except for advanced
// use-cases. The function is used for all invocations of Dial. To configure
// a dial function per individual calls to dial, use WithOneOffDialFunc.
func WithDialFunc(dial func(ctx context.Context, network, addr string) (net.Conn, error)) Option {
	return func(d *dialerConfig) {
		d.dialFunc = dial
	}
}

// WithIAMAuthN enables automatic IAM Authentication. If no token source has
// been configured (such as with WithTokenSource, WithCredentialsFile, etc), the
// dialer will use the default token source as defined by
// https://pkg.go.dev/golang.org/x/oauth2/google#FindDefaultCredentialsWithParams.
//
// For documentation on automatic IAM Authentication, see
// https://cloud.google.com/sql/docs/postgres/authentication.
func WithIAMAuthN() Option {
	return func(d *dialerConfig) {
		d.useIAMAuthN = true
	}
}

// WithResolver replaces the default resolver with an alternate
// implementation to resolve the name in the database DSN to a Cloud SQL
// instance.
func WithResolver(r instance.ConnectionNameResolver) Option {
	return func(d *dialerConfig) {
		d.resolver = r
	}
}

// WithDNSResolver replaces the default resolver (which only resolves instance
// names) with the DNSResolver, which will attempt to first parse the instance
// name, and then will attempt to resolve the DNS TXT record to determine
// the instance name.
//
// First, add a record for your Cloud SQL instance to a **private** DNS server
// or a private Google Cloud DNS Zone used by your application.
//
// **Note:** You are strongly discouraged from adding DNS records for your
// Cloud SQL instances to a public DNS server. This would allow anyone on the
// internet to discover the Cloud SQL instance name.
//
// For example: suppose you wanted to use the domain name
// `prod-db.mycompany.example.com` to connect to your database instance
// `my-project:region:my-instance`. You would create the following DNS record:
//
//   - Record type: `TXT`
//   - Name: `prod-db.mycompany.example.com` – This is the domain name used by
//     the application
//   - Value: `my-project:region:my-instance` – This is the instance name
func WithDNSResolver() Option {
	return func(d *dialerConfig) {
		d.useDNSNameResolver = true
	}
}

// WithFailoverPeriod will cause the connector to periodically check the SRV DNS
// records of instance configured using DNS names. By default, this is 30
// seconds. If this is set to 0, the connector will only check for domain name
// changes when establishing a new connection.
func WithFailoverPeriod(f time.Duration) Option {
	return func(d *dialerConfig) {
		d.failoverPeriod = f
	}
}

// WithDisableMetadataExchange enables or disables the dialer from sending the
// metadata exchange message to the instance on each connection. This
// is necessary for MySQL instances with caching_sha2_passwords. Metadata Exchange
// is enabled by default for instances that accept MDX.
func WithDisableMetadataExchange() Option {
	return func(cfg *dialerConfig) {
		cfg.metadataExchangeDisabled = true
	}
}

// WithBuiltInMetrics enables or disables the built-in metric export. When
// enabled, the Dialer reports on its internal operations (connection
// attempts, dial latency, open connections, connection durations) to Cloud
// Monitoring under the cloudsql.googleapis.com/client/connector metric prefix,
// using the cloudsql.googleapis.com/InstanceClient monitored resource. These
// metrics help Cloud SQL improve performance and identify client connectivity
// problems.
//
// Built-in metrics are enabled by default and are in BETA. Pass false to turn
// them off. In a non-default Cloud universe (a universe domain other than
// googleapis.com) they are off by default and must be enabled explicitly.
//
// Metrics never affect connectivity. If the exporter cannot be created, or an
// export fails (for example, a missing IAM permission), connections continue
// to work: the first permanent failure is reported once per process on the
// standard logger with remediation steps, further failures are reported
// through the debug logger only, and exports are retried with backoff.
//
// The identity used by the Dialer must have the roles/monitoring.metricWriter
// role on the Cloud SQL instance's project for the export to succeed.
//
// The metric export uses the same identity as the Dialer's API calls. When
// the Dialer detects credentials itself (Application Default Credentials,
// WithCredentialsFile, or WithCredentialsJSON), it requests the
// https://www.googleapis.com/auth/monitoring.write scope for the export.
// When a token source or credentials are supplied directly (WithTokenSource,
// WithCredentials, WithIAMAuthNTokenSources, WithIAMAuthNCredentials), the
// supplied API token source is used as-is and must carry either the
// monitoring.write or the cloud-platform scope.
func WithBuiltInMetrics(enabled bool) Option {
	return func(cfg *dialerConfig) {
		cfg.enableBuiltInMetrics = enabled
		cfg.builtInMetricsSet = true
	}
}

// withMetricClientOptions appends options for the built-in metrics client.
// It is unexported and intended for tests, e.g. to point the client at an
// in-memory gRPC server.
func withMetricClientOptions(opts ...apiopt.ClientOption) Option {
	return func(cfg *dialerConfig) {
		cfg.clientOpts = append(cfg.clientOpts, opts...)
	}
}

type debugLoggerWithoutContext struct {
	logger debug.Logger
}

// Debugf implements debug.ContextLogger.
func (d *debugLoggerWithoutContext) Debugf(_ context.Context, format string, args ...interface{}) {
	d.logger.Debugf(format, args...)
}

var _ debug.ContextLogger = new(debugLoggerWithoutContext)

// WithDebugLogger configures a debug lgoger for reporting on internal
// operations. By default the debug logger is disabled.
//
// Prefer WithContextDebugLogger instead
func WithDebugLogger(l debug.Logger) Option {
	return func(d *dialerConfig) {
		d.logger = &debugLoggerWithoutContext{l}
	}
}

// WithContextDebugLogger configures a debug logger for reporting on internal
// operations. By default the debug logger is disabled.
func WithContextDebugLogger(l debug.ContextLogger) Option {
	return func(d *dialerConfig) {
		d.logger = l
	}
}

// WithLazyRefresh configures the dialer to refresh certificates on an
// as-needed basis. If a certificate is expired when a connection request
// occurs, the Go Connector will block the attempt and refresh the certificate
// immediately. This option is useful when running the Go Connector in
// environments where the CPU may be throttled, thus preventing a background
// goroutine from running consistently (e.g., in Cloud Run the CPU is throttled
// outside of a request context causing the background refresh to fail).
func WithLazyRefresh() Option {
	return func(d *dialerConfig) {
		d.lazyRefresh = true
	}
}

// WithSQLDataEndpoint allows callers to override the default endpoint for the
// SQL Data API. Typically, this is for development only and should be ignored
// otherwise.
func WithSQLDataEndpoint(endpoint string) Option {
	return func(d *dialerConfig) {
		d.sqlDataEndpoint = endpoint
	}
}

// WithSQLDataDialer allows callers to override the default dialer for the SQLDataService
// with an alternate dialer.
func WithSQLDataDialer(dialer sqldataclient.Dialer) Option {
	return func(d *dialerConfig) {
		d.sqlDataDialer = dialer
	}
}

// A DialOption is an option for configuring how a Dialer's Dial call is executed.
type DialOption func(d *dialConfig)

type dialConfig struct {
	dialFunc              func(ctx context.Context, network, addr string) (net.Conn, error)
	connectionType        string
	tcpKeepAlive          time.Duration
	useIAMAuthN           bool
	mdxClientProtocolType string
	setSQLData            bool
	setConnectTunnel      bool
	// This will be set before options
	client *sqladmin.Service
}

// DialOptions turns a list of DialOption instances into an DialOption.
func DialOptions(opts ...DialOption) DialOption {
	return func(cfg *dialConfig) {
		for _, opt := range opts {
			opt(cfg)
		}
	}
}

// WithOneOffDialFunc configures the dial function on a one-off basis for an
// individual call to Dial. To configure a dial function across all invocations
// of Dial, use WithDialFunc.
func WithOneOffDialFunc(dial func(ctx context.Context, network, addr string) (net.Conn, error)) DialOption {
	return func(c *dialConfig) {
		c.dialFunc = dial
	}
}

// WithTCPKeepAlive returns a DialOption that specifies the tcp keep alive period for the connection returned by Dial.
func WithTCPKeepAlive(d time.Duration) DialOption {
	return func(cfg *dialConfig) {
		cfg.tcpKeepAlive = d
	}
}

// WithPublicIP returns a DialOption that specifies a public IP will be used to connect.
func WithPublicIP() DialOption {
	return func(cfg *dialConfig) {
		cfg.connectionType = cloudsql.PublicIP
	}
}

// WithPrivateIP returns a DialOption that specifies a private IP (VPC) will be used to connect.
func WithPrivateIP() DialOption {
	return func(cfg *dialConfig) {
		cfg.connectionType = cloudsql.PrivateIP
	}
}

// WithPSC returns a DialOption that specifies a PSC endpoint will be used to connect.
func WithPSC() DialOption {
	return func(cfg *dialConfig) {
		cfg.connectionType = cloudsql.PSC
	}
}

// WithAutoIP returns a DialOption that selects the public IP if available and
// otherwise falls back to private IP. This option is present for backwards
// compatibility only and is not recommended for use in production.
func WithAutoIP() DialOption {
	return func(cfg *dialConfig) {
		cfg.connectionType = cloudsql.AutoIP
	}
}

// WithSQLData returns a DialOption that routes traffic through the SQL Data API
// endpoint. This dial option allows the dialer to reach the target instance
// when there is not a network path.
func WithSQLData() DialOption {
	return func(cfg *dialConfig) {
		cfg.connectionType = cloudsql.SQLData
		cfg.setSQLData = true
	}
}

// WithConnectTunnel returns a DialOption that routes traffic through the SQL Data API
// endpoint without falling back to direct IP connections if the backend fails.
func WithConnectTunnel() DialOption {
	return func(cfg *dialConfig) {
		cfg.connectionType = cloudsql.ConnectTunnel
		cfg.setConnectTunnel = true
	}
}

// WithDialIAMAuthN allows you to enable or disable IAM Authentication for this
// instance as described in the documentation for WithIAMAuthN. This value will
// override the Dialer-level configuration set with WithIAMAuthN.
//
// WARNING: This DialOption can cause a new Refresh operation to be triggered.
// Toggling this option on or off between Dials may cause increased API usage
// and/or delayed connection attempts.
func WithDialIAMAuthN(b bool) DialOption {
	return func(cfg *dialConfig) {
		cfg.useIAMAuthN = b
	}
}

// WithMdxClientProtocolType controls client protocol type is sent to the server
// in the metadata exchange request. This may be one of cloudsql.ClientProtocolTLS cloudsql.ClientProtocolTCP
// or cloudsql.ClientProtocolUDS. If this is empty, it will be omitted from the MDX request.
// This is important for MySQL clients that use caching_sha2_password.
// Valid values: "tls" "tcp" "uds"
func WithMdxClientProtocolType(s string) DialOption {
	return func(cfg *dialConfig) {
		cfg.mdxClientProtocolType = s
	}
}
