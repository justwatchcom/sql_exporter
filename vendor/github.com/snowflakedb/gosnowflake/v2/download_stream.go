package gosnowflake

import (
	"compress/gzip"
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"

	sferrors "github.com/snowflakedb/gosnowflake/v2/internal/errors"
)

const downloadStreamDummyGetPath = "file:///tmp/gosnowflake-download-stream"

// DownloadStreamConfig holds optional settings for DownloadStreamWithConfig.
type DownloadStreamConfig struct {
	// Decompress, when true, wraps the returned reader in gzip decompression.
	// Default is false (object bytes as stored on the stage).
	// Decompression errors are returned by Read, including a gzip header error
	// discovered while constructing the returned reader. Truncated gzip may fail
	// on a later Read.
	Decompress bool
}

// DownloadStream downloads one file from a stage as a readable stream.
func (sc *snowflakeConn) DownloadStream(ctx context.Context, stageName, sourceFileName string) (io.ReadCloser, error) {
	return sc.DownloadStreamWithConfig(ctx, stageName, sourceFileName, DownloadStreamConfig{})
}

// DownloadStreamWithConfig is DownloadStream with optional configuration.
func (sc *snowflakeConn) DownloadStreamWithConfig(
	ctx context.Context,
	stageName, sourceFileName string,
	config DownloadStreamConfig,
) (io.ReadCloser, error) {
	if strings.TrimSpace(stageName) == "" || strings.TrimSpace(sourceFileName) == "" {
		return nil, exceptionTelemetry(&SnowflakeError{
			Number:  ErrDownloadStreamInvalidArg,
			Message: sferrors.ErrMsgDownloadStreamInvalidArg,
		}, sc)
	}

	sql := buildDownloadStreamGetSQL(stageName, sourceFileName)
	execCtx := withSkipFileTransfer(ctx)
	data, err := sc.exec(execCtx, sql, false, false, false, []driver.NamedValue{})
	if err != nil {
		return nil, err
	}

	sfa := &snowflakeFileTransferAgent{
		ctx:                    execCtx,
		sc:                     sc,
		data:                   &data.Data,
		command:                sql,
		options:                &SnowflakeFileTransferOptions{},
		streamDownload:         true,
		streamDownloadFileName: sourceFileName,
	}
	if sfa.options.MultiPartThreshold == 0 {
		sfa.options.MultiPartThreshold = streamingMultiPartThreshold
	}

	bodyCtx, cancel := context.WithCancel(ctx)
	rc, err := sfa.openDownloadStream(bodyCtx, sourceFileName)
	if err != nil {
		cancel()
		return nil, err
	}
	owned := &ownedReadCloser{Reader: rc, closer: rc, cancel: cancel}
	if !config.Decompress {
		return owned, nil
	}
	return wrapGzipDownloadStream(sc, owned), nil
}

type readErrorTracker struct {
	io.Reader
	last error
}

func (r *readErrorTracker) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil {
		r.last = err
	}
	return n, err
}

func wrapGzipDownloadStream(sc *snowflakeConn, owned *ownedReadCloser) io.ReadCloser {
	src := &readErrorTracker{Reader: owned}
	g := &gzipReadCloser{underlying: owned, sc: sc, src: src}
	gr, err := gzip.NewReader(src)
	if err != nil {
		g.initErr = mapGzipDecodeError(sc, src.last, err)
		return g
	}
	g.gr = gr
	return g
}

func mapGzipDecodeError(sc *snowflakeConn, srcErr, err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	if srcErr != nil && (errors.Is(err, srcErr) || errors.Is(srcErr, err)) {
		return err
	}
	return downloadStreamDecompressError(sc, err)
}

func downloadStreamDecompressError(sc *snowflakeConn, err error) *SnowflakeError {
	return exceptionTelemetry(&SnowflakeError{
		Number:      ErrDownloadStreamDecompress,
		Message:     sferrors.ErrMsgDownloadStreamDecompress,
		MessageArgs: []any{err},
	}, sc)
}

// ownedReadCloser owns the cloud body context. Close is idempotent: cancel the
// body context, then close the underlying reader.
type ownedReadCloser struct {
	io.Reader
	closer io.Closer
	cancel context.CancelFunc
	once   sync.Once
	err    error
}

func (o *ownedReadCloser) Close() error {
	o.once.Do(func() {
		if o.cancel != nil {
			o.cancel()
		}
		if o.closer != nil {
			o.err = o.closer.Close()
		}
	})
	return o.err
}

// gzipReadCloser holds either an initialized gzip.Reader or a latched
// initialization error. The first Read returns the latched error when
// gzip.NewReader failed. Close is idempotent and always closes the owned body.
type gzipReadCloser struct {
	gr         *gzip.Reader
	underlying io.Closer
	src        *readErrorTracker
	sc         *snowflakeConn
	initErr    error
	once       sync.Once
	closeErr   error
}

