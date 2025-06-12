package gosnowflake

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	oauthSuccessHTML = `<!DOCTYPE html><html><head><meta charset="UTF-8"/>
<title>OAuth for Snowflake</title></head>
<body>
OAuth authentication completed successfully.
</body></html>`
)

var defaultAuthorizationCodeProviderFactory = func() authorizationCodeProvider {
	return &browserBasedAuthorizationCodeProvider{}
}

type oauthClient struct {
	ctx    context.Context
	cfg    *Config
	client *http.Client

	port                int
	redirectURITemplate string

	authorizationCodeProviderFactory func() authorizationCodeProvider
}

func newOauthClient(ctx context.Context, cfg *Config) (*oauthClient, error) {
	port := 0
	if cfg.OauthRedirectURI != "" {
		logger.Debugf("Using oauthRedirectUri from config: %v", cfg.OauthRedirectURI)
		uri, err := url.Parse(cfg.OauthRedirectURI)
		if err != nil {
			return nil, err
		}
		portStr := uri.Port()
		if portStr != "" {
			if port, err = strconv.Atoi(portStr); err != nil {
				return nil, err
			}
		}
	}

	redirectURITemplate := ""
	if cfg.OauthRedirectURI == "" {
		redirectURITemplate = "http://127.0.0.1:%v/"
	}
	logger.Debugf("Redirect URI template: %v, port: %v", redirectURITemplate, port)

	client := &http.Client{
		Transport: getTransport(cfg),
	}
	return &oauthClient{
		ctx:                              context.WithValue(ctx, oauth2.HTTPClient, client),
		cfg:                              cfg,
		client:                           client,
		port:                             port,
		redirectURITemplate:              redirectURITemplate,
		authorizationCodeProviderFactory: defaultAuthorizationCodeProviderFactory,
	}, nil
}

type oauthBrowserResult struct {
	accessToken  string
	refreshToken string
	err          error
}

func (oauthClient *oauthClient) authenticateByOAuthAuthorizationCode() (string, error) {
	accessTokenSpec := oauthClient.accessTokenSpec()
	if oauthClient.cfg.ClientStoreTemporaryCredential == ConfigBoolTrue {
		if accessToken := credentialsStorage.getCredential(accessTokenSpec); accessToken != "" {
			logger.Debugf("Access token retrieved from cache")
			return accessToken, nil
		}
		if refreshToken := credentialsStorage.getCredential(oauthClient.refreshTokenSpec()); refreshToken != "" {
			return "", &SnowflakeError{Number: ErrMissingAccessATokenButRefreshTokenPresent}
		}
	}
	logger.Debugf("Access token not present in cache, running full auth code flow")

	resultChan := make(chan oauthBrowserResult, 1)
	tcpListener, callbackPort, err := oauthClient.setupListener()
	if err != nil {
		return "", err
	}
	defer func() {
		logger.Debug("Closing tcp listener")
		if err := tcpListener.Close(); err != nil {
			logger.Warnf("error while closing TCP listener. %v", err)
		}
	}()
	go GoroutineWrapper(oauthClient.ctx, func() {
		resultChan <- oauthClient.doAuthenticateByOAuthAuthorizationCode(tcpListener, callbackPort)
	})
	select {
	case <-time.After(oauthClient.cfg.ExternalBrowserTimeout):
		return "", errors.New("authentication via browser timed out")
	case result := <-resultChan:
		if oauthClient.cfg.ClientStoreTemporaryCredential == ConfigBoolTrue {
			logger.Debug("saving oauth access token in cache")
			credentialsStorage.setCredential(oauthClient.accessTokenSpec(), result.accessToken)
			credentialsStorage.setCredential(oauthClient.refreshTokenSpec(), result.refreshToken)
		}
		return result.accessToken, result.err
	}
}

func (oauthClient *oauthClient) doAuthenticateByOAuthAuthorizationCode(tcpListener *net.TCPListener, callbackPort int) oauthBrowserResult {
	authCodeProvider := oauthClient.authorizationCodeProviderFactory()

	successChan := make(chan []byte)
	errChan := make(chan error)
	responseBodyChan := make(chan string, 2)
	closeListenerChan := make(chan bool, 2)

	defer func() {
		closeListenerChan <- true
		close(successChan)
		close(errChan)
		close(responseBodyChan)
		close(closeListenerChan)
	}()

	logger.Debugf("opening socket on port %v", callbackPort)
	defer func(tcpListener *net.TCPListener) {
		<-closeListenerChan
	}(tcpListener)

	go handleOAuthSocket(tcpListener, successChan, errChan, responseBodyChan, closeListenerChan)

	oauth2cfg := oauthClient.buildAuthorizationCodeConfig(callbackPort)
	codeVerifier := authCodeProvider.createCodeVerifier()
	state := authCodeProvider.createState()
	authorizationURL := oauth2cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(codeVerifier))
	if err := authCodeProvider.run(authorizationURL); err != nil {
		responseBodyChan <- err.Error()
		closeListenerChan <- true
		return oauthBrowserResult{"", "", err}
	}

	err := <-errChan
	if err != nil {
		responseBodyChan <- err.Error()
		return oauthBrowserResult{"", "", err}
	}
	codeReqBytes := <-successChan

	codeReq, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(codeReqBytes)))
	if err != nil {
		responseBodyChan <- err.Error()
		return oauthBrowserResult{"", "", err}
	}
	logger.Debugf("Received authorization code from %v", oauthClient.authorizationURL())
	tokenResponse, err := oauthClient.exchangeAccessToken(codeReq, state, oauth2cfg, codeVerifier, responseBodyChan)
	if err != nil {
		return oauthBrowserResult{"", "", err}
	}
	logger.Debugf("Received token from %v", oauthClient.tokenURL())
	return oauthBrowserResult{tokenResponse.AccessToken, tokenResponse.RefreshToken, err}
}

