package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/f33d3r/feed-engine/internal/manhattan"
)

// PIAL keys and handle resolution.
//
// Two of this brain's oldest claims are settled here, both against Manhattan's
// authority map (migration 0002).
//
//	key    → elohim-veni. Signing and ECDH keys belong to the identity that
//	         holds them. feed-engine keeps no copy of either.
//	handle → elohim-veni allocates and binds it, along with the identity it
//	         points at. users.handle is only a projection of that, so a handle is resolved
//	         through the naming plane and never by joining this brain's own
//	         mirror of someone else's grant.

// resolveHandlesToPIALs resolves a set of @handles in one round trip, returning
// only those that name a live identity, keyed by the lowercased handle.
//
// The batch is the point: a body of text carries many mentions, and resolving
// them one at a time is what made copying handles into rows look cheap by
// comparison. A name that does not resolve is simply absent from the result —
// an unknown handle and a revoked one are the same answer by design.
func (h *Handler) resolveHandlesToPIALs(ctx context.Context, handles []string) (map[string]string, error) {
	names := make([]string, 0, len(handles))
	handleOf := make(map[string]string, len(handles))
	for _, raw := range handles {
		hd := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(raw, "@")))
		if hd == "" {
			continue
		}
		name := manhattan.HandleName(hd)
		if _, dup := handleOf[name]; dup {
			continue
		}
		handleOf[name] = hd
		names = append(names, name)
	}
	if len(names) == 0 {
		return map[string]string{}, nil
	}

	resolved, err := h.manhattan.ResolveBatch(ctx, names)
	if err != nil {
		return nil, err
	}

	out := make(map[string]string, len(resolved))
	for name, node := range resolved {
		hd, ok := handleOf[name]
		if !ok {
			continue
		}
		// Resolution answers "which node", not "which PIAL": the PIAL is a
		// second name on that same node, so the full node is what carries it.
		pial, err := h.pialOfNode(ctx, node.NodeID)
		if err != nil {
			if errors.Is(err, manhattan.ErrNotFound) {
				// A handle bound to a node with no live PIAL name is a broken
				// binding, not a transport failure. Skip it and say so.
				log.Printf("[manhattan] handle %q resolves to node %s which carries no live pial name", hd, node.NodeID)
				continue
			}
			return nil, err
		}
		out[hd] = pial
	}
	return out, nil
}

// pialOfNode returns the canonical PIAL name of an identity node.
func (h *Handler) pialOfNode(ctx context.Context, nodeID string) (string, error) {
	node, err := h.manhattan.GetNode(ctx, nodeID)
	if err != nil {
		return "", err
	}
	pial := node.PrimaryNameIn(manhattan.NamespacePIAL)
	if pial == "" {
		return "", manhattan.ErrNotFound
	}
	return pial, nil
}

// registerPIALSigningKey hands a browser-generated ECDSA-P256 public key to the
// brain that owns keys.
//
// Manhattan gives the `key` kind to elohim-veni, and this brain keeps no copy.
// It does not need one: the only reader of a signing key here — verifyMalkuthSig
// — already fetches it from elohim-veni, so a local table was a store with no
// reader and a second writer. That is the contradiction the authority map was
// written to end, and it is ended by deleting the write, not by mirroring it.
//
// The registration therefore IS the request to elohim-veni. Its status is
// checked, because a refused mirror used to leave the caller with an "ok" and
// the graph with no key node and no signs_for edge.
// POST /api/pial/signing-key/register  JSON: {"public_key_b64":"...","algorithm":"ECDSA-P256"}
func (h *Handler) registerPIALSigningKey(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		PublicKeyB64 string `json:"public_key_b64"`
		Algorithm    string `json:"algorithm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PublicKeyB64 == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	algo := req.Algorithm
	if algo == "" {
		algo = "ECDSA-P256"
	}

	if err := h.callElohim(r.Context(), http.MethodPost, "/v1/pial/signing-key/register",
		map[string]string{
			"pial_id":        user.PIALID,
			"public_key_b64": req.PublicKeyB64,
			"algorithm":      algo,
		}, nil); err != nil {
		log.Printf("[pial] signing key registration for PIAL %s refused by the key authority: %v", user.PIALID, err)
		http.Error(w, `{"error":"key_authority_unavailable"}`, http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}
