package gosnowflake

import (
	"bytes"
	"cmp"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
)

var azureGitRepositoryPathPattern = regexp.MustCompile(`^git-repositoryzz[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/blobs/$`)

type snowflakeAzureClient struct {
	cfg       *Config
	telemetry *snowflakeTelemetry
}

type azureLocation struct {
	containerName string
	path          string
}

type azureAPI interface {
	UploadStream(ctx context.Context, body io.Reader, o *azblob.UploadStreamOptions) (azblob.UploadStreamResponse, error)
	UploadFile(ctx context.Context, file *os.File, o *azblob.UploadFileOptions) (azblob.UploadFileResponse, error)
	DownloadFile(ctx context.Context, file *os.File, o *blob.DownloadFileOptions) (int64, error)
	DownloadStream(ctx context.Context, o *blob.DownloadStreamOptions) (azblob.DownloadStreamResponse, error)
	GetProperties(ctx context.Context, o *blob.GetPropertiesOptions) (blob.GetPropertiesResponse, error)
}

func (util *snowflakeAzureClient) createClient(info *execResponseStageInfo, _ bool, telemetry *snowflakeTelemetry) (cloudClient, error) {
	sasToken := info.Creds.AzureSasToken
	clientPath := info.Path
	// Git metadata includes the blob prefix in both Path and Location. Location
	// supplies the blob key, so the client base must omit the duplicate prefix.
	if info.Path == info.Location && azureGitRepositoryPathPattern.MatchString(clientPath) {
		clientPath = strings.TrimSuffix(clientPath, "blobs/")
	}
	u, err := url.Parse(fmt.Sprintf("https://%s.%s/%s%s", info.StorageAccount, info.EndPoint, clientPath, sasToken))
	if err != nil {
		return nil, err
	}
	transport, err := newTransportFactory(util.cfg, telemetry).createTransport(transportConfigFor(transportTypeCloudProvider))
	if err != nil {
		return nil, err
	}
	client, err := azblob.NewClientWithNoCredential(u.String(), &azblob.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Retry: policy.RetryOptions{
				MaxRetries: 60,
				RetryDelay: 2 * time.Second,
			},
			Transport: &http.Client{
				Transport: transport,
			},
		},
	})
	if err != nil {
		return nil, err
	}
	return client, nil
}

// cloudUtil implementation
func (util *snowflakeAzureClient) getFileHeader(ctx context.Context, meta *fileMetadata, filename string) (*fileHeader, error) {
	client, ok := meta.client.(*azblob.Client)
	if !ok {
		return nil, errors.New("failed to parse client to azblob.Client")
	}

	azureLoc, err := util.extractContainerNameAndPath(meta.stageInfo.Location)
	if err != nil {
		return nil, err
	}
	path := azureLoc.path + strings.TrimLeft(filename, "/")
	containerClient, err := createContainerClient(client.URL(), util.cfg, util.telemetry)
	if err != nil {
		return nil, &SnowflakeError{
			Message: "failed to create container client",
		}
	}
	var blobClient azureAPI
	blobClient = containerClient.NewBlockBlobClient(path)
	// for testing only
	if meta.mockAzureClient != nil {
		blobClient = meta.mockAzureClient
	}
	resp, err := withCloudStorageTimeout(ctx, util.cfg, func(ctx context.Context) (blob.GetPropertiesResponse, error) {
		return blobClient.GetProperties(ctx, &blob.GetPropertiesOptions{
			AccessConditions: &blob.AccessConditions{},
			CPKInfo:          &blob.CPKInfo{},
		})
	})
	if err != nil {
		var se *azcore.ResponseError
		if errors.As(err, &se) {
			if se.ErrorCode == string(bloberror.BlobNotFound) {
				meta.resStatus = notFoundFile
				return nil, errors.New("could not find file")
			} else if se.StatusCode == 403 {
				meta.resStatus = renewToken
				return nil, errors.New("received 403, attempting to renew")
			}
		}
		meta.resStatus = errStatus
		meta.lastError = err
		return nil, fmt.Errorf("unexpected error while retrieving file header from azure. %w", err)
	}

	meta.resStatus = uploaded
	metadata := withLowerKeys(resp.Metadata)
	var encData encryptionData

	_, ok = metadata["encryptiondata"]
	if ok {
		if err = json.Unmarshal([]byte(*metadata["encryptiondata"]), &encData); err != nil {
			return nil, err
		}
	}

	matdesc, ok := metadata["matdesc"]
	if !ok {
		// matdesc is not in response, use empty string
		matdesc = new(string)
	}
	encryptionMetadata := encryptMetadata{
		encData.WrappedContentKey.EncryptionKey,
		encData.ContentEncryptionIV,
		*matdesc,
	}

	digest, ok := metadata["sfcdigest"]
	if !ok {
		// sfcdigest is not in response, use empty string
		digest = new(string)
	}
	return &fileHeader{
		*digest,
		int64(len(metadata)),
		&encryptionMetadata,
	}, nil
}

