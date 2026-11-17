package gosnowflake

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
	"io"
	"net/http"
	"os"
	"strings"
)

const (
	sfcDigest  = "sfc-digest"
	amzMatdesc = "x-amz-matdesc"
	amzKey     = "x-amz-key"
	amzIv      = "x-amz-iv"

	notFound             = "NotFound"
	expiredToken         = "ExpiredToken"
	errNoWsaeconnaborted = "10053"
)

type snowflakeS3Client struct {
	cfg       *Config
	telemetry *snowflakeTelemetry
}

type s3Location struct {
	bucketName string
	s3Path     string
}

// S3LoggingMode allows to configure which logs should be included.
// By default no logs are included.
// See https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/aws#ClientLogMode for allowed values.
// Deprecated: will be moved to DSN/Config in a future release.
var S3LoggingMode aws.ClientLogMode

func (util *snowflakeS3Client) createClient(info *execResponseStageInfo, useAccelerateEndpoint bool, telemetry *snowflakeTelemetry) (cloudClient, error) {
	stageCredentials := info.Creds
	s3Logger := logging.LoggerFunc(s3LoggingFunc)
	endPoint := getS3CustomEndpoint(info)

	transport, err := newTransportFactory(util.cfg, telemetry).createTransport(util.cfg.transportConfigFor(transportTypeCloudProvider))
	if err != nil {
		return nil, err
	}
	return s3.New(s3.Options{
		Region: info.Region,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
			stageCredentials.AwsKeyID,
			stageCredentials.AwsSecretKey,
			stageCredentials.AwsToken)),
		BaseEndpoint:  endPoint,
		UseAccelerate: useAccelerateEndpoint,
		HTTPClient: &http.Client{
			Transport: transport,
		},
		ClientLogMode: S3LoggingMode,
		Logger:        s3Logger,
	}), nil
}

// to be used with S3 transferAccelerateConfigWithUtil
func (util *snowflakeS3Client) createClientWithConfig(info *execResponseStageInfo, useAccelerateEndpoint bool, cfg *Config, telemetry *snowflakeTelemetry) (cloudClient, error) {
	// copy snowflakeFileTransferAgent's config onto the cloud client so we could decide which Transport to use
	util.cfg = cfg
	util.telemetry = telemetry
	return util.createClient(info, useAccelerateEndpoint, telemetry)
}

func getS3CustomEndpoint(info *execResponseStageInfo) *string {
	var endPoint *string
	isRegionalURLEnabled := info.UseRegionalURL || info.UseS3RegionalURL
	if info.EndPoint != "" {
		tmp := fmt.Sprintf("https://%s", info.EndPoint)
		endPoint = &tmp
	} else if info.Region != "" && isRegionalURLEnabled {
		domainSuffixForRegionalURL := "amazonaws.com"
		if strings.HasPrefix(strings.ToLower(info.Region), "cn-") {
			domainSuffixForRegionalURL = "amazonaws.com.cn"
		}
		tmp := fmt.Sprintf("https://s3.%s.%s", info.Region, domainSuffixForRegionalURL)
		endPoint = &tmp
	}
	return endPoint
}

func s3LoggingFunc(classification logging.Classification, format string, v ...interface{}) {
	switch classification {
	case logging.Debug:
		logger.WithField("logger", "S3").Debugf(format, v...)
	case logging.Warn:
		logger.WithField("logger", "S3").Warnf(format, v...)
	}
}

