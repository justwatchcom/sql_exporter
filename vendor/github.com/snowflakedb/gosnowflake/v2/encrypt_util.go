package gosnowflake

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/snowflakedb/gosnowflake/v2/internal/errors"
	"io"
	"os"
	"strconv"
)

const gcmIvLengthInBytes = 12

var (
	defaultKeyAad  = make([]byte, 0)
	defaultDataAad = make([]byte, 0)
)

// override default behavior for wrapper
func (ew *encryptionWrapper) UnmarshalJSON(data []byte) error {
	// if GET, unmarshal slice of encryptionMaterial
	if err := json.Unmarshal(data, &ew.EncryptionMaterials); err == nil {
		return err
	}
	// else (if PUT), unmarshal the encryptionMaterial itself
	return json.Unmarshal(data, &ew.snowflakeFileEncryption)
}

// encryptStreamCBC encrypts a stream buffer using AES128 block cipher in CBC mode
// with PKCS5 padding
func encryptStreamCBC(
	sfe *snowflakeFileEncryption,
	src io.Reader,
	out io.Writer,
	chunkSize int) (*encryptMetadata, error) {
	if chunkSize == 0 {
		chunkSize = aes.BlockSize * 4 * 1024
	}
	kek, err := base64.StdEncoding.DecodeString(sfe.QueryStageMasterKey)
	if err != nil {
		return nil, err
	}
	keySize := len(kek)

	fileKey := getSecureRandom(keySize)
	block, err := aes.NewCipher(fileKey)
	if err != nil {
		return nil, err
	}
	dataIv := getSecureRandom(block.BlockSize())

	mode := cipher.NewCBCEncrypter(block, dataIv)
	cipherText := make([]byte, chunkSize)
	chunk := make([]byte, chunkSize)

	// encrypt file with CBC
	padded := false
	for {
		// read the stream buffer up to len(chunk) bytes into chunk
		// note that all spaces in chunk may be used even if Read() returns n < len(chunk)
		n, err := src.Read(chunk)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("reading: %w", err)
		}
		if n == 0 {
			break
		}

		if n%aes.BlockSize != 0 {
			// add padding to the end of the chunk and update the length n
			chunk = padBytesLength(chunk[:n], aes.BlockSize)
			n = len(chunk)
			padded = true
		}
		// make sure only n bytes of chunk is used
		mode.CryptBlocks(cipherText, chunk[:n])
		if _, err := out.Write(cipherText[:n]); err != nil {
			return nil, err
		}
	}

	// add padding if not yet added
	if !padded {
		padding := bytes.Repeat([]byte(string(rune(aes.BlockSize))), aes.BlockSize)
		mode.CryptBlocks(cipherText, padding)
		if _, err := out.Write(cipherText[:len(padding)]); err != nil {
			return nil, err
		}
	}

	// encrypt key with ECB
	fileKey = padBytesLength(fileKey, block.BlockSize())
	encryptedFileKey := make([]byte, len(fileKey))
	if err = encryptECB(encryptedFileKey, fileKey, kek); err != nil {
		return nil, err
	}

	matDesc := materialDescriptor{
		strconv.FormatInt(sfe.SMKID, 10),
		sfe.QueryID,
		strconv.Itoa(keySize * 8),
	}

	matDescUnicode, err := matdescToUnicode(matDesc)
	if err != nil {
		return nil, err
	}
	return &encryptMetadata{
		base64.StdEncoding.EncodeToString(encryptedFileKey),
		base64.StdEncoding.EncodeToString(dataIv),
		matDescUnicode,
	}, nil
}

func encryptECB(encrypted []byte, fileKey []byte, decodedKey []byte) error {
	block, err := aes.NewCipher(decodedKey)
	if err != nil {
		return err
	}
	if len(fileKey)%block.BlockSize() != 0 {
		return fmt.Errorf("input not full of blocks")
	}
	if len(encrypted) < len(fileKey) {
		return fmt.Errorf("output length is smaller than input length")
	}
	for len(fileKey) > 0 {
		block.Encrypt(encrypted, fileKey[:block.BlockSize()])
		encrypted = encrypted[block.BlockSize():]
		fileKey = fileKey[block.BlockSize():]
	}
	return nil
}

func decryptECB(decrypted []byte, keyBytes []byte, decodedKey []byte) error {
	block, err := aes.NewCipher(decodedKey)
	if err != nil {
		return err
	}
	if len(keyBytes)%block.BlockSize() != 0 {
		return fmt.Errorf("input not full of blocks")
	}
	if len(decrypted) < len(keyBytes) {
		return fmt.Errorf("output length is smaller than input length")
	}
	for len(keyBytes) > 0 {
		block.Decrypt(decrypted, keyBytes[:block.BlockSize()])
		keyBytes = keyBytes[block.BlockSize():]
		decrypted = decrypted[block.BlockSize():]
	}
	return nil
}

