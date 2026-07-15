package origin

import (
	"errors"
	"net/http"
)

// Auth contains credentials for the WebDAV origin. Basic and Bearer
// authentication are mutually exclusive.
type Auth struct {
	User        string
	Password    string
	BearerToken string
}

// Validate checks that credentials form one supported authentication mode.
func (a Auth) Validate() error {
	if (a.User == "") != (a.Password == "") {
		return errors.New("WebDAV user and password must be set together")
	}
	if a.BearerToken != "" && a.User != "" {
		return errors.New("WebDAV Basic and Bearer authentication are mutually exclusive")
	}
	return nil
}

func (a Auth) apply(req *http.Request) {
	if a.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.BearerToken)
	} else if a.User != "" {
		req.SetBasicAuth(a.User, a.Password)
	}
}
