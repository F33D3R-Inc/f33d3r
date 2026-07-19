package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// marketplacePage serves the public marketplace feed (GET /marketplace).
func (h *Handler) marketplacePage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || !user.IsVerified {
		http.Redirect(w, r, "/kyc", http.StatusFound)
		return
	}
	h.render(w, "marketplace.html", map[string]interface{}{
		"User":    user,
		"Surface": "marketplace",
		"Title":   "Marketplace",
		"Themes":  ThemesWithActive(user.ThemeID),
	})
}

// sellerDashboardMarketPage serves the seller's marketplace dashboard (GET /create/marketplace).
func (h *Handler) sellerDashboardMarketPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || !user.IsVerified {
		http.Redirect(w, r, "/kyc", http.StatusFound)
		return
	}

	// Pre-fetch shop status so the template renders the correct initial state server-side.
	shopExists := false
	btcAddress := ""
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
				}
				if json.NewDecoder(resp.Body).Decode(&status) == nil {
					shopExists = status.Exists && status.IsActive
					btcAddress = status.BtcAddress
				}
				resp.Body.Close()
			}
		}
	}

	h.render(w, "seller_dashboard_market.html", map[string]interface{}{
		"User":       user,
		"Surface":    "seller_dashboard",
		"Title":      "Your Shop",
		"Themes":     ThemesWithActive(user.ThemeID),
		"PIALID":     user.PIALID,
		"ShopExists": shopExists,
		"BtcAddress": btcAddress,
	})
}

// buyerLibraryPage serves the buyer's purchased content library (GET /library).
func (h *Handler) buyerLibraryPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	h.render(w, "buyer_library.html", map[string]interface{}{
		"User":    user,
		"Surface": "library",
		"Title":   "Library",
		"Themes":  ThemesWithActive(user.ThemeID),
	})
}

// facetMarketplaceListingForm serves the listing creation form (GET /facets/marketplace_listing_form).
func (h *Handler) facetMarketplaceListingForm(w http.ResponseWriter, r *http.Request) {
	h.renderPartial(w, "marketplace_listing_form", map[string]interface{}{})
}

// PublicListing is a safe subset of a Themis listing for browser rendering.
// content_url and cek_encrypted are deliberately omitted — only delivered post-purchase.
type PublicListing struct {
	ID          string `json:"id"`
	ShopPial    string `json:"shop_pial"`
	Title       string `json:"title"`
	Description string `json:"description"`
	PriceSats   int64  `json:"price_sats"`
	Visibility  string `json:"visibility"`
	IsActive    bool   `json:"is_active"`
	CreatedAt   string `json:"created_at"`
}

// facetMarketplaceFeed fetches public listings from Themis and renders them as HTML cards.
// GET /facets/marketplace/feed
func (h *Handler) facetMarketplaceFeed(w http.ResponseWriter, r *http.Request) {
	if h.themisURL == "" {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<div class="marketplace-empty-state"><p class="marketplace-empty-state__body">Marketplace unavailable.</p></div>`)
		return
	}

	offset := r.URL.Query().Get("offset")
	if offset == "" {
		offset = "0"
	}

	req, err := http.NewRequestWithContext(r.Context(), "GET",
		h.themisURL+"/v1/marketplace?limit=20&offset="+offset, nil)
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
		h.renderPartial(w, "marketplace_listing_card", map[string]interface{}{
			"ID":          listing.ID,
			"Title":       listing.Title,
			"Description": listing.Description,
			"PriceSats":   listing.PriceSats,
			"Visibility":  listing.Visibility,
			"IsActive":    listing.IsActive,
			"IsSeller":    false,
			"ShopPial":    listing.ShopPial,
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
		h.renderPartial(w, "marketplace_listing_card", map[string]interface{}{
			"ID":          listing.ID,
			"Title":       listing.Title,
			"Description": listing.Description,
			"PriceSats":   listing.PriceSats,
			"Visibility":  listing.Visibility,
			"IsActive":    listing.IsActive,
			"IsSeller":    isOwner,
			"ShopPial":    listing.ShopPial,
		})
	}
}
