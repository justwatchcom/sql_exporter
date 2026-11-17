package gosnowflake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"time"
)

const (
	defaultConcurrency = 1
	defaultMaxRetry    = 5
)

// implemented by localUtil and remoteStorageUtil
type storageUtil interface {
	createClient(*execResponseStageInfo, bool, *Config, *snowflakeTelemetry) (cloudClient, error)
	uploadOneFileWithRetry(context.Context, *fileMetadata) error
	downloadOneFile(context.Context, *fileMetadata) error
}

// implemented by snowflakeS3Util, snowflakeAzureUtil and snowflakeGcsUtil
type cloudUtil interface {
	createClient(*execResponseStageInfo, bool, *snowflakeTelemetry) (cloudClient, error)
	getFileHeader(context.Context, *fileMetadata, string) (*fileHeader, error)
	uploadFile(context.Context, string, *fileMetadata, int, int64) error
	nativeDownloadFile(context.Context, *fileMetadata, string, int64, int64) error
	downloadToStream(context.Context, *fileMetadata) (io.ReadCloser, error)
}

type cloudClient any

type remoteStorageUtil struct {
	cfg       *Config
	telemetry *snowflakeTelemetry
}

func (rsu *remoteStorageUtil) getNativeCloudType(cli string, cfg *Config) cloudUtil {
	if cloudType(cli) == s3Client {
		logger.Info("Using S3 client for remote storage")
		return &snowflakeS3Client{
			cfg,
			rsu.telemetry,
		}
	} else if cloudType(cli) == azureClient {
		logger.Info("Using Azure client for remote storage")
		return &snowflakeAzureClient{
			cfg,
			rsu.telemetry,
		}
	} else if cloudType(cli) == gcsClient {
		logger.Info("Using GCS client for remote storage")
		return &snowflakeGcsClient{
			cfg,
			rsu.telemetry,
		}
	}
	return nil
}

// call cloud utils' native create client methods
func (rsu *remoteStorageUtil) createClient(info *execResponseStageInfo, useAccelerateEndpoint bool, cfg *Config, telemetry *snowflakeTelemetry) (cloudClient, error) {
	utilClass := rsu.getNativeCloudType(info.LocationType, cfg)
	return utilClass.createClient(info, useAccelerateEndpoint, telemetry)
}

func (rsu *remoteStorageUtil) uploadOneFile(ctx context.Context, meta *fileMetadata) error {
	utilClass := rsu.getNativeCloudType(meta.stageInfo.LocationType, meta.sfa.sc.cfg)
	maxConcurrency := int(meta.parallel)
	var lastErr error
	var timer time.Time
	var elapsedTime string
	maxRetry := defaultMaxRetry
	logger.Debugf(
		"Started Uploading. File: %v, location: %v", meta.realSrcFileName, meta.stageInfo.Location)
	for retry := range maxRetry {
		if err := canceledUpload(meta.realSrcFileName, ctx.Err()); err != nil {
			return err
		}
		timer = time.Now()
		if !meta.overwrite {
			header, err := utilClass.getFileHeader(ctx, meta, meta.dstFileName)
			if err != nil {
				if canceled := canceledUpload(meta.realSrcFileName, err); canceled != nil {
					return canceled
				}
			}
			if meta.resStatus == notFoundFile {
				err := utilClass.uploadFile(ctx, meta.realSrcFileName, meta, maxConcurrency, meta.options.MultiPartThreshold)
				if err != nil {
					logger.Warnf("Error uploading %v. err: %v", meta.realSrcFileName, err)
					if canceled := canceledUpload(meta.realSrcFileName, err); canceled != nil {
						return canceled
					}
				}
			} else if err != nil {
				return err
			}
			if header != nil && meta.resStatus == uploaded {
				meta.dstFileSize = 0
				meta.resStatus = skipped
				return nil
			}
		}
		if meta.overwrite || meta.resStatus == notFoundFile {
			err := utilClass.uploadFile(ctx, meta.realSrcFileName, meta, maxConcurrency, meta.options.MultiPartThreshold)
			if err != nil {
				logger.Warnf("Error uploading %v. err: %v", meta.realSrcFileName, err)
				if canceled := canceledUpload(meta.realSrcFileName, err); canceled != nil {
					return canceled
				}
			}
		}
		elapsedTime = time.Since(timer).String()
		switch meta.resStatus {
		case uploaded, renewToken, renewPresignedURL:
			logger.Debugf("Uploading file: %v finished in %v ms with the status: %v.", meta.realSrcFileName, elapsedTime, meta.resStatus)
			return nil
		case needRetry:
			if !meta.noSleepingTime {
				sleepingTime := min(int(math.Exp2(float64(retry))), 16)
				logger.Debugf("Need to retry for uploading file: %v. Current retry: %v, Sleeping time: %v.", meta.realSrcFileName, retry, sleepingTime)
				if err := sleepContext(ctx, time.Second*time.Duration(sleepingTime)); err != nil {
					return canceledUpload(meta.realSrcFileName, err)
				}
			} else {
				logger.Debugf("Need to retry for uploading file:  %v. Current retry: %v without the sleeping time.", meta.realSrcFileName, retry)
			}
		case needRetryWithLowerConcurrency:
			maxConcurrency = int(meta.parallel) - (retry * int(meta.parallel) / maxRetry)
			maxConcurrency = max(defaultConcurrency, maxConcurrency)
			meta.lastMaxConcurrency = maxConcurrency
			if !meta.noSleepingTime {
				sleepingTime := min(int(math.Exp2(float64(retry))), 16)
				logger.Debugf("Need to retry with lower concurrency for uploading file: %v. Current retry: %v, Sleeping time: %v.", meta.realSrcFileName, retry, sleepingTime)
				if err := sleepContext(ctx, time.Second*time.Duration(sleepingTime)); err != nil {
					return canceledUpload(meta.realSrcFileName, err)
				}
			} else {
				logger.Debugf("Need to retry with lower concurrency for uploading file: %v. Current retry: %v without Sleeping time.", meta.realSrcFileName, retry)

			}
		}
		lastErr = meta.lastError
	}
	if lastErr != nil {
		logger.Errorf(`Failed to uploading file: %v, with error: %v`, meta.realSrcFileName, lastErr)
		return lastErr
	}
	return fmt.Errorf("unkown error uploading %v", meta.realSrcFileName)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// canceledUpload wraps a canceled context so errors.Is still matches it.
// Other errors return nil and the caller keeps the existing retry behavior.
func canceledUpload(name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("uploading %v: %w", name, err)
	}
	return nil
}

