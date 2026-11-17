package gosnowflake

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	gcsMetadataPrefix             = "x-goog-meta-"
	gcsMetadataSfcDigest          = gcsMetadataPrefix + sfcDigest
	gcsMetadataMatdescKey         = gcsMetadataPrefix + "matdesc"
	gcsMetadataEncryptionDataProp = gcsMetadataPrefix + "encryptiondata"
	gcsFileHeaderDigest           = "gcs-file-header-digest"
	gcsRegionMeCentral2           = "me-central2"
	minimumDownloadPartSize       = 1024 * 1024 * 5 // 5MB
)

type snowflakeGcsClient struct {
	cfg       *Config
	telemetry *snowflakeTelemetry
}

type gcsLocation struct {
	bucketName string
	path       string
}

func (util *snowflakeGcsClient) createClient(info *execResponseStageInfo, _ bool, telemetry *snowflakeTelemetry) (cloudClient, error) {
	if info.Creds.GcsAccessToken != "" {
		logger.Debug("Using GCS downscoped token")
		return info.Creds.GcsAccessToken, nil
	}
	logger.Debugf("No access token received from GS, using presigned url: %s", info.PresignedURL)
	return "", nil
}

// cloudUtil implementation
func (util *snowflakeGcsClient) getFileHeader(meta *fileMetadata, filename string) (*fileHeader, error) {
	if meta.resStatus == uploaded || meta.resStatus == downloaded {
		return &fileHeader{
			digest:             meta.gcsFileHeaderDigest,
			contentLength:      meta.gcsFileHeaderContentLength,
			encryptionMetadata: meta.gcsFileHeaderEncryptionMeta,
		}, nil
	}
	if meta.presignedURL != nil {
		meta.resStatus = notFoundFile
	} else {
		URL, err := util.generateFileURL(meta.stageInfo, strings.TrimLeft(filename, "/"))
		if err != nil {
			return nil, err
		}
		accessToken, ok := meta.client.(string)
		if !ok {
			return nil, fmt.Errorf("interface convertion. expected type string but got %T", meta.client)
		}
		gcsHeaders := map[string]string{
			"Authorization": "Bearer " + accessToken,
		}

		resp, err := withCloudStorageTimeout(util.cfg, func(ctx context.Context) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, "HEAD", URL.String(), nil)
			if err != nil {
				return nil, err
			}
			for k, v := range gcsHeaders {
				req.Header.Add(k, v)
			}
			client, err := newGcsClient(util.cfg, util.telemetry)
			if err != nil {
				return nil, err
			}
			// for testing only
			if meta.mockGcsClient != nil {
				client = meta.mockGcsClient
			}
			resp, err := client.Do(req)
			if err != nil && strings.HasSuffix(err.Error(), "EOF") {
				logger.Debug("Retrying HEAD request because of EOF")
				resp, err = client.Do(req)
			}
			return resp, err
		})
		if err != nil {
			return nil, err
		}
		defer func() {
			if resp.Body != nil {
				if err := resp.Body.Close(); err != nil {
					logger.Warnf("failed to close response body: %v", err)
				}
			}
		}()
		if resp.StatusCode != http.StatusOK {
			meta.lastError = fmt.Errorf("%v", resp.Status)
			meta.resStatus = errStatus
			if resp.StatusCode == 403 || resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode == 500 || resp.StatusCode == 503 {
				meta.lastError = fmt.Errorf("%v", resp.Status)
				meta.resStatus = needRetry
				return nil, meta.lastError
			}
			if resp.StatusCode == 404 {
				meta.resStatus = notFoundFile
			} else if util.isTokenExpired(resp) {
				meta.lastError = fmt.Errorf("%v", resp.Status)
				meta.resStatus = renewToken
			}
			return nil, meta.lastError
		}

		digest := resp.Header.Get(gcsMetadataSfcDigest)
		contentLength, err := strconv.Atoi(resp.Header.Get("content-length"))
		if err != nil {
			return nil, err
		}
		var encryptionMeta *encryptMetadata
		if resp.Header.Get(gcsMetadataEncryptionDataProp) != "" {
			var encryptData *encryptionData
			err := json.Unmarshal([]byte(resp.Header.Get(gcsMetadataEncryptionDataProp)), &encryptData)
			if err != nil {
				logger.Error(err)
			}
			if encryptData != nil {
				encryptionMeta = &encryptMetadata{
					key: encryptData.WrappedContentKey.EncryptionKey,
					iv:  encryptData.ContentEncryptionIV,
				}
				if resp.Header.Get(gcsMetadataMatdescKey) != "" {
					encryptionMeta.matdesc = resp.Header.Get(gcsMetadataMatdescKey)
				}
			}
		}
		meta.resStatus = uploaded
		return &fileHeader{
			digest:             digest,
			contentLength:      int64(contentLength),
			encryptionMetadata: encryptionMeta,
		}, nil
	}
	return nil, nil
}

