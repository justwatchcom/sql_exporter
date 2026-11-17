package gosnowflake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/golang-jwt/jwt/v5"
	sfconfig "github.com/snowflakedb/gosnowflake/v2/internal/config"
)

const (
	awsWif   wifProviderType = "AWS"
	gcpWif   wifProviderType = "GCP"
	azureWif wifProviderType = "AZURE"
	oidcWif  wifProviderType = "OIDC"

	gcpMetadataFlavorHeaderName     = "Metadata-Flavor"
	gcpMetadataFlavor               = "Google"
	defaultGcpMetadataServiceBase   = "http://metadata.google.internal"
	defaultAzureMetadataServiceBase = "http://169.254.169.254"
	defaultGcpIamCredentialsBase    = "https://iamcredentials.googleapis.com"
	snowflakeAudience               = "snowflakecomputing.com"
)

type wifProviderType string

type wifAttestation struct {
	ProviderType string            `json:"providerType"`
	Credential   string            `json:"credential"`
	Metadata     map[string]string `json:"metadata"`
}

type wifAttestationCreator interface {
	createAttestation() (*wifAttestation, error)
}

type wifAttestationProvider struct {
	context      context.Context
	cfg          *Config
	awsCreator   wifAttestationCreator
	gcpCreator   wifAttestationCreator
	azureCreator wifAttestationCreator
	oidcCreator  wifAttestationCreator
}

func createWifAttestationProvider(ctx context.Context, cfg *Config, telemetry *snowflakeTelemetry) *wifAttestationProvider {
	return &wifAttestationProvider{
		context: ctx,
		cfg:     cfg,
		awsCreator: &awsIdentityAttestationCreator{
			cfg:                       cfg,
			attestationServiceFactory: createDefaultAwsAttestationMetadataProvider,
			ctx:                       ctx,
		},
		gcpCreator: &gcpIdentityAttestationCreator{
			cfg:                    cfg,
			telemetry:              telemetry,
			metadataServiceBaseURL: defaultGcpMetadataServiceBase,
			iamCredentialsURL:      defaultGcpIamCredentialsBase,
		},
		azureCreator: &azureIdentityAttestationCreator{
			azureAttestationMetadataProvider: &defaultAzureAttestationMetadataProvider{},
			cfg:                              cfg,
			telemetry:                        telemetry,
			workloadIdentityEntraResource:    determineEntraResource(cfg),
			azureMetadataServiceBaseURL:      defaultAzureMetadataServiceBase,
		},
		oidcCreator: &oidcIdentityAttestationCreator{token: func() (string, error) { return sfconfig.GetToken(cfg) }},
	}
}

func (p *wifAttestationProvider) getAttestation(identityProvider string) (*wifAttestation, error) {
	switch strings.ToUpper(identityProvider) {
	case string(awsWif):
		return p.awsCreator.createAttestation()
	case string(gcpWif):
		return p.gcpCreator.createAttestation()
	case string(azureWif):
		return p.azureCreator.createAttestation()
	case string(oidcWif):
		return p.oidcCreator.createAttestation()
	default:
		return nil, fmt.Errorf("unknown WorkloadIdentityProvider specified: %s. Valid values are: %s, %s, %s, %s", identityProvider, awsWif, gcpWif, azureWif, oidcWif)
	}
}

type awsAttestastationMetadataProviderFactory func(ctx context.Context, cfg *Config) awsAttestationMetadataProvider

type awsIdentityAttestationCreator struct {
	cfg                       *Config
	attestationServiceFactory awsAttestastationMetadataProviderFactory
	ctx                       context.Context
}

type gcpIdentityAttestationCreator struct {
	cfg                    *Config
	telemetry              *snowflakeTelemetry
	metadataServiceBaseURL string
	iamCredentialsURL      string
}

type oidcIdentityAttestationCreator struct {
	token func() (string, error)
}

type awsAttestationMetadataProvider interface {
	awsCredentials() (aws.Credentials, error)
	awsCredentialsViaRoleChaining() (aws.Credentials, error)
	awsRegion() string
	awsWebIdentityToken(creds aws.Credentials, region string) (string, error)
}

