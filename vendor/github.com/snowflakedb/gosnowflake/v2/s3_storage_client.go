package gosnowflake

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
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

	transport, err := newTransportFactory(util.cfg, telemetry).createTransport(transportConfigFor(transportTypeCloudProvider))
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
		tmp := "https://" + info.EndPoint
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

func s3LoggingFunc(classification logging.Classification, format string, v ...any) {
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
func (util *snowflakeS3Client) getFileHeader(ctx context.Context, meta *fileMetadata, filename string) (*fileHeader, error) {
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
	out, err := withCloudStorageTimeout(ctx, util.cfg, func(ctx context.Context) (*s3.HeadObjectOutput, error) {
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
	UploadObject(ctx context.Context, params *transfermanager.UploadObjectInput, optFns ...func(*transfermanager.Options)) (*transfermanager.UploadObjectOutput, error)
}

// cloudUtil implementation
func (util *snowflakeS3Client) uploadFile(
	ctx context.Context,
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
	const minS3PartSize = 5 * 1024 * 1024 // AWS S3 minimum part size
	var uploader s3UploadAPI
	partSize := max(multiPartThreshold, minS3PartSize)
	uploader = transfermanager.New(client, func(o *transfermanager.Options) {
		o.Concurrency = maxConcurrency
		o.PartSizeBytes = partSize
		o.MultipartUploadThreshold = partSize // Files >= this size use multipart
	})
	// for testing only
	if meta.mockUploader != nil {
		uploader = meta.mockUploader
	}

	_, err = withCloudStorageTimeout(ctx, util.cfg, func(ctx context.Context) (any, error) {
		if meta.srcStream != nil {
			uploadStream := cmp.Or(meta.realSrcStream, meta.srcStream)
			return uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
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
		return uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
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
	DownloadObject(ctx context.Context, params *transfermanager.DownloadObjectInput, optFns ...func(*transfermanager.Options)) (*transfermanager.DownloadObjectOutput, error)
}

// writeAtBuffer implements io.WriterAt; WriteAt is called concurrently, so buf is guarded by mu.
type writeAtBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func newWriteAtBuffer(buf []byte) *writeAtBuffer {
	return &writeAtBuffer{buf: buf}
}

func (b *writeAtBuffer) WriteAt(p []byte, pos int64) (n int, err error) {
	pLen := len(p)
	expLen := pos + int64(pLen)
	b.mu.Lock()
	defer b.mu.Unlock()
	if int64(len(b.buf)) < expLen {
		if int64(cap(b.buf)) < expLen {
			newBuf := make([]byte, expLen, max(expLen, 2*int64(cap(b.buf))))
			copy(newBuf, b.buf)
			b.buf = newBuf
		} else {
			clear(b.buf[len(b.buf):expLen])
			b.buf = b.buf[:expLen]
		}
	}
	copy(b.buf[pos:], p)
	return pLen, nil
}

func (b *writeAtBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf
}

// cloudUtil implementation
func (util *snowflakeS3Client) nativeDownloadFile(
	ctx context.Context,
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

	const minS3PartSize = 5 * 1024 * 1024 // AWS S3 minimum part size
	var downloader s3DownloadAPI
	downloadPartSize := max(partSize, minS3PartSize)
	downloader = transfermanager.New(client, func(o *transfermanager.Options) {
		o.Concurrency = int(maxConcurrency)
		o.PartSizeBytes = downloadPartSize
		o.MultipartUploadThreshold = downloadPartSize // Downloads >= this size use multipart
	})
	// for testing only
	if meta.mockDownloader != nil {
		downloader = meta.mockDownloader
	}

	_, err := withCloudStorageTimeout(ctx, util.cfg, func(ctx context.Context) (any, error) {
		if isFileGetStream(ctx) {
			initial := []byte{}
			if meta.srcFileSize > 0 {
				initial = make([]byte, 0, meta.srcFileSize)
			}
			buf := newWriteAtBuffer(initial)
			if _, err := downloader.DownloadObject(ctx, &transfermanager.DownloadObjectInput{
				Bucket:   s3Obj.Bucket,
				Key:      s3Obj.Key,
				WriterAt: buf,
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
			if _, err = downloader.DownloadObject(ctx, &transfermanager.DownloadObjectInput{
				Bucket:   s3Obj.Bucket,
				Key:      s3Obj.Key,
				WriterAt: f,
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

func (util *snowflakeS3Client) downloadToStream(ctx context.Context, meta *fileMetadata) (io.ReadCloser, error) {
	s3Obj, err := util.getS3Object(meta, meta.srcFileName)
	if err != nil {
		return nil, err
	}
	client, ok := meta.client.(*s3.Client)
	if !ok {
		return nil, &SnowflakeError{Message: "failed to cast to s3 client"}
	}
	// Do not wrap GetObject in withCloudStorageTimeout: that helper cancels the
	// request context when the API call returns, which aborts the response body.
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: s3Obj.Bucket,
		Key:    s3Obj.Key,
	})
	if err != nil {
		var ae smithy.APIError
		if errors.As(err, &ae) && ae.ErrorCode() == expiredToken {
			meta.resStatus = renewToken
		}
		return nil, err
	}
	header := &fileHeader{
		digest:        out.Metadata[sfcDigest],
		contentLength: convertContentLength(out.ContentLength),
	}
	if out.Metadata[amzKey] != "" {
		header.encryptionMetadata = &encryptMetadata{
			out.Metadata[amzKey],
			out.Metadata[amzIv],
			out.Metadata[amzMatdesc],
		}
	}
	return maybeDecryptStream(out.Body, header, meta.encryptionMaterial)
}

func (util *snowflakeS3Client) extractBucketNameAndPath(location string) (*s3Location, error) {
	stageLocation, err := expandUser(location)
	if err != nil {
		return nil, err
	}
	bucketName := stageLocation
	s3Path := ""

	if before, after, ok := strings.Cut(stageLocation, "/"); ok {
		bucketName = before
		s3Path = after
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