type gcsAPI interface {
	Do(req *http.Request) (*http.Response, error)
}

// cloudUtil implementation
func (util *snowflakeGcsClient) uploadFile(
	dataFile string,
	meta *fileMetadata,
	maxConcurrency int,
	multiPartThreshold int64) error {
	uploadURL := meta.presignedURL
	var accessToken string
	var err error

	if uploadURL == nil {
		uploadURL, err = util.generateFileURL(meta.stageInfo, strings.TrimLeft(meta.dstFileName, "/"))
		if err != nil {
			return err
		}
		var ok bool
		accessToken, ok = meta.client.(string)
		if !ok {
			return fmt.Errorf("interface convertion. expected type string but got %T", meta.client)
		}
	}

	var contentEncoding string
	if meta.dstCompressionType != nil {
		contentEncoding = strings.ToLower(meta.dstCompressionType.name)
	}

	if contentEncoding == "gzip" {
		contentEncoding = ""
	}

	gcsHeaders := make(map[string]string)
	gcsHeaders[httpHeaderContentEncoding] = contentEncoding
	gcsHeaders[gcsMetadataSfcDigest] = meta.sha256Digest
	if accessToken != "" {
		gcsHeaders["Authorization"] = "Bearer " + accessToken
	}

	if meta.encryptMeta != nil {
		encryptData := encryptionData{
			"FullBlob",
			contentKey{
				"symmKey1",
				meta.encryptMeta.key,
				"AES_CBC_256",
			},
			encryptionAgent{
				"1.0",
				"AES_CBC_256",
			},
			meta.encryptMeta.iv,
			keyMetadata{
				"Java 5.3.0",
			},
		}
		b, err := json.Marshal(&encryptData)
		if err != nil {
			return err
		}
		gcsHeaders[gcsMetadataEncryptionDataProp] = string(b)
		gcsHeaders[gcsMetadataMatdescKey] = meta.encryptMeta.matdesc
	}

	var uploadSrc io.Reader
	if meta.srcStream != nil {
		uploadSrc = meta.srcStream
		if meta.realSrcStream != nil {
			uploadSrc = meta.realSrcStream
		}
	} else {
		var err error
		uploadSrc, err = os.Open(dataFile)
		if err != nil {
			return err
		}
		defer func(src io.Closer) {
			if err := src.Close(); err != nil {
				logger.Warnf("failed to close %v file: %v", dataFile, err)
			}
		}(uploadSrc.(io.Closer))
	}

	resp, err := withCloudStorageTimeout(util.cfg, func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL.String(), uploadSrc)
		if err != nil {
			return nil, err
		}
		for k, v := range gcsHeaders {
			req.Header.Add(k, v)
		}
		client, err := newGcsClient(util.cfg, util.telemetry)
		if err != nil {
			return nil, err
		}
		// for testing only
		if meta.mockGcsClient != nil {
			client = meta.mockGcsClient
		}
		return client.Do(req)
	})

	if err != nil {
		return err
	}
	defer func() {
		if resp.Body != nil {
			if err := resp.Body.Close(); err != nil {
				logger.Warnf("failed to close response body: %v", err)
			}
		}
	}()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == 403 || resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode == 500 || resp.StatusCode == 503 {
			meta.lastError = fmt.Errorf("%v", resp.Status)
			meta.resStatus = needRetry
		} else if accessToken == "" && resp.StatusCode == 400 && meta.lastError == nil {
			meta.lastError = fmt.Errorf("%v", resp.Status)
			meta.resStatus = renewPresignedURL
		} else if accessToken != "" && util.isTokenExpired(resp) {
			meta.lastError = fmt.Errorf("%v", resp.Status)
			meta.resStatus = renewToken
		} else {
			meta.lastError = fmt.Errorf("%v", resp.Status)
		}
		return meta.lastError
	}

	if meta.options.putCallback != nil {
		meta.options.putCallback = &snowflakeProgressPercentage{
			filename:        dataFile,
			fileSize:        float64(meta.srcFileSize),
			outputStream:    meta.options.putCallbackOutputStream,
			showProgressBar: meta.options.showProgressBar,
		}
	}

	meta.dstFileSize = meta.uploadSize
	meta.resStatus = uploaded

	meta.gcsFileHeaderDigest = gcsHeaders[gcsFileHeaderDigest]
	meta.gcsFileHeaderContentLength = meta.uploadSize
	if err = json.Unmarshal([]byte(gcsHeaders[gcsMetadataEncryptionDataProp]), &meta.encryptMeta); err != nil {
		return err
	}
	meta.gcsFileHeaderEncryptionMeta = meta.encryptMeta
	return nil
}