type defaultAwsAttestationMetadataProvider struct {
	ctx    context.Context
	cfg    *Config
	awsCfg aws.Config
}

// awsStsEndpoint resolves the STS endpoint for a given region. Populated by awsStsEndpointFor.
type awsStsEndpoint struct {
	authority  string // bare host[:port] for SigV4 Host header
	baseURL    string // full URL (with scheme) for SDK BaseEndpoint option
	overridden bool   // whether WorkloadIdentityHost was set (vs regional default)
}

// parseWorkloadIdentityHost normalizes user input: accepts bare host or full URL,
// adds https:// default, strips trailing slashes, validates scheme/host/no-query.
func parseWorkloadIdentityHost(host string) (awsStsEndpoint, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return awsStsEndpoint{}, fmt.Errorf("workloadIdentityHost is empty")
	}

	// If no scheme, prepend https://
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}

	u, err := url.Parse(host)
	if err != nil {
		return awsStsEndpoint{}, fmt.Errorf("workloadIdentityHost %q is malformed: %w", host, err)
	}

	if u.Scheme != "https" && u.Scheme != "http" {
		return awsStsEndpoint{}, fmt.Errorf("workloadIdentityHost %q must use https or http, got scheme %q", host, u.Scheme)
	}
	if u.Host == "" {
		return awsStsEndpoint{}, fmt.Errorf("workloadIdentityHost %q does not contain a hostname", host)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return awsStsEndpoint{}, fmt.Errorf("workloadIdentityHost %q must not contain user info, a query or a fragment", host)
	}

	baseURL := url.URL{
		Scheme: u.Scheme,
		Host:   u.Host,
		Path:   u.Path,
	}

	return awsStsEndpoint{
		authority:  u.Host,                                   // host[:port] for Host header
		baseURL:    strings.TrimRight(baseURL.String(), "/"), // full URL for SDK
		overridden: true,
	}, nil
}

// awsStsEndpointFor resolves the STS endpoint for the given region, checking
// WorkloadIdentityHost first, falling back to the regional default.
func awsStsEndpointFor(ctx context.Context, cfg *Config, region string) (awsStsEndpoint, error) {
	if cfg.WorkloadIdentityHost != "" {
		return parseWorkloadIdentityHost(cfg.WorkloadIdentityHost)
	}
	return defaultStsEndpoint(ctx, region)
}

// defaultStsEndpoint resolves the regional STS endpoint using the AWS SDK's own
// endpoint rules. The DNS suffix is partition-specific and not derivable from the
// region name - the ISO partitions and the European Sovereign Cloud do not use
// amazonaws.com at all - so formatting the hostname here would be wrong for every
// partition the SDK already knows about. Resolution is a pure evaluation of the
// endpoint rules compiled into the SDK: no network calls and no credentials.
//
// FIPS, dualstack and the legacy global endpoint are pinned off so this returns
// the plain regional endpoint. The SDK-driven flows (role chaining, outbound
// token) resolve their own endpoints and continue to honour the ambient
// AWS_USE_FIPS_ENDPOINT and AWS_USE_DUALSTACK_ENDPOINT preferences.
func defaultStsEndpoint(ctx context.Context, region string) (awsStsEndpoint, error) {
	resolved, err := sts.NewDefaultEndpointResolverV2().ResolveEndpoint(ctx, sts.EndpointParameters{
		Region:            aws.String(region),
		UseFIPS:           aws.Bool(false),
		UseDualStack:      aws.Bool(false),
		UseGlobalEndpoint: aws.Bool(false),
	})
	if err != nil {
		return awsStsEndpoint{}, fmt.Errorf("could not resolve an STS endpoint for region %q: %w", region, err)
	}

	uri := resolved.URI
	if uri.Host == "" {
		return awsStsEndpoint{}, fmt.Errorf("resolved STS endpoint for region %q has no host: %q", region, uri.String())
	}

	baseURL := url.URL{
		Scheme: uri.Scheme,
		Host:   uri.Host,
		Path:   uri.Path,
	}
	return awsStsEndpoint{
		authority:  uri.Host,
		baseURL:    strings.TrimRight(baseURL.String(), "/"),
		overridden: false,
	}, nil
}

