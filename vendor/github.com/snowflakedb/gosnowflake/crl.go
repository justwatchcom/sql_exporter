package gosnowflake

import (
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

const snowflakeCrlCacheValidityTimeEnv = "SNOWFLAKE_CRL_CACHE_VALIDITY_TIME"

var idpOID = asn1.ObjectIdentifier{2, 5, 29, 28}

type distributionPointName struct {
	FullName []asn1.RawValue `asn1:"optional,tag:0"`
}

type issuingDistributionPoint struct {
	DistributionPoint distributionPointName `asn1:"optional,tag:0"`
}

type crlValidator struct {
	certRevocationCheckMode        CertRevocationCheckMode
	allowCertificatesWithoutCrlURL bool
	inMemoryCacheDisabled          bool
	onDiskCacheDisabled            bool
	crlDownloadMaxSize             int
	httpClient                     *http.Client
	telemetry                      *snowflakeTelemetry
}

type crlCacheCleanerType struct {
	mu                      sync.Mutex
	cacheValidityTime       time.Duration
	onDiskCacheRemovalDelay time.Duration
	onDiskCacheDir          string
	cleanupStopChan         chan struct{}
	cleanupDoneChan         chan struct{}
}

type crlInMemoryCacheValueType struct {
	crl          *x509.RevocationList
	downloadTime *time.Time
}

var (
	crlCacheCleanerTickRate = time.Hour
	crlInMemoryCache        = make(map[string]*crlInMemoryCacheValueType)
	crlInMemoryCacheMutex   = &sync.Mutex{}
	crlURLMutexes           = make(map[string]*sync.Mutex)
	crlCacheCleanerMu       = &sync.Mutex{}
	crlCacheCleaner         *crlCacheCleanerType
)

func newCrlValidator(certRevocationCheckMode CertRevocationCheckMode, allowCertificatesWithoutCrlURL bool, inMemoryCacheDisabled, onDiskCacheDisabled bool, crlDownloadMaxSize int, httpClient *http.Client, telemetry *snowflakeTelemetry) (*crlValidator, error) {
	initCrlCacheCleaner()
	cv := &crlValidator{
		certRevocationCheckMode:        certRevocationCheckMode,
		allowCertificatesWithoutCrlURL: allowCertificatesWithoutCrlURL,
		inMemoryCacheDisabled:          inMemoryCacheDisabled,
		onDiskCacheDisabled:            onDiskCacheDisabled,
		crlDownloadMaxSize:             crlDownloadMaxSize,
		httpClient:                     httpClient,
		telemetry:                      telemetry,
	}
	return cv, nil
}

func initCrlCacheCleaner() {
	crlCacheCleanerMu.Lock()
	defer crlCacheCleanerMu.Unlock()
	if crlCacheCleaner != nil {
		return
	}
	var err error
	validityTime := defaultCrlCacheValidityTime
	if validityTimeStr := os.Getenv(snowflakeCrlCacheValidityTimeEnv); validityTimeStr != "" {
		if validityTime, err = time.ParseDuration(os.Getenv(snowflakeCrlCacheValidityTimeEnv)); err != nil {
			logger.Infof("failed to parse %v: %v, using default value %v", snowflakeCrlCacheValidityTimeEnv, err, defaultCrlCacheValidityTime)
			validityTime = defaultCrlCacheValidityTime
		}
	}

	onDiskCacheRemovalDelay := defaultCrlOnDiskCacheRemovalDelay
	if onDiskCacheRemovalDelayStr := os.Getenv("SNOWFLAKE_CRL_ON_DISK_CACHE_REMOVAL_DELAY"); onDiskCacheRemovalDelayStr != "" {
		if onDiskCacheRemovalDelay, err = time.ParseDuration(onDiskCacheRemovalDelayStr); err != nil {
			logger.Infof("failed to parse SNOWFLAKE_CRL_ON_DISK_CACHE_REMOVAL_DELAY: %v, using default value %v", err, defaultCrlOnDiskCacheRemovalDelay)
			onDiskCacheRemovalDelay = defaultCrlOnDiskCacheRemovalDelay
		}
	}

	onDiskCacheDir := os.Getenv("SNOWFLAKE_CRL_ON_DISK_CACHE_DIR")
	if onDiskCacheDir == "" {
		if onDiskCacheDir, err = defaultCrlOnDiskCacheDir(); err != nil {
			logger.Infof("failed to get default CRL on-disk cache directory: %v", err)
			onDiskCacheDir = "" // it will work only if on-disk cache is disabled
		}
	}
	if onDiskCacheDir != "" {
		if err = os.MkdirAll(onDiskCacheDir, 0755); err != nil {
			logger.Errorf("error while preparing cache dir for CRLs: %v", err)
		}
	}

	crlCacheCleaner = &crlCacheCleanerType{
		cacheValidityTime:       validityTime,
		onDiskCacheRemovalDelay: onDiskCacheRemovalDelay,
		onDiskCacheDir:          onDiskCacheDir,
		cleanupStopChan:         nil,
		cleanupDoneChan:         nil,
	}

}

// CertRevocationCheckMode defines the modes for certificate revocation checks.
type CertRevocationCheckMode int

const (
	// CertRevocationCheckDisabled means that certificate revocation checks are disabled.
	CertRevocationCheckDisabled CertRevocationCheckMode = iota
	// CertRevocationCheckAdvisory means that certificate revocation checks are advisory, and the driver will not fail if the checks end with error (cannot verify revocation status).
	// Driver will fail only if a certicate is revoked.
	CertRevocationCheckAdvisory
	// CertRevocationCheckEnabled means that every certificate revocation check must pass, otherwise the driver will fail.
	CertRevocationCheckEnabled
)

func (m CertRevocationCheckMode) String() string {
	switch m {
	case CertRevocationCheckDisabled:
		return "DISABLED"
	case CertRevocationCheckAdvisory:
		return "ADVISORY"
	case CertRevocationCheckEnabled:
		return "ENABLED"
	default:
		return fmt.Sprintf("unknown CertRevocationCheckMode: %d", m)
	}
}

func parseCertRevocationCheckMode(s string) (CertRevocationCheckMode, error) {
	switch strings.ToLower(s) {
	case "disabled":
		return CertRevocationCheckDisabled, nil
	case "advisory":
		return CertRevocationCheckAdvisory, nil
	case "enabled":
		return CertRevocationCheckEnabled, nil
	}
	return 0, fmt.Errorf("unknown CertRevocationCheckMode: %s", s)
}

type crlValidationResult int

const (
	crlRevoked crlValidationResult = iota
	crlUnrevoked
	crlError
)

type certValidationResult int

const (
	certRevoked certValidationResult = iota
	certUnrevoked
	certError
)

const (
	defaultCrlHTTPClientTimeout       = 10 * time.Second
	defaultCrlCacheValidityTime       = 24 * time.Hour
	defaultCrlOnDiskCacheRemovalDelay = 7 * time.Hour
	defaultCrlDownloadMaxSize         = 200 * 1024 * 1024 // 200 MB
)

func (cv *crlValidator) verifyPeerCertificates(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	if cv.certRevocationCheckMode == CertRevocationCheckDisabled {
		logger.Debug("certificate revocation check is disabled, skipping CRL validation")
		return nil
	}
	crlValidationResults := cv.validateChains(verifiedChains)

	allRevoked := true
	for _, result := range crlValidationResults {
		if result == crlUnrevoked {
			logger.Debug("found certificate chain with no revoked certificates")
			return nil
		}
		if result != crlRevoked {
			allRevoked = false
		}
	}

	if allRevoked {
		return fmt.Errorf("every verified certificate chain contained revoked certificates")
	}

	logger.Warn("some certificate chains didn't pass or driver wasn't able to peform the checks")
	if cv.certRevocationCheckMode == CertRevocationCheckAdvisory {
		logger.Warn("certificate revocation check is set to CERT_REVOCATION_CHECK_ADVISORY, so assuming that certificates are not revoked")
		return nil
	}
	return fmt.Errorf("certificate revocation check failed")
}

func (cv *crlValidator) validateChains(chains [][]*x509.Certificate) []crlValidationResult {
	crlValidationResults := make([]crlValidationResult, len(chains))
	for i, chain := range chains {
		crlValidationResults[i] = crlUnrevoked
		chainStr := ""
		for _, cert := range chain {
			chainStr += fmt.Sprintf("%v -> ", cert.Subject)
		}
		logger.Debugf("validating certificate chain %d: %s", i, chainStr)
		for j, cert := range chain {
			if j == len(chain)-1 {
				logger.Debugf("skipping root certificate %v for CRL validation", cert.Subject)
				continue
			}

			if isShortLivedCertificate(cert) {
				logger.Debugf("certificate %v is short-lived, skipping CRL validation", cert.Subject)
				continue
			}

			if len(cert.CRLDistributionPoints) == 0 {
				if cv.allowCertificatesWithoutCrlURL {
					logger.Debugf("certificate %v has no CRL distribution points, skipping CRL validation", cert.Subject)
					continue
				}
				logger.Warnf("certificate %v has no CRL distribution points, skipping CRL validation, but marking as error", cert.Subject)
				crlValidationResults[i] = crlError
				continue
			}

			certStatus := cv.validateCertificate(cert, chain[j+1])
			if certStatus == certRevoked {
				crlValidationResults[i] = crlRevoked
				break
			}

			if certStatus == certError {
				crlValidationResults[i] = crlError
				continue
			}
		}

		if crlValidationResults[i] == crlUnrevoked {
			logger.Debugf("certificate chain %d is unrevoked, skipping remaining chains", i)
			break
		}
	}

	return crlValidationResults
}

func (cv *crlValidator) validateCertificate(cert *x509.Certificate, parent *x509.Certificate) certValidationResult {
	var results []certValidationResult
	for _, crlURL := range cert.CRLDistributionPoints {
		result := cv.validateCrlAgainstCrlURL(cert, crlURL, parent)
		if result == certRevoked {
			return result
		}
		results = append(results, result)
	}
	if slices.Contains(results, certError) {
		return certError
	}
	return certUnrevoked
}

func (cv *crlValidator) validateCrlAgainstCrlURL(cert *x509.Certificate, crlURL string, parent *x509.Certificate) certValidationResult {
	now := time.Now()

	mu := cv.getOrCreateMutex(crlURL)
	mu.Lock()
	defer mu.Unlock()

	crl, downloadTime := cv.getFromCache(crlURL)
	needsFreshCrl := crl == nil || crl.NextUpdate.Before(now) || downloadTime.Add(crlCacheCleaner.cacheValidityTime).Before(now)
	shouldUpdateCrl := false

	if needsFreshCrl {
		newCrl, newDownloadTime, err := cv.downloadCrl(crlURL)
		if err != nil {
			logger.Warnf("failed to download CRL from %v: %v", crlURL, err)
		}
		if newCrl != nil && newCrl.NextUpdate.Before(now) {
			logger.Warnf("downloaded CRL from %v is already expired (next update at %v)", crlURL, newCrl.NextUpdate)
			newCrl = nil
			if crl == nil {
				return certError
			}
		}
		shouldUpdateCrl = newCrl != nil && (crl == nil || newCrl.ThisUpdate.After(crl.ThisUpdate))
		if shouldUpdateCrl {
			logger.Debugf("Found updated CRL for %v", crlURL)
			crl = newCrl
			downloadTime = newDownloadTime
		} else {
			if crl != nil && crl.NextUpdate.After(now) {
				logger.Debugf("CRL for %v is up-to-date, using cached version", crlURL)
			} else {
				logger.Warnf("CRL for %v is not available or outdated", crlURL)
				return certError
			}
		}
	}

	logger.Debugf("CRL has %v entries, next update at %v", len(crl.RevokedCertificateEntries), crl.NextUpdate)
	if err := cv.validateCrl(crl, parent, crlURL); err != nil {
		return certError
	}

	if shouldUpdateCrl {
		logger.Debugf("CRL for %v is valid, updating cache", crlURL)
		cv.updateCache(crlURL, crl, downloadTime)
	}

	for _, rce := range crl.RevokedCertificateEntries {
		if cert.SerialNumber.Cmp(rce.SerialNumber) == 0 {
			logger.Warnf("certificate for %v (serial number %v) has been revoked at %v, reason: %v", cert.Subject, rce.SerialNumber, rce.RevocationTime, rce.ReasonCode)
			return certRevoked
		}
	}

	return certUnrevoked
}

func (cv *crlValidator) validateCrl(crl *x509.RevocationList, parent *x509.Certificate, crlURL string) error {
	if crl.Issuer.String() != parent.Subject.String() {
		err := fmt.Errorf("CRL issuer %v does not match parent certificate subject %v for %v", crl.Issuer, parent.Subject, crlURL)
		logger.Warn(err)
		return err
	}
	if err := crl.CheckSignatureFrom(parent); err != nil {
		logger.Warnf("CRL signature verification failed for %v: %v", crlURL, err)
		return err
	}
	if err := cv.verifyAgainstIdpExtension(crl, crlURL); err != nil {
		logger.Warnf("CRL IDP extension verification failed for %v: %v", crlURL, err)
		return err
	}
	return nil
}

func (cv *crlValidator) getFromCache(crlURL string) (*x509.RevocationList, *time.Time) {
	if cv.inMemoryCacheDisabled {
		logger.Debugf("in-memory cache is disabled")
	} else {
		crlInMemoryCacheMutex.Lock()
		cacheValue, exists := crlInMemoryCache[crlURL]
		crlInMemoryCacheMutex.Unlock()
		if exists {
			logger.Debugf("found CRL in cache for %v", crlURL)
			return cacheValue.crl, cacheValue.downloadTime
		}
	}
	if cv.onDiskCacheDisabled {
		logger.Debugf("CRL cache is disabled, not checking disk for %v", crlURL)
		return nil, nil
	}
	crlFilePath := cv.crlURLToPath(crlURL)
	fileHandle, err := os.Open(crlFilePath)
	if err != nil {
		logger.Debugf("cannot open CRL from disk for %v (%v): %v", crlURL, crlFilePath, err)
		return nil, nil
	}
	defer func() {
		if err := fileHandle.Close(); err != nil {
			logger.Warnf("failed to close CRL file handle for %v (%v): %v", crlURL, crlFilePath, err)
		}
	}()
	stat, err := fileHandle.Stat()
	if err != nil {
		logger.Debugf("cannot stat CRL file for %v (%v): %v", crlURL, crlFilePath, err)
		return nil, nil
	}
	crlBytes, err := io.ReadAll(fileHandle)
	if err != nil {
		logger.Debugf("cannot read CRL from disk for %v (%v): %v", crlURL, crlFilePath, err)
		return nil, nil
	}
	crl, err := x509.ParseRevocationList(crlBytes)
	if err != nil {
		logger.Warnf("cannot parse CRL from disk for %v (%v): %v", crlURL, crlFilePath, err)
		return nil, nil
	}
	modTime := stat.ModTime()

	if !cv.inMemoryCacheDisabled {
		// promote CRL to in-memory cache
		crlInMemoryCacheMutex.Lock()
		crlInMemoryCache[crlURL] = &crlInMemoryCacheValueType{
			crl: crl,
			// modTime is not the exact time the CRL was downloaded, but rather the last modification time of the file
			// still, it is good enough for our purposes
			downloadTime: &modTime,
		}
		crlInMemoryCacheMutex.Unlock()
	}
	return crl, &modTime
}

func (cv *crlValidator) updateCache(crlURL string, crl *x509.RevocationList, downloadTime *time.Time) {
	if cv.inMemoryCacheDisabled {
		logger.Debugf("in-memory cache is disabled, not updating")
	} else {
		crlInMemoryCacheMutex.Lock()
		crlInMemoryCache[crlURL] = &crlInMemoryCacheValueType{
			crl:          crl,
			downloadTime: downloadTime,
		}
		crlInMemoryCacheMutex.Unlock()
	}
	if cv.onDiskCacheDisabled {
		logger.Debugf("CRL cache is disabled, not writing to disk for %v", crlURL)
		return
	}
	crlFilePath := cv.crlURLToPath(crlURL)
	crlDirPath := filepath.Dir(crlFilePath)
	crlDirParentPath := filepath.Dir(crlDirPath)
	if err := os.MkdirAll(crlDirParentPath, 0755); err != nil {
		logger.Warnf("failed to create directory for CRL file %v: %v", crlFilePath, err)
		return
	}
	if err := os.Mkdir(crlDirPath, 0700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			logger.Warnf("failed to create directory for CRL file %v: %v", crlFilePath, err)
			return
		}
		if err = os.Chmod(crlDirPath, 0700); err != nil {
			logger.Warnf("failed to chmod existing directory for CRL file %v: %v", crlFilePath, err)
			return
		}
	}
	if err := os.WriteFile(crlFilePath, crl.Raw, 0600); err != nil {
		logger.Warnf("failed to write CRL to disk for %v (%v): %v", crlURL, crlFilePath, err)
	}
}