// cloudUtil implementation
func (util *snowflakeAzureClient) uploadFile(
	ctx context.Context,
	dataFile string,
	meta *fileMetadata,
	maxConcurrency int,
	multiPartThreshold int64) error {
	azureMeta := map[string]*string{
		"sfcdigest": &meta.sha256Digest,
	}
	if meta.encryptMeta != nil {
		ed := &encryptionData{
			EncryptionMode: "FullBlob",
			WrappedContentKey: contentKey{
				"symmKey1",
				meta.encryptMeta.key,
				"AES_CBC_256",
			},
			EncryptionAgent: encryptionAgent{
				"1.0",
				"AES_CBC_128",
			},
			ContentEncryptionIV: meta.encryptMeta.iv,
			KeyWrappingMetadata: keyMetadata{
				"Java 5.3.0",
			},
		}
		metadata, err := json.Marshal(ed)
		if err != nil {
			return err
		}
		encryptionMetadata := string(metadata)
		azureMeta["encryptiondata"] = &encryptionMetadata
		azureMeta["matdesc"] = &meta.encryptMeta.matdesc
	}

	azureLoc, err := util.extractContainerNameAndPath(meta.stageInfo.Location)
	if err != nil {
		return err
	}
	path := azureLoc.path + strings.TrimLeft(meta.dstFileName, "/")
	client, ok := meta.client.(*azblob.Client)
	if !ok {
		return &SnowflakeError{
			Message: "failed to cast to azure client",
		}
	}
	containerClient, err := createContainerClient(client.URL(), util.cfg, util.telemetry)

	if err != nil {
		return &SnowflakeError{
			Message: "failed to create container client",
		}
	}
	var blobClient azureAPI
	blobClient = containerClient.NewBlockBlobClient(path)
	// for testing only
	if meta.mockAzureClient != nil {
		blobClient = meta.mockAzureClient
	}
	if meta.srcStream != nil {
		uploadSrc := cmp.Or(meta.realSrcStream, meta.srcStream)
		data := uploadSrc.Bytes()
		contentMD5 := md5.Sum(data)
		_, err = withCloudStorageTimeout(ctx, util.cfg, func(ctx context.Context) (azblob.UploadStreamResponse, error) {
			return blobClient.UploadStream(ctx, bytes.NewReader(data), &azblob.UploadStreamOptions{
				BlockSize: int64(len(data)),
				Metadata:  azureMeta,
				HTTPHeaders: &blob.HTTPHeaders{
					BlobContentMD5: contentMD5[:],
				},
			})
		})
	} else {
		var f *os.File
		f, err = os.Open(dataFile)
		if err != nil {
			return fmt.Errorf("failed to open file: %w", err)
		}
		defer func() {
			if err = f.Close(); err != nil {
				logger.Warnf("Failed to close the %v file: %v", dataFile, err)
			}
		}()

		var contentMD5 []byte
		contentMD5, err = computeMD5ForFile(f)
		if err != nil {
			return fmt.Errorf("failed to compute MD5: %w", err)
		}

		contentType := "application/octet-stream"
		contentEncoding := "utf-8"
		blobOptions := &azblob.UploadFileOptions{
			HTTPHeaders: &blob.HTTPHeaders{
				BlobContentType:     &contentType,
				BlobContentEncoding: &contentEncoding,
				BlobContentMD5:      contentMD5,
			},
			Metadata:    azureMeta,
			Concurrency: uint16(maxConcurrency),
		}
		if meta.options.putAzureCallback != nil {
			blobOptions.Progress = meta.options.putAzureCallback.call
		}
		_, err = withCloudStorageTimeout(ctx, util.cfg, func(ctx context.Context) (azblob.UploadFileResponse, error) {
			return blobClient.UploadFile(ctx, f, blobOptions)
		})
	}
	if err != nil {
		var se *azcore.ResponseError
		if errors.As(err, &se) {
			if se.StatusCode == 403 && util.detectAzureTokenExpireError(se.RawResponse) {
				meta.resStatus = renewToken
			} else {
				meta.resStatus = needRetry
				meta.lastError = err
			}
			return err
		}
		meta.resStatus = errStatus
		return err
	}

	meta.dstFileSize = meta.uploadSize
	meta.resStatus = uploaded
	return nil
}