// withStsBaseEndpoint returns a functional option that pins the SDK's STS endpoint
// resolution to the given override. When an override is set, it also clears
// FIPS and dualstack preferences so they don't conflict with the custom host.
func withStsBaseEndpoint(endpoint awsStsEndpoint) func(*config.LoadOptions) error {
	return func(opts *config.LoadOptions) error {
		if endpoint.overridden {
			opts.BaseEndpoint = endpoint.baseURL
			// Clear FIPS/dualstack preferences when override is set — they're only
			// endpoint-selection inputs, not crypto settings. Leaving them enabled
			// would make the SDK resolver reject the custom host.
			opts.UseFIPSEndpoint = aws.FIPSEndpointStateDisabled
			opts.UseDualStackEndpoint = aws.DualStackEndpointStateDisabled
			if os.Getenv("AWS_USE_FIPS_ENDPOINT") != "" || os.Getenv("AWS_USE_DUALSTACK_ENDPOINT") != "" {
				logger.Warnf("WorkloadIdentityHost is set; clearing FIPS/dualstack preferences to allow custom endpoint resolution")
			}
		}
		// If regional default, leave SDK's endpoint resolution alone — it respects
		// FIPS/dualstack env vars naturally.
		return nil
	}
}

func createDefaultAwsAttestationMetadataProvider(ctx context.Context, cfg *Config) awsAttestationMetadataProvider {
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithEC2IMDSRegion())
	if err != nil {
		logger.Debugf("Unable to load AWS config: %v", err)
		return nil
	}
	return &defaultAwsAttestationMetadataProvider{
		awsCfg: awsCfg,
		cfg:    cfg,
		ctx:    ctx,
	}
}

func (s *defaultAwsAttestationMetadataProvider) awsCredentials() (aws.Credentials, error) {
	return s.awsCfg.Credentials.Retrieve(s.ctx)
}

func (s *defaultAwsAttestationMetadataProvider) awsCredentialsViaRoleChaining() (aws.Credentials, error) {
	creds, err := s.awsCredentials()
	if err != nil {
		return aws.Credentials{}, err
	}
	for _, roleArn := range s.cfg.WorkloadIdentityImpersonationPath {
		if creds, err = s.assumeRole(creds, roleArn); err != nil {
			return aws.Credentials{}, err
		}
	}
	return creds, nil
}

func (s *defaultAwsAttestationMetadataProvider) assumeRole(creds aws.Credentials, roleArn string) (aws.Credentials, error) {
	logger.Debugf("assuming role %v", roleArn)
	region := s.awsRegion()
	endpoint, err := awsStsEndpointFor(s.ctx, s.cfg, region)
	if err != nil {
		return aws.Credentials{}, err
	}
	awsCfg, err := config.LoadDefaultConfig(
		s.ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.StaticCredentialsProvider{Value: creds}),
		withStsBaseEndpoint(endpoint),
	)
	if err != nil {
		return aws.Credentials{}, err
	}
	stsClient := sts.NewFromConfig(awsCfg)

	role, err := stsClient.AssumeRole(s.ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(roleArn),
		RoleSessionName: aws.String("identity-federation-session"),
	})
	if err != nil {
		logger.Debugf("failed to assume role %v: %v", roleArn, err)
		return aws.Credentials{}, err
	}

	return aws.Credentials{
		AccessKeyID:     *role.Credentials.AccessKeyId,
		SecretAccessKey: *role.Credentials.SecretAccessKey,
		SessionToken:    *role.Credentials.SessionToken,
		Expires:         *role.Credentials.Expiration,
	}, nil
}

func (s *defaultAwsAttestationMetadataProvider) awsRegion() string {
	return s.awsCfg.Region
}

