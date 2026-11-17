package logger

import (
	"regexp"
)

const (
	awsKeyPattern          = `(?i)(aws_key_id|aws_secret_key|access_key_id|secret_access_key)\s*=\s*'([^']+)'`
	awsTokenPattern        = `(?i)(accessToken|tempToken|keySecret)"\s*:\s*"([a-z0-9/+]{32,}={0,2})"`
	sasTokenPattern        = `(?i)(sig|signature|AWSAccessKeyId|password|passcode)=(?P<secret>[a-z0-9%/+]{16,})`
	privateKeyPattern      = `(?im)-----BEGIN PRIVATE KEY-----\\n([a-z0-9/+=\\n]{32,})\\n-----END PRIVATE KEY-----` // pragma: allowlist secret
	privateKeyDataPattern  = `(?i)"privateKeyData": "([a-z0-9/+=\\n]{10,})"`
	privateKeyParamPattern = `(?i)privateKey=([A-Za-z0-9/+=_%-]+)(&|$|\s)`
	// connectionTokenPattern's value class admits ':' because every documented
	// Snowflake session-token format begins "ver:" - per GlobalServices
	// SecurityToken.java these are ver:1-hint:<keyId>-<encrypted>,
	// ver:2-hint:<keyId>-did:<deployId>-<encrypted>, and the V3/V4 equivalents
	// currently minted, with V2 and V4 carrying a further ':' in "-did:". Without
	// ':' the match ended after "ver", three characters in and below the
	// 8-character minimum, so the pattern matched no version of the format it is
	// named for.
	connectionTokenPattern = `(?i)(token|assertion content)([\'\"\s:=]+)([a-z0-9=/_\-\+:]{8,})`
	passwordPattern        = `(?i)(password|pwd)([\'\"\s:=]+)([a-z0-9!\"#\$%&\\\'\(\)\*\+\,-\./:;<=>\?\@\[\]\^_\{\|\}~]{8,})`
	dsnPasswordPattern     = `([^/:]+):([^@/:]{3,})@` // Matches user:password@host format in DSN strings
	clientSecretPattern    = `(?i)(clientSecret)([\'\"\s:= ]+)([a-z0-9!\"#\$%&\\\'\(\)\*\+\,-\./:;<=>\?\@\[\]\^_\{\|\}~]+)`
	jwtTokenPattern        = `(?i)(jwt|bearer)[\s:=]*([a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)` // pragma: allowlist secret
	// sseCustomerKeyPattern masks the S3 SSE-C customer-key header value, which the
	// generic patterns above do not cover (SNOW-3649835).
	sseCustomerKeyPattern = `(?i)(x-amz-server-side-encryption-customer-key)((?:\s*,?\s*value)?\s*[:=]\s*)([a-z0-9+/]{16,}={0,2})` // pragma: allowlist secret
	// awsSigV4ParamPattern masks the X-Amz-Credential and X-Amz-Security-Token query
	// parameters (the signature is handled by sasTokenPattern) (SNOW-3649835).
	awsSigV4ParamPattern = `(?i)(x-amz-credential|x-amz-security-token)=([a-z0-9%/+=._-]{8,})` // pragma: allowlist secret
)

type patternAndReplace struct {
	regex       *regexp.Regexp
	replacement string
}

var secretDetectorPatterns = []patternAndReplace{
	// These two must precede connectionTokenPattern so an X-Amz-Security-Token value
	// (which contains the substring "token" and a '%') is masked in full first.
	{regexp.MustCompile(sseCustomerKeyPattern), "${1}${2}****"},
	{regexp.MustCompile(awsSigV4ParamPattern), "${1}=****"},
	{regexp.MustCompile(awsKeyPattern), "$1=****$2"},
	{regexp.MustCompile(awsTokenPattern), "${1}XXXX$2"},
	{regexp.MustCompile(sasTokenPattern), "${1}=****"},
	{regexp.MustCompile(privateKeyPattern), "-----BEGIN PRIVATE KEY-----\\\\\\\\nXXXX\\\\\\\\n-----END PRIVATE KEY-----"}, // pragma: allowlist secret
	{regexp.MustCompile(privateKeyDataPattern), `"privateKeyData": "XXXX"`},
	{regexp.MustCompile(privateKeyParamPattern), "privateKey=****$2"},
	{regexp.MustCompile(connectionTokenPattern), "$1${2}****"},
	{regexp.MustCompile(passwordPattern), "$1${2}****"},
	{regexp.MustCompile(dsnPasswordPattern), "$1:****@"},
	{regexp.MustCompile(clientSecretPattern), "$1${2}****"},
	{regexp.MustCompile(jwtTokenPattern), "$1 ****"},
}

// MaskSecrets masks secrets in text (exported for use by main package and secret masking logger)
func MaskSecrets(text string) (masked string) {
	res := text
	for _, pattern := range secretDetectorPatterns {
		res = pattern.regex.ReplaceAllString(res, pattern.replacement)
	}
	return res
}
