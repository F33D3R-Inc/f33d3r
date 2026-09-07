package api

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"net/http"

	"f33d3r.com/ios/devserver/internal/store"
)

// registerSigningKey — POST /api/pial/signing-key/register
// {"public_key_b64":"...","algorithm":"ECDSA-P256"} → {"ok":true}
//
// In production Nantar forwards this to Elohim Veni and keeps nothing. This
// server is the authority, so it keeps the key in pial_signing_keys and
// verifies against that. One key per PIAL: a new registration replaces the
// old, which is why the client registers on every launch.
func (s *Server) registerSigningKey(w http.ResponseWriter, r *http.Request, u *store.User) {
	var req struct {
		PublicKeyB64 string `json:"public_key_b64"`
		Algorithm    string `json:"algorithm"`
	}
	if err := readJSON(r, &req); err != nil || req.PublicKeyB64 == "" {
		plainError(w, http.StatusBadRequest, "invalid request")
		return
	}
	// Refuse a key that cannot verify anything: catching a malformed SPKI here
	// is the difference between one clear error and every later post failing
	// with `signature invalid`.
	if !verifySignatureKeyParses(req.PublicKeyB64) {
		plainError(w, http.StatusBadRequest, "public_key_b64 is not an SPKI ECDSA-P256 key")
		return
	}
	if err := s.store.RegisterSigningKey(r.Context(), u.PIALID, req.PublicKeyB64, req.Algorithm); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// verifySignatureKeyParses reports whether b64 decodes to an SPKI ECDSA key.
func verifySignatureKeyParses(b64 string) bool {
	spki, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		spki, err = base64.RawStdEncoding.DecodeString(b64)
		if err != nil {
			return false
		}
	}
	pub, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return false
	}
	_, ok := pub.(*ecdsa.PublicKey)
	return ok
}