func (cv *crlValidator) downloadCrl(crlURL string) (*x509.RevocationList, *time.Time, error) {
	telemetryEvent := &telemetryData{
		Timestamp: time.Now().UnixNano() / int64(time.Millisecond),
		Message: map[string]string{
			"type":    "client_crl_stats",
			"crl_url": crlURL,
		},
	}
	defer func() {
		if err := cv.telemetry.addLog(telemetryEvent); err != nil {
			logger.Warnf("failed to add telemetry log for CRL download: %v", err)
		}
	}()
	logger.Debugf("downloading CRL from %v", crlURL)
	now := time.Now()
	resp, err := cv.httpClient.Get(crlURL)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err = resp.Body.Close(); err != nil {
			logger.Warnf("failed to close response body for CRL downloaded from %v: %v", crlURL, err)
		}
	}()
	if resp.StatusCode >= 400 {
		return nil, nil, fmt.Errorf("failed to download CRL from %v, status code: %v", crlURL, resp.StatusCode)
	}
	maxSize := resp.ContentLength
	if maxSize <= 0 || maxSize > int64(cv.crlDownloadMaxSize) {
		maxSize = int64(cv.crlDownloadMaxSize)
	}
	crlBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxSize))
	if err != nil {
		return nil, nil, err
	}
	if cv.crlDownloadMaxSize > 0 && len(crlBytes) >= cv.crlDownloadMaxSize {
		return nil, nil, fmt.Errorf("CRL from %v exceeds maximum size of %d bytes", crlURL, cv.crlDownloadMaxSize)
	}
	telemetryEvent.Message["crl_bytes"] = fmt.Sprintf("%d", len(crlBytes))
	downloadTime := time.Since(now)
	telemetryEvent.Message["crl_download_time_ms"] = fmt.Sprintf("%d", downloadTime.Milliseconds())
	logger.Debugf("downloaded %v bytes for CRL %v", len(crlBytes), crlURL)
	timeBeforeParsing := time.Now()
	crl, err := x509.ParseRevocationList(crlBytes)
	logger.Debugf("parsed CRL from %v, error: %v", crlURL, err)
	if err != nil {
		return nil, nil, err
	}
	logger.Debugf("parsed CRL from %v, next update at %v", crlURL, crl.NextUpdate)
	telemetryEvent.Message["crl_parse_time_ms"] = fmt.Sprintf("%d", time.Since(timeBeforeParsing).Milliseconds())
	telemetryEvent.Message["crl_revoked_certificates"] = fmt.Sprintf("%d", len(crl.RevokedCertificateEntries))
	return crl, &now, err
}

