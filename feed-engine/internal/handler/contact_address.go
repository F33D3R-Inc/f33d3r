package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/f33d3r/feed-engine/internal/manhattan"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── Contact addresses ────────────────────────────────────────────────────────
//
// A contact address is a public, cheap, revocable name that resolves to an
// identity. It is what a creator puts on a conference badge, in a bio, or on a
// flyer, and it replaces the phone number every other messenger is welded to.
//
// What makes it different from an inbox filter: possession of a live address
// grants exactly one capability — the right to fetch that identity's current
// device key bundle and open an end-to-end channel. The private keys never
// touch the server, so the address grants the ability to *start* a conversation
// and nothing else.
//
// That is what makes rotation cheap. Revoking an address stops future channel
// establishment; it does not touch a single conversation already open, because
// those already hold their own device keys. The identity behind the address
// never moves. A phone number cannot do this, and a server that sits in the
// middle of the message does not need to.
//
// The addresses themselves live in Manhattan, as address nodes with a
// resolves_to edge to an identity node. feed-engine does not store them, which
// is the whole rule: an entity owned by another brain is reached by name.
//
// It does not mint them either: Manhattan gives `address`/`addr` to elohim-veni
// and rejects this brain's writes. Only resolve is direct; mint, revoke and list
// go via elohim-veni.

// facetContactAddresses — GET /facets/identity/addresses
// Renders the viewer's contact addresses, live and revoked.
func (h *Handler) facetContactAddresses(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h.renderContactAddresses(w, r, user, "")
}

// mintContactAddress — POST /identity/addresses/mint
// Issues a new address. Minting does not revoke the others: an identity may
// publish several at once (one per flyer, per event, per collaborator) and
// retire them independently, which is the point of them being cheap.
func (h *Handler) mintContactAddress(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	node, err := h.identityNode(r, user.PIALID)
	if err != nil {
		h.renderContactAddresses(w, r, user, addressErrorMessage(err))
		return
	}
	if err := h.elohimMintAddress(r.Context(), node.NodeID); err != nil {
		log.Printf("[contact-address] minting for pial %s: %v", user.PIALID, err)
		h.renderContactAddresses(w, r, user, "Could not mint an address right now.")
		return
	}
	h.renderContactAddresses(w, r, user, "")
}