type s3HeaderAPI interface {
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

// cloudUtil implementation
func (util *snowflakeS3Client) getFileHeader(meta *fileMetadata, filename string) (*fileHeader, error) {
	headObjInput, err := util.getS3Object(meta, filename)
	if err != nil {
		return nil, err
	}
	var s3Cli s3HeaderAPI
	s3Cli, ok := meta.client.(*s3.Client)
	if !ok {
		return nil, errors.New("could not parse client to s3.Client")
	}
	// for testing only
	if meta.mockHeader != nil {
		s3Cli = meta.mockHeader
	}
	out, err := withCloudStorageTimeout(util.cfg, func(ctx context.Context) (*s3.HeadObjectOutput, error) {
		return s3Cli.HeadObject(ctx, headObjInput)
	})
	if err != nil {
		var ae smithy.APIError
		if errors.As(err, &ae) {
			if ae.ErrorCode() == notFound {
				meta.resStatus = notFoundFile
				return nil, errors.New("could not find file")
			} else if ae.ErrorCode() == expiredToken {
				meta.resStatus = renewToken
				return nil, errors.New("received expired token. renewing")
			}
			meta.resStatus = errStatus
			meta.lastError = err
			return nil, fmt.Errorf("error while retrieving header, errorCode=%v. %w", ae.ErrorCode(), err)
		}
		meta.resStatus = errStatus
		meta.lastError = err
		return nil, fmt.Errorf("unexpected error while retrieving header: %w", err)
	}

	meta.resStatus = uploaded
	var encMeta encryptMetadata
	if out.Metadata[amzKey] != "" {
		encMeta = encryptMetadata{
			out.Metadata[amzKey],
			out.Metadata[amzIv],
			out.Metadata[amzMatdesc],
		}
	}
	contentLength := convertContentLength(out.ContentLength)
	return &fileHeader{
		out.Metadata[sfcDigest],
		contentLength,
		&encMeta,
	}, nil
}

// SNOW-974548 remove this function after upgrading AWS SDK
func convertContentLength(contentLength any) int64 {
	switch t := contentLength.(type) {
	case int64:
		return t
	case *int64:
		if t != nil {
			return *t
		}
	}
	return 0
}

type s3UploadAPI interface {
	Upload(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*manager.Uploader)) (*manager.UploadOutput, error)
}

// cloudUtil implementation
func (util *snowflakeS3Client) uploadFile(
	dataFile string,
	meta *fileMetadata,
	maxConcurrency int,
	multiPartThreshold int64) error {
	s3Meta := map[string]string{
		httpHeaderContentType: httpHeaderValueOctetStream,
		sfcDigest:             meta.sha256Digest,
	}
	if meta.encryptMeta != nil {
		s3Meta[amzIv] = meta.encryptMeta.iv
		s3Meta[amzKey] = meta.encryptMeta.key
		s3Meta[amzMatdesc] = meta.encryptMeta.matdesc
	}

	s3loc, err := util.extractBucketNameAndPath(meta.stageInfo.Location)
	if err != nil {
		return err
	}
	s3path := s3loc.s3Path + strings.TrimLeft(meta.dstFileName, "/")

	client, ok := meta.client.(*s3.Client)
	if !ok {
		return &SnowflakeError{
			Message: "failed to cast to s3 client",
		}
	}
	var uploader s3UploadAPI
	uploader = manager.NewUploader(client, func(u *manager.Uploader) {
		u.Concurrency = maxConcurrency
		u.PartSize = int64Max(multiPartThreshold, manager.DefaultUploadPartSize)
	})
	// for testing only
	if meta.mockUploader != nil {
		uploader = meta.mockUploader
	}

	_, err = withCloudStorageTimeout(util.cfg, func(ctx context.Context) (any, error) {
		if meta.srcStream != nil {
			uploadStream := cmp.Or(meta.realSrcStream, meta.srcStream)
			return uploader.Upload(ctx, &s3.PutObjectInput{
				Bucket:   &s3loc.bucketName,
				Key:      &s3path,
				Body:     bytes.NewBuffer(uploadStream.Bytes()),
				Metadata: s3Meta,
			})
		}
		var file *os.File
		file, err = os.Open(dataFile)
		if err != nil {
			return nil, err
		}
		defer func() {
			if err = file.Close(); err != nil {
				logger.Warnf("failed to close %v file: %v", dataFile, err)
			}
		}()
		return uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket:   &s3loc.bucketName,
			Key:      &s3path,
			Body:     file,
			Metadata: s3Meta,
		})

	})

	if err != nil {
		var ae smithy.APIError
		if errors.As(err, &ae) {
			if ae.ErrorCode() == expiredToken {
				meta.resStatus = renewToken
				return err
			} else if strings.Contains(ae.ErrorCode(), errNoWsaeconnaborted) {
				meta.lastError = err
				meta.resStatus = needRetryWithLowerConcurrency
				return err
			}
		}
		meta.lastError = err
		meta.resStatus = needRetry
		return fmt.Errorf("error while uploading file. %w", err)
	}
	meta.dstFileSize = meta.uploadSize
	meta.resStatus = uploaded
	return nil
}

