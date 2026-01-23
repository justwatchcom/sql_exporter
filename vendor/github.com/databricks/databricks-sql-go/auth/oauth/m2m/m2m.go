package m2m

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/databricks/databricks-sql-go/auth"
	"github.com/databricks/databricks-sql-go/auth/oauth"
	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

func NewAuthenticator(clientID, clientSecret, hostName string) auth.Authenticator {
	return NewAuthenticatorWithScopes(clientID, clientSecret, hostName, []string{})
}

func NewAuthenticatorWithScopes(clientID, clientSecret, hostName string, scopes []string) auth.Authenticator {
	scopes = GetScopes(hostName, scopes)
	return &authClient{
		clientID:     clientID,
		clientSecret: clientSecret,
		hostName:     hostName,
		scopes:       scopes,
	}
}

type authClient struct {
	clientID     string
	clientSecret string
	hostName     string
	scopes       []string
	tokenSource  oauth2.TokenSource
	mx           sync.Mutex
}

// Auth will start the OAuth Authorization Flow to authenticate the cli client
// using the users credentials in the browser. Compatible with SSO.
func (c *authClient) Authenticate(r *http.Request) error {
	c.mx.Lock()
	defer c.mx.Unlock()
	if c.tokenSource != nil {
		token, err := c.tokenSource.Token()
		if err != nil {
			return err
		}
		token.SetAuthHeader(r)
		return nil
	}

	config, err := GetConfig(context.Background(), c.hostName, c.clientID, c.clientSecret, c.scopes)
	if err != nil {
		return fmt.Errorf("unable to generate clientCredentials.Config: %w", err)
	}

	c.tokenSource = GetTokenSource(config)
	token, err := c.tokenSource.Token()
	log.Info().Msgf("token fetched successfully")
	if err != nil {
		log.Err(err).Msg("failed to get token")

		return err
	}
	token.SetAuthHeader(r)

	return nil

}

func GetTokenSource(config clientcredentials.Config) oauth2.TokenSource {
	tokenSource := config.TokenSource(context.Background())
	return tokenSource
}

func GetConfig(ctx context.Context, issuerURL, clientID, clientSecret string, scopes []string) (clientcredentials.Config, error) {
	// Get the endpoint based on the host name
	endpoint, err := oauth.GetEndpoint(ctx, issuerURL)
	if err != nil {
		return clientcredentials.Config{}, fmt.Errorf("could not lookup provider details: %w", err)
	}

	config := clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     endpoint.TokenURL,
		Scopes:       scopes,
	}

	return config, nil
}

func GetScopes(hostName string, scopes []string) []string {
	if !oauth.HasScope(scopes, "all-apis") {
		scopes = append(scopes, "all-apis")
	}

	return scopes
}
