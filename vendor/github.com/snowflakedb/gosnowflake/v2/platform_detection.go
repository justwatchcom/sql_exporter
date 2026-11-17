package gosnowflake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go/logging"
)

type platformDetectionState string

const (
	platformDetected         platformDetectionState = "detected"
	platformNotDetected      platformDetectionState = "not_detected"
	platformDetectionTimeout platformDetectionState = "timeout"
)

const disablePlatformDetectionEnv = "SNOWFLAKE_DISABLE_PLATFORM_DETECTION"

var (
	azureMetadataBaseURL = "http://169.254.169.254"
	gceMetadataRootURL   = "http://metadata.google.internal"
	gcpMetadataBaseURL   = "http://metadata.google.internal/computeMetadata/v1"
)

var (
	detectedPlatformsCache    []string
	initPlatformDetectionOnce sync.Once
	platformDetectionDone     = make(chan struct{})
)

func initPlatformDetection() {
	initPlatformDetectionOnce.Do(func() {
		go func() {
			detectedPlatformsCache = detectPlatforms(context.Background(), 200*time.Millisecond)
			defer close(platformDetectionDone)
		}()
	})
}

func getDetectedPlatforms() []string {
	logger.Debugf("getDetectedPlatforms: waiting for platform detection to complete")
	<-platformDetectionDone
	logger.Debugf("getDetectedPlatforms: returning cached detected platforms: %v", detectedPlatformsCache)
	return detectedPlatformsCache
}

func metadataServerHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true,
		},
	}
}

func isDetectionTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

type detectorFunc struct {
	name string
	fn   func(ctx context.Context, timeout time.Duration) (platformDetectionState, string, error)
}

// platformDetectionResult records how a single detector finished. Timeout and
// transport failures are errors; expected misses (404, missing env, unusable
// ARN) are a reason string so they are not logged as failures.
type platformDetectionResult struct {
	state    platformDetectionState
	reason   string
	err      error
	duration time.Duration
}

func (r platformDetectionResult) String() string {
	if r.err != nil {
		return fmt.Sprintf("%s after %v (%s)", r.state, r.duration, r.err)
	}
	if r.reason != "" {
		return fmt.Sprintf("%s after %v (%s)", r.state, r.duration, r.reason)
	}
	return fmt.Sprintf("%s after %v", r.state, r.duration)
}

func detectPlatforms(ctx context.Context, timeout time.Duration) []string {
	platforms, _ := detectPlatformsWithResults(ctx, timeout)
	return platforms
}

func detectPlatformsWithResults(ctx context.Context, timeout time.Duration) ([]string, map[string]platformDetectionResult) {
	if strings.EqualFold(os.Getenv(disablePlatformDetectionEnv), "true") {
		return []string{"disabled"}, map[string]platformDetectionResult{}
	}

	detectors := []detectorFunc{
		{name: "is_aws_lambda", fn: detectAwsLambdaEnv},
		{name: "is_azure_function", fn: detectAzureFunctionEnv},
		{name: "is_gce_cloud_run_service", fn: detectGceCloudRunServiceEnv},
		{name: "is_gce_cloud_run_job", fn: detectGceCloudRunJobEnv},
		{name: "is_github_action", fn: detectGithubActionsEnv},
		{name: "is_ec2_instance", fn: detectEc2Instance},
		{name: "has_aws_identity", fn: detectAwsIdentity},
		{name: "is_azure_vm", fn: detectAzureVM},
		{name: "has_azure_managed_identity", fn: detectAzureManagedIdentity},
		{name: "is_gce_vm", fn: detectGceVM},
		{name: "has_gcp_identity", fn: detectGcpIdentity},
	}

	detectionResults := make(map[string]platformDetectionResult, len(detectors))
	var waitGroup sync.WaitGroup
	var mutex sync.Mutex

	for _, detector := range detectors {
		waitGroup.Go(func() {
			start := time.Now()
			detectionState, reason, err := detector.fn(ctx, timeout)
			detectionResult := platformDetectionResult{state: detectionState, reason: reason, err: err, duration: time.Since(start)}
			mutex.Lock()
			detectionResults[detector.name] = detectionResult
			mutex.Unlock()
		})
	}
	waitGroup.Wait()

	detectedPlatformNames := []string{}
	for _, detector := range detectors {
		if detectionResults[detector.name].state == platformDetected {
			detectedPlatformNames = append(detectedPlatformNames, detector.name)
		}
	}

	for _, detector := range detectors {
		if detectionResult := detectionResults[detector.name]; detectionResult.state != platformDetected {
			logger.Debugf("detectPlatforms: %s %s - %s", detector.name, detectionResult.state, detectionResult)
		}
	}
	logger.Debugf("detectPlatforms: completed. Detection results: %v", detectionResults)
	return detectedPlatformNames, detectionResults
}

