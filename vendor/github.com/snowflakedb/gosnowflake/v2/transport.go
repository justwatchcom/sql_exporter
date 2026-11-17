package gosnowflake

import (
	"cmp"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	sfconfig "github.com/snowflakedb/gosnowflake/v2/internal/config"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"
)

type transportConfigs interface {
	forTransportType(transportType transportType) *transportConfig
}

type transportType int

const (
	transportTypeOAuth transportType = iota
	transportTypeCloudProvider
	transportTypeOCSP
	transportTypeCRL
	transportTypeSnowflake
	transportTypeWIF
)

var defaultTransportConfigs transportConfigs = newDefaultTransportConfigs()

// transportConfig holds the configuration for creating HTTP transports
type transportConfig struct {
	MaxIdleConns    int
	IdleConnTimeout time.Duration
	DialTimeout     time.Duration
	KeepAlive       time.Duration
	DisableProxy    bool
}

// TransportFactory handles creation of HTTP transports with different validation modes
type transportFactory struct {
	config    *Config
	telemetry *snowflakeTelemetry
}

func (tf *transportConfig) String() string {
	return fmt.Sprintf("{MaxIdleConns: %d, IdleConnTimeout: %s, DialTimeout: %s, KeepAlive: %s}",
		tf.MaxIdleConns,
		tf.IdleConnTimeout,
		tf.DialTimeout,
		tf.KeepAlive)
}

// NewTransportFactory creates a new transport factory
func newTransportFactory(config *Config, telemetry *snowflakeTelemetry) *transportFactory {
	return &transportFactory{config: config, telemetry: telemetry}
}

func (tf *transportFactory) createProxy(transportConfig *transportConfig) func(*http.Request) (*url.URL, error) {
	if transportConfig.DisableProxy {
		return nil
	}
	logger.Debug("Initializing proxy configuration")
	if tf.config == nil || tf.config.ProxyHost == "" {
		logger.Debug("Config is empty or ProxyHost is not set. Using proxy settings from environment variables.")
		return http.ProxyFromEnvironment
	}

	// JoinHostPort brackets literal IPv6 addresses, which a plain "host:port"
	// concatenation would leave unparseable. Users who already bracketed the
	// address themselves must not end up double-bracketed.
	proxyHost := strings.Trim(tf.config.ProxyHost, "[]")
	connectionProxy := &url.URL{
		Scheme: tf.config.ProxyProtocol,
		Host:   net.JoinHostPort(proxyHost, strconv.Itoa(tf.config.ProxyPort)),
	}
	if tf.config.ProxyUser != "" && tf.config.ProxyPassword != "" {
		connectionProxy.User = url.UserPassword(tf.config.ProxyUser, tf.config.ProxyPassword)
		logger.Infof("Connection Proxy is configured: Connection proxy %v: ****@%v NoProxy:%v", tf.config.ProxyUser, connectionProxy.Host, tf.config.NoProxy)
	} else {
		logger.Infof("Connection Proxy is configured: Connection proxy: %v NoProxy: %v", connectionProxy.Host, tf.config.NoProxy)
	}

	cfg := httpproxy.Config{
		HTTPSProxy: connectionProxy.String(),
		HTTPProxy:  connectionProxy.String(),
		NoProxy:    tf.config.NoProxy,
	}
	proxyURLFunc := cfg.ProxyFunc()

	return func(req *http.Request) (*url.URL, error) {
		return proxyURLFunc(req.URL)
	}
}

// createBaseTransport creates a base HTTP transport with the given configuration
func (tf *transportFactory) createBaseTransport(transportConfig *transportConfig, tlsConfig *tls.Config) (*http.Transport, error) {
	logger.Debugf("Create a new Base Transport with transportConfig %v", transportConfig.String())

	// Apply minimum TLS version from environment variable
	var err error
	tlsConfig, err = sfconfig.ApplyMinTLSVersion(tlsConfig)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{
		Timeout:   transportConfig.DialTimeout,
		KeepAlive: transportConfig.KeepAlive,
	}

	defaultTransport := http.DefaultTransport.(*http.Transport)
	return &http.Transport{
		TLSClientConfig:     tlsConfig,
		MaxIdleConns:        cmp.Or(transportConfig.MaxIdleConns, defaultTransport.MaxIdleConns),
		MaxIdleConnsPerHost: cmp.Or(transportConfig.MaxIdleConns, defaultTransport.MaxIdleConns),
		IdleConnTimeout:     cmp.Or(transportConfig.IdleConnTimeout, defaultTransport.IdleConnTimeout),
		Proxy:               tf.createProxy(transportConfig),
		DialContext:         dialer.DialContext,
	}, nil
}

