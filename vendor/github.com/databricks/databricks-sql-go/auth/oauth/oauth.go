package oauth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

var azureTenants = map[string]string{
	".dev.azuredatabricks.net":     "62a912ac-b58e-4c1d-89ea-b2dbfc7358fc",
	".staging.azuredatabricks.net": "4a67d088-db5c-48f1-9ff2-0aace800ae68",
	".azuredatabricks.net":         "2ff814a6-3304-4ab8-85cb-cd0e6f879c1d",
	".databricks.azure.us":         "2ff814a6-3304-4ab8-85cb-cd0e6f879c1d",
	".databricks.azure.cn":         "2ff814a6-3304-4ab8-85cb-cd0e6f879c1d",
}

func GetEndpoint(ctx context.Context, hostName string) (oauth2.Endpoint, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	cloud := InferCloudFromHost(hostName)

	if cloud == Unknown {
		return oauth2.Endpoint{}, errors.New("unsupported cloud type")
	}

	if cloud == Azure {
		authURL := fmt.Sprintf("https://%s/oidc/oauth2/v2.0/authorize", hostName)
		tokenURL := fmt.Sprintf("https://%s/oidc/oauth2/v2.0/token", hostName)
		return oauth2.Endpoint{AuthURL: authURL, TokenURL: tokenURL}, nil
	}

	issuerURL := fmt.Sprintf("https://%s/oidc", hostName)
	ctx = oidc.InsecureIssuerURLContext(ctx, issuerURL)
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return oauth2.Endpoint{}, err
	}

	endpoint := provider.Endpoint()

	return endpoint, err
}

func GetScopes(hostName string, scopes []string) []string {
	for _, s := range []string{oidc.ScopeOfflineAccess} {
		if !HasScope(scopes, s) {
			scopes = append(scopes, s)
		}
	}

	cloudType := InferCloudFromHost(hostName)
	if cloudType == Azure {
		userImpersonationScope := fmt.Sprintf("%s/user_impersonation", azureTenants[GetAzureDnsZone(hostName)])
		if !HasScope(scopes, userImpersonationScope) {
			scopes = append(scopes, userImpersonationScope)
		}
	} else {
		if !HasScope(scopes, "sql") {
			scopes = append(scopes, "sql")
		}
	}

	return scopes
}

func HasScope(scopes []string, scope string) bool {
	for _, s := range scopes {
		if s == scope {
			return true
		}
	}
	return false
}

var databricksAWSDomains []string = []string{
	".cloud.databricks.com",
	".cloud.databricks.us",
	".dev.databricks.com",
}

var databricksAzureDomains []string = []string{
	".azuredatabricks.net",
	".databricks.azure.cn",
	".databricks.azure.us",
}

var databricksGCPDomains []string = []string{
	".gcp.databricks.com",
}

type CloudType int

const (
	AWS = iota
	Azure
	GCP
	Unknown
)

func (cl CloudType) String() string {
	switch cl {
	case AWS:
		return "AWS"
	case Azure:
		return "Azure"
	case GCP:
		return "GCP"
	}

	return "Unknown"
}

func InferCloudFromHost(hostname string) CloudType {

	for _, d := range databricksAzureDomains {
		if strings.Contains(hostname, d) {
			return Azure
		}
	}

	for _, d := range databricksAWSDomains {
		if strings.Contains(hostname, d) {
			return AWS
		}
	}

	for _, d := range databricksGCPDomains {
		if strings.Contains(hostname, d) {
			return GCP
		}
	}
	return Unknown
}

func GetAzureDnsZone(hostname string) string {
	for _, d := range databricksAzureDomains {
		if strings.Contains(hostname, d) {
			return d
		}
	}
	return ""
}