func (g *gzipReadCloser) Read(p []byte) (int, error) {
	if g.initErr != nil {
		return 0, g.initErr
	}
	n, err := g.gr.Read(p)
	if err != nil {
		return n, mapGzipDecodeError(g.sc, g.src.last, err)
	}
	return n, nil
}

func (g *gzipReadCloser) Close() error {
	g.once.Do(func() {
		var err error
		if g.gr != nil {
			err = g.gr.Close()
		}
		if g.underlying != nil {
			if cerr := g.underlying.Close(); err == nil {
				err = cerr
			}
		}
		g.closeErr = err
	})
	return g.closeErr
}

func buildDownloadStreamGetSQL(stageName, sourceFileName string) string {
	ref := quoteStageRefIfNeeded(buildStageFileRef(stageName, sourceFileName))
	return "GET " + ref + " " + downloadStreamDummyGetPath
}

// embeddedStageURLPrefix marks embedded (versioned) stages, such as Workspaces,
// which are addressed by URL rather than by @-name, so they must not get an "@" prefix.
const embeddedStageURLPrefix = "snow://"

func isEmbeddedStageRef(stageRef string) bool {
	return len(stageRef) >= len(embeddedStageURLPrefix) &&
		strings.EqualFold(stageRef[:len(embeddedStageURLPrefix)], embeddedStageURLPrefix)
}

func normalizeStageNameForRef(stageName string) string {
	normalized := stageName
	if isDollarQuotedStageRef(normalized) {
		normalized = normalized[2 : len(normalized)-2]
	} else if isSingleQuotedStageRef(normalized) {
		normalized = strings.ReplaceAll(normalized[1:len(normalized)-1], "''", "'")
	}
	if isEmbeddedStageRef(normalized) {
		return normalized
	}
	if !strings.HasPrefix(normalized, "@") {
		normalized = "@" + normalized
	}
	return normalized
}

func buildStageFileRef(stageName, fileName string) string {
	fileName = strings.TrimPrefix(fileName, "/")
	return normalizeStageNameForRef(stageName) + "/" + fileName
}

func isSingleQuotedStageRef(stageRef string) bool {
	if len(stageRef) < 2 || !strings.HasPrefix(stageRef, "'") || !strings.HasSuffix(stageRef, "'") {
		return false
	}
	inner := stageRef[1 : len(stageRef)-1]
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\'' {
			if i+1 < len(inner) && inner[i+1] == '\'' {
				i++
				continue
			}
			return false
		}
	}
	return true
}

func isDollarQuotedStageRef(stageRef string) bool {
	return len(stageRef) >= 4 && strings.HasPrefix(stageRef, "$$") && strings.HasSuffix(stageRef, "$$")
}

func needsStageQuoting(stageRef string) bool {
	if stageRef == "" {
		return false
	}
	if isSingleQuotedStageRef(stageRef) || isDollarQuotedStageRef(stageRef) {
		return false
	}
	// Embedded stage URLs carry quoted identifiers and "//"; always pass them as a
	// string literal, as existing GET callers of these stages do.
	if isEmbeddedStageRef(stageRef) {
		return true
	}
	for _, r := range stageRef {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		if strings.ContainsRune(`_$./@~%"-:`, r) {
			continue
		}
		return true
	}
	return false
}

func quoteStageRefIfNeeded(stageRef string) string {
	if !needsStageQuoting(stageRef) {
		return stageRef
	}
	return "'" + strings.ReplaceAll(stageRef, "'", "''") + "'"
}

func matchDownloadStreamFile(sourceFiles []string, fileName string) (string, error) {
	switch len(sourceFiles) {
	case 0:
		return "", &SnowflakeError{
			Number:      ErrFileNotExists,
			SQLState:    SQLStateNoData,
			Message:     sferrors.ErrMsgDownloadStreamFileNotFound,
			MessageArgs: []any{fileName},
		}
	case 1:
		return sourceFiles[0], nil
	default:
		return "", &SnowflakeError{
			Number:  ErrGetStreamMultipleFiles,
			Message: sferrors.ErrMsgGetStreamMultipleFiles,
		}
	}
}