type s3DownloadAPI interface {
	Download(ctx context.Context, w io.WriterAt, params *s3.GetObjectInput, optFns ...func(*manager.Downloader)) (int64, error)
}

// cloudUtil implementation
func (util *snowflakeS3Client) nativeDownloadFile(
	meta *fileMetadata,
	fullDstFileName string,
	maxConcurrency int64,
	partSize int64) error {
	s3Obj, _ := util.getS3Object(meta, meta.srcFileName)
	client, ok := meta.client.(*s3.Client)
	if !ok {
		return &SnowflakeError{
			Message: "failed to cast to s3 client",
		}
	}
	logger.Debugf("S3 Client: Send Get Request to the Bucket: %v", meta.stageInfo.Location)

	var downloader s3DownloadAPI
	downloader = manager.NewDownloader(client, func(u *manager.Downloader) {
		u.Concurrency = int(maxConcurrency)
		u.PartSize = int64Max(partSize, manager.DefaultDownloadPartSize)
	})
	// for testing only
	if meta.mockDownloader != nil {
		downloader = meta.mockDownloader
	}

	_, err := withCloudStorageTimeout(util.cfg, func(ctx context.Context) (any, error) {
		if meta.options != nil && meta.options.GetFileToStream {
			buf := manager.NewWriteAtBuffer([]byte{})
			if _, err := downloader.Download(ctx, buf, &s3.GetObjectInput{
				Bucket: s3Obj.Bucket,
				Key:    s3Obj.Key,
			}); err != nil {
				return nil, err
			}
			meta.dstStream = bytes.NewBuffer(buf.Bytes())
		} else {
			f, err := os.OpenFile(fullDstFileName, os.O_CREATE|os.O_WRONLY, readWriteFileMode)
			if err != nil {
				return nil, err
			}
			defer func() {
				if err = f.Close(); err != nil {
					logger.Warnf("failed to close %v file: %v", fullDstFileName, err)
				}
			}()
			if _, err = downloader.Download(ctx, f, &s3.GetObjectInput{
				Bucket: s3Obj.Bucket,
				Key:    s3Obj.Key,
			}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})

	if err != nil {
		var ae smithy.APIError
		if errors.As(err, &ae) {
			if ae.ErrorCode() == expiredToken {
				meta.resStatus = renewToken
				return err
			} else if strings.Contains(ae.ErrorCode(), errNoWsaeconnaborted) {
				meta.lastError = err
				meta.resStatus = needRetryWithLowerConcurrency
				return err
			}
			meta.lastError = err
			meta.resStatus = errStatus
			return fmt.Errorf("error while downloading file, errorCode=%v. %w", ae.ErrorCode(), err)
		}
		meta.lastError = err
		meta.resStatus = needRetry
		return fmt.Errorf("error while downloading file. %w", err)
	}
	meta.resStatus = downloaded
	return nil
}

func (util *snowflakeS3Client) extractBucketNameAndPath(location string) (*s3Location, error) {
	stageLocation, err := expandUser(location)
	if err != nil {
		return nil, err
	}
	bucketName := stageLocation
	s3Path := ""

	if idx := strings.Index(stageLocation, "/"); idx >= 0 {
		bucketName = stageLocation[0:idx]
		s3Path = stageLocation[idx+1:]
		if s3Path != "" && !strings.HasSuffix(s3Path, "/") {
			s3Path += "/"
		}
	}
	return &s3Location{bucketName, s3Path}, nil
}

func (util *snowflakeS3Client) getS3Object(meta *fileMetadata, filename string) (*s3.HeadObjectInput, error) {
	s3loc, err := util.extractBucketNameAndPath(meta.stageInfo.Location)
	if err != nil {
		return nil, err
	}
	s3path := s3loc.s3Path + strings.TrimLeft(filename, "/")
	return &s3.HeadObjectInput{
		Bucket: &s3loc.bucketName,
		Key:    &s3path,
	}, nil
}
