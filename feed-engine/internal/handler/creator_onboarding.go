package handler

// creator_onboarding.go — dedicated adult creator onboarding flow (D-070 compliant).
//
// Routes:
//   GET /create/welcome  — Step 1: hero + three-pillar welcome page.
//   GET /create/setup    — Step 3: creator profile setup form.
//
// Step 2 (KYC gate) is handled by redirecting unverified users to /kyc?return_to=/create/setup.
// Step 4 (Done) is handled by the creator.setup.complete event handler in eventsPost,
//   which redirects to /create and pushes an SSE welcome banner.
//
// D-070: All writes go through POST /events (event_type=creator.setup.complete).
//        No REST write endpoints here.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// creatorWelcomePage serves GET /create/welcome — Step 1 of the adult creator onboarding flow.
// Any authenticated user can view this page; it sells the platform to prospective creators.
func (h *Handler) creatorWelcomePage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	h.render(w, r, "creator_welcome.html", map[string]interface{}{
		"User":  user,
		"Title": "Welcome, Creator · F33D3R",
	})
}

// creatorSetupPage serves GET /create/setup — Step 3 of the adult creator onboarding flow.
//
// Gate ordering:
//  1. Must be authenticated (enforced by requireHandle middleware).
//  2. Must have creator role — if not, silently enable creator mode so the form
//     still works (e.g. user chose Creator during onboarding but never saved the role).
//  3. KYC gate — unverified users are redirected to /kyc with return_to=/create/setup.
//
// The form itself renders in default state; no pre-population of content types or
// price because those are always empty for first-time setup.
func (h *Handler) creatorSetupPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// Auto-enable creator mode if not already set (supports role=creator onboardees
	// who haven't previously hit /create).
	if !user.IsCreator && h.db != nil {
		if err := dbpkg.SetCreatorMode(h.db, user.ID, true); err != nil {
			log.Printf("[creator_setup] SetCreatorMode error: %v", err)
		}
		// Reflect change on in-flight request object so template sees it.
		user.IsCreator = true
	}

	// KYC gate — creators must be identity-verified before completing setup.
	// The comment always said "identity-verified"; the condition read IsVerified,
	// which is PIAL age_verified and therefore true for every signed-up account.
	// It now asks what it says. Redirect preserves the return path so after KYC
	// the user lands back here.
	if !user.CanMonetize() {
		http.Redirect(w, r, "/kyc?return_to=/create/setup", http.StatusSeeOther)
		return
	}

	h.render(w, r, "creator_setup.html", map[string]interface{}{
		"User":  user,
		"Title": "Creator Setup · F33D3R",
	})
}

