package u2m

import (
	"context"
	"fmt"
	"strings"

	"github.com/databricks/databricks-sql-go/auth/oauth"
	"github.com/databricks/databricks-sql-go/auth/oauth/pkce"
	"golang.org/x/oauth2"
)

func GetConfig(ctx context.Context, hostName, clientID, clientSecret, callbackURL string, scopes []string) (oauth2.Config, error) {
	// Add necessary scopes for AWS or Azure
	scopes = oauth.GetScopes(hostName, scopes)

	// Get the endpoint based on the host name
	endpoint, err := oauth.GetEndpoint(ctx, hostName)
	if err != nil {
		return oauth2.Config{}, fmt.Errorf("could not lookup provider details: %w", err)
	}

	if !strings.HasPrefix(callbackURL, "http") {
		callbackURL = fmt.Sprintf("http://%s", callbackURL)
	}

	config := oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     endpoint,
		RedirectURL:  callbackURL,
		Scopes:       scopes,
	}

	return config, nil
}

func GetAuthCodeOptions() (challenge, challengeMethod, verifier oauth2.AuthCodeOption, err error) {
	code, err := pkce.Generate()
	if err != nil {
		return
	}

	return code.Challenge(), code.Method(), code.Verifier(), err
}

func GetLoginURL(config oauth2.Config, state string, options ...oauth2.AuthCodeOption) string {
	loginURL := config.AuthCodeURL(state, options...)

	return loginURL
}