// revokeContactAddress — POST /identity/addresses/revoke
// Retires one address. Every conversation already established through it stays
// open and readable; only future channel establishment is refused.
func (h *Handler) revokeContactAddress(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	addr := strings.TrimSpace(r.FormValue("address"))
	if addr == "" {
		http.Error(w, "address required", http.StatusBadRequest)
		return
	}

	// Revoking is only the owner's to do, so the address must resolve to this
	// viewer's own identity before it is touched.
	node, err := h.identityNode(r, user.PIALID)
	if err != nil {
		h.renderContactAddresses(w, r, user, addressErrorMessage(err))
		return
	}
	target, err := h.manhattan.ResolveAddress(r.Context(), addr)
	if err != nil || target.IdentityNodeID != node.NodeID {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	if err := h.elohimRevokeAddress(r.Context(), addr, node.NodeID); err != nil {
		log.Printf("[contact-address] revoking %s: %v", addr, err)
		h.renderContactAddresses(w, r, user, "Could not revoke that address right now.")
		return
	}
	h.renderContactAddresses(w, r, user, "")
}

// contactKeyBundle — GET /api/contact/{address}
// A live address is exchanged for the identity's current messaging public keys.
// A revoked or unknown address is a flat 404 — a rotated address must not confirm
// that it was ever real.
//
// The keys are X25519, which is what Gnosis actually seals to. The bundle
// previously returned ECDSA-P256 and ECDH-P256 keys: a caller following the
// documented purpose chain received keys no conversation could be opened with.
func (h *Handler) contactKeyBundle(w http.ResponseWriter, r *http.Request) {
	addr := strings.TrimSpace(r.PathValue("address"))
	if addr == "" {
		http.Error(w, "address required", http.StatusBadRequest)
		return
	}
	if !h.manhattan.Configured() {
		http.Error(w, "naming plane unavailable", http.StatusServiceUnavailable)
		return
	}

	target, err := h.manhattan.ResolveAddress(r.Context(), addr)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	node, err := h.manhattan.GetNode(r.Context(), target.IdentityNodeID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	pial := node.PrimaryNameIn(manhattan.NamespacePIAL)
	if pial == "" {
		http.NotFound(w, r)
		return
	}

	bundle, err := h.contactBundleFor(r.Context(), pial)
	if err != nil {
		log.Printf("[contact-address] key bundle for pial %s: %v", pial, err)
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(bundle)
}

// contactKeyDevice is one device's public messaging key.
type contactKeyDevice struct {
	DeviceID     string `json:"device_id"`
	PublicKeyB64 string `json:"public_key_b64"`
	Algorithm    string `json:"algorithm"`
}

// contactBundle is the messaging key bundle as a client receives it: public key
// halves only, and no identity. elohim-veni assembles its bundle around the
// target's pial_id and signs those bytes with its service key; handing that
// bundle on would give anyone who holds a contact address or a Number the
// person's root identity, which is exactly the harvesting the address and
// Number layers exist to prevent. So the bundle is projected here, the pial_id
// is left on the server, and elohim's signature — which covers the pial_id and
// could no longer be checked — is not forwarded either: Signed is false until
// elohim-veni signs a PIAL-free bundle.
type contactBundle struct {
	IssuedAt         string             `json:"issued_at"`
	Devices          []contactKeyDevice `json:"devices"`
	SigningPubkeyB64 *string            `json:"signing_pubkey_b64"`
	SigningAlgorithm *string            `json:"signing_algorithm"`
	Signed           bool               `json:"signed"`
}

// contactBundleFor fetches an identity's messaging key bundle from
// elohim-veni, which owns key material, and projects it for a client.
func (h *Handler) contactBundleFor(ctx context.Context, pialID string) (contactBundle, error) {
	var out struct {
		Bundle struct {
			IssuedAt         string             `json:"issued_at"`
			Devices          []contactKeyDevice `json:"devices"`
			SigningPubkeyB64 *string            `json:"signing_pubkey_b64"`
			SigningAlgorithm *string            `json:"signing_algorithm"`
		} `json:"bundle"`
	}
	if err := h.callElohim(ctx, http.MethodPost, "/v1/contact/keybundle",
		map[string]string{"pial_id": pialID}, &out); err != nil {
		return contactBundle{}, err
	}
	if len(out.Bundle.Devices) == 0 {
		return contactBundle{}, fmt.Errorf("key bundle for an identity carried no device keys")
	}
	return contactBundle{
		IssuedAt:         out.Bundle.IssuedAt,
		Devices:          out.Bundle.Devices,
		SigningPubkeyB64: out.Bundle.SigningPubkeyB64,
		SigningAlgorithm: out.Bundle.SigningAlgorithm,
	}, nil
}

// identityNode returns the Manhattan node for a PIAL, registering it if the
// outbox has not delivered it yet. Registration here is idempotent and resolves
// to the same node the drain would have created.
func (h *Handler) identityNode(r *http.Request, pialID string) (*manhattan.Node, error) {
	if !h.manhattan.Configured() {
		return nil, manhattan.ErrNotConfigured
	}
	return h.manhattan.EnsureNode(
		r.Context(),
		manhattan.KindIdentity,
		manhattan.PIALName(pialID),
		manhattan.NamespacePIAL,
	)
}

// renderContactAddresses draws the contact_addresses Facet for one identity.
// Every mutation above ends here, so the fragment the browser swaps in is
// always the server's current account of the address set.
func (h *Handler) renderContactAddresses(w http.ResponseWriter, r *http.Request, user *model.User, notice string) {
	h.renderPartial(w, "contact_addresses", h.contactAddressesData(r, user, notice))
}

// contactAddressesData hydrates the contact_addresses Facet. Shared by the
// standalone facet endpoint and by the settings contact section, so both render
// from one account of the address set. The PIAL resolves the identity node only;
// the Facet is addressed by the account id, because a PIAL is never written into
// a page.
func (h *Handler) contactAddressesData(r *http.Request, user *model.User, notice string) map[string]interface{} {
	pialID := user.PIALID
	data := map[string]interface{}{
		"AccountID": user.ID,
		"Notice":    notice,
	}

	node, err := h.identityNode(r, pialID)
	if err != nil {
		data["Unavailable"] = true
		if notice == "" {
			data["Notice"] = addressErrorMessage(err)
		}
		return data
	}

	addresses, err := h.elohimListAddresses(r.Context(), node.NodeID)
	if err != nil {
		log.Printf("[contact-address] listing for pial %s: %v", pialID, err)
		data["Unavailable"] = true
		if notice == "" {
			data["Notice"] = "Could not load your addresses right now."
		}
		return data
	}

	type addressRow struct {
		Address string
		Live    bool
	}
	rows := make([]addressRow, 0, len(addresses))
	for _, a := range addresses {
		rows = append(rows, addressRow{Address: a.Address, Live: a.Status == "active"})
	}
	data["Addresses"] = rows
	return data
}

// addressErrorMessage turns a client error into something a person can act on,
// without inventing a reason it does not know.
func addressErrorMessage(err error) string {
	if errors.Is(err, manhattan.ErrNotConfigured) {
		return "Contact addresses are not available on this deployment yet."
	}
	return "Could not reach the naming plane."
}

// ── Address mutations, via the brain that owns them ──────────────────────────

// elohimMintAddress asks elohim-veni to mint a contact address for an identity.
func (h *Handler) elohimMintAddress(ctx context.Context, identityNodeID string) error {
	return h.postElohim(ctx, "/v1/addresses/mint", map[string]string{
		"identity_node_id": identityNodeID,
	})
}

// elohimRevokeAddress retires a contact address. The identity is sent so elohim-veni
// re-checks ownership itself.
func (h *Handler) elohimRevokeAddress(ctx context.Context, addr, identityNodeID string) error {
	return h.postElohim(ctx, "/v1/addresses/revoke", map[string]string{
		"address":          addr,
		"identity_node_id": identityNodeID,
	})
}

// postElohim sends an internal-authenticated POST to elohim-veni; non-2xx is an error.
func (h *Handler) postElohim(ctx context.Context, path string, body interface{}) error {
	if h.cfg.ElohimVeniURL == "" {
		return manhattan.ErrNotConfigured
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.ElohimVeniURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Key", h.cfg.InternalAPIKey)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("elohim-veni %s: %s", path, resp.Status)
	}
	return nil
}

// elohimListAddresses lists an identity's contact addresses. Manhattan gates this
// read to the owning brain, so it cannot be called from here directly.
func (h *Handler) elohimListAddresses(ctx context.Context, identityNodeID string) ([]manhattan.Address, error) {
	var out struct {
		Addresses []manhattan.Address `json:"addresses"`
	}
	if err := h.postElohimJSON(ctx, "/v1/addresses/list", map[string]string{
		"identity_node_id": identityNodeID,
	}, &out); err != nil {
		return nil, err
	}
	return out.Addresses, nil
}

// postElohimJSON is postElohim with a decoded response body.
func (h *Handler) postElohimJSON(ctx context.Context, path string, body, target interface{}) error {
	if h.cfg.ElohimVeniURL == "" {
		return manhattan.ErrNotConfigured
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.ElohimVeniURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Key", h.cfg.InternalAPIKey)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("elohim-veni %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(target)
}