func (cv *crlValidator) crlURLToPath(crlURL string) string {
	// Convert CRL URL to a file path, e.g., by replacing slashes with underscores
	return filepath.Join(crlCacheCleaner.onDiskCacheDir, url.QueryEscape(crlURL))
}

func (cv *crlValidator) verifyAgainstIdpExtension(crl *x509.RevocationList, distributionPoint string) error {
	for _, ext := range append(crl.Extensions, crl.ExtraExtensions...) {
		if ext.Id.Equal(idpOID) {
			var idp issuingDistributionPoint
			_, err := asn1.Unmarshal(ext.Value, &idp)
			if err != nil {
				return fmt.Errorf("failed to unmarshal IDP extension: %w", err)
			}
			for _, dp := range idp.DistributionPoint.FullName {
				if string(dp.Bytes) == distributionPoint {
					logger.Debugf("distribution point %v matches CRL IDP extension", distributionPoint)
					return nil
				}
			}
			return fmt.Errorf("distribution point %v not found in CRL IDP extension", distributionPoint)
		}
	}
	return nil
}

func (cv *crlValidator) getOrCreateMutex(crlURL string) *sync.Mutex {
	crlInMemoryCacheMutex.Lock()
	mu, ok := crlURLMutexes[crlURL]
	if !ok {
		mu = &sync.Mutex{}
		crlURLMutexes[crlURL] = mu
	}
	crlInMemoryCacheMutex.Unlock()
	return mu
}

