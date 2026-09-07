// session manages multi-identity test contexts for the Walker.
// Each identity simulates a different user tier: guest, user, creator, admin.
package session

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"time"

	"golang.org/x/net/publicsuffix"
)

type Identity struct {
	Name   string
	Handle string
	Role   string
	Client *http.Client
	Cookie string // f33d3r_handle value
}

// All returns all test identities Walker should simulate.
func All(baseURL string) []*Identity {
	ids := []*Identity{
		{Name: "guest",   Handle: "",        Role: "none"},
		{Name: "user",    Handle: "edd",     Role: "founder"},
		{Name: "creator", Handle: "creator", Role: "creator"},
		{Name: "admin",   Handle: "admin",   Role: "admin"},
	}
	for _, id := range ids {
		jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
		id.Client = &http.Client{
			Timeout: 8 * time.Second,
			Jar:     jar,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		}
		id.Cookie = id.Handle
	}
	return ids
}

func (id *Identity) NewRequest(method, url string) (*http.Request, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil { return nil, err }
	if id.Cookie != "" {
		req.AddCookie(&http.Cookie{Name: "f33d3r_handle", Value: id.Cookie})
	}
	req.Header.Set("User-Agent", "AethyrWalker/1.0 (+E2E)")
	return req, nil
}
