package gosnowflake

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	errors2 "github.com/snowflakedb/gosnowflake/v2/internal/errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/browser"
)

const (
	samlSuccessHTML = `<!DOCTYPE html><html><head><meta charset="UTF-8"/>
<title>SAML Response for Snowflake</title></head>
<body>
Your identity was confirmed and propagated to Snowflake %v.
You can close this window now and go back where you started from.
</body></html>`

	bufSize = 8192
)

// Builds a response to show to the user after successfully
// getting a response from Snowflake.
func buildResponse(body string) (bytes.Buffer, error) {
	t := &http.Response{
		Status:        "200 OK",
		StatusCode:    200,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Body:          io.NopCloser(bytes.NewBufferString(body)),
		ContentLength: int64(len(body)),
		Request:       nil,
		Header:        make(http.Header),
	}
	var b bytes.Buffer
	err := t.Write(&b)
	return b, err
}

// This opens a socket that listens on all available unicast
// and any anycast IP addresses locally. By specifying "0", we are
// able to bind to a free port.
func createLocalTCPListener(port int) (*net.TCPListener, error) {
	logger.Debugf("creating local TCP listener on port %v", port)
	allAddressesListener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%v", port))
	if err != nil {
		logger.Warnf("error while setting up 0.0.0.0 listener: %v", err)
		return nil, err
	}
	logger.Debug("Closing 0.0.0.0 tcp listener")
	if err := allAddressesListener.Close(); err != nil {
		logger.Errorf("error while closing TCP listener. %v", err)
		return nil, err
	}

	l, err := net.Listen("tcp", fmt.Sprintf("localhost:%v", port))
	if err != nil {
		logger.Warnf("error while setting up listener: %v", err)
		return nil, err
	}

	tcpListener, ok := l.(*net.TCPListener)
	if !ok {
		return nil, fmt.Errorf("failed to assert type as *net.TCPListener")
	}

	return tcpListener, nil
}

// Opens a browser window (or new tab) with the configured login Url.
// This can / will fail if running inside a shell with no display, ie
// ssh'ing into a box attempting to authenticate via external browser.
func openBrowser(browserURL string) error {
	parsedURL, err := url.ParseRequestURI(browserURL)
	if err != nil {
		logger.Errorf("error parsing url %v, err: %v", browserURL, err)
		return err
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return fmt.Errorf("invalid browser URL: %v", browserURL)
	}
	err = browser.OpenURL(browserURL)
	if err != nil {
		logger.Errorf("failed to open a browser. err: %v", err)
		return err
	}
	return nil
}

// Gets the IDP Url and Proof Key from Snowflake.
// Note: FuncPostAuthSaml will return a fully qualified error if
// there is something wrong getting data from Snowflake.
func getIdpURLProofKey(
	ctx context.Context,
	sr *snowflakeRestful,
	authenticator string,
	application string,
	account string,
	user string,
	callbackPort int) (string, string, error) {

	headers := make(map[string]string)
	headers[httpHeaderContentType] = headerContentTypeApplicationJSON
	headers[httpHeaderAccept] = headerContentTypeApplicationJSON
	headers[httpHeaderUserAgent] = userAgent

	clientEnvironment := newAuthRequestClientEnvironment()
	clientEnvironment.Application = application

	requestMain := authRequestData{
		ClientAppID:             clientType,
		ClientAppVersion:        SnowflakeGoDriverVersion,
		AccountName:             account,
		LoginName:               user,
		ClientEnvironment:       clientEnvironment,
		Authenticator:           authenticator,
		BrowserModeRedirectPort: strconv.Itoa(callbackPort),
	}

	authRequest := authRequest{
		Data: requestMain,
	}

	jsonBody, err := json.Marshal(authRequest)
	if err != nil {
		logger.WithContext(ctx).Errorf("failed to serialize json. err: %v", err)
		return "", "", err
	}

	respd, err := sr.FuncPostAuthSAML(ctx, sr, headers, jsonBody, sr.LoginTimeout)
	if err != nil {
		return "", "", err
	}
	if !respd.Success {
		logger.WithContext(ctx).Error("Authentication FAILED")
		sr.TokenAccessor.SetTokens("", "", -1)
		code, err := strconv.Atoi(respd.Code)
		if err != nil {
			return "", "", err
		}
		return "", "", &SnowflakeError{
			Number:   code,
			SQLState: SQLStateConnectionRejected,
			Message:  respd.Message,
		}
	}
	return respd.Data.SSOURL, respd.Data.ProofKey, nil
}

