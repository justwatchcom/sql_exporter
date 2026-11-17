package gosnowflake

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/base64"
	"io"
	"net/url"
	"os"
	"strings"

	sferrors "github.com/snowflakedb/gosnowflake/v2/internal/errors"
)

const (
	uploadStreamDummyPutPath = "file:///tmp/gosnowflake-upload-stream"
	// uploadStreamDefaultSpillThreshold matches JDBC FileBackedOutputStream MAX_BUFFER_SIZE (1 << 27).
	uploadStreamDefaultSpillThreshold int64 = 128 << 20
	// MaxBufferUnbounded keeps the post-compression payload in memory.
	MaxBufferUnbounded int64 = -1
)

// Compression selects how UploadStream encodes the payload before upload.
// The zero value is gzip. The .gz suffix check is case-sensitive:
// hello.txt.GZ is stored as hello.txt.GZ.gz.
type Compression int

const (
	// CompressionGzip gzip-compresses the payload. It is the zero value.
	CompressionGzip Compression = iota
	// CompressionNone stores the bytes as given.
	CompressionNone
)

// UploadStreamConfig holds optional settings for UploadStreamWithConfig.
// The zero value gzips the payload, overwrites the destination, and keeps it
// in memory until 128 MiB, then spills to disk.
type UploadStreamConfig struct {
	// DestPrefix is a directory under the stage. Blank or whitespace means the stage root.
	// Directories belong here. destFileName is only the object name.
	DestPrefix string
	// Compression selects the payload encoding. The zero value and CompressionGzip
	// gzip the bytes. CompressionNone stores them as given. When compressing, .gz
	// is appended unless destFileName already ends with .gz. That check is case-sensitive.
	Compression Compression
	// MaxBuffer is the in-memory cap for the post-compression payload, in bytes.
	// 0 uses 128 MiB and then spills. MaxBufferUnbounded never spills.
	// A value greater than 0 spills once the payload exceeds that many bytes;
	// 1 spills on any larger write.
	MaxBuffer int64
	// SpillDir is the directory for the spill file and for the encryption copy of
	// a spilled upload. Blank uses Config.TmpDirPath, which is the process temp
	// directory when unset.
	SpillDir string
	// Overwrite replaces an existing stage object. The zero value and
	// ConfigBoolTrue overwrite. ConfigBoolFalse returns ErrUploadStreamSkipped
	// when the object already exists. That check runs before the source is read
	// when the stage credentials can HEAD the object. A GCS upload that only has
	// a presigned URL cannot HEAD, so that path still reads the source and then
	// returns the same error.
	Overwrite ConfigBool
}

// uploadStreamSink receives the post-gzip payload. finish closes any file and
// reports where the bytes landed. release drops a sink that was not finished.
type uploadStreamSink interface {
	io.Writer
	finish() (*uploadStreamSpool, error)
	release()
}

// UploadStream uploads one object to a stage from src. See UploadStreamWithConfig.
func (sc *snowflakeConn) UploadStream(ctx context.Context, stageName, destFileName string, src io.Reader) error {
	return sc.UploadStreamWithConfig(ctx, stageName, destFileName, src, UploadStreamConfig{})
}