// cloudUtil implementation
func (util *snowflakeGcsClient) nativeDownloadFile(
	meta *fileMetadata,
	fullDstFileName string,
	maxConcurrency int64,
	partSize int64) error {
	partSize = int64Max(partSize, minimumDownloadPartSize)
	downloadURL := meta.presignedURL
	var accessToken string
	var err error
	gcsHeaders := make(map[string]string)

	if downloadURL == nil || downloadURL.String() == "" {
		downloadURL, err = util.generateFileURL(meta.stageInfo, strings.TrimLeft(meta.srcFileName, "/"))
		if err != nil {
			return err
		}
		var ok bool
		accessToken, ok = meta.client.(string)
		if !ok {
			return fmt.Errorf("interface convertion. expected type string but got %T", meta.client)
		}
		if accessToken != "" {
			gcsHeaders["Authorization"] = "Bearer " + accessToken
		}
	}
	logger.Debugf("GCS Client: Send Get Request to %v", downloadURL.String())

	// First, get file size with a HEAD request to determine if multi-part download is needed
	// Also extract metadata during this request
	fileHeader, err := util.getFileHeaderForDownload(downloadURL, gcsHeaders, accessToken, meta)
	if err != nil {
		return err
	}
	fileSize := fileHeader.ContentLength

	// Use multi-part download for files larger than partSize or when maxConcurrency > 1
	if fileSize > partSize && maxConcurrency > 1 {
		err = util.downloadFileInParts(downloadURL, gcsHeaders, accessToken, meta, fullDstFileName, fileSize, maxConcurrency, partSize)
	} else {
		// Fall back to single-part download for smaller files
		err = util.downloadFileSinglePart(downloadURL, gcsHeaders, accessToken, meta, fullDstFileName)
	}
	if err != nil {
		return err
	}

	var encryptMeta encryptMetadata
	if fileHeader.Header.Get(gcsMetadataEncryptionDataProp) != "" {
		var encryptData *encryptionData
		if err = json.Unmarshal([]byte(fileHeader.Header.Get(gcsMetadataEncryptionDataProp)), &encryptData); err != nil {
			return err
		}
		if encryptData != nil {
			encryptMeta = encryptMetadata{
				encryptData.WrappedContentKey.EncryptionKey,
				encryptData.ContentEncryptionIV,
				"",
			}
			if key := fileHeader.Header.Get(gcsMetadataMatdescKey); key != "" {
				encryptMeta.matdesc = key
			}
		}
	}
	meta.resStatus = downloaded
	meta.gcsFileHeaderDigest = fileHeader.Header.Get(gcsMetadataSfcDigest)
	meta.gcsFileHeaderContentLength = fileSize
	meta.gcsFileHeaderEncryptionMeta = &encryptMeta
	return nil
}

// getFileHeaderForDownload gets the file header using a HEAD request
func (util *snowflakeGcsClient) getFileHeaderForDownload(downloadURL *url.URL, gcsHeaders map[string]string, accessToken string, meta *fileMetadata) (*http.Response, error) {
	resp, err := withCloudStorageTimeout(util.cfg, func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "HEAD", downloadURL.String(), nil)
		if err != nil {
			return nil, err
		}
		for k, v := range gcsHeaders {
			req.Header.Add(k, v)
		}
		client, err := newGcsClient(util.cfg, util.telemetry)
		if err != nil {
			return nil, err
		}
		// for testing only
		if meta.mockGcsClient != nil {
			client = meta.mockGcsClient
		}
		return client.Do(req)
	})

	if err != nil {
		return nil, err
	}
	defer func() {
		if resp.Body != nil {
			if err := resp.Body.Close(); err != nil {
				logger.Warnf("Failed to close response body: %v", err)
			}
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, util.handleHTTPError(resp, meta, accessToken)
	}

	return resp, nil
}