// Gets the login URL for multiple SAML
func getLoginURL(sr *snowflakeRestful, user string, callbackPort int) (string, string, error) {
	proofKey := generateProofKey()

	params := &url.Values{}
	params.Add("login_name", user)
	params.Add("browser_mode_redirect_port", strconv.Itoa(callbackPort))
	params.Add("proof_key", proofKey)
	url := sr.getFullURL(consoleLoginRequestPath, params)

	return url.String(), proofKey, nil
}

func generateProofKey() string {
	randomness := getSecureRandom(32)
	return base64.StdEncoding.WithPadding(base64.StdPadding).EncodeToString(randomness)
}

// The response returned from Snowflake looks like so:
// GET /?token=encodedSamlToken
// Host: localhost:54001
// Connection: keep-alive
// Upgrade-Insecure-Requests: 1
// User-Agent: userAgentStr
// Accept: text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,image/apng,*/*;q=0.8
// Referer: https://myaccount.snowflakecomputing.com/fed/login
// Accept-Encoding: gzip, deflate, br
// Accept-Language: en-US,en;q=0.9
// This extracts the token portion of the response.
func getTokenFromResponse(response string) (string, error) {
	start := "GET /?token="
	arr := strings.Split(response, "\r\n")
	if !strings.HasPrefix(arr[0], start) {
		logger.Errorf("response is malformed. ")
		return "", &SnowflakeError{
			Number:      ErrFailedToParseResponse,
			SQLState:    SQLStateConnectionRejected,
			Message:     errors2.ErrMsgFailedToParseResponse,
			MessageArgs: []any{response},
		}
	}
	token := strings.TrimPrefix(arr[0], start)
	token = strings.Split(token, " ")[0]
	return token, nil
}

func getTokenFromPostRequest(request *http.Request) (string, error) {
	if request.Body == nil {
		return "", nil
	}
	body, err := io.ReadAll(request.Body)
	if closeErr := request.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if token := tokenFromJSONCallbackBody(body); token != "" {
		return token, nil
	}
	return tokenFromFormCallbackBody(string(body)), nil
}

func tokenFromJSONCallbackBody(body []byte) string {
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Token
}

func tokenFromFormCallbackBody(body string) string {
	for pair := range strings.SplitSeq(body, "&") {
		key, value, found := strings.Cut(pair, "=")
		if found && key == "token" {
			decoded, err := url.QueryUnescape(value)
			if err != nil {
				return ""
			}
			return decoded
		}
	}
	return ""
}

func externalBrowserOriginPathIsEmpty(path string) bool {
	return path == "" || path == "/"
}

func externalBrowserOriginMatchesAccount(origin string, accountURL *url.URL) bool {
	parsedOrigin, err := url.Parse(origin)
	if err != nil || accountURL == nil ||
		parsedOrigin.User != nil || !externalBrowserOriginPathIsEmpty(parsedOrigin.Path) ||
		parsedOrigin.RawQuery != "" || parsedOrigin.Fragment != "" {
		return false
	}
	return strings.EqualFold(parsedOrigin.Scheme, accountURL.Scheme) &&
		strings.EqualFold(parsedOrigin.Hostname(), accountURL.Hostname()) &&
		effectiveURLPort(parsedOrigin) == effectiveURLPort(accountURL)
}