// UploadStreamWithConfig is UploadStream with optional configuration.
// src is read to completion and is not closed. A typed nil such as (*os.File)(nil)
// passed as an io.Reader is a caller error and is not detected.
// Canceling ctx fails the next Read of src. A Read already blocked inside src
// is not interrupted. The same ctx aborts an in-flight cloud PUT and stops the
// storage retry loop, including its backoff sleep.
func (sc *snowflakeConn) UploadStreamWithConfig(
	ctx context.Context,
	stageName, destFileName string,
	src io.Reader,
	config UploadStreamConfig,
) error {
	if strings.TrimSpace(stageName) == "" || strings.TrimSpace(destFileName) == "" {
		return uploadStreamError(sc, ErrUploadStreamInvalidArg, sferrors.ErrMsgUploadStreamInvalidArg)
	}
	if src == nil {
		return uploadStreamError(sc, ErrUploadStreamInvalidArg, sferrors.ErrMsgUploadStreamNilReader)
	}
	if config.MaxBuffer < MaxBufferUnbounded {
		return uploadStreamError(sc, ErrUploadStreamInvalidArg, sferrors.ErrMsgUploadStreamBadThreshold)
	}
	if config.Compression != CompressionGzip && config.Compression != CompressionNone {
		return uploadStreamError(sc, ErrUploadStreamInvalidArg, sferrors.ErrMsgUploadStreamBadCompression)
	}

	stageName = strings.TrimSpace(stageName)
	destFileName = strings.TrimSpace(destFileName)
	if strings.Contains(destFileName, "/") {
		return uploadStreamError(sc, ErrUploadStreamInvalidArg, sferrors.ErrMsgUploadStreamInvalidArg)
	}
	sql := buildUploadStreamPutSQL(stageName, config.DestPrefix, config.overwrite())
	data, err := sc.exec(withSkipFileTransfer(ctx), sql, false, false, false, []driver.NamedValue{})
	if err != nil {
		return err
	}
	return sc.uploadStreamFromResponse(ctx, data, sql, destFileName, src, config)
}

func (sc *snowflakeConn) uploadStreamFromResponse(
	ctx context.Context,
	data *execResponse,
	sql, destFileName string,
	src io.Reader,
	config UploadStreamConfig,
) error {
	sfa := &snowflakeFileTransferAgent{
		// Keep skip-file-transfer on the context renewExpiredClient reuses.
		// A plain ctx would exec the dummy PUT and, if that path exists, upload it.
		ctx:          withSkipFileTransfer(ctx),
		sc:           sc,
		data:         &data.Data,
		command:      sql,
		options:      &SnowflakeFileTransferOptions{},
		streamUpload: true,
	}
	if err := sfa.parseCommand(); err != nil {
		return err
	}
	if err := sfa.rejectUploadStreamLocalStage(); err != nil {
		return err
	}
	if !sfa.overwrite {
		exists, err := sfa.uploadStreamDestExists(destFileName, config.compress())
		if err != nil {
			return err
		}
		if exists {
			return uploadStreamError(sc, ErrUploadStreamSkipped, sferrors.ErrMsgUploadStreamSkipped)
		}
	}

	spool, err := spoolUploadStream(uploadStreamCtxReader{ctx: ctx, r: src}, config.compress(), config.MaxBuffer, uploadStreamSpillDir(config.SpillDir, sc.cfg))
	if err != nil {
		return err
	}
	defer spool.cleanup()

	if sfa.options.MultiPartThreshold == 0 {
		sfa.options.MultiPartThreshold = multiPartThreshold
	}
	return sfa.uploadStreamObject(newUploadStreamMetadata(destFileName, config.compress(), spool))
}

func (c UploadStreamConfig) compress() bool {
	return c.Compression != CompressionNone
}

func (c UploadStreamConfig) overwrite() bool {
	return c.Overwrite != ConfigBoolFalse
}

func uploadStreamSpillDir(spillDir string, cfg *Config) string {
	if spillDir != "" {
		return spillDir
	}
	return tmpDirPath(cfg)
}

func uploadStreamError(sc *snowflakeConn, number int, message string) error {
	return exceptionTelemetry(&SnowflakeError{
		Number:  number,
		Message: message,
	}, sc)
}

func buildUploadStreamPutSQL(stageName, destPrefix string, overwrite bool) string {
	stage := normalizeStageNameForRef(strings.TrimSpace(stageName))
	prefix := strings.TrimSpace(destPrefix)
	if prefix != "" {
		if !strings.HasPrefix(prefix, "/") {
			stage += "/"
		}
		stage += prefix
	}
	ow := "true"
	if !overwrite {
		ow = "false"
	}
	return "PUT " + uploadStreamDummyPutPath + " " + quoteStageRefIfNeeded(stage) + " overwrite=" + ow + " AUTO_COMPRESS=FALSE"
}