// downloadPart is a struct for downloading a part of a file in memory
type downloadPart struct {
	data  []byte
	index int64
	err   error
}

// downloadPartStream is a struct for downloading a part of a file in a stream
type downloadPartStream struct {
	stream io.ReadCloser
	index  int64
	err    error
}

type downloadJob struct {
	index int64
	start int64
	end   int64
}

func (util *snowflakeGcsClient) downloadFileInParts(
	downloadURL *url.URL,
	gcsHeaders map[string]string,
	accessToken string,
	meta *fileMetadata,
	fullDstFileName string,
	fileSize int64,
	maxConcurrency int64,
	partSize int64) error {

	// Calculate number of parts based on desired part size
	numParts := (fileSize + partSize - 1) / partSize

	// For streaming, use batched approach to avoid buffering all parts in memory
	if meta.options != nil && meta.options.GetFileToStream {
		return util.downloadInPartsForStream(downloadURL, gcsHeaders, accessToken, meta, fileSize, numParts, maxConcurrency, partSize)
	}
	return util.downloadInPartsForFile(downloadURL, gcsHeaders, accessToken, meta, fullDstFileName, fileSize, numParts, maxConcurrency, partSize)
}

// downloadInPartsForStream downloads file in batches, streaming parts sequentially
func (util *snowflakeGcsClient) downloadInPartsForStream(
	downloadURL *url.URL,
	gcsHeaders map[string]string,
	accessToken string,
	meta *fileMetadata,
	fileSize, numParts, maxConcurrency, partSize int64) error {

	// Create a single HTTP client for all downloads to reuse connections
	client, err := newGcsClient(util.cfg, util.telemetry)
	if err != nil {
		return err
	}
	// for testing only
	if meta.mockGcsClient != nil {
		client = meta.mockGcsClient
	}

	// The first part's index for each batch
	var nextPartIndex int64 = 0

	for nextPartIndex < numParts {
		// Calculate this batch size
		batchSize := maxConcurrency
		if nextPartIndex+batchSize > numParts {
			batchSize = numParts - nextPartIndex
		}

		// Download this batch
		jobs := make(chan downloadJob, batchSize)
		results := make(chan downloadPartStream, batchSize)

		// Start workers for this batch
		for i := int64(0); i < batchSize; i++ {
			go func() {
				for job := range jobs {
					stream, err := util.downloadRangeStream(downloadURL, gcsHeaders, accessToken, meta, client, job.start, job.end)
					results <- downloadPartStream{stream: stream, index: job.index, err: err}
				}
			}()
		}

		// Send jobs for this batch
		for i := int64(0); i < batchSize; i++ {
			partIndex := nextPartIndex + i
			start := partIndex * partSize
			end := start + partSize - 1
			if end >= fileSize {
				end = fileSize - 1
			}
			jobs <- downloadJob{index: i, start: start, end: end}
		}
		close(jobs) // Signal no more jobs

		// Collect results for this batch
		batchResults := make([]downloadPartStream, batchSize)
		for i := int64(0); i < batchSize; i++ {
			result := <-results
			if result.err != nil {
				// Close any successful streams before returning error
				for j := int64(0); j < i; j++ {
					if batchResults[j].stream != nil {
						if closeErr := batchResults[j].stream.Close(); closeErr != nil {
							logger.Warnf("Failed to close stream: %v", closeErr)
						}
					}
				}
				return result.err
			}
			batchResults[result.index] = result
		}

		// Stream parts sequentially in order, closing streams as we go
		for i := int64(0); i < batchSize; i++ {
			part := batchResults[i]
			if part.stream != nil {
				// Stream directly from HTTP response to destination stream
				_, err := io.Copy(meta.dstStream, part.stream)
				// Close the stream immediately after copying
				if closeErr := part.stream.Close(); closeErr != nil {
					logger.Warnf("Failed to close stream: %v", closeErr)
				}
				if err != nil {
					// Close remaining streams before returning error
					for j := i + 1; j < batchSize; j++ {
						if batchResults[j].stream != nil {
							if closeErr := batchResults[j].stream.Close(); closeErr != nil {
								logger.Warnf("Failed to close stream: %v", closeErr)
							}
						}
					}
					return err
				}
			}
		}

		nextPartIndex += batchSize
	}

	return nil
}