func (rsu *remoteStorageUtil) uploadOneFileWithRetry(ctx context.Context, meta *fileMetadata) error {
	utilClass := rsu.getNativeCloudType(meta.stageInfo.LocationType, rsu.cfg)
	retryOuter := true
	for range 10 {
		if err := canceledUpload(meta.realSrcFileName, ctx.Err()); err != nil {
			return err
		}
		// retry
		if err := rsu.uploadOneFile(ctx, meta); err != nil {
			return err
		}
		// The file-transfer agent renews the client or presigned URL and retries.
		// Keep the status so that renewal runs instead of repeating this client.
		if meta.resStatus == renewToken || meta.resStatus == renewPresignedURL {
			return nil
		}
		retryInner := true
		if meta.resStatus == uploaded || meta.resStatus == skipped {
			for range 10 {
				status := meta.resStatus
				if _, err := utilClass.getFileHeader(ctx, meta, meta.dstFileName); err != nil {
					logger.Warnf("error while getting file %v header. %v", meta.dstFileSize, err)
				}
				// check file header status and verify upload/skip
				if meta.resStatus == notFoundFile {
					if !meta.noSleepingTime {
						if err := sleepContext(ctx, time.Second); err != nil { // wait 1 second for S3 eventual consistency
							return canceledUpload(meta.realSrcFileName, err)
						}
					}
					continue
				} else {
					retryInner = false
					meta.resStatus = status
					break
				}
			}
		}
		if !retryInner {
			retryOuter = false
			break
		} else {
			continue
		}
	}
	if retryOuter {
		// wanted to continue retrying but could not upload/find file
		meta.resStatus = errStatus
	}
	return nil
}