func (oauthClient *oauthClient) setupListener() (*net.TCPListener, int, error) {
	tcpListener, err := createLocalTCPListener(oauthClient.port)
	if err != nil {
		return nil, 0, err
	}
	callbackPort := tcpListener.Addr().(*net.TCPAddr).Port
	logger.Debugf("oauthClient.port: %v, callbackPort: %v", oauthClient.port, callbackPort)
	return tcpListener, callbackPort, nil
}

func (oauthClient *oauthClient) exchangeAccessToken(codeReq *http.Request, state string, oauth2cfg *oauth2.Config, codeVerifier string, responseBodyChan chan string) (*oauth2.Token, error) {
	queryParams := codeReq.URL.Query()
	errorMsg := queryParams.Get("error")
	if errorMsg != "" {
		errorDesc := queryParams.Get("error_description")
		errMsg := fmt.Sprintf("error while getting authentication from oauth: %v. Details: %v", errorMsg, errorDesc)
		responseBodyChan <- html.EscapeString(errMsg)
		return nil, errors.New(errMsg)
	}

	receivedState := queryParams.Get("state")
	if state != receivedState {
		errMsg := "invalid oauth state received"
		responseBodyChan <- errMsg
		return nil, errors.New(errMsg)
	}

	code := queryParams.Get("code")
	token, err := oauth2cfg.Exchange(oauthClient.ctx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		responseBodyChan <- err.Error()
		return nil, err
	}
	responseBodyChan <- oauthSuccessHTML
	return token, nil
}

func (oauthClient *oauthClient) buildAuthorizationCodeConfig(callbackPort int) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     oauthClient.cfg.OauthClientID,
		ClientSecret: oauthClient.cfg.OauthClientSecret,
		RedirectURL:  oauthClient.buildRedirectURI(callbackPort),
		Scopes:       oauthClient.buildScopes(),
		Endpoint: oauth2.Endpoint{
			AuthURL:   oauthClient.authorizationURL(),
			TokenURL:  oauthClient.tokenURL(),
			AuthStyle: oauth2.AuthStyleInHeader,
		},
	}
}

func (oauthClient *oauthClient) authorizationURL() string {
	return cmp.Or(oauthClient.cfg.OauthAuthorizationURL, oauthClient.defaultAuthorizationURL())
}

func (oauthClient *oauthClient) defaultAuthorizationURL() string {
	return fmt.Sprintf("%v://%v:%v/oauth/authorize", oauthClient.cfg.Protocol, oauthClient.cfg.Host, oauthClient.cfg.Port)
}

func (oauthClient *oauthClient) tokenURL() string {
	return cmp.Or(oauthClient.cfg.OauthTokenRequestURL, oauthClient.defaultTokenURL())
}

func (oauthClient *oauthClient) defaultTokenURL() string {
	return fmt.Sprintf("%v://%v:%v/oauth/token-request", oauthClient.cfg.Protocol, oauthClient.cfg.Host, oauthClient.cfg.Port)
}

func (oauthClient *oauthClient) buildRedirectURI(port int) string {
	if oauthClient.cfg.OauthRedirectURI != "" {
		return oauthClient.cfg.OauthRedirectURI
	}
	return fmt.Sprintf(oauthClient.redirectURITemplate, port)
}

func (oauthClient *oauthClient) buildScopes() []string {
	if oauthClient.cfg.OauthScope == "" {
		return []string{"session:role:" + oauthClient.cfg.Role}
	}
	scopes := strings.Split(oauthClient.cfg.OauthScope, " ")
	for i, scope := range scopes {
		scopes[i] = strings.TrimSpace(scope)
	}
	return scopes
}

