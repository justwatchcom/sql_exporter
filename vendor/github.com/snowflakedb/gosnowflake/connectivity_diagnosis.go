package gosnowflake

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type connectivityDiagnoser struct {
	diagnosticClient *http.Client
}

type allowlistEntry struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	Type string `json:"type"`
}

type allowlist struct {
	Entries []allowlistEntry
}

// acceptable HTTP status codes for connectivity diagnosis
// for the sake of connectivity, e.g. HTTP403 from AWS S3 is perfectly fine
// GCS bucket and Azure blob responds HTTP400 upon connecting with plain GET, its okay from connection standpoint
var connDiagAcceptableStatusCodes = []int{http.StatusOK, http.StatusForbidden, http.StatusBadRequest}

// map of already-fetched CRLs to not test them more than once as they can be quite large
var connDiagTestedCrls = make(map[string]string)

// create a diagnostic client with the appropriate transport for the given config
func (cd *connectivityDiagnoser) createDiagnosticClient(cfg *Config) *http.Client {
	transport := cd.createDiagnosticTransport(cfg)

	clientTimeout := cfg.ClientTimeout
	if clientTimeout == 0 {
		clientTimeout = defaultClientTimeout
	}

	return &http.Client{
		Timeout:   clientTimeout,
		Transport: transport,
	}
}

// necessary to be able to log the IP address of the remote host to which we actually connected
// might be even different from the result of DNS resolution
func (cd *connectivityDiagnoser) createDiagnosticDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		if remoteAddr := conn.RemoteAddr(); remoteAddr != nil {
			remoteIPStr := remoteAddr.String()
			// parse out just the IP (maybe port is present)
			if host, _, err := net.SplitHostPort(remoteIPStr); err == nil {
				remoteIPStr = host
			}

			// get hostname
			hostname, _, _ := net.SplitHostPort(addr)
			if hostname == "" {
				hostname = addr
			}

			logger.Infof("[createDiagnosticDialContext] Connected to %s (remote IP: %s)", hostname, remoteIPStr)
		}

		return conn, nil
	}
}

// enhance the transport with IP logging
func (cd *connectivityDiagnoser) createDiagnosticTransport(cfg *Config) *http.Transport {
	baseTransport, err := newTransportFactory(cfg, &snowflakeTelemetry{enabled: false}).createTransport(cfg.transportConfigFor(transportTypeSnowflake))
	if err != nil {
		logger.Fatalf("[createDiagnosticTransport] failed to get the transport from the config: %v", err)
	}

	var httpTransport *http.Transport
	if t, ok := baseTransport.(*http.Transport); ok {
		httpTransport = t
	} else {
		logger.Warnf("[createDiagnosticTransport] unexpected transport type: %T, using default SnowflakeTransport", baseTransport)
		httpTransport = SnowflakeTransport
	}

	// return a new transport enhanced with remote IP logging
	// for SnowflakeNoOcspTransport, TLSClientConfig is nil
	return &http.Transport{
		TLSClientConfig: httpTransport.TLSClientConfig,
		MaxIdleConns:    httpTransport.MaxIdleConns,
		IdleConnTimeout: httpTransport.IdleConnTimeout,
		Proxy:           httpTransport.Proxy,
		DialContext:     cd.createDiagnosticDialContext(),
	}
}

func (cd *connectivityDiagnoser) openAndReadAllowlistJSON(filePath string) (allowlist allowlist, err error) {
	if filePath == "" {
		logger.Info("[openAndReadAllowlistJSON] allowlist.json location not specified, trying to load from current directory.")
		filePath = "allowlist.json"
	}
	logger.Infof("[openAndReadAllowlistJSON] reading allowlist from %s.", filePath)
	fileContent, err := os.ReadFile(filePath)
	if err != nil {
		return allowlist, err
	}

	logger.Debug("[openAndReadAllowlistJSON] parsing allowlist.json")
	err = json.Unmarshal(fileContent, &allowlist.Entries)
	return allowlist, err
}

// look up the host, using the local resolver
func (cd *connectivityDiagnoser) resolveHostname(hostname string) {
	ips, err := net.LookupIP(hostname)
	if err != nil {
		logger.Errorf("[resolveHostname] error resolving hostname %s: %s", hostname, err)
		return
	}
	for _, ip := range ips {
		logger.Infof("[resolveHostname] resolved hostname %s to %s", hostname, ip.String())
		if checkIsPrivateLink(hostname) && !ip.IsPrivate() {
			logger.Errorf("[resolveHostname] this hostname %s should resolve to a private IP, but %s is public IP. Please, check your DNS configuration.", hostname, ip.String())
		}
	}
}