func (s *defaultAwsAttestationMetadataProvider) awsWebIdentityToken(creds aws.Credentials, region string) (string, error) {
	endpoint, err := awsStsEndpointFor(s.ctx, s.cfg, region)
	if err != nil {
		return "", err
	}
	awsCfg, err := config.LoadDefaultConfig(
		s.ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.StaticCredentialsProvider{Value: creds}),
		withStsBaseEndpoint(endpoint),
	)
	if err != nil {
		return "", err
	}
	stsClient := sts.NewFromConfig(awsCfg)

	resp, err := stsClient.GetWebIdentityToken(s.ctx, &sts.GetWebIdentityTokenInput{
		Audience:         []string{snowflakeAudience},
		SigningAlgorithm: aws.String("ES384"),
	})
	if err != nil {
		logger.Debugf("failed to obtain AWS web identity token from STS: %v", err)
		return "", err
	}
	if resp.WebIdentityToken == nil {
		return "", nil
	}
	return *resp.WebIdentityToken, nil
}

func (c *awsIdentityAttestationCreator) createAttestation() (*wifAttestation, error) {
	logger.Debug("Creating AWS identity attestation...")

	attestationService := c.attestationServiceFactory(c.ctx, c.cfg)
	if attestationService == nil {
		return nil, errors.New("AWS attestation service could not be created")
	}

	var creds aws.Credentials
	var err error

	if len(c.cfg.WorkloadIdentityImpersonationPath) == 0 {
		if creds, err = attestationService.awsCredentials(); err != nil {
			logger.Debugf("error while getting for aws credentials. %v", err)
			return nil, err
		}
	} else {
		if creds, err = attestationService.awsCredentialsViaRoleChaining(); err != nil {
			logger.Debugf("error while getting for aws credentials via role chaining. %v", err)
			return nil, err
		}
	}

	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return nil, fmt.Errorf("no AWS credentials were found")
	}

	region := attestationService.awsRegion()
	if region == "" {
		return nil, fmt.Errorf("no AWS region was found")
	}

	if c.cfg.WorkloadIdentityAwsUseOutboundToken == ConfigBoolTrue {
		return c.createOutboundIdentityAttestation(attestationService, creds, region)
	}
	return c.createCallerIdentityAttestation(creds, region)
}

// createCallerIdentityAttestation produces the attestation as a base64-encoded,
// SigV4-signed STS GetCallerIdentity request envelope.
func (c *awsIdentityAttestationCreator) createCallerIdentityAttestation(creds aws.Credentials, region string) (*wifAttestation, error) {
	endpoint, err := awsStsEndpointFor(c.ctx, c.cfg, region)
	if err != nil {
		return nil, err
	}
	req, err := c.createStsRequest(endpoint)
	if err != nil {
		return nil, err
	}

	err = c.signRequestWithSigV4(c.ctx, req, creds, region)
	if err != nil {
		return nil, err
	}

	credential, err := c.createBase64EncodedRequestCredential(req)
	if err != nil {
		return nil, err
	}

	return &wifAttestation{
		ProviderType: string(awsWif),
		Credential:   credential,
		Metadata:     map[string]string{},
	}, nil
}

// createOutboundIdentityAttestation produces the attestation as a signed JWT
// obtained from the STS GetWebIdentityToken API.
func (c *awsIdentityAttestationCreator) createOutboundIdentityAttestation(attestationService awsAttestationMetadataProvider, creds aws.Credentials, region string) (*wifAttestation, error) {
	token, err := attestationService.awsWebIdentityToken(creds, region)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("failed to obtain AWS web identity token from STS")
	}
	return &wifAttestation{
		ProviderType: string(awsWif),
		Credential:   token,
		Metadata:     map[string]string{},
	}, nil
}

func (c *awsIdentityAttestationCreator) createStsRequest(endpoint awsStsEndpoint) (*http.Request, error) {
	url := endpoint.baseURL + "?Action=GetCallerIdentity&Version=2011-06-15"
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Host", endpoint.authority)
	req.Header.Set("X-Snowflake-Audience", "snowflakecomputing.com")
	return req, nil
}

