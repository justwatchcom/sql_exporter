package config

import (
	"net/url"
	"strings"

	sferrors "github.com/snowflakedb/gosnowflake/v2/internal/errors"
)

// AuthType indicates the type of authentication in Snowflake
type AuthType int

const (
	// AuthTypeSnowflake is the general username password authentication
	AuthTypeSnowflake AuthType = iota
	// AuthTypeOAuth is the OAuth authentication
	AuthTypeOAuth
	// AuthTypeExternalBrowser is to use a browser to access an Fed and perform SSO authentication
	AuthTypeExternalBrowser
	// AuthTypeOkta is to use a native okta URL to perform SSO authentication on Okta
	AuthTypeOkta
	// AuthTypeJwt is to use Jwt to perform authentication
	AuthTypeJwt
	// AuthTypeTokenAccessor is to use the provided token accessor and bypass authentication
	AuthTypeTokenAccessor
	// AuthTypeUsernamePasswordMFA is to use username and password with mfa
	AuthTypeUsernamePasswordMFA
	// AuthTypePat is to use programmatic access token
	AuthTypePat
	// AuthTypeOAuthAuthorizationCode is to use browser-based OAuth2 flow
	AuthTypeOAuthAuthorizationCode
	// AuthTypeOAuthClientCredentials is to use non-interactive OAuth2 flow
	AuthTypeOAuthClientCredentials
	// AuthTypeWorkloadIdentityFederation is to use CSP identity for authentication
	AuthTypeWorkloadIdentityFederation
)

func (authType AuthType) String() string {
	switch authType {
	case AuthTypeSnowflake:
		return "SNOWFLAKE"
	case AuthTypeOAuth:
		return "OAUTH"
	case AuthTypeExternalBrowser:
		return "EXTERNALBROWSER"
	case AuthTypeOkta:
		return "OKTA"
	case AuthTypeJwt:
		return "SNOWFLAKE_JWT"
	case AuthTypeTokenAccessor:
		return "TOKENACCESSOR"
	case AuthTypeUsernamePasswordMFA:
		return "USERNAME_PASSWORD_MFA"
	case AuthTypePat:
		return "PROGRAMMATIC_ACCESS_TOKEN"
	case AuthTypeOAuthAuthorizationCode:
		return "OAUTH_AUTHORIZATION_CODE"
	case AuthTypeOAuthClientCredentials:
		return "OAUTH_CLIENT_CREDENTIALS"
	case AuthTypeWorkloadIdentityFederation:
		return "WORKLOAD_IDENTITY"
	default:
		return "UNKNOWN"
	}
}

// DetermineAuthenticatorType parses the authenticator string and sets the Config.Authenticator field.
func DetermineAuthenticatorType(cfg *Config, value string) error {
	upperCaseValue := strings.ToUpper(value)
	lowerCaseValue := strings.ToLower(value)
	if strings.Trim(value, " ") == "" || upperCaseValue == AuthTypeSnowflake.String() {
		cfg.Authenticator = AuthTypeSnowflake
		return nil
	} else if upperCaseValue == AuthTypeOAuth.String() {
		cfg.Authenticator = AuthTypeOAuth
		return nil
	} else if upperCaseValue == AuthTypeJwt.String() {
		cfg.Authenticator = AuthTypeJwt
		return nil
	} else if upperCaseValue == AuthTypeExternalBrowser.String() {
		cfg.Authenticator = AuthTypeExternalBrowser
		return nil
	} else if upperCaseValue == AuthTypeUsernamePasswordMFA.String() {
		cfg.Authenticator = AuthTypeUsernamePasswordMFA
		return nil
	} else if upperCaseValue == AuthTypeTokenAccessor.String() {
		cfg.Authenticator = AuthTypeTokenAccessor
		return nil
	} else if upperCaseValue == AuthTypePat.String() {
		cfg.Authenticator = AuthTypePat
		return nil
	} else if upperCaseValue == AuthTypeOAuthAuthorizationCode.String() {
		cfg.Authenticator = AuthTypeOAuthAuthorizationCode
		return nil
	} else if upperCaseValue == AuthTypeOAuthClientCredentials.String() {
		cfg.Authenticator = AuthTypeOAuthClientCredentials
		return nil
	} else if upperCaseValue == AuthTypeWorkloadIdentityFederation.String() {
		cfg.Authenticator = AuthTypeWorkloadIdentityFederation
		return nil
	} else {
		// possibly Okta case
		oktaURLString, err := url.QueryUnescape(lowerCaseValue)
		if err != nil {
			return &sferrors.SnowflakeError{
				Number:      sferrors.ErrCodeFailedToParseAuthenticator,
				Message:     sferrors.ErrMsgFailedToParseAuthenticator,
				MessageArgs: []any{lowerCaseValue},
			}
		}

		oktaURL, err := url.Parse(oktaURLString)
		if err != nil {
			return &sferrors.SnowflakeError{
				Number:      sferrors.ErrCodeFailedToParseAuthenticator,
				Message:     sferrors.ErrMsgFailedToParseAuthenticator,
				MessageArgs: []any{oktaURLString},
			}
		}

		if oktaURL.Scheme != "https" {
			return &sferrors.SnowflakeError{
				Number:      sferrors.ErrCodeFailedToParseAuthenticator,
				Message:     sferrors.ErrMsgFailedToParseAuthenticator,
				MessageArgs: []any{oktaURLString},
			}
		}
		cfg.OktaURL = oktaURL
		cfg.Authenticator = AuthTypeOkta
	}
	return nil
}