func effectiveURLPort(parsedURL *url.URL) string {
	if port := parsedURL.Port(); port != "" {
		return port
	}
	switch strings.ToLower(parsedURL.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func validExternalBrowserPreflight(request *http.Request, accountURL *url.URL) bool {
	origins := request.Header.Values("Origin")
	if len(origins) != 1 || !externalBrowserOriginMatchesAccount(origins[0], accountURL) {
		return false
	}
	requestedMethods := request.Header.Values("Access-Control-Request-Method")
	if len(requestedMethods) != 1 || !strings.EqualFold(requestedMethods[0], http.MethodPost) {
		return false
	}
	requestedHeaderValues := request.Header.Values("Access-Control-Request-Headers")
	if len(requestedHeaderValues) == 0 {
		return true
	}
	for _, requestedHeaders := range requestedHeaderValues {
		for header := range strings.SplitSeq(requestedHeaders, ",") {
			if !strings.EqualFold(strings.TrimSpace(header), httpHeaderContentType) {
				return false
			}
		}
	}
	return true
}

func writeExternalBrowserResponse(w http.ResponseWriter, statusCode int, headers http.Header, body string) error {
	maps.Copy(w.Header(), headers)
	w.Header().Set("Connection", "close")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if body != "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	w.WriteHeader(statusCode)
	if _, err := io.WriteString(w, body); err != nil {
		return err
	}
	// The receiver closes the server as soon as a token is published. Flush a
	// complete response first so the browser still receives the success page.
	return http.NewResponseController(w).Flush()
}

func receiveExternalBrowserCallback(ctx context.Context, listener net.Listener, accountURL *url.URL, application string) (string, error) {
	tokens := make(chan string, 1)
	server := &http.Server{
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			// GET callbacks do not use the body. Do not wait for a client to send
			// an unused body before writing its response.
			if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
				logger.Debugf("unable to enable external browser callback response: %v", err)
				return
			}
			respond := func(statusCode int, headers http.Header, body string) {
				if err := writeExternalBrowserResponse(w, statusCode, headers, body); err != nil {
					logger.Debugf("unable to write external browser callback response: %v", err)
				}
			}
			if request.Method == http.MethodOptions {
				headers := make(http.Header)
				statusCode := http.StatusForbidden
				if validExternalBrowserPreflight(request, accountURL) {
					origin := request.Header.Get("Origin")
					headers.Set("Access-Control-Allow-Origin", origin)
					headers.Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
					headers.Set("Access-Control-Allow-Headers", httpHeaderContentType)
					statusCode = http.StatusOK
				}
				respond(statusCode, headers, "")
				return
			}

			origins := request.Header.Values("Origin")
			originPresent := len(origins) != 0
			origin := ""
			if len(origins) == 1 {
				origin = origins[0]
			}
			originMatchesAccount := len(origins) == 1 &&
				!strings.EqualFold(strings.TrimSpace(origin), "null") &&
				externalBrowserOriginMatchesAccount(origin, accountURL)
			rejectOrigin := false
			if request.Method == http.MethodPost {
				rejectOrigin = !originMatchesAccount
			} else {
				rejectOrigin = originPresent && !strings.EqualFold(strings.TrimSpace(origin), "null") && !originMatchesAccount
			}
			if rejectOrigin {
				respond(http.StatusForbidden, nil, "")
				return
			}

			var encodedSamlResponse string
			var err error
			switch request.Method {
			case http.MethodPost:
				encodedSamlResponse, err = getTokenFromPostRequest(request)
			case http.MethodGet:
				if request.URL.Path != "/" || !strings.HasPrefix(request.URL.RawQuery, "token=") {
					respond(http.StatusBadRequest, nil, "")
					return
				}
				encodedSamlResponse, err = getTokenFromResponse(
					request.Method + " " + request.RequestURI + " HTTP/1.1\r\n",
				)
			default:
				respond(http.StatusMethodNotAllowed, nil, "")
				return
			}
			if err != nil || encodedSamlResponse == "" {
				respond(http.StatusBadRequest, nil, "")
				return
			}
			body := fmt.Sprintf(samlSuccessHTML, application)
			headers := make(http.Header)
			if origin != "" && !strings.EqualFold(strings.TrimSpace(origin), "null") {
				headers.Set("Access-Control-Allow-Origin", origin)
				headers.Set("Vary", "Origin")
			}
			respond(http.StatusOK, headers, body)
			select {
			case tokens <- encodedSamlResponse:
			default:
			}
		}),
	}
	serveErrors := make(chan error, 1)
	// Each connection is handled concurrently, so idle preconnects and partial
	// requests cannot block a later authentication callback.
	go func() {
		serveErrors <- server.Serve(listener)
	}()
	defer func() {
		if err := server.Close(); err != nil {
			logger.Warnf("error while closing external browser callback server: %v", err)
		}
	}()

	select {
	case token := <-tokens:
		return token, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case err := <-serveErrors:
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
}