func (c *awsIdentityAttestationCreator) signRequestWithSigV4(ctx context.Context, req *http.Request, creds aws.Credentials, region string) error {
	signer := v4.NewSigner()
	// as per docs of SignHTTP, the payload hash must be present even if the payload is empty
	payloadHash := hex.EncodeToString(sha256.New().Sum(nil))
	return signer.SignHTTP(ctx, creds, req, payloadHash, "sts", region, time.Now())
}

func (c *awsIdentityAttestationCreator) createBase64EncodedRequestCredential(req *http.Request) (string, error) {
	headers := make(map[string]string)
	for key, values := range req.Header {
		headers[key] = values[0]
	}

	assertion := map[string]any{
		"url":     req.URL.String(),
		"method":  req.Method,
		"headers": headers,
	}

	assertionJSON, err := json.Marshal(assertion)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(assertionJSON), nil
}

func (c *gcpIdentityAttestationCreator) createAttestation() (*wifAttestation, error) {
	logger.Debugf("Creating GCP identity attestation...")
	if len(c.cfg.WorkloadIdentityImpersonationPath) == 0 {
		return c.createGcpIdentityTokenFromMetadataService()
	}
	return c.createGcpIdentityViaImpersonation()
}

func (c *gcpIdentityAttestationCreator) createGcpIdentityTokenFromMetadataService() (*wifAttestation, error) {
	req, err := c.createTokenRequest()
	if err != nil {
		return nil, fmt.Errorf("failed to create GCP token request: %w", err)
	}
	token := fetchTokenFromMetadataService(req, c.cfg, c.telemetry)
	if token == "" {
		return nil, fmt.Errorf("no GCP token was found")
	}
	sub, _, err := extractSubIssWithoutVerifyingSignature(token)
	if err != nil {
		return nil, fmt.Errorf("could not extract claims from token: %v", err)
	}
	return &wifAttestation{
		ProviderType: string(gcpWif),
		Credential:   token,
		Metadata:     map[string]string{"sub": sub},
	}, nil
}

func (c *gcpIdentityAttestationCreator) createTokenRequest() (*http.Request, error) {
	uri := fmt.Sprintf("%s/computeMetadata/v1/instance/service-accounts/default/identity?audience=%s",
		c.metadataServiceBaseURL, snowflakeAudience)
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %v", err)
	}
	req.Header.Set(gcpMetadataFlavorHeaderName, gcpMetadataFlavor)
	return req, nil
}

func (c *gcpIdentityAttestationCreator) createGcpIdentityViaImpersonation() (*wifAttestation, error) {
	// initialize transport
	transport, err := newTransportFactory(c.cfg, c.telemetry).createTransport(transportConfigFor(transportTypeWIF))
	if err != nil {
		logger.Debugf("Failed to create HTTP transport: %v", err)
		return nil, err
	}
	client := &http.Client{Transport: transport}

	// fetch access token for impersonation
	accessToken, err := c.fetchServiceToken(client)
	if err != nil {
		return nil, err
	}

	// map paths to full service account paths
	var fullServiceAccountPaths []string
	for _, path := range c.cfg.WorkloadIdentityImpersonationPath {
		fullServiceAccountPaths = append(fullServiceAccountPaths, "projects/-/serviceAccounts/"+path)
	}
	targetServiceAccount := fullServiceAccountPaths[len(fullServiceAccountPaths)-1]
	delegates := fullServiceAccountPaths[:len(fullServiceAccountPaths)-1]

	// fetch impersonated token
	impersonationToken, err := c.fetchImpersonatedToken(targetServiceAccount, delegates, accessToken, client)
	if err != nil {
		return nil, err
	}

	// create attestation
	sub, _, err := extractSubIssWithoutVerifyingSignature(impersonationToken)
	if err != nil {
		return nil, fmt.Errorf("could not extract claims from token: %v", err)
	}
	return &wifAttestation{
		ProviderType: string(gcpWif),
		Credential:   impersonationToken,
		Metadata:     map[string]string{"sub": sub},
	}, nil
}

