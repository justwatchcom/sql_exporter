package config

import (
	"crypto/rsa"
	"crypto/x509"

	sferrors "github.com/snowflakedb/gosnowflake/v2/internal/errors"
)

// ParsePKCS8PrivateKey parses a PKCS8 encoded private key.
func ParsePKCS8PrivateKey(block []byte) (*rsa.PrivateKey, error) {
	privKey, err := x509.ParsePKCS8PrivateKey(block)
	if err != nil {
		return nil, &sferrors.SnowflakeError{
			Number:  sferrors.ErrCodePrivateKeyParseError,
			Message: "Error decoding private key using PKCS8.",
		}
	}
	return privKey.(*rsa.PrivateKey), nil
}

// MarshalPKCS8PrivateKey marshals a private key to PKCS8 format.
func MarshalPKCS8PrivateKey(key *rsa.PrivateKey) ([]byte, error) {
	keyInBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, &sferrors.SnowflakeError{
			Number:  sferrors.ErrCodePrivateKeyParseError,
			Message: "Error encoding private key using PKCS8."}
	}
	return keyInBytes, nil
}