// cloudUtil implementation
func (util *snowflakeAzureClient) nativeDownloadFile(
	ctx context.Context,
	meta *fileMetadata,
	fullDstFileName string,
	maxConcurrency int64,
	partSize int64) error {
	azureLoc, err := util.extractContainerNameAndPath(meta.stageInfo.Location)
	if err != nil {
		return err
	}
	path := azureLoc.path + strings.TrimLeft(meta.srcFileName, "/")
	logger.Debugf("AZURE CLIENT: Send Get Request to the bucket: %v, file: %v", meta.stageInfo.Location, meta.srcFileName)
	client, ok := meta.client.(*azblob.Client)
	if !ok {
		return &SnowflakeError{
			Message: "failed to cast to azure client",
		}
	}
	containerClient, err := createContainerClient(client.URL(), util.cfg, util.telemetry)
	if err != nil {
		return &SnowflakeError{
			Message: "failed to create container client",
		}
	}
	var blobClient azureAPI
	blobClient = containerClient.NewBlockBlobClient(path)
	// for testing only
	if meta.mockAzureClient != nil {
		blobClient = meta.mockAzureClient
	}
	if isFileGetStream(ctx) {
		blobDownloadResponse, err := withCloudStorageTimeout(ctx, util.cfg, func(ctx context.Context) (azblob.DownloadStreamResponse, error) {
			return blobClient.DownloadStream(ctx, &azblob.DownloadStreamOptions{})
		})
		if err != nil {
			return err
		}
		retryReader := blobDownloadResponse.NewRetryReader(context.Background(), &azblob.RetryReaderOptions{})
		defer func() {
			if err = retryReader.Close(); err != nil {
				logger.Warnf("failed to close the Azure reader: %v", err)
			}
		}()
		_, err = meta.dstStream.ReadFrom(retryReader)
		if err != nil {
			return err
		}
	} else {
		f, err := os.OpenFile(fullDstFileName, os.O_CREATE|os.O_WRONLY, readWriteFileMode)
		if err != nil {
			return fmt.Errorf("failed to open file: %w", err)
		}
		defer func() {
			if err = f.Close(); err != nil {
				logger.Warnf("failed to close the %v file: %v", fullDstFileName, err)
			}
		}()
		_, err = withCloudStorageTimeout(ctx, util.cfg, func(ctx context.Context) (any, error) {
			return blobClient.DownloadFile(
				ctx, f, &azblob.DownloadFileOptions{
					Concurrency: uint16(maxConcurrency),
					BlockSize:   max(partSize, blob.DefaultDownloadBlockSize),
				})
		})
		if err != nil {
			return err
		}
	}
	meta.resStatus = downloaded
	return nil
}

