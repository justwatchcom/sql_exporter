package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const (
	CLOUDSQL_POSTGRES = "cloudsql-postgres"
	CLOUDSQL_MYSQL    = "cloudsql-mysql"
)

func isValidCloudSQLDriver(conn string) (bool, string) {
	switch {
	case strings.HasPrefix(conn, CLOUDSQL_POSTGRES):
		return true, CLOUDSQL_POSTGRES
	case strings.HasPrefix(conn, CLOUDSQL_MYSQL):
		return true, CLOUDSQL_MYSQL
	default:
		return false, ""
	}
}

var cloudSQLHostRegex = regexp.MustCompile(`(.*@)(.*?)(/.*)`)

type CloudSQLUrl struct {
	*url.URL
	Project  string
	Region   string
	Instance string
}

func ParseCloudSQLUrl(u string) (*CloudSQLUrl, error) {
	parts := cloudSQLHostRegex.FindStringSubmatch(u)
	if len(parts) != 4 {
		return nil, fmt.Errorf("did get invalid part count from regex expected 4, got %d. %v", len(parts), parts)
	}

	sanitizedUrl := fmt.Sprintf("%shost%s", parts[1], parts[3])
	urlParsed, err := url.Parse(sanitizedUrl)
	if err != nil {
		return nil, fmt.Errorf("could not parse sanized url %q: %w", sanitizedUrl, err)
	}

	hostParts := strings.Split(parts[2], ":")
	if len(hostParts) != 3 {
		return nil, fmt.Errorf("could not parse cloudsql host. Expected 3 elements, but got %d: %v", len(hostParts), hostParts)
	}
	urlParsed.Host = parts[2]

	cloudSQLUrl := &CloudSQLUrl{
		URL:      urlParsed,
		Project:  hostParts[0],
		Region:   hostParts[1],
		Instance: hostParts[2],
	}
	return cloudSQLUrl, nil
}

func (u *CloudSQLUrl) GetConnectionURL(driver, instance, database string) (string, error) {
	pass, isSet := u.User.Password()
	if !isSet {
		return "", fmt.Errorf("invalid url: cannot find password")
	}

	switch driver {
	case CLOUDSQL_POSTGRES:
		return fmt.Sprintf("host=%s user=%s password=%s dbname=%s sslmode=disable", instance, u.User.Username(), pass, database), nil
	case CLOUDSQL_MYSQL:
		return fmt.Sprintf("%s@cloudsql-mysql(%s)/%s", u.User, instance, database), nil
	}
	return "", fmt.Errorf("driver not supported: %s", driver)
}