func uploadStreamDestName(destFileName string, compress bool) string {
	name := strings.TrimLeft(destFileName, "/")
	if compress && !strings.HasSuffix(name, compressionTypes["GZIP"].fileExtension) {
		name += compressionTypes["GZIP"].fileExtension
	}
	return name
}

// uploadStreamPresignedCommand replaces the dummy PUT path with a quoted local
// path whose name is dstFileName, and turns server-side compression off.
// Compression was already applied while spooling.
func uploadStreamPresignedCommand(command, dstFileName string) string {
	localPath := "/tmp/" + strings.TrimLeft(dstFileName, "/")
	quoted := "'" + fileProtocol + strings.ReplaceAll(localPath, "'", "''") + "'"
	rewritten := strings.Replace(command, uploadStreamDummyPutPath, quoted, 1)
	if !strings.Contains(strings.ToUpper(rewritten), "AUTO_COMPRESS") {
		rewritten += " AUTO_COMPRESS=FALSE"
	}
	return rewritten
}

func presignedURLTargetsDest(u *url.URL, dstFileName string) bool {
	if u == nil {
		return false
	}
	dest := strings.TrimLeft(dstFileName, "/")
	if dest == "" {
		return false
	}
	decodedPath, err := url.PathUnescape(u.EscapedPath())
	if err != nil {
		decodedPath = u.Path
	}
	if strings.HasSuffix(decodedPath, "/"+dest) {
		return true
	}
	name := u.Query().Get("name")
	return name == dest || strings.HasSuffix(name, "/"+dest)
}

// uploadStreamCtxReader fails the next Read when ctx is canceled. It does not
// close r, and it does not interrupt a Read already blocked in r.
type uploadStreamCtxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r uploadStreamCtxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type uploadStreamSpool struct {
	buf    *bytes.Buffer
	path   string
	dir    string
	size   int64
	digest string
}

func (s *uploadStreamSpool) cleanup() {
	if s == nil || s.dir == "" {
		return
	}
	if err := os.RemoveAll(s.dir); err != nil {
		logger.Warnf("failed to remove upload stream temp dir %v: %v", s.dir, err)
	}
}

func (s *uploadStreamSpool) bytes() ([]byte, error) {
	if s.buf != nil {
		return s.buf.Bytes(), nil
	}
	return os.ReadFile(s.path)
}

func spoolUploadStream(src io.Reader, compress bool, maxBuffer int64, spillDir string) (spool *uploadStreamSpool, err error) {
	threshold := maxBuffer
	if threshold == 0 {
		threshold = uploadStreamDefaultSpillThreshold
	}
	var sink uploadStreamSink
	switch {
	case maxBuffer == MaxBufferUnbounded:
		sink = &uploadStreamMemorySink{}
	case threshold > 0:
		sink = &uploadStreamSpillSink{threshold: threshold, tmpRoot: spillDir}
	default:
		return nil, &SnowflakeError{
			Number:  ErrUploadStreamInvalidArg,
			Message: sferrors.ErrMsgUploadStreamBadThreshold,
		}
	}
	defer func() {
		if err != nil {
			sink.release()
		}
	}()

	hash := sha256.New()
	dst := io.MultiWriter(hash, sink)
	if compress {
		gz := gzip.NewWriter(dst)
		if _, err = io.Copy(gz, src); err != nil {
			return nil, err
		}
		if err = gz.Close(); err != nil {
			return nil, err
		}
	} else if _, err = io.Copy(dst, src); err != nil {
		return nil, err
	}

	spool, err = sink.finish()
	if err != nil {
		return nil, err
	}
	spool.digest = base64.StdEncoding.EncodeToString(hash.Sum(nil))
	return spool, nil
}

type uploadStreamMemorySink struct {
	buf bytes.Buffer
}

func (s *uploadStreamMemorySink) Write(p []byte) (int, error) {
	return s.buf.Write(p)
}

func (s *uploadStreamMemorySink) finish() (*uploadStreamSpool, error) {
	return &uploadStreamSpool{buf: &s.buf, size: int64(s.buf.Len())}, nil
}