func encryptFileCBC(
	sfe *snowflakeFileEncryption,
	filename string,
	chunkSize int,
	tmpDir string) (
	meta *encryptMetadata, fileName string, err error) {
	if chunkSize == 0 {
		chunkSize = aes.BlockSize * 4 * 1024
	}
	tmpOutputFile, err := os.CreateTemp(tmpDir, baseName(filename)+"#")
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if tmpErr := tmpOutputFile.Close(); tmpErr != nil && err == nil {
			err = tmpErr
		}
	}()
	infile, err := os.OpenFile(filename, os.O_CREATE|os.O_RDONLY, readWriteFileMode)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if tmpErr := infile.Close(); tmpErr != nil && err == nil {
			err = tmpErr
		}
	}()

	meta, err = encryptStreamCBC(sfe, infile, tmpOutputFile, chunkSize)
	if err != nil {
		return nil, "", err
	}
	return meta, tmpOutputFile.Name(), err
}

func decryptFileKeyECB(
	metadata *encryptMetadata,
	sfe *snowflakeFileEncryption) ([]byte, []byte, error) {
	decodedKey, err := base64.StdEncoding.DecodeString(sfe.QueryStageMasterKey)
	if err != nil {
		return nil, nil, err
	}
	keyBytes, err := base64.StdEncoding.DecodeString(metadata.key) // encrypted file key
	if err != nil {
		return nil, nil, err
	}
	ivBytes, err := base64.StdEncoding.DecodeString(metadata.iv)
	if err != nil {
		return nil, nil, err
	}

	// decrypt file key
	decryptedKey := make([]byte, len(keyBytes))
	if err = decryptECB(decryptedKey, keyBytes, decodedKey); err != nil {
		return nil, nil, err
	}
	decryptedKey, err = paddingTrim(decryptedKey)
	if err != nil {
		return nil, nil, err
	}

	return decryptedKey, ivBytes, err
}

func initCBC(decryptedKey []byte, ivBytes []byte) (cipher.BlockMode, error) {
	block, err := aes.NewCipher(decryptedKey)
	if err != nil {
		return nil, err
	}
	mode := cipher.NewCBCDecrypter(block, ivBytes)

	return mode, err
}

func decryptFileCBC(
	metadata *encryptMetadata,
	sfe *snowflakeFileEncryption,
	filename string,
	chunkSize int,
	tmpDir string) (outputFileName string, err error) {
	tmpOutputFile, err := os.CreateTemp(tmpDir, baseName(filename)+"#")
	if err != nil {
		return "", err
	}
	defer func() {
		if tmpErr := tmpOutputFile.Close(); tmpErr != nil && err == nil {
			err = tmpErr
		}
	}()
	infile, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer func() {
		if tmpErr := infile.Close(); tmpErr != nil && err == nil {
			err = tmpErr
		}
	}()
	dec, err := decryptStreamCBC(metadata, sfe, chunkSize, infile)
	if err != nil {
		return "", err
	}
	if _, err = io.Copy(tmpOutputFile, dec); err != nil {
		return "", err
	}
	return tmpOutputFile.Name(), err
}

// decryptStreamCBC returns a reader that decrypts AES-CBC ciphertext from src.
// PKCS padding is stripped at EOF. Close does not close src; the caller owns that.
func decryptStreamCBC(
	metadata *encryptMetadata,
	sfe *snowflakeFileEncryption,
	chunkSize int,
	src io.Reader) (io.ReadCloser, error) {
	if chunkSize == 0 {
		chunkSize = aes.BlockSize * 4 * 1024
	}
	decryptedKey, ivBytes, err := decryptFileKeyECB(metadata, sfe)
	if err != nil {
		return nil, err
	}
	mode, err := initCBC(decryptedKey, ivBytes)
	if err != nil {
		return nil, err
	}
	return &cbcDecryptReader{
		src:     src,
		mode:    mode,
		readBuf: make([]byte, chunkSize),
	}, nil
}

type cbcDecryptReader struct {
	src       io.Reader
	mode      cipher.BlockMode
	readBuf   []byte
	cipherBuf []byte
	held      []byte
	plain     []byte
	readErr   error
}

func (r *cbcDecryptReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if len(r.plain) > 0 {
			n := copy(p, r.plain)
			r.plain = r.plain[n:]
			return n, nil
		}
		if r.readErr != nil {
			return 0, r.readErr
		}
		if err := r.pull(); err != nil {
			if err == io.EOF {
				if ferr := r.finalize(); ferr != nil {
					r.readErr = ferr
					return 0, ferr
				}
				r.readErr = io.EOF
				continue
			}
			r.readErr = fmt.Errorf("reading: %w", err)
			return 0, r.readErr
		}
	}
}

