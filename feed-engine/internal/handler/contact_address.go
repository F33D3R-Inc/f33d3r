package handler

import (
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

	if err := h.elohimMintAddress(r.Context(), user.ID); err != nil {
		log.Printf("[contact-address] minting for account %s: %v", user.ID, err)
		h.renderContactAddresses(w, r, user, h.addressErrorMessage("Could not mint an address right now."))
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

	// Revoking is only the owner's to do: elohim-veni, which records which
	// account minted each address, refuses an address this account did not mint
	// — another persona's of the same person included.
	if err := h.elohimRevokeAddress(r.Context(), addr, user.ID); err != nil {
		var refusal *elohimRefusal
		if errors.As(err, &refusal) && (refusal.Status == http.StatusForbidden || refusal.Status == http.StatusNotFound) {
			http.Error(w, "403 Forbidden", http.StatusForbidden)
			return
		}
		log.Printf("[contact-address] revoking %s: %v", addr, err)
		h.renderContactAddresses(w, r, user, h.addressErrorMessage("Could not revoke that address right now."))
		return
	}
	h.renderContactAddresses(w, r, user, "")
}

// contactKeyBundle — GET /api/contact/{address}
// A live address is exchanged for the current messaging public keys of the
// account that minted it.
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
	// elohim-veni resolves the address to the account that minted it — the
	// persona the address was handed out for, never its person's primary — and
	// answers with that account's signed bundle.
	bundle, err := h.contactBundleFor(r.Context(), map[string]string{"address": addr})
	if err != nil {
		var refusal *elohimRefusal
		if errors.As(err, &refusal) && (refusal.Status == http.StatusNotFound || refusal.Status == http.StatusConflict) {
			http.NotFound(w, r)
			return
		}
		log.Printf("[contact-address] key bundle for an address: %v", err)
		http.Error(w, "key directory unavailable", http.StatusServiceUnavailable)
		return
	}
	writeContactBundle(w, http.StatusOK, bundle)
}

// contactBundle is the messaging key bundle as a client receives it, exactly as
// elohim-veni signed it.
//
// Bundle is elohim-veni's signed object, carried as the raw bytes it sent: one
// account's id, the time it was issued and that account's per-device public
// keys — no PIAL and no identity-wide key, because whoever holds a Number or an
// address receives it and anything one person's personas share would link
// them. Signature covers exactly those bytes under SignatureKeyB64 (Ed25519),
// so it is forwarded untouched rather than re-encoded: a bundle decoded and
// re-marshalled here would no longer be the bytes that were signed.
type contactBundle struct {
	Bundle             json.RawMessage `json:"bundle"`
	Signed             bool            `json:"signed"`
	Signature          *string         `json:"signature"`
	SignatureAlgorithm *string         `json:"signature_algorithm"`
	SignatureKeyB64    *string         `json:"signature_key_b64"`
}

// contactBundleFor fetches one account's signed messaging key bundle from
// elohim-veni, which owns key material. target names the account directly
// ({"account_id": ...}) or by a contact address ({"address": ...}).
func (h *Handler) contactBundleFor(ctx context.Context, target map[string]string) (contactBundle, error) {
	var out contactBundle
	if err := h.callElohim(ctx, http.MethodPost, "/v1/contact/keybundle", target, &out); err != nil {
		return contactBundle{}, err
	}
	var shape struct {
		AccountID string            `json:"account_id"`
		Devices   []json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(out.Bundle, &shape); err != nil {
		return contactBundle{}, fmt.Errorf("key bundle is not an object: %w", err)
	}
	if shape.AccountID == "" || len(shape.Devices) == 0 {
		return contactBundle{}, errors.New("key bundle names no account or carries no device keys")
	}
	return out, nil
}

// writeContactBundle answers with a bundle. HTML escaping is off so the signed
// bytes pass through exactly as elohim-veni produced them.
func writeContactBundle(w http.ResponseWriter, status int, bundle contactBundle) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(bundle); err != nil {
		log.Printf("[contact] writing a key bundle: %v", err)
	}
}

// renderContactAddresses draws the contact_addresses Facet for one identity.
// Every mutation above ends here, so the fragment the browser swaps in is
// always the server's current account of the address set.
func (h *Handler) renderContactAddresses(w http.ResponseWriter, r *http.Request, user *model.User, notice string) {
	h.renderPartial(w, "contact_addresses", h.contactAddressesData(r, user, notice))
}

// contactAddressesData hydrates the contact_addresses Facet. Shared by the
// standalone facet endpoint and by the settings contact section, so both render
// from one account of the address set. The set is the viewing account's own:
// elohim-veni answers only the addresses this account minted, never another
// persona's of the same person.
func (h *Handler) contactAddressesData(r *http.Request, user *model.User, notice string) map[string]interface{} {
	data := map[string]interface{}{
		"AccountID": user.ID,
		"Notice":    notice,
	}

	addresses, err := h.elohimListAddresses(r.Context(), user.ID)
	if err != nil {
		log.Printf("[contact-address] listing for account %s: %v", user.ID, err)
		data["Unavailable"] = true
		if notice == "" {
			data["Notice"] = h.addressErrorMessage("Could not load your addresses right now.")
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

// addressErrorMessage turns a failure into something a person can act on,
// without inventing a reason it does not know: a deployment with no identity
// authority configured says so, and anything else is the caller's fallback.
func (h *Handler) addressErrorMessage(fallback string) string {
	if h.cfg.ElohimVeniURL == "" {
		return "Contact addresses are not available on this deployment yet."
	}
	return fallback
}

// ── Address mutations, via the brain that owns them ──────────────────────────
//
// Every call names the acting ACCOUNT. elohim-veni derives the identity from it
// and records which account minted each address, so an address belongs to one
// persona and leads to that persona alone.

// elohimMintAddress asks elohim-veni to mint a contact address for an account.
func (h *Handler) elohimMintAddress(ctx context.Context, accountID string) error {
	return h.callElohim(ctx, http.MethodPost, "/v1/addresses/mint",
		map[string]string{"account_id": accountID}, nil)
}

// elohimRevokeAddress retires one of an account's contact addresses. elohim-veni
// re-checks that the account minted it.
func (h *Handler) elohimRevokeAddress(ctx context.Context, addr, accountID string) error {
	return h.callElohim(ctx, http.MethodPost, "/v1/addresses/revoke",
		map[string]string{"address": addr, "account_id": accountID}, nil)
}

// elohimListAddresses lists the contact addresses one account minted. Manhattan
// gates the identity's listing to elohim-veni, which answers per account.
func (h *Handler) elohimListAddresses(ctx context.Context, accountID string) ([]manhattan.Address, error) {
	var out struct {
		Addresses []manhattan.Address `json:"addresses"`
	}
	if err := h.callElohim(ctx, http.MethodPost, "/v1/addresses/list",
		map[string]string{"account_id": accountID}, &out); err != nil {
		return nil, err
	}
	return out.Addresses, nil
}