func (cd *connectivityDiagnoser) isAcceptableStatusCode(statusCode int, acceptableCodes []int) bool {
	for _, code := range acceptableCodes {
		if statusCode == code {
			return true
		}
	}
	return false
}

func (cd *connectivityDiagnoser) fetchCRL(uri string) error {
	if _, ok := connDiagTestedCrls[uri]; ok {
		logger.Infof("[fetchCRL] CRL for %s already fetched and parsed.", uri)
		return nil
	}
	logger.Infof("[fetchCRL] fetching %s", uri)
	req, err := cd.createRequest(uri)
	if err != nil {
		logger.Errorf("[fetchCRL] error creating request: %v", err)
		return err
	}
	resp, err := cd.diagnosticClient.Do(req)
	if err != nil {
		return fmt.Errorf("[fetchCRL] HTTP GET to %s endpoint failed: %w", uri, err)
	}
	// if closing response body is unsuccessful for some reason
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logger.Errorf("[fetchCRL] Failed to close response body: %v", err)
			return
		}
	}(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("[fetchCRL] HTTP response status from endpoint: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("[fetchCRL] failed to read response body: %w", err)
	}
	logger.Infof("[fetchCRL] %s retrieved successfully (%d bytes)", uri, len(body))
	logger.Infof("[fetchCRL] Parsing CRL fetched from %s", uri)
	crl, err := x509.ParseRevocationList(body)
	if err != nil {
		return fmt.Errorf("[fetchCRL] Failed to parse CRL: %w", err)
	}
	logger.Infof("    CRL Issuer: %s", crl.Issuer)
	logger.Infof("    This Update: %s", crl.ThisUpdate)
	logger.Infof("    Next Update: %s", crl.NextUpdate)
	logger.Infof("    Revoked Certificates#: %s", strconv.Itoa(len(crl.RevokedCertificateEntries)))

	connDiagTestedCrls[uri] = ""
	return nil
}

func (cd *connectivityDiagnoser) doHTTP(request *http.Request) error {
	if strings.HasPrefix(request.URL.Host, "ocsp.snowflakecomputing.") {
		fullOCSPCacheURI := request.URL.String() + "/ocsp_response_cache.json"
		newURL, err := url.Parse(fullOCSPCacheURI)
		if err != nil {
			return fmt.Errorf("failed to parse the full OCSP cache URL: %w", err)
		}
		request.URL = newURL
	}
	logger.Infof("[doHTTP] testing HTTP connection to %s", request.URL.String())
	resp, err := cd.diagnosticClient.Do(request)
	if err != nil {
		return fmt.Errorf("HTTP GET to %s endpoint failed: %w", request.URL.String(), err)
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logger.Errorf("[doHTTP] Failed to close response body: %v", err)
			return
		}
	}(resp.Body)

	if !cd.isAcceptableStatusCode(resp.StatusCode, connDiagAcceptableStatusCodes) {
		return fmt.Errorf("HTTP response status from %s endpoint: %s", request.URL.String(), resp.Status)
	}
	logger.Infof("[doHTTP] Successfully connected to %s, HTTP response status: %s", request.URL.String(), resp.Status)
	return nil
}