func isShortLivedCertificate(cert *x509.Certificate) bool {
	// https://cabforum.org/working-groups/server/baseline-requirements/requirements/
	// See Short-lived Subscriber Certificate section
	if cert.NotBefore.Before(time.Date(2024, time.March, 15, 0, 0, 0, 0, time.UTC)) {
		// Certificates issued before March 15, 2024 are not considered short-lived
		return false
	}
	maximumValidityPeriod := 7 * 24 * time.Hour
	if cert.NotBefore.Before(time.Date(2026, time.March, 15, 0, 0, 0, 0, time.UTC)) {
		maximumValidityPeriod = 10 * 24 * time.Hour
	}
	maximumValidityPeriod += time.Minute // Fix inclusion start and end time
	certValidityPeriod := cert.NotAfter.Sub(cert.NotBefore)
	return maximumValidityPeriod > certValidityPeriod
}

func (ccc *crlCacheCleanerType) startPeriodicCacheCleanup() {
	ccc.mu.Lock()
	defer ccc.mu.Unlock()
	if ccc.cleanupStopChan != nil {
		logger.Debug("CRL cache cleaner is already running, not starting again")
		return
	}
	logger.Debugf("starting periodic CRL cache cleanup with tick rate %v", crlCacheCleanerTickRate)
	ccc.cleanupStopChan = make(chan struct{})
	ccc.cleanupDoneChan = make(chan struct{})
	go func() {
		ticker := time.NewTicker(crlCacheCleanerTickRate)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ccc.cleanupInMemoryCache()
				ccc.cleanupOnDiskCache()
			case <-ccc.cleanupStopChan:
				close(ccc.cleanupDoneChan)
				return
			}
		}
	}()
}