func (util *snowflakeAzureClient) downloadToStream(ctx context.Context, meta *fileMetadata) (io.ReadCloser, error) {
	azureLoc, err := util.extractContainerNameAndPath(meta.stageInfo.Location)
	if err != nil {
		return nil, err
	}
	path := azureLoc.path + strings.TrimLeft(meta.srcFileName, "/")
	client, ok := meta.client.(*azblob.Client)
	if !ok {
		return nil, &SnowflakeError{Message: "failed to cast to azure client"}
	}
	containerClient, err := createContainerClient(client.URL(), util.cfg, util.telemetry)
	if err != nil {
		return nil, &SnowflakeError{Message: "failed to create container client"}
	}
	var blobClient azureAPI = containerClient.NewBlockBlobClient(path)
	if meta.mockAzureClient != nil {
		blobClient = meta.mockAzureClient
	}
	// Do not wrap DownloadStream in withCloudStorageTimeout: that helper cancels
	// the request context when the call returns, which aborts the response body.
	resp, err := blobClient.DownloadStream(ctx, &azblob.DownloadStreamOptions{})
	if err != nil {
		var se *azcore.ResponseError
		if errors.As(err, &se) && se.StatusCode == 403 {
			meta.resStatus = renewToken
		}
		return nil, err
	}
	header, err := azureFileHeaderFromMetadata(resp.Metadata, resp.ContentLength)
	if err != nil {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	retryReader := resp.NewRetryReader(ctx, &azblob.RetryReaderOptions{})
	return maybeDecryptStream(retryReader, header, meta.encryptionMaterial)
}

func azureFileHeaderFromMetadata(metadata map[string]*string, contentLength *int64) (*fileHeader, error) {
	h := &fileHeader{}
	if contentLength != nil {
		h.contentLength = *contentLength
	}
	if metadata == nil {
		return h, nil
	}
	metadata = withLowerKeys(metadata)
	if digest, ok := metadata["sfcdigest"]; ok && digest != nil {
		h.digest = *digest
	}
	encJSON, ok := metadata["encryptiondata"]
	if !ok || encJSON == nil || *encJSON == "" {
		return h, nil
	}
	var encData encryptionData
	if err := json.Unmarshal([]byte(*encJSON), &encData); err != nil {
		return nil, err
	}
	matdesc := ""
	if m, ok := metadata["matdesc"]; ok && m != nil {
		matdesc = *m
	}
	h.encryptionMetadata = &encryptMetadata{
		encData.WrappedContentKey.EncryptionKey,
		encData.ContentEncryptionIV,
		matdesc,
	}
	return h, nil
}

func (util *snowflakeAzureClient) extractContainerNameAndPath(location string) (*azureLocation, error) {
	stageLocation, err := expandUser(location)
	if err != nil {
		return nil, err
	}
	containerName := stageLocation
	path := ""

	if strings.Contains(stageLocation, "/") {
		containerName = stageLocation[:strings.Index(stageLocation, "/")]
		path = stageLocation[strings.Index(stageLocation, "/")+1:]
		if path != "" && !strings.HasSuffix(path, "/") {
			path += "/"
		}
	}
	return &azureLocation{containerName, path}, nil
}

func (util *snowflakeAzureClient) detectAzureTokenExpireError(resp *http.Response) bool {
	if resp.StatusCode != 403 {
		return false
	}
	azureErr, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	errStr := string(azureErr)
	return strings.Contains(errStr, "Signature not valid in the specified time frame") ||
		strings.Contains(errStr, "Server failed to authenticate the request")
}

// computeMD5ForFile reads a file to compute its MD5 digest, then seeks back to
// the start so the file can be read again for upload. Azure does not compute
// Content-MD5 for multi-part (block blob) uploads, so we must provide it.
func computeMD5ForFile(f *os.File) ([]byte, error) {
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func createContainerClient(clientURL string, cfg *Config, telemetry *snowflakeTelemetry) (*container.Client, error) {
	transport, err := newTransportFactory(cfg, telemetry).createTransport(transportConfigFor(transportTypeCloudProvider))
	if err != nil {
		return nil, err
	}
	return container.NewClientWithNoCredential(clientURL, &container.ClientOptions{ClientOptions: azcore.ClientOptions{
		Transport: &http.Client{
			Transport: transport,
		},
	}})
}
