package secrets

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// LoginPrefix starts the name of a site login among a unit's secrets:
// login-<name>, whose value is a Login as JSON. The deployer gives them to
// the gateway, whose browser types them into the site's own pages for the
// agents a unit's logins.yaml names; the agents never see a password.
const LoginPrefix = "login-"

// LoginName is what a login may be called: a secret's name, with room for
// the prefix.
var LoginName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,24}[a-z0-9])?$`)

// Login is an account on a site.
type Login struct {
	Site     string `json:"site"` // https://shop.example.com: the pages it may be typed into
	Username string `json:"username"`
	Password string `json:"password"`
}

// Host is the site's host, lower case: the login is typed only into pages
// there or below.
func (l Login) Host() string {
	u, err := url.Parse(l.Site)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// For tells whether a page at u is the login's site: its host, or a name
// below it, over https (http only if the site says http).
func (l Login) For(u string) bool {
	p, err := url.Parse(u)
	if err != nil || l.Host() == "" {
		return false
	}
	site, _ := url.Parse(l.Site)
	if p.Scheme != "https" && p.Scheme != site.Scheme {
		return false
	}
	h := strings.ToLower(p.Hostname())
	return h == l.Host() || strings.HasSuffix(h, "."+l.Host())
}

// NewLogin checks an account and gives it as a secret's value.
func NewLogin(site, username, password string) (string, error) {
	site = strings.TrimSpace(site)
	if !strings.Contains(site, "://") {
		site = "https://" + site
	}
	u, err := url.Parse(site)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return "", fmt.Errorf("%w: the site is an address such as https://shop.example.com", ErrInvalid)
	}
	if strings.TrimSpace(username) == "" || password == "" {
		return "", fmt.Errorf("%w: a login needs a username and a password", ErrInvalid)
	}
	l := Login{Site: u.Scheme + "://" + strings.ToLower(u.Host), Username: strings.TrimSpace(username), Password: password}
	data, _ := json.Marshal(l)
	return string(data), nil
}

// ParseLogin reads a login's value.
func ParseLogin(value string) (Login, error) {
	var l Login
	if err := json.Unmarshal([]byte(value), &l); err != nil || l.Host() == "" || l.Username == "" || l.Password == "" {
		return Login{}, fmt.Errorf("not a login: it needs site, username and password")
	}
	return l, nil
}