func (ccc *crlCacheCleanerType) stopPeriodicCacheCleanup() {
	ccc.mu.Lock()
	defer ccc.mu.Unlock()
	logger.Debug("stopping periodic CRL cache cleanup")
	if ccc.cleanupStopChan != nil {
		close(ccc.cleanupStopChan)
		<-ccc.cleanupDoneChan
		ccc.cleanupStopChan = nil
		ccc.cleanupDoneChan = nil
	} else {
		logger.Debugf("CRL cache cleaner was not running, nothing to stop")
	}
}

func (ccc *crlCacheCleanerType) cleanupInMemoryCache() {
	now := time.Now()
	logger.Debugf("cleaning up in-memory CRL cache at %v", now)
	crlInMemoryCacheMutex.Lock()
	for k, v := range crlInMemoryCache {
		expired := v.crl.NextUpdate.Before(now)
		evicted := v.downloadTime.Add(ccc.cacheValidityTime).Before(now)
		logger.Debugf("testing CRL for %v (nextUpdate=%v, downloadTime=%v) from in-memory cache (expired: %v, evicted: %v)", k, v.crl.NextUpdate, v.downloadTime, expired, evicted)
		if expired || evicted {
			delete(crlInMemoryCache, k)
		}
	}
	crlInMemoryCacheMutex.Unlock()
}