// createOCSPTransport creates a transport with OCSP validation
func (tf *transportFactory) createOCSPTransport(transportConfig *transportConfig) (*http.Transport, error) {
	// Chain OCSP verification with custom TLS config
	ov := newOcspValidator(tf.config)
	tlsConfig, ok := sfconfig.GetTLSConfig(tf.config.TLSConfigName)
	if ok && tlsConfig != nil {
		tlsConfig.VerifyPeerCertificate = tf.chainVerificationCallbacks(tlsConfig.VerifyPeerCertificate, ov.verifyPeerCertificateSerial)
	} else {
		tlsConfig = &tls.Config{
			VerifyPeerCertificate: ov.verifyPeerCertificateSerial,
		}
	}
	return tf.createBaseTransport(transportConfig, tlsConfig)
}

// createNoRevocationTransport creates a transport without certificate revocation checking
func (tf *transportFactory) createNoRevocationTransport(transportConfig *transportConfig) (http.RoundTripper, error) {
	if tf.config != nil && tf.config.Transporter != nil {
		return tf.config.Transporter, nil
	}
	return tf.createBaseTransport(transportConfig, nil)
}

// createCRLValidator creates a CRL validator
func (tf *transportFactory) createCRLValidator() (*crlValidator, error) {
	allowCertificatesWithoutCrlURL := tf.config.CrlAllowCertificatesWithoutCrlURL == ConfigBoolTrue
	transport, err := tf.createNoRevocationTransport(transportConfigFor(transportTypeCRL))
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout:   cmp.Or(tf.config.CrlHTTPClientTimeout, defaultCrlHTTPClientTimeout),
		Transport: transport,
	}
	return newCrlValidator(
		tf.config.CertRevocationCheckMode,
		allowCertificatesWithoutCrlURL,
		tf.config.CrlInMemoryCacheDisabled,
		tf.config.CrlOnDiskCacheDisabled,
		cmp.Or(tf.config.CrlDownloadMaxSize, defaultCrlDownloadMaxSize),
		client,
		tf.telemetry,
	)
}

// createTransport is the main entry point for creating transports
func (tf *transportFactory) createTransport(transportConfig *transportConfig) (http.RoundTripper, error) {
	if tf.config == nil {
		// should never happen in production, only in tests
		logger.Warn("createTransport: got nil Config, using default one")
		return tf.createNoRevocationTransport(transportConfig)
	}

	// if user configured a custom Transporter, prioritize that
	if tf.config.Transporter != nil {
		logger.Debug("createTransport: using Transporter configured by the user")
		// If it's an *http.Transport, try to apply MinTLSVersion to its TLS config
		if httpTransport, ok := tf.config.Transporter.(*http.Transport); ok {
			var err error
			httpTransport.TLSClientConfig, err = sfconfig.ApplyMinTLSVersion(httpTransport.TLSClientConfig)
			if err != nil {
				return nil, err
			}
		}
		return tf.config.Transporter, nil
	}

	// Validate configuration
	if err := tf.validateRevocationConfig(); err != nil {
		return nil, err
	}

	// Handle CRL validation path
	if tf.config.CertRevocationCheckMode != CertRevocationCheckDisabled {
		logger.Debug("createTransport: will perform CRL validation")
		crlValidator, err := tf.createCRLValidator()
		if err != nil {
			return nil, err
		}
		crlCacheCleaner.startPeriodicCacheCleanup()
		// Chain CRL verification with custom TLS config
		tlsConfig, ok := sfconfig.GetTLSConfig(tf.config.TLSConfigName)
		if ok && tlsConfig != nil {
			crlVerify := crlValidator.verifyPeerCertificates
			tlsConfig.VerifyPeerCertificate = tf.chainVerificationCallbacks(tlsConfig.VerifyPeerCertificate, crlVerify)
		} else {
			tlsConfig = &tls.Config{
				VerifyPeerCertificate: crlValidator.verifyPeerCertificates,
			}
		}

		return tf.createBaseTransport(transportConfig, tlsConfig)
	}

	// Handle no revocation checking path
	if !sfconfig.OCSPEnabled(tf.config) {
		logger.Debug("createTransport: skipping OCSP validation")
		// A user-registered TLS config (e.g. certificate pinning via
		// RegisterTLSConfig + tlsConfigName) must still be applied even when
		// revocation checking is disabled, so that disabling OCSP does not also
		// drop the registered configuration and fall back to Go's default
		// verification (SNOW-3649867). Transporter precedence is handled above,
		// so here config is non-nil and Transporter is nil. GetTLSConfig returns
		// a clone, so it is safe to use directly.
		if tlsConfig, ok := sfconfig.GetTLSConfig(tf.config.TLSConfigName); ok && tlsConfig != nil {
			return tf.createBaseTransport(transportConfig, tlsConfig)
		}
		return tf.createNoRevocationTransport(transportConfig)
	}

	logger.Debug("createTransport: will perform OCSP validation")
	return tf.createOCSPTransport(transportConfig)
}

