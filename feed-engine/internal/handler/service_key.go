package handler

import (
	"net/http"
	"net/url"
)

// serviceKeyHeader proves a call came from this server. Themis and the transcoder accept
// it and nothing else. It is separate from X-Internal-Key, which on Themis also means
// "may act as anyone": a browser's request through a proxy must never hold that.
const serviceKeyHeader = "X-Service-Key"

// serviceKeyTransport adds the key to calls bound for the named hosts, in one place, so
// a new call site cannot forget it and nothing else ever sees it.
type serviceKeyTransport struct {
	base  http.RoundTripper
	key   string
	hosts map[string]bool
}

func (t serviceKeyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.hosts[req.URL.Host] {
		return t.base.RoundTrip(req)
	}
	signed := req.Clone(req.Context())
	signed.Header.Set(serviceKeyHeader, t.key)
	return t.base.RoundTrip(signed)
}

// keyServiceCalls applies the transport to a client.
func keyServiceCalls(c *http.Client, key string, serviceURLs ...string) {
	if c == nil {
		return
	}
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	hosts := map[string]bool{}
	for _, raw := range serviceURLs {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			hosts[u.Host] = true
		}
	}
	c.Transport = serviceKeyTransport{base: base, key: key, hosts: hosts}
}
