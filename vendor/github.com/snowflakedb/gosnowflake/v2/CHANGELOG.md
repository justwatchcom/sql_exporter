# Changelog

## Upcoming release

## 2.4.0

New features:
- Added the monitoring endpoint's timing breakdown (`TotalDuration`, `GsCompileDuration`, `GsExecDuration`, `XpExecDuration`, `QueryRetryTime`, `FaultHandlingTime`) to `SnowflakeQueryStatus`, so `GetQueryStatus` callers can read them. These fields were already returned by `/monitoring/queries/{id}` but dropped while decoding the response (snowflakedb/gosnowflake@912cb8ec2a6e).
- Added `UploadStream` / `UploadStreamWithConfig` on `SnowflakeConnection`: upload one stage object from an `io.Reader`. Gzip is the default, the destination name is a basename, and an optional prefix selects the stage directory. The payload stays in memory until 128 MiB, then spills to disk. Prefer this over `WithFilePutStream`, which still buffers the whole file (snowflakedb/gosnowflake@150d083fc10c).
- Added the `SF_TEMPORARY_CREDENTIAL_CACHE_FILE_NAME` environment variable to override the Linux token cache file name (default `credential_cache_v2.json`) (snowflakedb/gosnowflake@fc0309a6c496).

Internal changes:
- Upload GHA coverage reports to Dedicated Codecov (`snowflake.codecov.io`) from Jenkins snowos instead of `codecov-action` on GitHub-hosted runners, and bump golangci-lint to v2.14 for Go 1.27 (snowflakedb/gosnowflake@c05b13ef97ed).

