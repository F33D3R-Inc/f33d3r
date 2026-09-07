package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// marketplacePage serves the public marketplace feed (GET /marketplace).
//
// This is the buyer surface. Listings are encrypted works that may be adult, so
// the question it asks is how old the viewer is, not who they are — a
// self-reported age (IsAgeVerified) is the correct gate here. IsVerified is
// OR'd in too, same idiom as profile.go/handlers.go/api_v1_reads.go/admin.go:
// the stricter documented-identity bar obviously also clears the age bar, but
// it must never be required on its own — that would force every buyer through
// full eKYC just to browse. Buying is not being paid; the identity predicate
// belongs on the seller side.
func (h *Handler) marketplacePage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || !(user.IsAgeVerified || user.IsVerified) {
		http.Redirect(w, r, "/kyc", http.StatusFound)
		return
	}
	h.render(w, r, "marketplace.html", map[string]interface{}{
		"User":    user,
		"Surface": "marketplace",
		"Title":   "Marketplace",
		"Themes":  ThemesWithActive(user.ThemeID),
	})
}

// sellerDashboardMarketPage serves the seller's marketplace dashboard (GET /create/marketplace).
//
// This is the seller side — it displays the BTC address sale proceeds are paid
// to — so it is an identity surface, not the age surface the public marketplace
// feed above is. It asks model.CanMonetize.
func (h *Handler) sellerDashboardMarketPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || !user.CanMonetize() {
		http.Redirect(w, r, "/kyc", http.StatusFound)
		return
	}

	// Pre-fetch shop status so the template renders the correct initial state server-side.
	shopExists := false
	btcAddress := ""
	xrpAddress := ""
	if h.themisURL != "" && user.PIALID != "" {
		req, err := http.NewRequestWithContext(r.Context(), "GET",
			h.themisURL+"/v1/shop/"+user.PIALID+"/status", nil)
		if err == nil {
			resp, err := h.httpClient.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				var status struct {
					Exists     bool   `json:"exists"`
					IsActive   bool   `json:"is_active"`
					BtcAddress string `json:"btc_address"`
					XrpAddress string `json:"xrp_address"`
				}
				if json.NewDecoder(resp.Body).Decode(&status) == nil {
					shopExists = status.Exists && status.IsActive
					btcAddress = status.BtcAddress
					xrpAddress = status.XrpAddress
				}
				resp.Body.Close()
			}
		}
	}

	h.render(w, r, "seller_dashboard_market.html", map[string]interface{}{
		"User":       user,
		"Surface":    "seller_dashboard",
		"Title":      "Your Shop",
		"Themes":     ThemesWithActive(user.ThemeID),
		"PIALID":     user.PIALID,
		"ShopExists": shopExists,
		"BtcAddress": btcAddress,
		"XrpAddress": xrpAddress,
	})
}

// buyerLibraryPage serves the buyer's purchased content library (GET /library).
func (h *Handler) buyerLibraryPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	h.render(w, r, "buyer_library.html", map[string]interface{}{
		"User":    user,
		"Surface": "library",
		"Title":   "Library",
		"Themes":  ThemesWithActive(user.ThemeID),
	})
}

// facetMarketplaceListingForm serves the listing creation form (GET /facets/marketplace_listing_form).
// The form carries Themis's ECDH public key as a rendered attribute so the
// composer can wrap the content key without asking a JSON endpoint for it.
func (h *Handler) facetMarketplaceListingForm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "marketplace_listing_form", map[string]interface{}{
		"ThemisPubkeyB64": h.themisPubkey(r.Context()),
	})
}