// downloadInPartsForFile downloads all parts and writes to file
func (util *snowflakeGcsClient) downloadInPartsForFile(
	downloadURL *url.URL,
	gcsHeaders map[string]string,
	accessToken string,
	meta *fileMetadata,
	fullDstFileName string,
	fileSize, numParts, maxConcurrency, partSize int64) error {

	// Create a single HTTP client for all downloads to reuse connections
	client, err := newGcsClient(util.cfg, util.telemetry)
	if err != nil {
		return err
	}
	// for testing only
	if meta.mockGcsClient != nil {
		client = meta.mockGcsClient
	}

	// Start all workers and download all parts
	jobs := make(chan downloadJob, numParts)
	results := make(chan downloadPart, numParts)

	// Start worker pool with maxConcurrency workers
	for i := int64(0); i < maxConcurrency; i++ {
		go func() {
			for job := range jobs {
				data, err := util.downloadRangeBytes(downloadURL, gcsHeaders, accessToken, meta, client, job.start, job.end)
				results <- downloadPart{data: data, index: job.index, err: err}
			}
		}()
	}

	// Send all jobs to workers
	for i := int64(0); i < numParts; i++ {
		start := i * partSize
		end := start + partSize - 1
		if end >= fileSize {
			end = fileSize - 1
		}
		jobs <- downloadJob{index: i, start: start, end: end}
	}
	close(jobs) // Signal no more jobs

	// Collect results and store in order
	parts := make([][]byte, numParts)
	for i := int64(0); i < numParts; i++ {
		result := <-results
		if result.err != nil {
			return result.err
		}
		parts[result.index] = result.data
	}

	f, err := os.OpenFile(fullDstFileName, os.O_CREATE|os.O_WRONLY, readWriteFileMode)
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); err != nil {
			logger.Warnf("Failed to close file: %v", err)
		}
	}()

	for _, part := range parts {
		if _, err := f.Write(part); err != nil {
			return err
		}
	}
	fi, err := os.Stat(fullDstFileName)
	if err != nil {
		return err
	}
	meta.srcFileSize = fi.Size()

	return nil
}

// downloadRangeStream downloads a specific byte range and returns the response stream
func (util *snowflakeGcsClient) downloadRangeStream(
	downloadURL *url.URL,
	gcsHeaders map[string]string,
	accessToken string,
	meta *fileMetadata,
	client gcsAPI,
	start, end int64) (io.ReadCloser, error) {

	resp, err := withCloudStorageTimeout(util.cfg, func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", downloadURL.String(), nil)
		if err != nil {
			return nil, err
		}

		// Add range header for partial content
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

		for k, v := range gcsHeaders {
			req.Header.Add(k, v)
		}

		return client.Do(req)
	})

	if err != nil {
		return nil, err
	}

	// Accept both 200 (full content) and 206 (partial content) status codes
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		return nil, util.handleHTTPError(resp, meta, accessToken)
	}

	// Return the response body stream directly - caller is responsible for closing
	return resp.Body, nil
}

// downloadRangeBytes downloads a specific byte range and returns the bytes
func (util *snowflakeGcsClient) downloadRangeBytes(
	downloadURL *url.URL,
	gcsHeaders map[string]string,
	accessToken string,
	meta *fileMetadata,
	client gcsAPI,
	start, end int64) ([]byte, error) {

	stream, err := util.downloadRangeStream(downloadURL, gcsHeaders, accessToken, meta, client, start, end)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := stream.Close(); err != nil {
			logger.Warnf("Failed to close stream: %v", err)
		}
	}()

	// Download the data into memory
	data, err := io.ReadAll(stream)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// downloadFileSinglePart downloads a file using a single request (original implementation)