// validateRevocationConfig checks for conflicting revocation settings
func (tf *transportFactory) validateRevocationConfig() error {
	if sfconfig.OCSPEnabled(tf.config) && tf.config.CertRevocationCheckMode != CertRevocationCheckDisabled {
		return errors.New("both OCSP and CRL cannot be enabled at the same time, please disable one of them")
	}
	return nil
}

// chainVerificationCallbacks chains a user's custom verification with the provided verification function
func (tf *transportFactory) chainVerificationCallbacks(orignalVerificationFunc func([][]byte, [][]*x509.Certificate) error, verificationFunc func([][]byte, [][]*x509.Certificate) error) func([][]byte, [][]*x509.Certificate) error {
	if orignalVerificationFunc == nil {
		return verificationFunc
	}

	// Chain the existing verification with the new one
	newVerify := func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		// Run the user's custom verification first
		if err := orignalVerificationFunc(rawCerts, verifiedChains); err != nil {
			return err
		}
		// Then run the provided verification
		return verificationFunc(rawCerts, verifiedChains)
	}
	return newVerify
}

type defaultTransportConfigsType struct {
	oauthTransportConfig         *transportConfig
	cloudProviderTransportConfig *transportConfig
	ocspTransportConfig          *transportConfig
	crlTransportConfig           *transportConfig
	snowflakeTransportConfig     *transportConfig
	wifTransportConfig           *transportConfig
}

func newDefaultTransportConfigs() *defaultTransportConfigsType {
	return &defaultTransportConfigsType{
		oauthTransportConfig: &transportConfig{
			MaxIdleConns:    1,
			IdleConnTimeout: 30 * time.Second,
			DialTimeout:     30 * time.Second,
		},
		cloudProviderTransportConfig: &transportConfig{
			MaxIdleConns:    15,
			IdleConnTimeout: 30 * time.Second,
			DialTimeout:     30 * time.Second,
		},
		ocspTransportConfig: &transportConfig{
			MaxIdleConns:    1,
			IdleConnTimeout: 5 * time.Second,
			DialTimeout:     5 * time.Second,
			KeepAlive:       -1,
		},
		crlTransportConfig: &transportConfig{
			MaxIdleConns:    1,
			IdleConnTimeout: 5 * time.Second,
			DialTimeout:     5 * time.Second,
			KeepAlive:       -1,
		},
		snowflakeTransportConfig: &transportConfig{
			MaxIdleConns:    3,
			IdleConnTimeout: 30 * time.Minute,
			DialTimeout:     30 * time.Second,
		},
		wifTransportConfig: &transportConfig{
			MaxIdleConns:    1,
			IdleConnTimeout: 30 * time.Second,
			DialTimeout:     30 * time.Second,
			DisableProxy:    true,
		},
	}
}

func (dtc *defaultTransportConfigsType) forTransportType(transportType transportType) *transportConfig {
	switch transportType {
	case transportTypeOAuth:
		return dtc.oauthTransportConfig
	case transportTypeCloudProvider:
		return dtc.cloudProviderTransportConfig
	case transportTypeOCSP:
		return dtc.ocspTransportConfig
	case transportTypeCRL:
		return dtc.crlTransportConfig
	case transportTypeSnowflake:
		return dtc.snowflakeTransportConfig
	case transportTypeWIF:
		return dtc.wifTransportConfig
	}
	panic("unknown transport type: " + strconv.Itoa(int(transportType)))
}