func (r *cbcDecryptReader) pull() error {
	n, err := r.src.Read(r.readBuf)
	if n > 0 {
		r.cipherBuf = append(r.cipherBuf, r.readBuf[:n]...)
		r.decryptReady(false)
	}
	if n == 0 && err == nil {
		return io.ErrNoProgress
	}
	if err != nil {
		return err
	}
	return nil
}

func (r *cbcDecryptReader) decryptReady(final bool) {
	usable := len(r.cipherBuf) - len(r.cipherBuf)%aes.BlockSize
	if usable == 0 {
		return
	}
	dec := make([]byte, usable)
	r.mode.CryptBlocks(dec, r.cipherBuf[:usable])
	r.cipherBuf = append([]byte(nil), r.cipherBuf[usable:]...)
	r.held = append(r.held, dec...)
	if !final && len(r.held) > aes.BlockSize {
		flush := len(r.held) - aes.BlockSize
		r.plain = append(r.plain, r.held[:flush]...)
		r.held = append([]byte(nil), r.held[flush:]...)
	}
}

func (r *cbcDecryptReader) finalize() error {
	if len(r.cipherBuf) != 0 {
		return io.ErrUnexpectedEOF
	}
	r.decryptReady(true)
	if len(r.held) == 0 {
		return nil
	}
	stripped, err := paddingTrim(r.held)
	if err != nil {
		return err
	}
	r.plain = append(r.plain, stripped...)
	r.held = nil
	return nil
}

func (r *cbcDecryptReader) Close() error {
	return nil
}

func encryptGCM(iv []byte, plaintext []byte, encryptionKey []byte, aad []byte) ([]byte, error) {
	aead, err := initGcm(encryptionKey)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, iv, plaintext, aad), nil
}

func decryptGCM(iv []byte, ciphertext []byte, encryptionKey []byte, aad []byte) ([]byte, error) {
	aead, err := initGcm(encryptionKey)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, iv, ciphertext, aad)
}

func initGcm(encryptionKey []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func encryptFileGCM(
	sfe *snowflakeFileEncryption,
	filename string,
	tmpDir string) (
	meta *gcmEncryptMetadata, outputFileName string, err error) {
	tmpOutputFile, err := os.CreateTemp(tmpDir, baseName(filename)+"#")
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if tmpErr := tmpOutputFile.Close(); tmpErr != nil && err == nil {
			err = tmpErr
		}
	}()
	infile, err := os.OpenFile(filename, os.O_CREATE|os.O_RDONLY, readWriteFileMode)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if tmpErr := infile.Close(); tmpErr != nil && err == nil {
			err = tmpErr
		}
	}()
	plaintext, err := os.ReadFile(filename)
	if err != nil {
		return nil, "", err
	}

	kek, err := base64.StdEncoding.DecodeString(sfe.QueryStageMasterKey)
	if err != nil {
		return nil, "", err
	}
	keySize := len(kek)
	fileKey := getSecureRandom(keySize)
	keyIv := getSecureRandom(gcmIvLengthInBytes)
	encryptedFileKey, err := encryptGCM(keyIv, fileKey, kek, defaultKeyAad)
	if err != nil {
		return nil, "", err
	}

	dataIv := getSecureRandom(gcmIvLengthInBytes)
	encryptedData, err := encryptGCM(dataIv, plaintext, fileKey, defaultDataAad)
	if err != nil {
		return nil, "", err
	}
	_, err = tmpOutputFile.Write(encryptedData)
	if err != nil {
		return nil, "", err
	}

	matDesc := materialDescriptor{
		strconv.FormatInt(sfe.SMKID, 10),
		sfe.QueryID,
		strconv.Itoa(keySize * 8),
	}

	matDescUnicode, err := matdescToUnicode(matDesc)
	if err != nil {
		return nil, "", err
	}
	meta = &gcmEncryptMetadata{
		key:     base64.StdEncoding.EncodeToString(encryptedFileKey),
		keyIv:   base64.StdEncoding.EncodeToString(keyIv),
		dataIv:  base64.StdEncoding.EncodeToString(dataIv),
		keyAad:  base64.StdEncoding.EncodeToString(defaultKeyAad),
		dataAad: base64.StdEncoding.EncodeToString(defaultDataAad),
		matdesc: matDescUnicode,
	}
	return meta, tmpOutputFile.Name(), nil
}