func (ccc *crlCacheCleanerType) cleanupOnDiskCache() {
	now := time.Now()
	logger.Debugf("cleaning up on-disk CRL cache at %v", now)
	entries, err := os.ReadDir(ccc.onDiskCacheDir)
	if err != nil {
		logger.Warnf("failed to read CRL cache dir: %v", err)
		return
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		path := filepath.Join(ccc.onDiskCacheDir, entry.Name())
		crlBytes, err := os.ReadFile(path)
		if err != nil {
			logger.Warnf("failed to read CRL file %v: %v", path, err)
			continue
		}
		crl, err := x509.ParseRevocationList(crlBytes)
		if err != nil {
			logger.Warnf("failed to parse CRL file %v: %v", path, err)
			continue
		}
		if crl.NextUpdate.Add(ccc.onDiskCacheRemovalDelay).Before(now) {
			logger.Debugf("CRL file %v is expired, removing", path)
			if err := os.Remove(path); err != nil {
				logger.Warnf("failed to remove expired CRL file %v: %v", path, err)
			}
		}
	}
}

func defaultCrlOnDiskCacheDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local", "Snowflake", "Caches", "crls"), nil
	case "darwin":
		home := os.Getenv("HOME")
		if home == "" {
			return "", errors.New("HOME is blank")
		}
		return filepath.Join(home, "Library", "Caches", "Snowflake", "crls"), nil
	default:
		home := os.Getenv("HOME")
		if home == "" {
			return "", errors.New("HOME is blank")
		}
		return filepath.Join(home, ".cache", "snowflake", "crls"), nil
	}
}