func (c *gcpIdentityAttestationCreator) fetchServiceToken(client *http.Client) (string, error) {
	// initialize and do request
	req, err := http.NewRequest("GET", c.metadataServiceBaseURL+"/computeMetadata/v1/instance/service-accounts/default/token", nil)
	if err != nil {
		logger.Debugf("cannot create token request for impersonation. %v", err)
		return "", err
	}
	req.Header.Set(gcpMetadataFlavorHeaderName, gcpMetadataFlavor)
	resp, err := client.Do(req)
	if err != nil {
		logger.Debugf("cannot fetch token for impersonation. %v", err)
		return "", err
	}
	defer func(body io.ReadCloser) {
		if err = body.Close(); err != nil {
			logger.Debugf("cannot close token response body for impersonation. %v", err)
		}
	}(resp.Body)

	// if it is not 200, do not parse the response
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token response status is %v, not parsing", resp.StatusCode)
	}

	// parse response and extract access token
	accessTokenResponse := struct {
		AccessToken string `json:"access_token"`
	}{}
	if err = json.NewDecoder(resp.Body).Decode(&accessTokenResponse); err != nil {
		logger.Debugf("cannot decode token for impersonation. %v", err)
		return "", err
	}
	accessToken := accessTokenResponse.AccessToken
	return accessToken, nil
}

func (c *gcpIdentityAttestationCreator) fetchImpersonatedToken(targetServiceAccount string, delegates []string, accessToken string, client *http.Client) (string, error) {
	// prepare the request
	url := fmt.Sprintf("%v/v1/%v:generateIdToken", c.iamCredentialsURL, targetServiceAccount)
	body := struct {
		Delegates []string `json:"delegates,omitempty"`
		Audience  string   `json:"audience"`
	}{
		Delegates: delegates,
		Audience:  snowflakeAudience,
	}
	payload := new(bytes.Buffer)
	if err := json.NewEncoder(payload).Encode(body); err != nil {
		logger.Debugf("cannot encode impersonation request body. %v", err)
		return "", err
	}
	req, err := http.NewRequest("POST", url, payload)
	if err != nil {
		logger.Debugf("cannot create token request for impersonation. %v", err)
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	// send the request
	resp, err := client.Do(req)
	if err != nil {
		logger.Debugf("cannot call impersonation service. %v", err)
		return "", err
	}
	defer func(body io.ReadCloser) {
		if err = body.Close(); err != nil {
			logger.Debugf("cannot close token response body for impersonation. %v", err)
		}
	}(resp.Body)

	// handle the response
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("response status is %v, not parsing", resp.StatusCode)
	}
	tokenResponse := struct {
		Token string `json:"token"`
	}{}
	if err = json.NewDecoder(resp.Body).Decode(&tokenResponse); err != nil {
		logger.Debugf("cannot decode token response. %v", err)
		return "", err
	}
	return tokenResponse.Token, nil
}