func decryptFileGCM(
	metadata *gcmEncryptMetadata,
	sfe *snowflakeFileEncryption,
	filename string,
	tmpDir string) (
	string, error) {
	kek, err := base64.StdEncoding.DecodeString(sfe.QueryStageMasterKey)
	if err != nil {
		return "", err
	}
	encryptedFileKey, err := base64.StdEncoding.DecodeString(metadata.key)
	if err != nil {
		return "", err
	}
	keyIv, err := base64.StdEncoding.DecodeString(metadata.keyIv)
	if err != nil {
		return "", err
	}
	keyAad, err := base64.StdEncoding.DecodeString(metadata.keyAad)
	if err != nil {
		return "", err
	}
	dataIv, err := base64.StdEncoding.DecodeString(metadata.dataIv)
	if err != nil {
		return "", err
	}
	dataAad, err := base64.StdEncoding.DecodeString(metadata.dataAad)
	if err != nil {
		return "", err
	}

	fileKey, err := decryptGCM(keyIv, encryptedFileKey, kek, keyAad)
	if err != nil {
		return "", err
	}

	ciphertext, err := os.ReadFile(filename)
	if err != nil {
		return "", err
	}
	plaintext, err := decryptGCM(dataIv, ciphertext, fileKey, dataAad)
	if err != nil {
		return "", err
	}

	tmpOutputFile, err := os.CreateTemp(tmpDir, baseName(filename)+"#")
	if err != nil {
		return "", err
	}
	_, err = tmpOutputFile.Write(plaintext)
	if err != nil {
		return "", err
	}
	return tmpOutputFile.Name(), nil
}

type materialDescriptor struct {
	SmkID   string `json:"smkId"`
	QueryID string `json:"queryId"`
	KeySize string `json:"keySize"`
}

func matdescToUnicode(matdesc materialDescriptor) (string, error) {
	s, err := json.Marshal(&matdesc)
	if err != nil {
		return "", err
	}
	return string(s), nil
}

func getSecureRandom(byteLength int) []byte {
	token := make([]byte, byteLength)
	_, err := rand.Read(token)
	if err != nil {
		logger.Errorf("cannot init secure random. %v", err)
	}
	return token
}

func padBytesLength(src []byte, blockSize int) []byte {
	padLength := blockSize - len(src)%blockSize
	padText := bytes.Repeat([]byte{byte(padLength)}, padLength)
	return append(src, padText...)
}

func paddingTrim(src []byte) ([]byte, error) {
	if len(src) == 0 {
		logger.Errorf("padding trim failed - data length is 0")
		return nil, &SnowflakeError{
			Number:  ErrInvalidPadding,
			Message: "padding validation failed",
		}
	}
	unpadding := src[len(src)-1]
	n := int(unpadding)
	if n == 0 || n > len(src) {
		logger.Errorf("padding validation failed - invalid padding detected. data length: %d, padding value: %d",
			len(src), n)
		return nil, &SnowflakeError{
			Number:  ErrInvalidPadding,
			Message: errors.ErrMsgInvalidPadding,
		}
	}
	return src[:len(src)-n], nil
}

type contentKey struct {
	KeyID         string `json:"KeyId,omitempty"`
	EncryptionKey string `json:"EncryptedKey,omitempty"`
	Algorithm     string `json:"Algorithm,omitempty"`
}

type encryptionAgent struct {
	Protocol            string `json:"Protocol,omitempty"`
	EncryptionAlgorithm string `json:"EncryptionAlgorithm,omitempty"`
}

type keyMetadata struct {
	EncryptionLibrary string `json:"EncryptionLibrary,omitempty"`
}

type encryptionData struct {
	EncryptionMode      string          `json:"EncryptionMode,omitempty"`
	WrappedContentKey   contentKey      `json:"WrappedContentKey"`
	EncryptionAgent     encryptionAgent `json:"EncryptionAgent"`
	ContentEncryptionIV string          `json:"ContentEncryptionIV,omitempty"`
	KeyWrappingMetadata keyMetadata     `json:"KeyWrappingMetadata"`
}

type snowflakeFileEncryption struct {
	QueryStageMasterKey string `json:"queryStageMasterKey,omitempty"`
	QueryID             string `json:"queryId,omitempty"`
	SMKID               int64  `json:"smkId,omitempty"`
}

// PUT requests return a single encryptionMaterial object whereas GET requests
// return a slice (array) of encryptionMaterial objects, both under the field
// 'encryptionMaterial'
type encryptionWrapper struct {
	snowflakeFileEncryption
	EncryptionMaterials []snowflakeFileEncryption
}

type encryptMetadata struct {
	key     string
	iv      string
	matdesc string
}

type gcmEncryptMetadata struct {
	key     string
	keyIv   string
	dataIv  string
	keyAad  string
	dataAad string
	matdesc string
}
