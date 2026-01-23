package auth

import (
	"net/http"
	"strings"
)

type Authenticator interface {
	Authenticate(*http.Request) error
}

type AuthType int

const (
	AuthTypeUnknown AuthType = iota
	AuthTypePat
	AuthTypeOauthU2M
	AuthTypeOauthM2M
)

var authTypeNames []string = []string{"Unknown", "Pat", "OauthU2M", "OauthM2M"}

func (at AuthType) String() string {
	if at >= 0 && int(at) < len(authTypeNames) {
		return authTypeNames[at]
	}

	return authTypeNames[0]
}

func ParseAuthType(typeString string) AuthType {
	typeString = strings.ToLower(typeString)
	for i, n := range authTypeNames {
		if strings.ToLower(n) == typeString {
			return AuthType(i)
		}
	}

	return AuthTypeUnknown
}