func handleOAuthSocket(tcpListener *net.TCPListener, successChan chan []byte, errChan chan error, responseBodyChan chan string, closeListenerChan chan bool) {
	conn, err := tcpListener.AcceptTCP()
	if err != nil {
		logger.Warnf("error creating socket. %v", err)
		return
	}
	defer conn.Close()
	var buf [bufSize]byte
	codeResp := bytes.NewBuffer(nil)
	for {
		readBytes, err := conn.Read(buf[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			errChan <- err
			return
		}
		codeResp.Write(buf[0:readBytes])
		if readBytes < bufSize {
			break
		}
	}

	errChan <- nil
	successChan <- codeResp.Bytes()

	responseBody := <-responseBodyChan
	respToBrowser, err := buildResponse(responseBody)
	if err != nil {
		logger.Warnf("cannot create response to browser. %v", err)
	}
	_, err = conn.Write(respToBrowser.Bytes())
	if err != nil {
		logger.Warnf("cannot write response to browser. %v", err)
	}
	closeListenerChan <- true
}

type authorizationCodeProvider interface {
	run(authorizationURL string) error
	createState() string
	createCodeVerifier() string
}

type browserBasedAuthorizationCodeProvider struct {
}

func (provider *browserBasedAuthorizationCodeProvider) run(authorizationURL string) error {
	return openBrowser(authorizationURL)
}

func (provider *browserBasedAuthorizationCodeProvider) createState() string {
	return NewUUID().String()
}

func (provider *browserBasedAuthorizationCodeProvider) createCodeVerifier() string {
	return oauth2.GenerateVerifier()
}

func (oauthClient *oauthClient) authenticateByOAuthClientCredentials() (string, error) {
	accessTokenSpec := oauthClient.accessTokenSpec()
	if oauthClient.cfg.ClientStoreTemporaryCredential == ConfigBoolTrue {
		if accessToken := credentialsStorage.getCredential(accessTokenSpec); accessToken != "" {
			return accessToken, nil
		}
	}
	oauth2Cfg, err := oauthClient.buildClientCredentialsConfig()
	if err != nil {
		return "", err
	}
	token, err := oauth2Cfg.Token(oauthClient.ctx)
	if err != nil {
		return "", err
	}
	if oauthClient.cfg.ClientStoreTemporaryCredential == ConfigBoolTrue {
		credentialsStorage.setCredential(accessTokenSpec, token.AccessToken)
	}
	return token.AccessToken, nil
}

func (oauthClient *oauthClient) buildClientCredentialsConfig() (*clientcredentials.Config, error) {
	if oauthClient.cfg.OauthTokenRequestURL == "" {
		return nil, errors.New("client credentials flow requires tokenRequestURL")
	}
	return &clientcredentials.Config{
		ClientID:     oauthClient.cfg.OauthClientID,
		ClientSecret: oauthClient.cfg.OauthClientSecret,
		TokenURL:     oauthClient.cfg.OauthTokenRequestURL,
		Scopes:       oauthClient.buildScopes(),
	}, nil
}

func (oauthClient *oauthClient) refreshToken() error {
	if oauthClient.cfg.ClientStoreTemporaryCredential != ConfigBoolTrue {
		logger.Debug("credentials storage is disabled, cannot use refresh tokens")
		return nil
	}
	refreshTokenSpec := newOAuthRefreshTokenSpec(oauthClient.cfg.OauthTokenRequestURL, oauthClient.cfg.User)
	refreshToken := credentialsStorage.getCredential(refreshTokenSpec)
	if refreshToken == "" {
		logger.Debug("no refresh token in cache, full flow must be run")
		return nil
	}
	body := url.Values{}
	body.Add("grant_type", "refresh_token")
	body.Add("refresh_token", refreshToken)
	body.Add("scope", strings.Join(oauthClient.buildScopes(), " "))
	req, err := http.NewRequest("POST", oauthClient.tokenURL(), strings.NewReader(body.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(oauthClient.cfg.OauthClientID, oauthClient.cfg.OauthClientSecret)
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	resp, err := oauthClient.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		credentialsStorage.deleteCredential(refreshTokenSpec)
		return errors.New(string(respBody))
	}
	var tokenResponse tokenExchangeResponseBody
	if err = json.NewDecoder(resp.Body).Decode(&tokenResponse); err != nil {
		return err
	}
	accessTokenSpec := oauthClient.accessTokenSpec()
	credentialsStorage.setCredential(accessTokenSpec, tokenResponse.AccessToken)
	if tokenResponse.RefreshToken != "" {
		credentialsStorage.setCredential(refreshTokenSpec, tokenResponse.RefreshToken)
	}
	return nil
}

type tokenExchangeResponseBody struct {
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token"`
}

func (oauthClient *oauthClient) accessTokenSpec() *secureTokenSpec {
	return newOAuthAccessTokenSpec(oauthClient.tokenURL(), oauthClient.cfg.User)
}

func (oauthClient *oauthClient) refreshTokenSpec() *secureTokenSpec {
	return newOAuthRefreshTokenSpec(oauthClient.tokenURL(), oauthClient.cfg.User)
}