// PublicListing is a safe subset of a Themis listing for browser rendering.
// content_url and cek_encrypted are deliberately omitted — only delivered post-purchase.
type PublicListing struct {
	ID              string `json:"id"`
	ShopPial        string `json:"shop_pial"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	PriceSats       int64  `json:"price_sats"`
	PriceXrpDrops   int64  `json:"price_xrp_drops"`
	PaymentCurrency string `json:"payment_currency"`
	Visibility      string `json:"visibility"`
	IsActive        bool   `json:"is_active"`
	CreatedAt       string `json:"created_at"`
}

// marketplaceFeedPageSize is one page of the public marketplace grid; the feed
// sentinel pages by offset in multiples of it.
const marketplaceFeedPageSize = 20

// listingCardData is the single shape every marketplace_listing_card render is
// fed from, so a card on the public grid, a shop page and the seller dashboard
// cannot disagree about a field.
func listingCardData(l PublicListing, isSeller bool) map[string]interface{} {
	return map[string]interface{}{
		"ID":              l.ID,
		"Title":           l.Title,
		"Description":     l.Description,
		"PriceSats":       l.PriceSats,
		"PriceXrpDrops":   l.PriceXrpDrops,
		"PaymentCurrency": l.PaymentCurrency,
		"Visibility":      l.Visibility,
		"IsActive":        l.IsActive,
		"IsSeller":        isSeller,
		"ShopPial":        l.ShopPial,
	}
}

// facetMarketplaceFeed fetches public listings from Themis and renders them as HTML cards.
// GET /facets/marketplace/feed
func (h *Handler) facetMarketplaceFeed(w http.ResponseWriter, r *http.Request) {
	if h.themisURL == "" {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<div class="marketplace-empty-state"><p class="marketplace-empty-state__body">Marketplace unavailable.</p></div>`)
		return
	}

	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset == 0 {
		// The sentinel's data-next-after cursor, as a native Shell sends it back.
		offset, _ = strconv.Atoi(r.URL.Query().Get("after"))
	}
	if offset < 0 {
		offset = 0
	}

	req, err := http.NewRequestWithContext(r.Context(), "GET",
		fmt.Sprintf("%s/v1/marketplace?limit=%d&offset=%d", h.themisURL, marketplaceFeedPageSize, offset), nil)
	if err != nil {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<div class="marketplace-empty-state"><p class="marketplace-empty-state__body">Could not load listings.</p></div>`)
		return
	}
	// Inject PIAL identity so Themis knows the viewer (for subscription checks)
	user := h.userFromRequest(w, r)
	if user != nil && user.PIALID != "" {
		req.Header.Set("X-Pial-Identity", user.PIALID)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<div class="marketplace-empty-state"><p class="marketplace-empty-state__body">Could not load listings.</p></div>`)
		return
	}
	defer resp.Body.Close()

	var result struct {
		Listings []PublicListing `json:"listings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Listings) == 0 {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<div class="marketplace-empty-state">`+
			`<svg class="marketplace-empty-state__icon" width="40" height="40" fill="none" stroke="currentColor" stroke-width="1.5" viewBox="0 0 24 24"><path d="M6 2 3 6v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2V6l-3-4z"/><line x1="3" y1="6" x2="21" y2="6"/><path d="M16 10a4 4 0 0 1-8 0"/></svg>`+
			`<h3 class="marketplace-empty-state__title">No listings yet</h3>`+
			`<p class="marketplace-empty-state__body">Be the first creator to open a shop.</p>`+
			`</div>`)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	for _, listing := range result.Listings {
		if !listing.IsActive {
			continue
		}
		h.renderPartial(w, "marketplace_listing_card", listingCardData(listing, false))
	}
	// A full page means there may be another: the sentinel asks for the next
	// offset and replaces itself with those cards and the next sentinel.
	if len(result.Listings) == marketplaceFeedPageSize {
		next := strconv.Itoa(offset + marketplaceFeedPageSize)
		h.renderPartial(w, "feed_sentinel", map[string]interface{}{
			"URL":    "/facets/marketplace/feed?offset=" + next,
			"Cursor": next,
		})
	}
}

// facetShopListings fetches a creator's Themis listings and renders them for the shop page.
// The viewer's subscription status determines which listings are visible.
// GET /facets/shop/{handle}/listings
func (h *Handler) facetShopListings(w http.ResponseWriter, r *http.Request) {
	handle := r.PathValue("handle")
	if handle == "" || h.themisURL == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	shopUser, err := dbpkg.GetUserByHandle(h.db, handle)
	if err != nil || shopUser == nil || shopUser.PIALID == "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	viewer := h.userFromRequest(w, r)

	req, err := http.NewRequestWithContext(r.Context(), "GET",
		h.themisURL+"/v1/shop/"+shopUser.PIALID, nil)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Send viewer PIAL so Themis can include subscribers_only items if subscribed
	if viewer != nil && viewer.PIALID != "" {
		req.Header.Set("X-Pial-Identity", viewer.PIALID)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	defer resp.Body.Close()

	var result struct {
		Listings []PublicListing `json:"listings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Listings) == 0 {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<div class="marketplace-empty-state"><p class="marketplace-empty-state__body">No listings yet.</p></div>`)
		return
	}

	isOwner := viewer != nil && viewer.Handle == shopUser.Handle
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	for _, listing := range result.Listings {
		if !listing.IsActive && !isOwner {
			continue
		}
		h.renderPartial(w, "marketplace_listing_card", listingCardData(listing, isOwner))
	}
}
