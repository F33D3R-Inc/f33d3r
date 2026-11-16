// Package mediatoken implements HMAC-SHA256 signed tokens for NSFW media URL gating.
//
// Token structure (URL-safe):
//
//	base64url(path + "|" + accountID + "|" + unixExpiry) + "." + base64url(hmac_sha256(secret, payload))
//
// The optional binding is to the viewer's account, never to a PIAL: the token
// travels in a URL, and a PIAL is never written into a page or a URL.
//
// Tokens have a 5-minute TTL.  Non-NSFW content never receives a token — this
// package is only invoked when the post's IsNSFW flag is true.
package mediatoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const ttl = 5 * time.Minute

var (
	secretOnce sync.Once
	secret     []byte
)

// getSecret returns the signing secret, derived once from the environment.
// Falls back to a deterministic key based on the process environment when
// MEDIA_TOKEN_SECRET is not set (dev only — always set it in production).
func getSecret() []byte {
	secretOnce.Do(func() {
		if s := os.Getenv("MEDIA_TOKEN_SECRET"); s != "" {
			secret = []byte(s)
			return
		}
		// Fallback: derive from SESSION_SECRET so dev works without extra config.
		fallback := os.Getenv("SESSION_SECRET")
		if fallback == "" {
			fallback = "f33d3r-media-dev-key-change-in-production"
		}
		h := hmac.New(sha256.New, []byte("media-token-v1"))
		h.Write([]byte(fallback))
		secret = h.Sum(nil)
	})
	return secret
}

// sign computes HMAC-SHA256(secret, payload) and returns it base64url-encoded.
func sign(payload string) string {
	mac := hmac.New(sha256.New, getSecret())
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Generate returns a signed token for the given media path, optionally bound to
// one viewer account ("" binds it to nobody).
// The token is valid for ttl (5 minutes) from the time of generation.
func Generate(mediaPath, accountID string) string {
	expiry := time.Now().Add(ttl).Unix()
	payload := fmt.Sprintf("%s|%s|%d",
		base64.RawURLEncoding.EncodeToString([]byte(mediaPath)),
		base64.RawURLEncoding.EncodeToString([]byte(accountID)),
		expiry,
	)
	sig := sign(payload)
	return payload + "." + sig
}

// ErrInvalid is returned when a token cannot be parsed or verified.
var ErrInvalid = errors.New("mediatoken: invalid token")

// ErrExpired is returned when a token has passed its TTL.
var ErrExpired = errors.New("mediatoken: token expired")

// Validate verifies the token signature and TTL, and returns the embedded
// media path and bound account id on success.
func Validate(token string) (mediaPath, accountID string, err error) {
	// Split at the last "." to get payload + signature.
	idx := strings.LastIndex(token, ".")
	if idx < 0 {
		return "", "", ErrInvalid
	}
	payload := token[:idx]
	sig := token[idx+1:]

	// Constant-time HMAC comparison.
	if !hmac.Equal([]byte(sign(payload)), []byte(sig)) {
		return "", "", ErrInvalid
	}

	// Parse payload: encPath|encAccount|expiry
	parts := strings.Split(payload, "|")
	if len(parts) != 3 {
		return "", "", ErrInvalid
	}

	pathBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", ErrInvalid
	}
	accountBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", ErrInvalid
	}
	expiry, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", "", ErrInvalid
	}

	if time.Now().Unix() > expiry {
		return "", "", ErrExpired
	}

	return string(pathBytes), string(accountBytes), nil
}