func detectAwsLambdaEnv(_ context.Context, _ time.Duration) (platformDetectionState, string, error) {
	if os.Getenv("LAMBDA_TASK_ROOT") != "" {
		return platformDetected, "", nil
	}
	return platformNotDetected, "", nil
}

func detectGithubActionsEnv(_ context.Context, _ time.Duration) (platformDetectionState, string, error) {
	if os.Getenv("GITHUB_ACTIONS") != "" {
		return platformDetected, "", nil
	}
	return platformNotDetected, "", nil
}

func detectAzureFunctionEnv(_ context.Context, _ time.Duration) (platformDetectionState, string, error) {
	if os.Getenv("FUNCTIONS_WORKER_RUNTIME") != "" &&
		os.Getenv("FUNCTIONS_EXTENSION_VERSION") != "" &&
		os.Getenv("AzureWebJobsStorage") != "" {
		return platformDetected, "", nil
	}
	return platformNotDetected, "", nil
}

func detectGceCloudRunServiceEnv(_ context.Context, _ time.Duration) (platformDetectionState, string, error) {
	if os.Getenv("K_SERVICE") != "" && os.Getenv("K_REVISION") != "" && os.Getenv("K_CONFIGURATION") != "" {
		return platformDetected, "", nil
	}
	return platformNotDetected, "", nil
}

func detectGceCloudRunJobEnv(_ context.Context, _ time.Duration) (platformDetectionState, string, error) {
	if os.Getenv("CLOUD_RUN_JOB") != "" && os.Getenv("CLOUD_RUN_EXECUTION") != "" {
		return platformDetected, "", nil
	}
	return platformNotDetected, "", nil
}

func classificationForRequestError(err error) platformDetectionState {
	if isDetectionTimeout(err) {
		return platformDetectionTimeout
	}
	return platformNotDetected
}

func metadataRequestError(metadataURL string, err error) (platformDetectionState, string, error) {
	if isDetectionTimeout(err) {
		return platformDetectionTimeout, "", fmt.Errorf("request to %s timed out: %w", metadataURL, err)
	}
	return platformNotDetected, "", fmt.Errorf("request to %s failed: %w", metadataURL, err)
}

func detectEc2Instance(ctx context.Context, timeout time.Duration) (platformDetectionState, string, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cfg, err := config.LoadDefaultConfig(timeoutCtx, config.WithLogger(logging.NewStandardLogger(io.Discard)))
	if err != nil {
		return classificationForRequestError(err), "", fmt.Errorf("cannot load AWS configuration: %w", err)
	}

	client := imds.NewFromConfig(cfg)
	result, err := client.GetInstanceIdentityDocument(timeoutCtx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return classificationForRequestError(err), "", fmt.Errorf("cannot get EC2 instance identity document: %w", err)
	}
	if result != nil && result.InstanceID != "" {
		return platformDetected, "", nil
	}

	return platformNotDetected, "EC2 instance identity document contains no instance ID", nil
}