func (cd *connectivityDiagnoser) doHTTPSGetCerts(request *http.Request, downloadCRLs bool) error {
	logger.Infof("[doHTTPSGetCerts] connecting to %s", request.URL.String())
	resp, err := cd.diagnosticClient.Do(request)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logger.Errorf("[doHTTPSGetCerts] Failed to close response body: %v", err)
			return
		}
	}(resp.Body)

	if !cd.isAcceptableStatusCode(resp.StatusCode, connDiagAcceptableStatusCodes) {
		return fmt.Errorf("HTTP response status from %s endpoint: %s", request.URL.String(), resp.Status)
	}
	logger.Infof("[doHTTPSGetCerts] Successfully connected to %s, HTTP response status: %s", request.URL.String(), resp.Status)

	logger.Debug("[doHTTPSGetCerts] getting TLS connection state")
	tlsState := resp.TLS
	if tlsState == nil {
		return errors.New("no TLS connection state available")
	}

	logger.Debug("[doHTTPSGetCerts] getting certificate chain")
	certs := tlsState.PeerCertificates
	logger.Infof("[doHTTPSGetCerts] Retrieved %d certificate(s).", len(certs))

	// log individual cert details
	for i, cert := range certs {
		logger.Infof("[doHTTPSGetCerts] Certificate %d, serial number: %x", i+1, cert.SerialNumber)
		logger.Infof("[doHTTPSGetCerts]   Subject: %s", cert.Subject)
		logger.Infof("[doHTTPSGetCerts]   Issuer:  %s", cert.Issuer)
		logger.Infof("[doHTTPSGetCerts]   Valid:   %s to %s", cert.NotBefore, cert.NotAfter)
		logger.Infof("[doHTTPSGetCerts]   For further details, check https://crt.sh/?serial=%x (non-Snowflake site)", cert.SerialNumber)

		// if cert has CRL endpoint, log them too
		if len(cert.CRLDistributionPoints) > 0 {
			logger.Infof("[doHTTPSGetCerts]   CRL Distribution Points:")
			for _, dp := range cert.CRLDistributionPoints {
				logger.Infof("[doHTTPSGetCerts]    - %s", dp)
				// only try to download the actual CRL if configured to do so
				if downloadCRLs {
					if err := cd.fetchCRL(dp); err != nil {
						logger.Errorf("[doHTTPSGetCerts]      Failed to fetch or parse CRL: %v", err)
					}
				}
			}
		} else {
			logger.Infof("[doHTTPSGetCerts]   CRL Distribution Points not included in the certificate.")
		}

		// dump the full PEM data too on DEBUG loglevel
		pemData := pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: cert.Raw,
		})
		logger.Debug("[doHTTPSGetCerts]   certificate PEM:")
		logger.Debug(string(pemData))
	}
	return nil
}

func (cd *connectivityDiagnoser) createRequest(uri string) (*http.Request, error) {
	logger.Infof("[createRequest] creating GET request to %s", uri)
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return nil, err
	}
	return req, nil
}

func (cd *connectivityDiagnoser) checkProxy(req *http.Request) {
	diagnosticTransport := cd.diagnosticClient.Transport.(*http.Transport)
	if diagnosticTransport == nil {
		logger.Errorf("[checkProxy] diagnosticTransport is nil")
		return
	}
	if diagnosticTransport.Proxy == nil {
		// no proxy configured, nothing to log
		return
	}
	p, err := diagnosticTransport.Proxy(req)
	if err != nil {
		logger.Errorf("[checkProxy] problem determining PROXY: %v", err)
	}
	if p != nil {
		logger.Infof("[checkProxy] PROXY detected in the connection: %v", p)
	}
}

func (cd *connectivityDiagnoser) performConnectivityCheck(entryType, host string, port int, downloadCRLs bool) (err error) {
	var protocol string
	var req *http.Request

	switch port {
	case 80:
		protocol = "http"
	case 443:
		protocol = "https"
	default:
		return fmt.Errorf("[performConnectivityCheck] unsupported port: %d", port)
	}

	logger.Infof("[performConnectivityCheck] %s check for %s %s", strings.ToUpper(protocol), entryType, host)
	req, err = cd.createRequest(fmt.Sprintf("%s://%s", protocol, host))
	if err != nil {
		logger.Errorf("[performConnectivityCheck] error creating request: %v", err)
		return err
	}

	cd.checkProxy(req)

	switch protocol {
	case "http":
		err = cd.doHTTP(req)
	case "https":
		err = cd.doHTTPSGetCerts(req, downloadCRLs)
	}

	if err != nil {
		logger.Errorf("[performConnectivityCheck] error performing %s check: %v", strings.ToUpper(protocol), err)
		return err
	}

	return nil
}

func performDiagnosis(cfg *Config, downloadCRLs bool) {
	allowlistFile := cfg.ConnectionDiagnosticsAllowlistFile

	logger.Info("[performDiagnosis] starting connectivity diagnosis based on allowlist file.")
	if downloadCRLs {
		logger.Info("[performDiagnosis] CRLs will be attempted to be downloaded and parsed during https tests.")
	}

	var diag connectivityDiagnoser
	// diagnostic client - its transport is based on the Config. default: SnowflakeTransport
	diag.diagnosticClient = diag.createDiagnosticClient(cfg)

	allowlist, err := diag.openAndReadAllowlistJSON(allowlistFile)
	if err != nil {
		logger.Errorf("[performDiagnosis] error opening and parsing allowlist file: %v", err)
		return
	}

	for _, entry := range allowlist.Entries {
		host := entry.Host
		port := entry.Port
		entryType := entry.Type
		logger.Infof("[performDiagnosis] DNS check - resolving %s hostname %s", entryType, host)
		diag.resolveHostname(host)

		if port == 80 || port == 443 {
			err := diag.performConnectivityCheck(entryType, host, port, downloadCRLs)
			if err != nil {
				continue
			}
		}
	}
}
