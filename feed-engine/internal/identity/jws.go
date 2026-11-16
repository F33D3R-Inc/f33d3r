package identity

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The PIAL access token.
//
// elohim-veni signs a compact JWS for every resolved session: alg EdDSA (an
// Ed25519 signature), typ PIAL, a kid naming the signing key, and the claims
// below, valid for 300 seconds. feed-engine verifies it against the
// authority's published JWKS and forwards it to other brains as
//
//	Authorization: PIAL <jws>
//
// so a downstream brain learns who is asking from a signature it can check,
// not from a header feed-engine merely asserts.
//
// The verifier is strict by construction. It accepts exactly one algorithm and
// one type, so there is no "alg":"none", no HMAC-with-the-public-key confusion,
// and no token minted for another purpose that happens to verify. It caches the
// key set and refreshes it only when a token names a kid it has not seen — the
// rotation case — and never more often than jwksMinRefresh, so a stream of
// forged kids cannot turn into a stream of requests at the authority.

// Token constants.
const (
	TokenAlg    = "EdDSA"
	TokenType   = "PIAL"
	TokenIssuer = "elohim-veni"
)

// clockSkew is how far a token's timestamps may disagree with this host's
// clock. Small and fixed: the token lives five minutes.
const clockSkew = 30 * time.Second

// jwksMinRefresh bounds how often an unknown kid may cause a key-set fetch.
const jwksMinRefresh = 30 * time.Second

// Claims are the verified contents of an access token.
type Claims struct {
	Issuer    string   `json:"iss"`
	PIALID    string   `json:"sub"`
	AccountID string   `json:"acc"`
	Handle    string   `json:"hnd"`
	Role      string   `json:"rol"`
	Caps      []string `json:"cap"`
	SessionID string   `json:"sid"`
	IssuedAt  int64    `json:"iat"`
	ExpiresAt int64    `json:"exp"`
}

// Verification failures. Each is a reason to refuse the token, and all of them
// are refusals: a token that fails here proves nothing about anyone.
var (
	ErrTokenMalformed = errors.New("identity: access token malformed")
	ErrTokenAlg       = errors.New("identity: access token algorithm is not EdDSA")
	ErrTokenType      = errors.New("identity: access token type is not PIAL")
	ErrTokenKID       = errors.New("identity: access token names an unknown key")
	ErrTokenSignature = errors.New("identity: access token signature does not verify")
	ErrTokenIssuer    = errors.New("identity: access token was not issued by elohim-veni")
	ErrTokenExpired   = errors.New("identity: access token expired")
	ErrTokenNotYet    = errors.New("identity: access token issued in the future")
	ErrTokenSubject   = errors.New("identity: access token names no identity or account")
)

// Verifier checks access tokens against elohim-veni's key set.
type Verifier struct {
	client *Client
	now    func() time.Time

	mu          sync.RWMutex
	keys        map[string]ed25519.PublicKey
	lastRefresh time.Time
	refreshing  sync.Mutex // one key-set fetch at a time
}

// NewVerifier returns a verifier that fetches keys through client.
func NewVerifier(client *Client) *Verifier {
	return &Verifier{client: client, now: time.Now, keys: map[string]ed25519.PublicKey{}}
}

type tokenHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// Verify checks a compact JWS and returns its claims.
func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, ErrTokenMalformed
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrTokenMalformed, err)
	}
	var hdr tokenHeader
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrTokenMalformed, err)
	}
	if hdr.Alg != TokenAlg {
		return nil, ErrTokenAlg
	}
	if hdr.Typ != TokenType {
		return nil, ErrTokenType
	}
	if hdr.Kid == "" {
		return nil, ErrTokenKID
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: signature encoding", ErrTokenMalformed)
	}

	key, err := v.key(ctx, hdr.Kid)
	if err != nil {
		return nil, err
	}
	signingInput := parts[0] + "." + parts[1]
	if !ed25519.Verify(key, []byte(signingInput), sig) {
		return nil, ErrTokenSignature
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload: %v", ErrTokenMalformed, err)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("%w: payload: %v", ErrTokenMalformed, err)
	}
	if c.Issuer != TokenIssuer {
		return nil, ErrTokenIssuer
	}
	now := v.now()
	if c.ExpiresAt == 0 || now.After(time.Unix(c.ExpiresAt, 0).Add(clockSkew)) {
		return nil, ErrTokenExpired
	}
	if c.IssuedAt != 0 && time.Unix(c.IssuedAt, 0).After(now.Add(clockSkew)) {
		return nil, ErrTokenNotYet
	}
	if c.PIALID == "" || c.AccountID == "" {
		return nil, ErrTokenSubject
	}
	return &c, nil
}

// key returns the public key a kid names, refreshing the key set once when the
// kid is new. A kid still unknown after a refresh is refused.
func (v *Verifier) key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	v.mu.RLock()
	k, ok := v.keys[kid]
	v.mu.RUnlock()
	if ok {
		return k, nil
	}

	v.refreshing.Lock()
	defer v.refreshing.Unlock()
	// Another caller may have refreshed while this one waited.
	v.mu.RLock()
	k, ok = v.keys[kid]
	recent := !v.lastRefresh.IsZero() && v.now().Sub(v.lastRefresh) < jwksMinRefresh
	v.mu.RUnlock()
	if ok {
		return k, nil
	}
	if recent {
		return nil, ErrTokenKID
	}
	keys, err := v.client.fetchJWKS(ctx)
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	v.keys = keys
	v.lastRefresh = v.now()
	k, ok = v.keys[kid]
	v.mu.Unlock()
	if !ok {
		return nil, ErrTokenKID
	}
	return k, nil
}

// jwk is one key of the set. Only Ed25519 OKP signing keys are accepted.
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Kid string `json:"kid"`
	X   string `json:"x"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

// fetchJWKS reads the authority's key set. The JWKS is public, so it is the one
// request made without the internal key.
func (c *Client) fetchJWKS(ctx context.Context) (map[string]ed25519.PublicKey, error) {
	if c == nil || c.base == "" {
		return nil, fmt.Errorf("%w: ELOHIM_VENI_URL is not configured", ErrUnavailable)
	}
	ctx, cancel := context.WithTimeout(ctx, JWKSTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/auth/jwks", nil)
	if err != nil {
		return nil, fmt.Errorf("identity: jwks: building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: jwks: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: jwks: HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("%w: jwks: reading: %v", ErrUnavailable, err)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("%w: jwks: undecodable: %v", ErrUnavailable, err)
	}
	keys := make(map[string]ed25519.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" || k.Kid == "" {
			continue // not a key this verifier may ever use
		}
		if k.Alg != "" && k.Alg != TokenAlg {
			continue
		}
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: jwks: key %q is not a valid Ed25519 public key", ErrUnavailable, k.Kid)
		}
		keys[k.Kid] = ed25519.PublicKey(x)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: jwks: no Ed25519 signing keys published", ErrUnavailable)
	}
	return keys, nil
}

// AuthorizationScheme is the scheme an access token is forwarded under.
const AuthorizationScheme = "PIAL"

// Authorize sets `Authorization: PIAL <jws>` on an outbound request to another
// brain. Whatever Authorization the request already carried is removed first —
// a proxied request may hold the caller's own session token, which is a
// credential for this brain and must never reach another. With no token the
// request leaves without the header: the receiving brain sees an anonymous
// caller, which is the truth.
func Authorize(req *http.Request, accessToken string) {
	if req == nil {
		return
	}
	req.Header.Del("Authorization")
	if accessToken != "" {
		req.Header.Set("Authorization", AuthorizationScheme+" "+accessToken)
	}
}
