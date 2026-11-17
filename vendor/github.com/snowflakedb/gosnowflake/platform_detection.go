package gosnowflake

import (
	"context"
	"errors"
	"io"
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

type detectorFunc struct {
	name string
	fn   func(ctx context.Context, timeout time.Duration) platformDetectionState
}

func detectPlatforms(ctx context.Context, timeout time.Duration) []string {
	if strings.EqualFold(os.Getenv(disablePlatformDetectionEnv), "true") {
		return []string{"disabled"}
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

	detectionStates := make(map[string]platformDetectionState, len(detectors))
	var waitGroup sync.WaitGroup
	var mutex sync.Mutex
	waitGroup.Add(len(detectors))

	for _, detector := range detectors {
		go func(detector detectorFunc) {
			defer waitGroup.Done()
			detectionState := detector.fn(ctx, timeout)
			mutex.Lock()
			detectionStates[detector.name] = detectionState
			mutex.Unlock()
		}(detector)
	}
	waitGroup.Wait()

	detectedPlatformNames := []string{}
	for _, detector := range detectors {
		if detectionStates[detector.name] == platformDetected {
			detectedPlatformNames = append(detectedPlatformNames, detector.name)
		}
	}

	logger.Debugf("detectPlatforms: completed. Detection states: %v", detectionStates)
	return detectedPlatformNames
}

func detectAwsLambdaEnv(_ context.Context, _ time.Duration) platformDetectionState {
	if os.Getenv("AWS_LAMBDA_TASK_ROOT") != "" {
		return platformDetected
	}
	return platformNotDetected
}

func detectGithubActionsEnv(_ context.Context, _ time.Duration) platformDetectionState {
	if os.Getenv("GITHUB_ACTIONS") != "" {
		return platformDetected
	}
	return platformNotDetected
}

func detectAzureFunctionEnv(_ context.Context, _ time.Duration) platformDetectionState {
	if os.Getenv("FUNCTIONS_WORKER_RUNTIME") != "" &&
		os.Getenv("FUNCTIONS_EXTENSION_VERSION") != "" &&
		os.Getenv("AzureWebJobsStorage") != "" {
		return platformDetected
	}
	return platformNotDetected
}

func detectGceCloudRunServiceEnv(_ context.Context, _ time.Duration) platformDetectionState {
	if os.Getenv("K_SERVICE") != "" && os.Getenv("K_REVISION") != "" && os.Getenv("K_CONFIGURATION") != "" {
		return platformDetected
	}
	return platformNotDetected
}

func detectGceCloudRunJobEnv(_ context.Context, _ time.Duration) platformDetectionState {
	if os.Getenv("CLOUD_RUN_JOB") != "" && os.Getenv("CLOUD_RUN_EXECUTION") != "" {
		return platformDetected
	}
	return platformNotDetected
}

func detectEc2Instance(ctx context.Context, timeout time.Duration) platformDetectionState {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cfg, err := config.LoadDefaultConfig(timeoutCtx, config.WithLogger(logging.NewStandardLogger(io.Discard)))
	if err != nil {
		return platformNotDetected
	}

	client := imds.NewFromConfig(cfg)
	result, err := client.GetInstanceIdentityDocument(timeoutCtx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return platformDetectionTimeout
		}
		return platformNotDetected
	}
	if result != nil && result.InstanceID != "" {
		return platformDetected
	}

	return platformNotDetected
}

func detectAwsIdentity(ctx context.Context, timeout time.Duration) platformDetectionState {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cfg, err := config.LoadDefaultConfig(timeoutCtx, config.WithLogger(logging.NewStandardLogger(io.Discard)))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return platformDetectionTimeout
		}
		return platformNotDetected
	}

	client := sts.NewFromConfig(cfg)
	out, err := client.GetCallerIdentity(timeoutCtx, &sts.GetCallerIdentityInput{})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return platformDetectionTimeout
		}
		return platformNotDetected
	}
	if out == nil || out.Arn == nil || *out.Arn == "" {
		return platformNotDetected
	}
	if isValidArnForWif(*out.Arn) {
		return platformDetected
	}
	return platformNotDetected
}

func detectAzureVM(ctx context.Context, timeout time.Duration) platformDetectionState {
	client := metadataServerHTTPClient(timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, azureMetadataBaseURL+"/metadata/instance?api-version=2019-03-11", nil)
	if err != nil {
		return platformNotDetected
	}
	req.Header.Set("Metadata", "true")
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return platformDetectionTimeout
		}
		return platformNotDetected
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return platformDetected
	}
	return platformNotDetected
}

func detectAzureManagedIdentity(ctx context.Context, timeout time.Duration) platformDetectionState {
	if detectAzureFunctionEnv(ctx, timeout) == platformDetected && os.Getenv("IDENTITY_HEADER") != "" {
		return platformDetected
	}
	client := metadataServerHTTPClient(timeout)
	values := url.Values{}
	values.Set("api-version", "2018-02-01")
	values.Set("resource", "https://management.azure.com")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, azureMetadataBaseURL+"/metadata/identity/oauth2/token?"+values.Encode(), nil)
	if err != nil {
		return platformNotDetected
	}
	req.Header.Set("Metadata", "True")
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return platformDetectionTimeout
		}
		return platformNotDetected
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return platformDetected
	}
	return platformNotDetected
}

func detectGceVM(ctx context.Context, timeout time.Duration) platformDetectionState {
	client := metadataServerHTTPClient(timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gceMetadataRootURL, nil)
	if err != nil {
		return platformNotDetected
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return platformDetectionTimeout
		}
		return platformNotDetected
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get(gcpMetadataFlavorHeaderName) == gcpMetadataFlavor {
		return platformDetected
	}
	return platformNotDetected
}

func detectGcpIdentity(ctx context.Context, timeout time.Duration) platformDetectionState {
	client := metadataServerHTTPClient(timeout)
	url := gcpMetadataBaseURL + "/instance/service-accounts/default/email"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return platformNotDetected
	}
	req.Header.Set(gcpMetadataFlavorHeaderName, gcpMetadataFlavor)
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return platformDetectionTimeout
		}
		return platformNotDetected
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return platformDetected
	}
	return platformNotDetected
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