func (sfa *snowflakeFileTransferAgent) openDownloadStream(ctx context.Context, fileName string) (io.ReadCloser, error) {
	if err := sfa.parseCommand(); err != nil {
		return nil, err
	}
	if sfa.stageLocationType == local {
		return nil, exceptionTelemetry(&SnowflakeError{
			Number:   ErrDownloadStreamLocalFS,
			SQLState: sfa.data.SQLState,
			QueryID:  sfa.data.QueryID,
			Message:  sferrors.ErrMsgDownloadStreamLocalFS,
		}, sfa.sc)
	}
	if err := sfa.initFileMetadata(); err != nil {
		return nil, err
	}
	if err := sfa.transferAccelerateConfig(); err != nil {
		return nil, err
	}
	if err := sfa.updateFileMetadataWithPresignedURL(); err != nil {
		return nil, err
	}

	src, err := matchDownloadStreamFile(sfa.srcFiles, fileName)
	if err != nil {
		var se *SnowflakeError
		if errors.As(err, &se) {
			if se.SQLState == "" {
				se.SQLState = sfa.data.SQLState
			}
			se.QueryID = sfa.data.QueryID
			return nil, exceptionTelemetry(se, sfa.sc)
		}
		return nil, err
	}

	var meta *fileMetadata
	for _, m := range sfa.fileMetadata {
		if m.srcFileName == src {
			meta = m
			break
		}
	}
	if meta == nil {
		return nil, exceptionTelemetry(&SnowflakeError{
			Number:      ErrFileNotExists,
			SQLState:    SQLStateNoData,
			QueryID:     sfa.data.QueryID,
			Message:     sferrors.ErrMsgDownloadStreamFileNotFound,
			MessageArgs: []any{fileName},
		}, sfa.sc)
	}
	if src != fileName {
		logger.Debugf("Changing file to download location from %s to %s", fileName, src)
	}

	rsu := &remoteStorageUtil{
		cfg:       sfa.sc.cfg,
		telemetry: sfa.sc.telemetry,
	}
	utilClass := rsu.getNativeCloudType(meta.stageInfo.LocationType, sfa.sc.cfg)
	if utilClass == nil {
		return nil, exceptionTelemetry(&SnowflakeError{
			Number:      ErrInvalidStageFs,
			SQLState:    sfa.data.SQLState,
			QueryID:     sfa.data.QueryID,
			Message:     sferrors.ErrMsgInvalidStageFs,
			MessageArgs: []any{meta.stageInfo.LocationType},
		}, sfa.sc)
	}
	client, err := utilClass.createClient(sfa.stageInfo, sfa.useAccelerateEndpoint, sfa.sc.telemetry)
	if err != nil {
		return nil, err
	}
	meta.client = client
	meta.sfa = sfa
	meta.options = sfa.options

	return sfa.downloadToStreamWithRenew(ctx, utilClass, meta)
}

func (sfa *snowflakeFileTransferAgent) downloadToStreamWithRenew(ctx context.Context, utilClass cloudUtil, meta *fileMetadata) (io.ReadCloser, error) {
	rc, err := utilClass.downloadToStream(ctx, meta)
	if err == nil {
		return rc, nil
	}
	switch meta.resStatus {
	case renewToken:
		client, rerr := sfa.renewExpiredClient()
		if rerr != nil {
			return nil, rerr
		}
		meta.client = client
		meta.resStatus = errStatus
		return utilClass.downloadToStream(ctx, meta)
	case renewPresignedURL:
		if rerr := sfa.updateFileMetadataWithPresignedURL(); rerr != nil {
			return nil, rerr
		}
		meta.resStatus = errStatus
		return utilClass.downloadToStream(ctx, meta)
	default:
		return nil, err
	}
}

func maybeDecryptStream(body io.ReadCloser, header *fileHeader, enc *snowflakeFileEncryption) (io.ReadCloser, error) {
	if enc == nil {
		return body, nil
	}
	if header == nil || header.encryptionMetadata == nil || header.encryptionMetadata.key == "" {
		if closeErr := body.Close(); closeErr != nil {
			logger.Warnf("failed to close download body: %v", closeErr)
		}
		return nil, &SnowflakeError{
			Number:  ErrFailedToDecrypt,
			Message: sferrors.ErrMsgFailedToDecrypt,
		}
	}
	dec, err := decryptStreamCBC(header.encryptionMetadata, enc, 0, body)
	if err != nil {
		if closeErr := body.Close(); closeErr != nil {
			logger.Warnf("failed to close download body: %v", closeErr)
		}
		return nil, err
	}
	return &bodyClosingReader{r: dec, body: body}, nil
}

// bodyClosingReader decrypts (or otherwise reads) r and closes the cloud body on Close.
type bodyClosingReader struct {
	r    io.Reader
	body io.Closer
	once sync.Once
	err  error
}

func (b *bodyClosingReader) Read(p []byte) (int, error) {
	return b.r.Read(p)
}

func (b *bodyClosingReader) Close() error {
	b.once.Do(func() {
		if b.body != nil {
			b.err = b.body.Close()
		}
	})
	return b.err
}