func (util *snowflakeGcsClient) downloadFileSinglePart(
	downloadURL *url.URL,
	gcsHeaders map[string]string,
	accessToken string,
	meta *fileMetadata,
	fullDstFileName string) error {

	resp, err := withCloudStorageTimeout(util.cfg, func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", downloadURL.String(), nil)
		if err != nil {
			return nil, err
		}
		for k, v := range gcsHeaders {
			req.Header.Add(k, v)
		}
		client, err := newGcsClient(util.cfg, util.telemetry)
		if err != nil {
			return nil, err
		}
		// for testing only
		if meta.mockGcsClient != nil {
			client = meta.mockGcsClient
		}
		return client.Do(req)
	})

	if err != nil {
		return err
	}
	defer func() {
		if resp.Body != nil {
			if err := resp.Body.Close(); err != nil {
				logger.Warnf("Failed to close response body: %v", err)
			}
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return util.handleHTTPError(resp, meta, accessToken)
	}

	if meta.options != nil && meta.options.GetFileToStream {
		if _, err := io.Copy(meta.dstStream, resp.Body); err != nil {
			return err
		}
	} else {
		f, err := os.OpenFile(fullDstFileName, os.O_CREATE|os.O_WRONLY, readWriteFileMode)
		if err != nil {
			return err
		}
		defer func() {
			if err = f.Close(); err != nil {
				logger.Warnf("Failed to close the file: %v", err)
			}
		}()
		if _, err = io.Copy(f, resp.Body); err != nil {
			return err
		}
		fi, err := os.Stat(fullDstFileName)
		if err != nil {
			return err
		}
		meta.srcFileSize = fi.Size()
	}

	return nil
}

// handleHTTPError handles HTTP error responses consistently
func (util *snowflakeGcsClient) handleHTTPError(resp *http.Response, meta *fileMetadata, accessToken string) error {
	if resp.StatusCode == 403 || resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode == 500 || resp.StatusCode == 503 {
		meta.lastError = fmt.Errorf("%v", resp.Status)
		meta.resStatus = needRetry
	} else if resp.StatusCode == 404 {
		meta.lastError = fmt.Errorf("%v", resp.Status)
		meta.resStatus = notFoundFile
	} else if accessToken == "" && resp.StatusCode == 400 && meta.lastError == nil {
		meta.lastError = fmt.Errorf("%v", resp.Status)
		meta.resStatus = renewPresignedURL
	} else if accessToken != "" && util.isTokenExpired(resp) {
		meta.lastError = fmt.Errorf("%v", resp.Status)
		meta.resStatus = renewToken
	} else {
		meta.lastError = fmt.Errorf("%v", resp.Status)
	}
	return meta.lastError
}

func (util *snowflakeGcsClient) extractBucketNameAndPath(location string) *gcsLocation {
	containerName := location
	var path string
	if strings.Contains(location, "/") {
		containerName = location[:strings.Index(location, "/")]
		path = location[strings.Index(location, "/")+1:]
		if path != "" && !strings.HasSuffix(path, "/") {
			path += "/"
		}
	}
	return &gcsLocation{containerName, path}
}

func (util *snowflakeGcsClient) generateFileURL(stageInfo *execResponseStageInfo, filename string) (result *url.URL, err error) {
	gcsLoc := util.extractBucketNameAndPath(stageInfo.Location)
	fullFilePath := gcsLoc.path + filename
	endPoint := "https://storage.googleapis.com"

	// TODO: SNOW-1789759 hardcoded region will be replaced in the future
	isRegionalURLEnabled := (strings.ToLower(stageInfo.Region) == gcsRegionMeCentral2) || stageInfo.UseRegionalURL
	if stageInfo.EndPoint != "" {
		endPoint = fmt.Sprintf("https://%s", stageInfo.EndPoint)
	} else if stageInfo.UseVirtualURL {
		endPoint = fmt.Sprintf("https://%s.storage.googleapis.com", gcsLoc.bucketName)
	} else if stageInfo.Region != "" && isRegionalURLEnabled {
		endPoint = fmt.Sprintf("https://storage.%s.rep.googleapis.com", strings.ToLower(stageInfo.Region))
	}

	if stageInfo.UseVirtualURL {
		result, err = url.Parse(endPoint + "/" + url.PathEscape(fullFilePath))
	} else {
		result, err = url.Parse(endPoint + "/" + gcsLoc.bucketName + "/" + url.PathEscape(fullFilePath))
	}
	logger.Debugf("generated file URL from location=%v, path=%v, fileName=%v, endpoint=%v, useVirtualUrl=%v, result=%v, err=%v", stageInfo.Location, gcsLoc.path, filename, stageInfo.EndPoint, stageInfo.UseVirtualURL, cmp.Or(result, &url.URL{}).String(), err)
	return result, err
}

func (util *snowflakeGcsClient) isTokenExpired(resp *http.Response) bool {
	return resp.StatusCode == 401
}

func newGcsClient(cfg *Config, telemetry *snowflakeTelemetry) (gcsAPI, error) {
	transport, err := newTransportFactory(cfg, telemetry).createTransport(cfg.transportConfigFor(transportTypeCloudProvider))
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: transport,
	}, nil
}