func detectAwsIdentity(ctx context.Context, timeout time.Duration) (platformDetectionState, string, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cfg, err := config.LoadDefaultConfig(timeoutCtx, config.WithLogger(logging.NewStandardLogger(io.Discard)))
	if err != nil {
		return classificationForRequestError(err), "", fmt.Errorf("cannot load AWS configuration: %w", err)
	}

	client := sts.NewFromConfig(cfg)
	out, err := client.GetCallerIdentity(timeoutCtx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return classificationForRequestError(err), "", fmt.Errorf("cannot get AWS caller identity: %w", err)
	}
	if out == nil || out.Arn == nil || *out.Arn == "" {
		return platformNotDetected, "AWS caller identity contains no ARN", nil
	}
	if isValidArnForWif(*out.Arn) {
		return platformDetected, "", nil
	}
	return platformNotDetected, "AWS caller identity ARN is not usable for workload identity", nil
}

func detectAzureVM(ctx context.Context, timeout time.Duration) (platformDetectionState, string, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := metadataServerHTTPClient(timeout)
	metadataURL := azureMetadataBaseURL + "/metadata/instance?api-version=2019-03-11"
	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return platformNotDetected, "", fmt.Errorf("cannot create request to %s: %w", metadataURL, err)
	}
	req.Header.Set("Metadata", "true")
	resp, err := client.Do(req)
	if err != nil {
		return metadataRequestError(metadataURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return platformDetected, "", nil
	}
	return platformNotDetected, fmt.Sprintf("request to %s returned status %v", metadataURL, resp.StatusCode), nil
}

func detectAzureManagedIdentity(ctx context.Context, timeout time.Duration) (platformDetectionState, string, error) {
	if azureFunctionState, _, _ := detectAzureFunctionEnv(ctx, timeout); azureFunctionState == platformDetected && os.Getenv("IDENTITY_HEADER") != "" {
		return platformDetected, "", nil
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := metadataServerHTTPClient(timeout)
	values := url.Values{}
	values.Set("api-version", "2018-02-01")
	values.Set("resource", "https://management.azure.com")
	metadataURL := azureMetadataBaseURL + "/metadata/identity/oauth2/token?" + values.Encode()
	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return platformNotDetected, "", fmt.Errorf("cannot create request to %s: %w", metadataURL, err)
	}
	req.Header.Set("Metadata", "true")
	resp, err := client.Do(req)
	if err != nil {
		return metadataRequestError(metadataURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return platformDetected, "", nil
	}
	return platformNotDetected, fmt.Sprintf("request to %s returned status %v", metadataURL, resp.StatusCode), nil
}

func detectGceVM(ctx context.Context, timeout time.Duration) (platformDetectionState, string, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := metadataServerHTTPClient(timeout)
	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodGet, gceMetadataRootURL, nil)
	if err != nil {
		return platformNotDetected, "", fmt.Errorf("cannot create request to %s: %w", gceMetadataRootURL, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return metadataRequestError(gceMetadataRootURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get(gcpMetadataFlavorHeaderName) == gcpMetadataFlavor {
		return platformDetected, "", nil
	}
	return platformNotDetected, fmt.Sprintf("response from %s (status %v) has no %s: %s header", gceMetadataRootURL, resp.StatusCode, gcpMetadataFlavorHeaderName, gcpMetadataFlavor), nil
}

func detectGcpIdentity(ctx context.Context, timeout time.Duration) (platformDetectionState, string, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := metadataServerHTTPClient(timeout)
	metadataURL := gcpMetadataBaseURL + "/instance/service-accounts/default/email"
	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return platformNotDetected, "", fmt.Errorf("cannot create request to %s: %w", metadataURL, err)
	}
	req.Header.Set(gcpMetadataFlavorHeaderName, gcpMetadataFlavor)
	resp, err := client.Do(req)
	if err != nil {
		return metadataRequestError(metadataURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return platformDetected, "", nil
	}
	return platformNotDetected, fmt.Sprintf("request to %s returned status %v", metadataURL, resp.StatusCode), nil
}

func isValidArnForWif(arn string) bool {
	patterns := []string{
		`^arn:[^:]+:iam::[^:]+:user/.+$`,
		`^arn:[^:]+:sts::[^:]+:assumed-role/.+$`,
	}
	for _, pattern := range patterns {
		matched, err := regexp.MatchString(pattern, arn)
		if err == nil && matched {
			return true
		}
	}
	return false
}