func fetchTokenFromMetadataService(req *http.Request, cfg *Config, telemetry *snowflakeTelemetry) string {
	transport, err := newTransportFactory(cfg, telemetry).createTransport(transportConfigFor(transportTypeWIF))
	if err != nil {
		logger.Debugf("Failed to create HTTP transport: %v", err)
		return ""
	}
	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		logger.Debugf("Metadata server request was not successful: %v", err)
		return ""
	}
	defer func() {
		if err = resp.Body.Close(); err != nil {
			logger.Debugf("Failed to close response body: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Debugf("Failed to read response body: %v", err)
		return ""
	}
	return string(body)
}

func extractSubIssWithoutVerifyingSignature(token string) (subject string, issuer string, err error) {
	claims, err := extractClaimsMap(token)
	if err != nil {
		return "", "", err
	}
	issuerClaim, ok := claims["iss"]
	if !ok {
		return "", "", errors.New("missing issuer claim in JWT token")
	}
	subjectClaim, ok := claims["sub"]
	if !ok {
		return "", "", errors.New("missing sub claim in JWT token")
	}
	subject, ok = subjectClaim.(string)
	if !ok {
		return "", "", errors.New("sub claim is not a string in JWT token")
	}
	issuer, ok = issuerClaim.(string)
	if !ok {
		return "", "", errors.New("iss claim is not a string in JWT token")
	}
	return
}

// extractClaimsMap parses a JWT token and returns its claims as a map.
// It does not verify the token signature.
func extractClaimsMap(token string) (map[string]any, error) {
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	claims := jwt.MapClaims{}
	_, _, err := parser.ParseUnverified(token, claims)
	if err != nil {
		return nil, fmt.Errorf("unable to extract JWT claims from token: %w", err)
	}
	return claims, nil
}

func (c *oidcIdentityAttestationCreator) createAttestation() (*wifAttestation, error) {
	logger.Debugf("Creating OIDC identity attestation...")
	token, err := c.token()
	if err != nil {
		return nil, fmt.Errorf("failed to get OIDC token: %w", err)
	}
	if token == "" {
		return nil, fmt.Errorf("no OIDC token was specified")
	}
	sub, iss, err := extractSubIssWithoutVerifyingSignature(token)
	if err != nil {
		return nil, err
	}
	if sub == "" || iss == "" {
		return nil, errors.New("missing sub or iss claim in JWT token")
	}
	return &wifAttestation{
		ProviderType: string(oidcWif),
		Credential:   token,
		Metadata:     map[string]string{"sub": sub},
	}, nil
}

// azureAttestationMetadataProvider defines the interface for Azure attestation services
type azureAttestationMetadataProvider interface {
	identityEndpoint() string
	identityHeader() string
	clientID() string
}

type defaultAzureAttestationMetadataProvider struct{}

func (p *defaultAzureAttestationMetadataProvider) identityEndpoint() string {
	return os.Getenv("IDENTITY_ENDPOINT")
}

func (p *defaultAzureAttestationMetadataProvider) identityHeader() string {
	return os.Getenv("IDENTITY_HEADER")
}

func (p *defaultAzureAttestationMetadataProvider) clientID() string {
	return os.Getenv("MANAGED_IDENTITY_CLIENT_ID")
}

type azureIdentityAttestationCreator struct {
	azureAttestationMetadataProvider azureAttestationMetadataProvider
	cfg                              *Config
	telemetry                        *snowflakeTelemetry
	workloadIdentityEntraResource    string
	azureMetadataServiceBaseURL      string
}

// createAttestation creates an attestation using Azure identity
func (a *azureIdentityAttestationCreator) createAttestation() (*wifAttestation, error) {
	logger.Debug("Creating Azure identity attestation...")

	identityEndpoint := a.azureAttestationMetadataProvider.identityEndpoint()
	var request *http.Request
	var err error

	clientID := a.managedIdentityClientID()
	if identityEndpoint == "" {
		request, err = a.azureVMIdentityRequest(clientID)
		if err != nil {
			return nil, fmt.Errorf("failed to create Azure VM identity request: %v", err)
		}
	} else {
		identityHeader := a.azureAttestationMetadataProvider.identityHeader()
		if identityHeader == "" {
			return nil, fmt.Errorf("managed identity is not enabled on this Azure function")
		}
		request, err = a.azureFunctionsIdentityRequest(
			identityEndpoint,
			identityHeader,
			clientID,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create Azure Functions identity request: %v", err)
		}
	}

	tokenJSON := fetchTokenFromMetadataService(request, a.cfg, a.telemetry)
	if tokenJSON == "" {
		return nil, fmt.Errorf("could not fetch Azure token")
	}

	token, err := extractTokenFromJSON(tokenJSON)
	if err != nil {
		return nil, fmt.Errorf("failed to extract token from JSON: %v", err)
	}
	if token == "" {
		return nil, fmt.Errorf("no access token found in Azure response")
	}

	sub, iss, err := extractSubIssWithoutVerifyingSignature(token)
	if err != nil {
		return nil, fmt.Errorf("failed to extract sub and iss claims from token: %v", err)
	}
	if sub == "" || iss == "" {
		return nil, fmt.Errorf("missing sub or iss claim in JWT token")
	}

	return &wifAttestation{
		ProviderType: string(azureWif),
		Credential:   token,
		Metadata:     map[string]string{"sub": sub, "iss": iss},
	}, nil
}

func determineEntraResource(config *Config) string {
	if config != nil && config.WorkloadIdentityEntraResource != "" {
		return config.WorkloadIdentityEntraResource
	}
	// default resource if none specified
	return "api://fd3f753b-eed3-462c-b6a7-a4b5bb650aad"
}

func extractTokenFromJSON(tokenJSON string) (string, error) {
	var response struct {
		AccessToken string `json:"access_token"`
	}

	err := json.Unmarshal([]byte(tokenJSON), &response)
	if err != nil {
		return "", err
	}

	return response.AccessToken, nil
}

// managedIdentityClientID is the user-assigned managed identity to request.
// An explicit config value takes precedence over MANAGED_IDENTITY_CLIENT_ID.
func (a *azureIdentityAttestationCreator) managedIdentityClientID() string {
	if a.cfg != nil && a.cfg.WorkloadIdentityAzureClientID != "" {
		return a.cfg.WorkloadIdentityAzureClientID
	}
	if a.azureAttestationMetadataProvider != nil {
		return a.azureAttestationMetadataProvider.clientID()
	}
	return ""
}

func (a *azureIdentityAttestationCreator) azureFunctionsIdentityRequest(identityEndpoint, identityHeader, managedIdentityClientID string) (*http.Request, error) {
	// IDENTITY_ENDPOINT comes from the process environment. Azure's contract
	// places it on a loopback / link-local address (App Service, Functions and
	// Arc all expose MSI on localhost). Verify that before the IDENTITY_HEADER
	// value is attached and the request is issued.
	if err := validateLocalMetadataEndpoint(identityEndpoint); err != nil {
		return nil, err
	}

	// Build the query with url.Values so the Entra resource and the
	// managed-identity client id are percent-encoded. With raw interpolation a
	// value such as "api://app&client_id=<other>" would add or replace other
	// IMDS query parameters instead of staying confined to its own.
	values := url.Values{}
	values.Set("api-version", "2019-08-01")
	values.Set("resource", a.workloadIdentityEntraResource)
	if managedIdentityClientID != "" {
		values.Set("client_id", managedIdentityClientID)
	}

	requestURL := fmt.Sprintf("%s?%s", identityEndpoint, values.Encode())
	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("X-IDENTITY-HEADER", identityHeader)

	return req, nil
}

// validateLocalMetadataEndpoint requires the identity endpoint URL to address
// the local metadata service (loopback or link-local) before any platform value
// is attached to a request sent to it. Remote hosts are rejected.
func validateLocalMetadataEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid identity endpoint URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("identity endpoint must use http or https scheme, got %q", parsed.Scheme)
	}
	host := parsed.Hostname()
	if !isLoopbackOrLinkLocalHost(host) {
		return fmt.Errorf("identity endpoint host %q is not a permitted local metadata address", host)
	}
	return nil
}

// isLoopbackOrLinkLocalHost reports whether host is the loopback hostname or a
// loopback / link-local IP literal: localhost, 127.0.0.0/8, ::1,
// 169.254.0.0/16 and fe80::/10.
func isLoopbackOrLinkLocalHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

func (a *azureIdentityAttestationCreator) azureVMIdentityRequest(managedIdentityClientID string) (*http.Request, error) {
	// Percent-encode the query so the Entra resource cannot inject additional
	// IMDS parameters (e.g. client_id/object_id/mi_res_id) that would select a
	// different managed identity attached to the same VM.
	values := url.Values{}
	values.Set("api-version", "2018-02-01")
	values.Set("resource", a.workloadIdentityEntraResource)
	if managedIdentityClientID != "" {
		values.Set("client_id", managedIdentityClientID)
	}

	requestURL := a.azureMetadataServiceBaseURL + "/metadata/identity/oauth2/token?" + values.Encode()
	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Metadata", "true")

	return req, nil
}