type authenticateByExternalBrowserResult struct {
	escapedSamlResponse []byte
	proofKey            []byte
	err                 error
}

func authenticateByExternalBrowser(ctx context.Context, sr *snowflakeRestful, authenticator string, application string,
	account string, user string, externalBrowserTimeout time.Duration, disableConsoleLogin ConfigBool) ([]byte, []byte, error) {
	authCtx, cancel := context.WithTimeout(ctx, externalBrowserTimeout)
	defer cancel()
	resultChan := make(chan authenticateByExternalBrowserResult, 1)
	go GoroutineWrapper(
		authCtx,
		func() {
			resultChan <- doAuthenticateByExternalBrowser(authCtx, sr, authenticator, application, account, user, disableConsoleLogin)
		},
	)
	select {
	case <-authCtx.Done():
		if errors.Is(authCtx.Err(), context.DeadlineExceeded) {
			return nil, nil, errors.New("authentication timed out")
		}
		return nil, nil, authCtx.Err()
	case result := <-resultChan:
		if errors.Is(result.err, context.DeadlineExceeded) {
			return nil, nil, errors.New("authentication timed out")
		}
		return result.escapedSamlResponse, result.proofKey, result.err
	}
}

// Authentication by an external browser takes place via the following:
//   - the golang snowflake driver communicates to Snowflake that the user wishes to
//     authenticate via external browser
//   - snowflake sends back the IDP Url configured at the Snowflake side for the
//     provided account, or use the multiple SAML way via console login
//   - the default browser is opened to that URL
//   - user authenticates at the IDP, and is redirected to Snowflake
//   - Snowflake directs the user back to the driver
//   - authenticate is complete!
func doAuthenticateByExternalBrowser(ctx context.Context, sr *snowflakeRestful, authenticator string, application string, account string, user string, disableConsoleLogin ConfigBool) authenticateByExternalBrowserResult {
	l, err := createLocalTCPListener(0)
	if err != nil {
		return authenticateByExternalBrowserResult{nil, nil, err}
	}
	defer func() {
		if err = l.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			logger.Errorf("error while closing TCP listener for external browser (%v). %v", l.Addr().String(), err)
		}
	}()

	callbackPort := l.Addr().(*net.TCPAddr).Port

	var loginURL string
	var proofKey string
	if disableConsoleLogin == ConfigBoolTrue {
		// Gets the IDP URL and Proof Key from Snowflake
		loginURL, proofKey, err = getIdpURLProofKey(ctx, sr, authenticator, application, account, user, callbackPort)
	} else {
		// Multiple SAML way to do authentication via console login
		loginURL, proofKey, err = getLoginURL(sr, user, callbackPort)
	}

	if err != nil {
		return authenticateByExternalBrowserResult{nil, nil, err}
	}

	if err = defaultSamlResponseProvider().run(loginURL); err != nil {
		return authenticateByExternalBrowserResult{nil, nil, err}
	}

	encodedSamlResponse, err := receiveExternalBrowserCallback(ctx, l, sr.getURL(), application)
	if err != nil {
		if ctx.Err() == nil {
			logger.WithContext(ctx).Errorf("unable to receive external browser response. err: %v", err)
		}
		return authenticateByExternalBrowserResult{nil, nil, err}
	}

	escapedSamlResponse, err := url.QueryUnescape(encodedSamlResponse)
	if err != nil {
		logger.WithContext(ctx).Errorf("unable to unescape saml response. err: %v", err)
		return authenticateByExternalBrowserResult{nil, nil, err}
	}
	return authenticateByExternalBrowserResult{[]byte(escapedSamlResponse), []byte(proofKey), nil}
}

type samlResponseProvider interface {
	run(url string) error
}

type externalBrowserSamlResponseProvider struct {
}

func (e externalBrowserSamlResponseProvider) run(url string) error {
	return openBrowser(url)
}

var defaultSamlResponseProvider = func() samlResponseProvider {
	return &externalBrowserSamlResponseProvider{}
}
