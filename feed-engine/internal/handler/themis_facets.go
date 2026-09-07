package handler

// themis_facets.go — the Themis commerce brain, projected as Facets.
//
// Every browser-facing read of Themis lands here. The browser never receives
// Themis JSON: the server asks Themis on the viewer's behalf (X-Pial-Identity
// carries the session-derived PIAL, exactly as the /events dispatcher does),
// shapes the answer and renders the Facet. A native Shell and the browser Shell
// therefore project the same fragments from the same routes.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/f33d3r/feed-engine/internal/model"
)

// themisTimeout bounds one Themis call made while rendering a Facet. A Facet
// that waits longer than this has already lost the viewer; it answers with its
// unavailable state instead.
const themisTimeout = 3 * time.Second

// themisMaxBody caps what a Themis answer may occupy in memory. A QR PNG is a
// few kilobytes; a library or listing page is far below this.
const themisMaxBody = 4 << 20

var errThemisUnconfigured = errors.New("themis: no THEMIS_URL configured")

// themisGet performs one authenticated GET against Themis for viewer. The
// viewer's PIAL travels in X-Pial-Identity — Themis authorises on that header
// and on nothing in the body or path.
func (h *Handler) themisGet(ctx context.Context, path string, viewer *model.User) (int, []byte, error) {
	if h.themisURL == "" {
		return 0, nil, errThemisUnconfigured
	}
	ctx, cancel := context.WithTimeout(ctx, themisTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.themisURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	if viewer != nil && viewer.PIALID != "" {
		req.Header.Set("X-Pial-Identity", viewer.PIALID)
		req.Header.Set("X-Pial-Handle", viewer.Handle)
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, themisMaxBody))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// ── Themis ECDH public key ────────────────────────────────────────────────────
//
// The listing composer wraps each content key for Themis with ECIES, so it needs
// Themis's public key. That key is static for the life of a Themis deployment;
// it is fetched server-side, cached, and written into the listing form Facet as
// data-themis-pubkey — the browser reads a rendered attribute, not a JSON
// endpoint.

const themisPubkeyTTL = 10 * time.Minute

var themisPubkeyCache struct {
	sync.Mutex
	key       string
	fetchedAt time.Time
}

// themisPubkey returns Themis's base64 ECDH public key, or "" when Themis
// cannot answer — the form then renders its unavailable state.
func (h *Handler) themisPubkey(ctx context.Context) string {
	themisPubkeyCache.Lock()
	defer themisPubkeyCache.Unlock()
	if themisPubkeyCache.key != "" && time.Since(themisPubkeyCache.fetchedAt) < themisPubkeyTTL {
		return themisPubkeyCache.key
	}
	status, body, err := h.themisGet(ctx, "/v1/pubkey", nil)
	if err != nil || status != http.StatusOK {
		log.Printf("[themis] pubkey: status=%d err=%v", status, err)
		return themisPubkeyCache.key
	}
	var out struct {
		PublicKeyB64 string `json:"public_key_b64"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.PublicKeyB64 == "" {
		log.Printf("[themis] pubkey decode: %v", err)
		return themisPubkeyCache.key
	}
	themisPubkeyCache.key = out.PublicKeyB64
	themisPubkeyCache.fetchedAt = time.Now()
	return out.PublicKeyB64
}

// ── Buyer library ─────────────────────────────────────────────────────────────

// themisPurchase is the row Themis's GET /v1/library returns for each delivered
// purchase (themis/src/models.rs Purchase, serialised by serde). cek_for_buyer
// is a byte vector — serde writes it as a JSON array of numbers, while
// /v1/purchase/{id} writes the same bytes as base64 — so it is decoded from
// either shape.
type themisPurchase struct {
	ID           string          `json:"id"`
	ListingID    string          `json:"listing_id"`
	Currency     string          `json:"currency"`
	ExpectedSats int64           `json:"expected_sats"`
	Status       string          `json:"status"`
	CekForBuyer  json.RawMessage `json:"cek_for_buyer"`
	ConfirmedAt  *time.Time      `json:"confirmed_at"`
	DeliveredAt  *time.Time      `json:"delivered_at"`
	ExpiresAt    time.Time       `json:"expires_at"`
	// The following are not part of the Purchase row today. They are the
	// listing fields a library row wants (title, content location) and are
	// read when Themis joins them onto the row; until then the row falls back
	// to the listing reference and the marketplace link.
	ListingTitle string `json:"listing_title"`
	ContentURL   string `json:"content_url"`
	ContentHash  string `json:"content_hash"`
}

// decodeBytesField reads a byte vector serialised either as a JSON array of
// numbers (serde Vec<u8>) or as a base64 string.
func decodeBytesField(raw json.RawMessage) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var nums []int
	if err := json.Unmarshal(raw, &nums); err == nil {
		out := make([]byte, len(nums))
		for i, n := range nums {
			out[i] = byte(n)
		}
		return out
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if b, err := base64.StdEncoding.DecodeString(s); err == nil {
			return b
		}
	}
	return nil
}

// libraryRow is one purchase as the buyer_library Facet renders it.
type libraryRow struct {
	PurchaseID  string
	ListingID   string
	ListingRef  string
	Title       string
	Price       string
	Delivered   string
	ListingURL  string
	ContentURL  string
	ContentHash string
	CekForBuyer string
}

func libraryRowFrom(p themisPurchase) libraryRow {
	row := libraryRow{
		PurchaseID:  p.ID,
		ListingID:   p.ListingID,
		Title:       p.ListingTitle,
		ContentURL:  p.ContentURL,
		ContentHash: p.ContentHash,
		ListingURL:  "/marketplace#listing-" + p.ListingID,
	}
	if len(p.ListingID) >= 8 {
		row.ListingRef = p.ListingID[:8]
	} else {
		row.ListingRef = p.ListingID
	}
	if row.Title == "" {
		row.Title = "Listing " + row.ListingRef
	}
	switch p.Currency {
	case "xrp":
		row.Price = "Paid in XRP"
	default:
		row.Price = fmt.Sprintf("%d sats", p.ExpectedSats)
	}
	if p.DeliveredAt != nil {
		row.Delivered = TimeAgo(*p.DeliveredAt)
	} else if p.ConfirmedAt != nil {
		row.Delivered = TimeAgo(*p.ConfirmedAt)
	}
	if cek := decodeBytesField(p.CekForBuyer); len(cek) > 0 {
		row.CekForBuyer = base64.StdEncoding.EncodeToString(cek)
	}
	return row
}

// buyerLibraryFacet — GET /themis/v1/library
// Renders the viewer's delivered purchases as the buyer_library Facet.
func (h *Handler) buyerLibraryFacet(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	data := map[string]interface{}{
		"FacetID":  "facet:f33d3r:library:" + user.PIALID + ":list",
		"Rows":     []libraryRow{},
		"Error":    "",
		"RetryURL": "/themis/v1/library",
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	status, body, err := h.themisGet(r.Context(), "/v1/library", user)
	if err != nil || status != http.StatusOK {
		log.Printf("[library] themis: status=%d err=%v", status, err)
		data["Error"] = "Your library could not be loaded right now."
		h.renderPartial(w, "buyer_library", data)
		return
	}
	var out struct {
		Purchases []themisPurchase `json:"purchases"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		log.Printf("[library] decode: %v", err)
		data["Error"] = "Your library could not be read right now."
		h.renderPartial(w, "buyer_library", data)
		return
	}
	rows := make([]libraryRow, 0, len(out.Purchases))
	for _, p := range out.Purchases {
		rows = append(rows, libraryRowFrom(p))
	}
	data["Rows"] = rows
	h.renderPartial(w, "buyer_library", data)
}

// ── Purchase QR ───────────────────────────────────────────────────────────────

// purchaseQR — GET /themis/v1/qr/{id}
// Streams the BIP21 QR PNG for a purchase. Themis renders the QR for any id, so
// the buyer check is made here first through /v1/purchase/{id}, which Themis
// only answers to the purchase's buyer.
func (h *Handler) purchaseQR(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		http.NotFound(w, r)
		return
	}
	status, _, err := h.themisGet(r.Context(), "/v1/purchase/"+id, user)
	if err != nil {
		http.Error(w, "payment service unavailable", http.StatusBadGateway)
		return
	}
	if status != http.StatusOK {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	status, png, err := h.themisGet(r.Context(), "/v1/qr/"+id, user)
	if err != nil || status != http.StatusOK || len(png) == 0 {
		http.Error(w, "qr unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	_, _ = w.Write(png)
}

// ── Purchase status ───────────────────────────────────────────────────────────

// facetPurchaseStatus — GET /facets/marketplace/purchase/{id}/status
// The polling line inside the purchase modal. While the purchase is pending or
// confirmed the fragment carries its own hx-trigger="every 10s" so it keeps
// asking; a delivered or expired purchase renders its final state and stops.
func (h *Handler) facetPurchaseStatus(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		http.NotFound(w, r)
		return
	}
	state := "unavailable"
	status, body, err := h.themisGet(r.Context(), "/v1/purchase/"+id, user)
	if err == nil && status == http.StatusOK {
		var out struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(body, &out) == nil && out.Status != "" {
			state = out.Status
		}
	} else if err == nil && (status == http.StatusNotFound || status == http.StatusForbidden) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "marketplace_purchase_status", map[string]interface{}{
		"PurchaseID": id,
		"Status":     state,
		"Polling":    state == "pending" || state == "confirmed" || state == "unavailable",
	})
}

// ── Seller dashboard ──────────────────────────────────────────────────────────

// facetSellerStats — GET /facets/marketplace/seller/stats
func (h *Handler) facetSellerStats(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	data := map[string]interface{}{
		"FacetID": "facet:f33d3r:shop:" + user.PIALID + ":stats",
		"Sales":   "—",
		"Revenue": "—",
		"Pending": "—",
	}
	status, body, err := h.themisGet(r.Context(), "/v1/seller/"+user.PIALID+"/stats", user)
	if err == nil && status == http.StatusOK {
		var out struct {
			Sales       int64 `json:"sales"`
			RevenueSats int64 `json:"revenue_sats"`
			Pending     int64 `json:"pending"`
		}
		if json.Unmarshal(body, &out) == nil {
			data["Sales"] = strconv.FormatInt(out.Sales, 10)
			data["Revenue"] = fmtThousands(out.RevenueSats) + " sats"
			data["Pending"] = strconv.FormatInt(out.Pending, 10)
		}
	} else {
		log.Printf("[seller-stats] themis: status=%d err=%v", status, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "marketplace_seller_stats", data)
}

// sellerListingLimit is the number of listings one shop may hold; the count
// line under the shop heading reads against it.
const sellerListingLimit = 12

// facetSellerListings — GET /facets/marketplace/seller/listings
// The viewer's own listings, active and inactive, with seller actions.
func (h *Handler) facetSellerListings(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	data := map[string]interface{}{
		"FacetID":  "facet:f33d3r:shop:" + user.PIALID + ":listings",
		"Listings": []PublicListing{},
		"Count":    0,
		"Limit":    sellerListingLimit,
		"Error":    "",
	}
	status, body, err := h.themisGet(r.Context(), "/v1/shop/"+user.PIALID, user)
	if err != nil || status != http.StatusOK {
		log.Printf("[seller-listings] themis: status=%d err=%v", status, err)
		data["Error"] = "Could not load listings."
	} else {
		var out struct {
			Listings []PublicListing `json:"listings"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			log.Printf("[seller-listings] decode: %v", err)
			data["Error"] = "Could not read listings."
		} else {
			data["Listings"] = out.Listings
			data["Count"] = len(out.Listings)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "marketplace_seller_listings", data)
}

// fmtThousands writes n with comma separators (1234567 → "1,234,567").
func fmtThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := false
	if len(s) > 0 && s[0] == '-' {
		neg = true
		s = s[1:]
	}
	var out []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(c))
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