func (rsu *remoteStorageUtil) downloadOneFile(ctx context.Context, meta *fileMetadata) error {
	fullDstFileName := path.Join(meta.localLocation, baseName(meta.dstFileName))
	fullDstFileName, err := expandUser(fullDstFileName)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(fullDstFileName) {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		fullDstFileName = filepath.Join(cwd, fullDstFileName)
	}
	baseDir, err := getDirectory()
	if err != nil {
		return err
	}
	if _, err = os.Stat(baseDir); os.IsNotExist(err) {
		if err = os.MkdirAll(baseDir, os.ModePerm); err != nil {
			return err
		}
	}

	utilClass := rsu.getNativeCloudType(meta.stageInfo.LocationType, meta.sfa.sc.cfg)
	header, err := utilClass.getFileHeader(ctx, meta, meta.srcFileName)
	if err != nil {
		return err
	}
	if header != nil {
		meta.srcFileSize = header.contentLength
	}

	maxConcurrency := meta.parallel
	partSize := meta.options.MultiPartThreshold
	var lastErr error
	maxRetry := defaultMaxRetry

	timer := time.Now()
	for range maxRetry {
		tempDownloadFile := fullDstFileName + ".tmp"
		defer func() {
			// Clean up temp file if it still exists
			if _, statErr := os.Stat(tempDownloadFile); statErr == nil {
				logger.Debugf("Cleaning up temporary download file: %s", tempDownloadFile)
				if removeErr := os.Remove(tempDownloadFile); removeErr != nil {
					logger.Warnf("Failed to clean up temporary file %s: %v", tempDownloadFile, removeErr)
				}
			}
		}()

		if err = utilClass.nativeDownloadFile(ctx, meta, tempDownloadFile, maxConcurrency, partSize); err != nil {
			logger.Errorf("Failed to download file to temporary location %s: %v", tempDownloadFile, err)
			return err
		}
		if meta.resStatus == downloaded {
			logger.Debugf("Downloading file: %v finished in %v ms. File size: %v", meta.srcFileName, time.Since(timer).String(), meta.srcFileSize)
			if meta.encryptionMaterial != nil {
				if meta.presignedURL != nil {
					header, err = utilClass.getFileHeader(ctx, meta, meta.srcFileName)
					if err != nil {
						logger.Errorf("Failed to get file header for %s: %v", meta.srcFileName, err)
						return err
					}
				}
				timer = time.Now()
				if isFileGetStream(ctx) {
					dec, err := decryptStreamCBC(header.encryptionMetadata,
						meta.encryptionMaterial, 0, meta.dstStream)
					if err != nil {
						logger.Errorf("Stream decryption failed for %s - temp file will be cleaned up to prevent corrupted data: %v", meta.srcFileName, err)
						return err
					}
					totalFileSize, err := io.Copy(meta.sfa.streamBuffer, dec)
					_ = dec.Close()
					if err != nil {
						logger.Errorf("Stream decryption failed for %s - temp file will be cleaned up to prevent corrupted data: %v", meta.srcFileName, err)
						return err
					}
					logger.Debugf("Total file size: %d", totalFileSize)
					meta.dstFileSize = totalFileSize
				} else {
					if err = rsu.processEncryptedFileToDestination(meta, header, tempDownloadFile, fullDstFileName); err != nil {
						return err
					}
				}
				logger.Debugf("Decrypting file: %v finished in %v ms.", meta.srcFileName, time.Since(timer).String())

			} else {
				// file is not encrypted
				if !isFileGetStream(ctx) {
					// if we have a real file, and not a stream, move the file
					if err = os.Rename(tempDownloadFile, fullDstFileName); err != nil {
						return fmt.Errorf("failed to move downloaded file to destination: %w", err)
					}
				} else {
					// if we have a stream and no encyrption, just reuse the stream
					meta.sfa.streamBuffer = meta.dstStream
				}
			}
			if !isFileGetStream(ctx) {
				if fi, err := os.Stat(fullDstFileName); err == nil {
					meta.dstFileSize = fi.Size()
				} else {
					logger.Warnf("Failed to get file size for %s: %v", fullDstFileName, err)
				}
			}
			logger.Debugf("File download completed successfully for %s (size: %d bytes)", meta.srcFileName, meta.dstFileSize)
			return nil
		}
		lastErr = meta.lastError
	}
	if lastErr != nil {
		logger.Errorf(`Failed to downloading file: %v, with error: %v`, meta.srcFileName, lastErr)

		return lastErr
	}
	return fmt.Errorf("unkown error downloading %v", fullDstFileName)
}

func (rsu *remoteStorageUtil) processEncryptedFileToDestination(meta *fileMetadata, header *fileHeader, tempDownloadFile, fullDstFileName string) error {
	// Clean up the temp download file on any exit path
	defer func() {
		if _, statErr := os.Stat(tempDownloadFile); statErr == nil {
			logger.Debugf("Cleaning up temporary download file: %s", tempDownloadFile)
			err := os.Remove(tempDownloadFile)
			if err != nil {
				logger.Warnf("Failed to clean up temporary download file %s: %v", tempDownloadFile, err)
			}
		}
	}()

	tmpDstFileName, err := decryptFileCBC(header.encryptionMetadata, meta.encryptionMaterial, tempDownloadFile, 0, meta.tmpDir)
	// Ensure cleanup of the decrypted temp file if decryption or rename fails
	defer func() {
		if _, statErr := os.Stat(tmpDstFileName); statErr == nil {
			err := os.Remove(tmpDstFileName)
			if err != nil {
				logger.Warnf("Failed to clean up temporary decrypted file %s: %v", tmpDstFileName, err)
			}
		}
	}()
	if err != nil {
		logger.Errorf("File decryption failed for %s: %v", meta.srcFileName, err)
		return err
	}

	if err = os.Rename(tmpDstFileName, fullDstFileName); err != nil {
		logger.Errorf("Failed to move decrypted file from %s to final destination %s: %v", tmpDstFileName, fullDstFileName, err)
		return err
	}
	logger.Debugf("Successfully decrypted and moved file to %s", fullDstFileName)
	return nil
}