// handleCreatorSetupComplete handles event_type=creator.setup.complete (D-070 Lane 1).
//
// Payload (form or JSON):
//
//	display_name           string
//	bio                    string
//	content_types          []string  (multi-value: video, photos, music, writing, gaming, fitness)
//	is_adult               "1"|""
//	subscription_price_aet int       (AET cents, 0 = free)
//	btc_address            string    (optional)
//
// Side effects (all async-safe, fire-and-forget where possible):
//  1. Updates display_name + bio via dbpkg.SaveProfile.
//  2. Stores content_type tags in user_tags.
//  3. Calls Themis /v1/plans to create/update subscription plan if price > 0.
//  4. Calls Themis marketplace.shop.open if btc_address is provided.
//  5. Pushes SSE event creator_welcome_banner to the user's session.
//  6. Returns HX-Redirect to /create.
func (h *Handler) handleCreatorSetupComplete(w http.ResponseWriter, r *http.Request, rawBody map[string]json.RawMessage) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// The write path carried no verification check at all while the page that
	// leads to it redirected to /kyc — so the gate lived entirely in a GET and
	// POST /events {event_type:creator.setup.complete} went straight past it.
	// This handler flips is_creator, creates a Themis subscription plan, and
	// opens a marketplace shop against a caller-supplied BTC payout address
	// (steps 4 and 5 below). All three are money reaching a person, so all three
	// ask the identity predicate before any of them run.
	if !user.CanMonetize() {
		htmxError(w, r, "Creator setup requires identity verification. Complete verification at /kyc, then return here.", http.StatusForbidden)
		return
	}

	// ── 1. Parse payload ─────────────────────────────────────────────────────

	readStr := func(key string) string {
		if rawBody != nil {
			var s string
			if v, ok := rawBody[key]; ok && json.Unmarshal(v, &s) == nil {
				return strings.TrimSpace(s)
			}
		}
		return strings.TrimSpace(r.FormValue(key))
	}

	displayName := readStr("display_name")
	bio := readStr("bio")
	isAdultStr := readStr("is_adult")
	btcAddress := readStr("btc_address")
	priceStr := readStr("subscription_price_aet")

	// Multi-value content_types — form sends repeated name=value pairs.
	var contentTypes []string
	if rawBody != nil {
		if v, ok := rawBody["content_types"]; ok {
			json.Unmarshal(v, &contentTypes)
		}
	} else {
		contentTypes = r.Form["content_types"]
	}

	isAdult := isAdultStr == "1" || isAdultStr == "true"

	var priceAET int
	fmt.Sscanf(priceStr, "%d", &priceAET)
	if priceAET < 0 {
		priceAET = 0
	}
	if priceAET > 1_000_000 { // sanity cap
		priceAET = 1_000_000
	}

	// ── 2. Profile update (display_name + bio + adult flag) ───────────────────

	if h.db != nil {
		if displayName == "" {
			displayName = user.DisplayName
		}
		// IsAdultCreator rides on this write. A silent failure here leaves an adult
		// creator unflagged, which is the flag every age gate keys their content off.
		if err := dbpkg.SaveProfile(h.db, &model.ProfileSave{
			UserID:         user.ID,
			DisplayName:    truncate(displayName, 100),
			Bio:            truncate(bio, 500),
			ThemeID:        user.ThemeID,
			IsAdultCreator: isAdult,
		}); err != nil {
			log.Printf("[creator_setup] CRITICAL: profile not saved for %s (is_adult_creator=%v): %v",
				user.Handle, isAdult, err)
			htmxError(w, r, "Could not save your creator profile — please try again.", http.StatusInternalServerError)
			return
		}

		// Ensure creator mode is enabled.
		if err := dbpkg.SetCreatorMode(h.db, user.ID, true); err != nil {
			log.Printf("[creator_setup] SetCreatorMode error for %s: %v", user.Handle, err)
			htmxError(w, r, "Could not enable creator mode — please try again.", http.StatusInternalServerError)
			return
		}

		// Founding Creator window: auto-award to every creator who onboards before 2026-09-01.
		// Idempotent — GrantFoundingCreator uses ON CONFLICT DO NOTHING on founding_creator_at.
		if !user.IsFoundingCreator && time.Now().Before(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
			if err := dbpkg.GrantFoundingCreator(h.db, user.ID); err == nil {
				go dbpkg.AwardXP(h.db, user.ID, "founding_creator_badge", 500)
			}
		}

		// ── 3. Save content type tags ────────────────────────────────────────
		// Stored as a simple JSON array in user_profiles.content_types (TEXT/JSONB).
		// Gracefully no-ops if the column doesn't exist yet.
		if len(contentTypes) > 0 {
			tagsJSON, _ := json.Marshal(contentTypes)
			// Deliberately not fatal: content_types is a discovery tag list, and this
			// column may not exist on an older schema. Reported so a real failure is
			// not mistaken for that.
			if _, err := h.db.Exec(
				`UPDATE user_profiles SET content_types = $1::jsonb WHERE user_id = $2`,
				string(tagsJSON), user.ID,
			); err != nil {
				log.Printf("[creator_setup] content types not saved for %s: %v", user.Handle, err)
			}
		}

		// Mark as adult creator in user_profiles if flag set. This is the flag every
		// age gate keys this creator's content off, so it is never best-effort.
		if isAdult {
			if _, err := h.db.Exec(
				`UPDATE user_profiles SET is_adult_creator = TRUE WHERE user_id = $1`,
				user.ID,
			); err != nil {
				log.Printf("[creator_setup] CRITICAL: adult-creator flag NOT set for %s: %v", user.Handle, err)
				htmxError(w, r, "Could not save your creator profile — please try again.", http.StatusInternalServerError)
				return
			}
		}
	}

	// ── 4. Themis: create subscription plan ────────────────────────────────────

	if priceAET > 0 && h.themisURL != "" && user.PIALID != "" {
		go h.creatorSetupThemisPlan(user.PIALID, user.Handle, displayName, priceAET)
	}

	// ── 5. Themis: open marketplace shop with BTC address ───────────────────────

	if btcAddress != "" && h.themisURL != "" && user.PIALID != "" {
		go h.creatorSetupShopOpen(user.PIALID, user.Handle, btcAddress)
	}

	// ── 6. SSE: push welcome banner to the user's live session ─────────────────

	if user.PIALID != "" {
		bannerHTML := `<div class="creator-welcome-banner" role="status">` +
			`<span class="creator-welcome-banner-icon">🎉</span>` +
			`<span class="creator-welcome-banner-text">Creator profile set up! Welcome to the studio.</span>` +
			`</div>`
		PublishToUser(user.PIALID, SSEEvent{
			Type: "creator_welcome_banner",
			Data: bannerHTML,
		})
	}

	// ── 7. Redirect to creator dashboard ────────────────────────────────────────

	w.Header().Set("HX-Redirect", "/create")
	w.WriteHeader(http.StatusNoContent)
}

// creatorSetupThemisPlan calls Themis to create or update the creator's default
// subscription plan. Fire-and-forget — errors are logged but do not fail the setup.
func (h *Handler) creatorSetupThemisPlan(pialID, handle, displayName string, priceAET int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	body, _ := json.Marshal(map[string]interface{}{
		"event_type":  "plan.create_or_update",
		"pial_id":     pialID,
		"name":        displayName + " Fan Club",
		"description": "Monthly subscription to " + displayName + "'s exclusive content.",
		"price_aet":   priceAET,
		"interval":    "month",
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.themisURL+"/v1/plans", bytes.NewReader(body))
	if err != nil {
		log.Printf("[creator_setup] themis plan request build error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pial-Identity", pialID)
	req.Header.Set("X-Pial-Handle", handle)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		log.Printf("[creator_setup] themis plan error: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("[creator_setup] themis plan non-OK: %d", resp.StatusCode)
	}
}

// creatorSetupShopOpen calls Themis to open a marketplace shop with the user's BTC address.
// Fire-and-forget — errors are logged but do not fail the setup.
func (h *Handler) creatorSetupShopOpen(pialID, handle, btcAddress string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	body, _ := json.Marshal(map[string]interface{}{
		"event_type":  "marketplace.shop.open",
		"pial_id":     pialID,
		"btc_address": btcAddress,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.themisURL+"/v1/events", bytes.NewReader(body))
	if err != nil {
		log.Printf("[creator_setup] themis shop.open request build error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pial-Identity", pialID)
	req.Header.Set("X-Pial-Handle", handle)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		log.Printf("[creator_setup] themis shop.open error: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("[creator_setup] themis shop.open non-OK: %d", resp.StatusCode)
	}
}