func (s *uploadStreamMemorySink) release() {}

type uploadStreamFileSink struct {
	file *os.File
	dir  string
	path string
	n    int64
}

func openUploadStreamFile(tmpRoot string) (*uploadStreamFileSink, error) {
	dir, err := os.MkdirTemp(tmpRoot, "upload-stream-")
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dir, "payload-")
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &uploadStreamFileSink{file: file, dir: dir, path: file.Name()}, nil
}

func (s *uploadStreamFileSink) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, err := s.file.Write(p)
	s.n += int64(n)
	return n, err
}

func (s *uploadStreamFileSink) finish() (*uploadStreamSpool, error) {
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			return nil, err
		}
		s.file = nil
	}
	return &uploadStreamSpool{path: s.path, dir: s.dir, size: s.n}, nil
}

func (s *uploadStreamFileSink) release() {
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
		s.dir = ""
	}
}

// uploadStreamSpillSink keeps bytes in memory until threshold, then copies them to a temp file.
type uploadStreamSpillSink struct {
	threshold int64
	tmpRoot   string
	mem       bytes.Buffer
	file      *uploadStreamFileSink
	n         int64
}

func (s *uploadStreamSpillSink) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if s.file != nil {
		n, err := s.file.Write(p)
		s.n += int64(n)
		return n, err
	}
	if s.n >= s.threshold || int64(len(p)) > s.threshold-s.n {
		room := max(s.threshold-s.n, 0)
		if room > 0 {
			if _, err := s.mem.Write(p[:room]); err != nil {
				return 0, err
			}
			s.n += room
			p = p[room:]
		}
		if err := s.spill(); err != nil {
			return int(room), err
		}
		if len(p) == 0 {
			return int(room), nil
		}
		n, err := s.file.Write(p)
		s.n += int64(n)
		return int(room) + n, err
	}
	n, err := s.mem.Write(p)
	s.n += int64(n)
	return n, err
}

func (s *uploadStreamSpillSink) spill() error {
	if s.file != nil {
		return nil
	}
	file, err := openUploadStreamFile(s.tmpRoot)
	if err != nil {
		return err
	}
	if s.mem.Len() > 0 {
		if _, err = io.Copy(file, bytes.NewReader(s.mem.Bytes())); err != nil {
			file.release()
			return err
		}
		s.mem = bytes.Buffer{}
	}
	s.file = file
	return nil
}

func (s *uploadStreamSpillSink) finish() (*uploadStreamSpool, error) {
	if s.file == nil {
		return &uploadStreamSpool{buf: &s.mem, size: s.n}, nil
	}
	spool, err := s.file.finish()
	if err != nil {
		return nil, err
	}
	spool.size = s.n
	return spool, nil
}

func (s *uploadStreamSpillSink) release() {
	if s.file != nil {
		s.file.release()
		s.file = nil
	}
	s.mem = bytes.Buffer{}
}

func tmpDirPath(cfg *Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.TmpDirPath
}

func newUploadStreamMetadata(destFileName string, compress bool, spool *uploadStreamSpool) *fileMetadata {
	name := uploadStreamDestName(destFileName, compress)
	meta := &fileMetadata{
		name:            baseName(name),
		srcFileName:     name,
		dstFileName:     name,
		srcFileSize:     spool.size,
		uploadSize:      spool.size,
		sha256Digest:    spool.digest,
		requireCompress: false,
	}
	if compress {
		meta.dstCompressionType = compressionTypes["GZIP"]
	}
	if spool.path != "" {
		meta.srcFileName = spool.path
		// Encryption of a spilled object writes a second full-size file.
		// Keep that copy next to the spill file, not in Config.TmpDirPath.
		// The child directory stays while a credential or presigned-URL retry
		// uploads the same ciphertext, and is removed when that upload finishes.
		meta.encTempRoot = spool.dir
		return meta
	}
	meta.srcStream = spool.buf
	meta.fileStream = bytes.NewReader(spool.buf.Bytes())
	return meta
}