Bug fixes:
- Fixed token cache key v2 normalization to lowercase URLs and identifiers and PascalCase token types (`IdToken`, `MfaToken`, `OauthAccessToken`, `OauthRefreshToken`) so keys match the cross-driver contract. Users of MFA, external-browser SSO, and OAuth token caching re-authenticate once after upgrading (snowflakedb/gosnowflake@fc0309a6c496).
- Changed where cached tokens are stored so they can be shared with the universal driver: macOS Keychain and Windows Credential Manager entries use service `snowflake_credential_cache`, and the Linux cache file is `credential_cache_v2.json`. Tokens in the old locations are not read (snowflakedb/gosnowflake@fc0309a6c496).
- Fixed the Linux token cache directory lookup to read `XDG_CACHE_HOME` instead of `XDG_CACHE_DIR`, and to disable caching with a warning instead of falling back to another directory when `SF_TEMPORARY_CREDENTIAL_CACHE_DIR` is set but unusable (snowflakedb/gosnowflake@fc0309a6c496).
- Fixed the Linux token cache lock to use `flock` on `<cache file>.lck` instead of a lock directory that was deleted after one second, and to replace a corrupt cache file on the next write instead of disabling caching (snowflakedb/gosnowflake@fc0309a6c496).
- Fixed `loadCore()` leaking the `gosnowflake-cgo*` temp directory on early returns (unsupported platform, statically-linked binary, or a failed `writeLibraryToFile()`) that occur before the library-write success path registers cleanup (snowflakedb/gosnowflake#1856).
- Fixed Azure workload-identity authentication on a VM that has only a user-assigned managed identity by sending `client_id` on the IMDS token request when `WorkloadIdentityAzureClientID` or `MANAGED_IDENTITY_CLIENT_ID` is set (snowflakedb/gosnowflake@739b862f936d).
- Avoided a process-global logger lock and per-value argument formatting while converting result cells when debug logging is off (snowflakedb/gosnowflake@62ee24639b92).
- Demoted per-timestamp `sec`/`nsec` logging in `extractTimestamp` from INFO to DEBUG (snowflakedb/gosnowflake@62ee24639b92).
- Fixed a panic when an ARRAY or OBJECT arrives as native Arrow. With structured types disabled, the query returns the existing structured-types error. With no field metadata, it returns an error that names the Arrow type. Previously both cases terminated the process (snowflakedb/gosnowflake@af4bbe437bb5).
- Fixed external-browser authentication hanging when an idle connection reaches the callback listener before the real browser redirect (snowflakedb/gosnowflake@5f174118ad08).
- Fixed Azure Git repository downloads requesting a duplicate `blobs/` prefix (snowflakedb/gosnowflake@5d3ada21b10c).
- Fixed a data race in `WithFileGetStream` that could panic or return corrupted bytes for S3 objects downloaded in multiple parts (snowflakedb/gosnowflake@f62f4f9d9a44).

## 2.3.0

Updates:
- OCSP certificate revocation checking is now off by default; opt in with `ocspFailOpen`/`OCSPFailOpen` or `SF_DISABLE_OCSP_CHECKS=false`.
  - `FillMissingConfigParameters` no longer defaults `OCSPFailOpen` nor snapshots the env onto `DisableOCSPChecks`.
  - `DSN()` omits unset `ocspFailOpen`, so newly generated DSNs no longer carry the opt-in, while DSNs stored from v2.2 still enable OCSP.
  - `SF_DISABLE_OCSP_CHECKS` is now read live at connection time rather than at fill time.
  - `SF_DISABLE_OCSP_CHECKS=true` still disables, except under fail-closed.

New features:
- Added `WorkloadIdentityHost` config option (DSN field `workloadIdentityHost`) that overrides the STS host used by the AWS WIF flows, for endpoints the driver cannot derive from the region (such as an interface VPC endpoint). Must be an STS endpoint for the region the workload runs in (snowflakedb/gosnowflake@b1ea3911a7e2).
- Added `DownloadStream` / `DownloadStreamWithConfig` on `SnowflakeConnection`: sequential GET of one stage object as `io.ReadCloser`, with optional gzip decompression. Prefer this over `WithFileGetStream`, which still buffers the whole file (snowflakedb/gosnowflake@bf1dad211856).

Bug fixes:
- Hardened external-browser callback handling by checking `Origin` headers against the connected Snowflake account endpoint, accepting matching-Origin POST callbacks, and continuing to wait after unrelated or incomplete requests (snowflakedb/gosnowflake@386bbc9d8caa).
- Fixed `GetQueryStatus` treating `RESTARTED` as success by counting that status as still running (snowflakedb/gosnowflake#1842).
- Fixed stage array bind of TIMESTAMP_LTZ/NTZ/TZ writing local wallclock with no offset, so bulk inserts disagreed with scalar bind on non-UTC hosts.

Internal changes:
- Platform detection now logs why each platform was not detected.

## 2.2.0

New features:
- Added `WorkloadIdentityAwsUseOutboundToken` config option (DSN field `workloadIdentityAwsUseOutboundToken`) that produces the AWS WIF attestation as an STS `GetWebIdentityToken` JWT instead of the default signed `GetCallerIdentity` request envelope (snowflakedb/gosnowflake#1824).
- Added `CleanupTimeout` config option (DSN field `cleanupTimeout`, in seconds) that bounds post-cancellation cleanup (snowflakedb/gosnowflake#1816).
- Increased CRL disk cache removal delay to 7 days (snowflakedb/gosnowflake#1820).
- Added option to load private key from `connections.toml` file (snowflakedb/gosnowflake#1822).
- Added support for Go 1.27, dropped support for Go 1.24 (snowflakedb/gosnowflake#1837).
- Added `SNOWFLAKE_MIN_TLS_VERSION` environment variable to enforce a minimum TLS version for all connections (snowflakedb/gosnowflake#1840).

Security fixes:
- Restricted the WORKLOAD_IDENTITY authenticator to recognized Snowflake hosts (*.snowflakecomputing.com/.cn/.mil), normalizing the host before a suffix-anchored match. The `SNOWFLAKE_WIF_ALLOWED_HOST_SUFFIXES` environment variable additively extends the recognized-host list (SNOW-3649730).
- Azure workload-identity IMDS requests now URL-encode the `workloadIdentityEntraResource` and client-id query parameters, so their values are confined to their own parameter and cannot alter the rest of the query string (SNOW-3649876).
- The Azure `IDENTITY_ENDPOINT` is now required to be a loopback or link-local address before the `IDENTITY_HEADER` value is attached to the request (SNOW-3649837).
- Account-identifier and region values in DSN parameters are now validated as well-formed URL-authority components before they are incorporated into driver-constructed URLs, and OCSP cache-server and retry URLs are built via `net/url` rather than direct string formatting, so neither can embed URL-significant characters that would alter the resulting URL authority (SNOW-3711332).
- Improved OCSP response validation to correctly distinguish between transient network failures and definitive certificate status results; definitive results are now always reflected accurately regardless of the `ocspFailOpen` setting (SNOW-3649697).
- Extended secret masking in debug logs to cover the S3 SSE-C customer-key header and the `X-Amz-Credential` / `X-Amz-Security-Token` URL query parameters (SNOW-3649835).
- HTTP response headers are now logged as header names only, without their values, at every site that previously logged the whole header collection (`restful.go`, `auth.go`, `authokta.go`, `heartbeat.go`). Header values carry session tokens, storage credentials and the SSE-C customer key, and formatting an `http.Header` with `%v` produced a shape the header-specific masking patterns did not match, so the values are omitted rather than masked (SNOW-3649835).
- The chunk-header debug line now logs the header name only, not its value (SNOW-3649835).
- The TOML connection-config loader now logs setting names only, not their values, when reporting a parsing error (SNOW-3649818).
- Corrected the SAS-token masking pattern so signed-URL query parameters are fully masked in logs (SNOW-3649773).
- Corrected the connection-token masking pattern to admit `:` in the token value, so a Snowflake session token (`ver:1-hint:...`) is masked; previously the match ended at the first colon and fell below the pattern's minimum length, so the pattern never matched the format it targets (SNOW-3649773).

Bug fixes:
- Fixed token cache key collisions for multi-account (shared IdP) and multi-role scenarios by switching to a versioned, SHA256-hashed canonical-JSON key with the token type in the key prefix, applied uniformly across keyring (macOS/Windows) and file (Linux) backends; also replaced the bag-of-fields spec struct with typed token-spec types (`hostUserTokenSpec` for MFA/ID flows, `oauthTokenSpec` for OAuth flows) so each spec validates and serializes only its own fields (snowflakedb/gosnowflake#1817, snowflakedb/gosnowflake#1821).
- Fixed nil pointer dereference panic when a corrupt or malformed OCSP cache key is encountered, either from the remote OCSP cache server or the local cache file (snowflakedb/gosnowflake#1819).
- Fixed `WithFileGetStream` returning corrupt or wrong-file bytes when a `GET` prefix-matched more than one file; a get-stream can return only one file, so a multi-file match now returns the new `ErrGetStreamMultipleFiles` (pointing to the GET `PATTERN` argument) instead of a nondeterministic mix (snowflakedb/gosnowflake#1809).
- Do not attempt to get S3 bucket accelerate config for Snowflake-internal stages (matched by bucket name `sfc-*`) since s3:GetAccelerateConfiguration not granted anyways (snowflakedb/gosnowflake#1805).
- Fixed gosnowflake writing a `gosnowflake-cgo` directory under the system temp dir at package import time even when the driver was never used (e.g. when imported only as a transitive dependency). Minicore now loads lazily when the driver is first referenced (`NewConnector`/`OpenWithConfig`) instead of in `init()` (snowflakedb/gosnowflake#1807).
- Fixed `GET` from a LOCAL_FS stage downloading 0 files and returning `264011: not implemented`. Cloud downloads were unaffected (snowflakedb/gosnowflake#1810).
- Fixed `NUMBER` columns with a non-zero scale losing precision: values needing more significant digits than a binary float's mantissa were silently rounded, so `NUMBER(20,10)` holding `1234567890.1234567890` returned `1234567890.1234567889`. The `WithHigherPrecision` path is unchanged and still returns a 64-bit `*big.Float` (snowflakedb/gosnowflake#1835).
- Fixed proxy configuration for a literal IPv6 `ProxyHost`, which was concatenated into an unbracketed `host:port` and left the proxy URL unparseable (snowflakedb/gosnowflake#TBD).
- Fixed the `region` parameter rewriting an explicitly provided `host` (e.g. `host=myacct.snowflakecomputing.com` with `region=us-east-1` became `myacct.us-east-1.snowflakecomputing.com`); an explicit host now takes precedence (snowflakedb/gosnowflake#1841).

Internal changes:
- Migrated from deprecated `github.com/aws/aws-sdk-go-v2/feature/s3/manager` (v1.16.15) to `github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager` (v0.3.5).
- Introduced `ConversionOptions` for Arrow batches configuration (snowflakedb/gosnowflake#1832).
- Updated AWS SDK dependencies:
  - `github.com/aws/aws-sdk-go-v2`: v1.38.1 → v1.43.0
  - `github.com/aws/aws-sdk-go-v2/config`: v1.27.11 → v1.32.31
  - `github.com/aws/aws-sdk-go-v2/credentials`: v1.17.11 → v1.19.30
  - `github.com/aws/aws-sdk-go-v2/feature/ec2/imds`: v1.16.1 → v1.18.31
  - `github.com/aws/aws-sdk-go-v2/service/s3`: v1.53.1 → v1.106.0
  - `github.com/aws/aws-sdk-go-v2/service/sts`: v1.28.6 → v1.45.0
  - `github.com/aws/smithy-go`: v1.22.5 → v1.27.4
- Adopted Go 1.25 idioms now that the minimum supported version is 1.25: `sync.WaitGroup.Go` for driver-managed goroutines, and the `min`/`max` builtins in place of the hand-rolled `intMin`/`intMax`/`int64Max`/`durationMin`/`durationMax` helpers.
- Enabled the `modernize`, `nosprintfhostport`, `perfsprint` and `usetesting` linters and the Go 1.25 `hostport`/`waitgroup` `go vet` analyzers; bumped golangci-lint to v2.13.
- Fixed the `fakeResponseBody` test double reporting more bytes read than fit in the caller's buffer, which panicked the Go 1.27 `encoding/json` decoder.

## 2.1.0

New features:

- Added `SF_DISABLE_OCSP_CHECKS` environment variable as an additional option to override OCSP checks' default behaviour besides the existing `DisableOCSPChecks` config. (snowflakedb/gosnowflake#1798).
  - Note: the env var is explicitly refused when OCSP fail-closed mode is active.
- Added `ArrowStreamBatch.Reset()` method that closes any existing stream and clears the cached reader, allowing callers to retry `GetStream` after a mid-stream failure (e.g. TCP RST) without re-executing the entire query. Inline (RowSetBase64) batches are restored from cached bytes on reset.
- Added one in-band telemetry record per successful login describing which connection-identifier fields the user supplied (`account_provided`, `account_with_region`, `account_org_provided`, `region_provided`, `host_provided`). No hostname or account value is included. This is gated by the existing server-side `CLIENT_TELEMETRY_ENABLED` parameter and can additionally be disabled locally by setting `SF_TELEMETRY_DISABLE_CONNECTION_SHAPE=true`. The telemetry collection is time-boxed and will be removed in a future release.

Bug fixes:

- Fixed stale OCSP cache `.lck` directory permanently blocking cache writes (and forcing an online OCSP validation if OCSP is enabled, as is by default) by using `os.RemoveAll` instead of `os.Remove` for stale lock recovery (snowflakedb/gosnowflake#1793).
- Fixed regular chunk downloader reads so canceling the query context now interrupts stalled chunk downloads and wakes waiting row readers instead of hanging on the HTTP body read (snowflakedb/gosnowflake#1789).
- Fixed `QueryArrowStream` chunk reads so canceling the query context now interrupts stalled Arrow stream downloads and reports the cancellation instead of hanging on the HTTP body read (snowflakedb/gosnowflake#1789).
- Fixed `baseName` silently dropping files whose name ends with a dot (e.g. `myfile.txt.`), which caused PUT uploads to discard such files without error (snowflakedb/gosnowflake#1788).
- Improved error message when `Host` is incorrectly configured with a URL scheme (e.g. `https://myorg-myaccount.snowflakecomputing.com`), previously this produced a cryptic `260004: failed to parse a port number` error (snowflakedb/gosnowflake#1784).
- Fixed minicore build on OpenBSD by skipping the `-ldl` linker flag, since libdl is not a separate library on OpenBSD (`dlopen`/`dlsym` are provided by libc) (snowflakedb/gosnowflake#1791).

Internal changes:

- Introduced `SKIP_FILE_PERMISSIONS_VERIFICATION` environment variable to allow bypassing file permissions checks for `connections.toml` and the credential cache, which is useful for environments where strict permissions cannot be set (snowflakedb/gosnowflake#1780).
- Added support for `SPCS_TOKEN` in the login request. When the driver detects it is running inside a Snowpark Container Services workload (via the `SNOWFLAKE_RUNNING_INSIDE_SPCS` environment variable), it reads an opaque service token from `/snowflake/session/spcs_token` on every login and attaches it to the login-request payload as `SPCS_TOKEN`. Read failures are logged at warn and do not affect login (snowflakedb/gosnowflake#1783).
- Minicore binaries for Windows and Mac are now signed - content is the same. (snowflakedb/gosnowflake#1790).

## 2.0.2

New features:

- Added `QueryResultFormatProvider` interface to expose the server-reported query result format ("arrow" or "json") from `QueryArrowStream`, enabling callers to distinguish Arrow IPC from JSON responses before interpreting batch streams (snowflakedb/gosnowflake#1773).

Bug fixes:

- Fixed empty `Account` when connecting with programmatic `Config` and `database/sql.Connector` by deriving `Account` from the first DNS label of `Host` in `FillMissingConfigParameters` when `Host` matches the Snowflake hostname pattern (snowflakedb/gosnowflake#1772).
- Fixed PAT (Programmatic Access Token) `authenticator` to actually require the Token or TokenFilePath field, instead of silently accepting Password which was never forwarded (snowflakedb/gosnowflake#1772).
- Fix logger reporting incorrect source location when called without `WithContext` (snowflakedb/gosnowflake#1768).
- GCP WIF attestation now uses hostname `metadata.google.internal` instead of the IPv4 link-local address, so it works on IPv6-only GCP VMs (snowflakedb/gosnowflake#1775).
- Fixed query failures on large inline results (e.g. 64MB LOB) caused by truncated HTTP response bodies. The driver now retries the query when `json.Decoder` returns `io.ErrUnexpectedEOF`, reusing the same request ID so Snowflake returns the cached result (snowflakedb/gosnowflake#1777).

## 2.0.1

Bug fixes:

- Fixed default `CrlDownloadMaxSize` to be 20MB instead of 200MB, as the previous value was set too high and could cause out-of-memory issues (snowflakedb/gosnowflake#1735).
- Replaced global `paramsMutex` with per-connection `syncParams` to encapsulate parameter synchronization and avoid cross-connection contention (snowflakedb/gosnoflake#1747).
- `Config.Params` map is not modified anymore, to avoid changing parameter values across connections of the same connection pool (snowflakedb/gosnowflake#1747).
- Set `BlobContentMD5` on Azure uploads so that multi-part uploads have the blob content-MD5 property populated (snowflakedb/gosnowflake#1757).
- Fixed 403 errors from Google/GCP/GCS PUT queries on versioned stages (snowflakedb/gosnowflake#1760).
- Fixed not updating query context cache for failed queries (snowflakedb/gosnowflake#1763).

Internal changes:

- Moved configuration to a dedicated internal package (snowflakedb/gosnowflake#1720).
- Modernized Go syntax idioms throughout the codebase.
- Added libc family, version and dynamic linking marker to client environment telemetry (snowflakedb/gosnowflake#1750).
- Bumped a few libraries to fix vulnerabilities (snowflakedb/gosnowflake#1751, snowflakedb/gosnowflake#1756).
- Depointerised query context cache in `snowflakeConn` (snowflakedb/gosnowflake#1763).

## 2.0.0

Breaking changes:

- Removed `RaisePutGetError` from `SnowflakeFileTransferOptions` - current behaviour is aligned to always raise errors for PUT/GET operations (snowflakedb/gosnowflake#1690).
- Removed `GetFileToStream` from `SnowflakeFileTransferOptions` - using `WithFileGetStream` automatically enables file streaming for GETs (snowflakedb/gosnowflake#1690).
- Renamed `WithFileStream` to `WithFilePutStream` for consistency (snowflakedb/gosnowflake#1690).
- `Array` function now returns error for unsupported types (snowflakedb/gosnowflake#1693).
- `WithMultiStatement` does not return error anymore (snowflakedb/gosnowflake#1693).
- `WithOriginalTimestamp` is removed, use `WithArrowBatchesTimestampOption(UseOriginalTimestamp)` instead (snowflakedb/gosnowflake#1693).
- `WithMapValuesNullable` and `WithArrayValuesNullable` combined into one option `WithEmbeddedValuesNullable` (snowflakedb/gosnowflake#1693).
- Hid streaming chunk downloader. It will be removed completely in the future (snowflakedb/gosnowflake#1696).
- Maximum number of chunk download goroutines is now configured with `CLIENT_PREFETCH_THREADS` session parameter (snowflakedb/gosnowflake#1696)
  and default to 4.
- Fixed typo in `GOSNOWFLAKE_SKIP_REGISTRATION` env variable (snowflakedb/gosnowflake#1696).
- Removed `ClientIP` field from `Config` struct. This field was never used and is not needed for any functionality (snowflakedb/gosnowflake#1692).
- Unexported MfaToken and IdToken (snowflakedb/gosnowflake#1692).
- Removed `InsecureMode` field from `Config` struct. Use `DisableOCSPChecks` instead (snowflakedb/gosnowflake#1692).
- Renamed `KeepSessionAlive` field in `Config` struct to `ServerSessionKeepAlive` to adjust with the remaining drivers (snowflakedb/gosnowflake#1692).
- Removed `DisableTelemetry` field from `Config` struct. Use `CLIENT_TELEMETRY_ENABLED` session parameter instead (snowflakedb/gosnowflake#1692).
- Removed stream chunk downloader. Use a regular, default downloader instead. (snowflakedb/gosnowflake#1702).
- Removed `SnowflakeTransport`. Use `Config.Transporter` or simply register your own TLS config with `RegisterTLSConfig` if you just need a custom root certificates set (snowflakedb/gosnowflake#1703).
- Arrow batches changes (snowflakedb/gosnowflake#1706):
  - Arrow batches have been extracted to a separate package. It should significantly drop the compilation size for those who don't need arrow batches (~34MB -> ~18MB).
  - Removed `GetArrowBatches` from `SnowflakeRows` and `SnowflakeResult`. Use `arrowbatches.GetArrowBatches(rows.(SnowflakeRows))` instead.
  - Migrated functions:
    - `sf.WithArrowBatchesTimestampOption` -> `arrowbatches.WithTimstampOption`
    - `sf.WithArrowBatchesUtf8Validation` -> `arrowbatches.WithUtf8Validation`
    - `sf.ArrowSnowflakeTimestampToTime` -> `arrowbatches.ArrowSnowflakeTimestampToTime`
- Logging changes (snowflakedb/gosnowflake#1710):
  - Removed Logrus logger and migrated to slog.
  - Simplified `SFLogger` interface.
  - Added `SFSlogLogger` interface for setting custom slog handler.

Bug fixes:

- The query `context.Context` is now propagated to cloud storage operations for PUT and GET queries, allowing for better cancellation handling (snowflakedb/gosnowflake#1690).

New features:

- Added support for Go 1.26, dropped support for Go 1.23 (snowflakedb/gosnowflake#1707).
- Added support for FIPS-only mode (snowflakedb/gosnowflake#1496).

Bug fixes:

- Added panic recovery block for stage file uploads and downloads operation (snowflakedb/gosnowflake#1687).
- Fixed WIF metadata request from Azure container, manifested with HTTP 400 error (snowflakedb/gosnowflake#1701).
- Fixed SAML authentication port validation bypass in `isPrefixEqual` where the second URL's port was never checked (snowflakedb/gosnowflake#1712).
- Fixed a race condition in OCSP cache clearer (snowflakedb/gosnowflake#1704).
- The query `context.Context` is now propagated to cloud storage operations for PUT and GET queries, allowing for better cancellation handling (snowflakedb/gosnowflake#1690).
- Fixed `tokenFilePath` DSN parameter triggering false validation error claiming both `token` and `tokenFilePath` were specified when only `tokenFilePath` was provided in the DSN string (snowflakedb/gosnowflake#1715).
- Fixed minicore crash (SIGFPE) on fully statically linked Linux binaries by detecting static linking via ELF PT_INTERP inspection and skipping `dlopen` gracefully (snowflakedb/gosnowflake#1721).

Internal changes:

- Moved configuration to a dedicated internal package (snowflakedb/gosnowflake#1720).

## 1.19.0

New features:

- Added ability to disable minicore loading at compile time (snowflakedb/gosnowflake#1679).
- Exposed `tokenFilePath` in `Config` (snowflakedb/gosnowflake#1666).
- `tokenFilePath` is now read for every new connection (snowflakedb/gosnowflake#1666).
- Added support for identity impersonation when using workload identity federation (snowflakedb/gosnowflake#1652, snowflakedb/gosnowflake#1660).

Bug fixes:

- Fixed getting file from an unencrypted stage (snowflakedb/gosnowflake#1672).
- Fixed minicore file name gathering in client environment (snowflakedb/gosnowflake#1661).
- Fixed file descriptor leaks in cloud storage calls (snowflakedb/gosnowflake#1682)
- Fixed path escaping for GCS urls (snowflakedb/gosnowflake#1678).

Internal changes:

- Improved Linux telemetry gathering (snowflakedb/gosnowflake#1677).
- Improved some logs returned from cloud storage clients (snowflakedb/gosnowflake#1665).

## 1.18.1

Bug fixes:

- Handle HTTP307 & 308 in drivers to achieve better resiliency to backend errors (snowflakedb/gosnowflake#1616).
- Create temp directory only if needed during file transfer (snowflakedb/gosnowflake#1647)
- Fix unnecessary user expansion for file paths (snowflakedb/gosnowflake#1646).

Internal changes:
- Remove spammy "telemetry disabled" log messages (snowflakedb/gosnowflake#1638).
- Introduced shared library ([source code](https://github.com/snowflakedb/universal-driver/tree/main/sf_mini_core)) for extended telemetry to identify and prepare testing platform for native rust extensions (snowflakedb/gosnowflake#1629)

## 1.18.0

New features:

- Added validation of CRL `NextUpdate` for freshly downloaded CRLs (snowflakedb/gosnowflake#1617)
- Exposed function to send arbitrary telemetry data (snowflakedb/gosnowflake#1627)
- Added logging of query text and parameters (snowflakedb/gosnowflake#1625)

Bug fixes:

- Fixed a data race error in tests caused by platform_detection init() function (snowflakedb/gosnowflake#1618)
- Make secrets detector initialization thread safe and more maintainable (snowflakedb/gosnowflake#1621)

Internal changes:

- Added ISA to login request telemetry (snowflakedb/gosnowflake#1620)

## 1.17.1

- Fix unsafe reflection of nil pointer on DECFLOAT func in bind uploader (snowflakedb/gosnowflake#1604).
- Added temporary download files cleanup (snowflakedb/gosnowflake#1577)
- Marked fields as deprecated (snowflakedb/gosnowflake#1556)
- Exposed `QueryStatus` from `SnowflakeResult` and `SnowflakeRows` in `GetStatus()` function (snowflakedb/gosnowflake#1556)
- Split timeout settings into separate groups based on target service types (snowflakedb/gosnowflake#1531)
- Added small clarification in oauth.go example on token escaping (snowflakedb/gosnowflake#1574)
- Ensured proper permissions for CRL cache directory (snowflakedb/gosnowflake#1588)
- Added `CrlDownloadMaxSize` to limit the size of CRL downloads (snowflakedb/gosnowflake#1588)
- Added platform telemetry to login requests. Can be disabled with `SNOWFLAKE_DISABLE_PLATFORM_DETECTION` environment variable (snowflakedb/gosnowflake#1601)
- Bypassed proxy settings for WIF metadata requests (snowflakedb/gosnowflake#1593)
- Fixed a bug where GCP PUT/GET operations would fail when the connection context was cancelled (snowflakedb/gosnowflake#1584)
- Fixed nil pointer dereference while calling long-running queries (snowflakedb/gosnowflake#1592) (snowflakedb/gosnowflake#1596)
- Moved keyring-based secure storage manager into separate file to avoid the need to initialize keyring on Linux. (snowflakedb/gosnowflake#1595)
- Enabling official support for RHEL9 by testing and enabling CI/CD checks for Rocky Linux in CICD, (snowflakedb/gosnowflake#1597)
- Improve logging (snowflakedb/gosnowflake#1570)

## 1.17.0

- Added ability to configure OCSP per connection (snowflakedb/gosnowflake#1528)
- Added `DECFLOAT` support, see details in `doc.go` (snowflakedb/gosnowflake#1504, snowflakedb/gosnowflake#1506)
- Added support for Go 1.25, dropped support for Go 1.22 (snowflakedb/gosnowflake#1544)
- Added proxy options to connection parameters (snowflakedb/gosnowflake#1511)
- Added `client_session_keep_alive_heartbeat_frequency` connection param (snowflakedb/gosnowflake#1576)
- Added support for multi-part downloads for S3, Azure and GCP (snowflakedb/gosnowflake#1549)
- Added `singleAuthenticationPrompt` to control whether only one authentication should be performed at the same time for authentications that need human interactions (like MFA or OAuth authorization code). Default is true. (snowflakedb/gosnowflake#1561)
- Fixed missing `DisableTelemetry` option in connection parameters (snowflakedb/gosnowflake#1520)
- Fixed multistatements in large result sets (snowflakedb/gosnowflake#1539, snowflakedb/gosnowflake#1543, snowflakedb/gosnowflake#1547)
- Fixed unnecessary retries when context is cancelled (snowflakedb/gosnowflake#1540)
- Fixed regression in TOML connection file (snowflakedb/gosnowflake#1530)

## Prior Releases

Release notes available at https://docs.snowflake.com/en/release-notes/clients-drivers/golang
