package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// shopPlans renders a creator's projected plans with the ledger's unit.
func (h *Handler) shopPlans(ctx context.Context, creatorID string) ([]model.ShopPlan, error) {
	raw, err := dbpkg.GetCreatorPlans(h.db, creatorID)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return []model.ShopPlan{}, nil
	}
	unit, err := h.aetUnit(ctx)
	if err != nil {
		return nil, err
	}
	plans := make([]model.ShopPlan, 0, len(raw))
	for _, p := range raw {
		plans = append(plans, model.ShopPlan{
			ID:           p.ID,
			Name:         p.Name,
			Description:  p.Description,
			PriceUnits:   p.PriceUnits,
			PriceDisplay: formatAETUnits(p.PriceUnits, unit),
			IsActive:     true,
		})
	}
	return plans, nil
}

// subscriptionState is what the viewer's subscription controls render from.
type subscriptionState struct {
	CreatorID     string
	CreatorHandle string
	IsSubscribed  bool
	// CancelledUntil is set when the subscription was cancelled and runs to the
	// end of its paid period.
	CancelledUntil string
	// Unavailable means the state could not be read; the controls fail closed.
	Unavailable bool
}

func (h *Handler) subscriptionStateFor(viewer, creator *model.User) subscriptionState {
	st := subscriptionState{CreatorID: creator.ID, CreatorHandle: creator.Handle}
	if viewer == nil || h.db == nil {
		return st
	}
	sub, err := dbpkg.SubscriptionFor(h.db, viewer.ID, creator.ID)
	if err != nil {
		log.Printf("[subscriptions] state for %s → %s: %v", viewer.ID, creator.ID, err)
		st.Unavailable = true
		return st
	}
	if sub != nil {
		st.IsSubscribed = true
		if sub.CancelledAt != nil {
			st.CancelledUntil = sub.ExpiresAt.Format("Jan 2")
		}
	}
	return st
}

func (h *Handler) shopPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	handle := r.PathValue("handle")
	if handle == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	shopUser, err := dbpkg.GetUserByHandle(h.db, handle)
	if err != nil || shopUser == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}

	isOwner := user != nil && user.ID == shopUser.ID

	// Themis listings for this creator's shop; the viewer's identity decides
	// which subscriber-only listings Themis includes.
	var themisListings []PublicListing
	if shopUser.PIALID != "" {
		status, body, terr := h.themisGet(r.Context(), "/v1/shop/"+shopUser.PIALID, user)
		if terr == nil && status == http.StatusOK {
			var tResult struct {
				Listings []PublicListing `json:"listings"`
			}
			if json.Unmarshal(body, &tResult) == nil {
				for _, l := range tResult.Listings {
					if l.IsActive || isOwner {
						themisListings = append(themisListings, l)
					}
				}
			}
		} else if terr != nil && terr != errThemisUnconfigured {
			log.Printf("[shop] listings for %s: status=%d err=%v", shopUser.ID, status, terr)
		}
	}

	plans, perr := h.shopPlans(r.Context(), shopUser.ID)
	plansError := ""
	if perr != nil {
		log.Printf("[shop] plans for %s: %v", shopUser.ID, perr)
		plansError = "Plans could not be loaded right now."
	}

	isEnabled := shopUser.IsCreator || len(plans) > 0
	sub := h.subscriptionStateFor(user, shopUser)

	var earnings *model.ShopEarnings
	if isOwner && shopUser.PIALID != "" {
		earnings = h.shopEarnings(r.Context(), user)
	}

	h.render(w, r, "shop.html", h.withRail(map[string]interface{}{
		"User":           user,
		"ShopUser":       shopUser,
		"IsOwner":        isOwner,
		"IsEnabled":      isEnabled,
		"Plans":          plans,
		"PlansError":     plansError,
		"Earnings":       earnings,
		"IsSubscribed":   sub.IsSubscribed,
		"Subscription":   sub,
		"PPVItems":       []model.PPVItem{},
		"ThemisListings": themisListings,
		"Title":          "@" + shopUser.Handle + " · Shop · F33D3R",
		"SessionID":      uuid.New().String(),
		"ShowScores":     h.cfg.ShowScores,
		"Themes":         ThemesWithActive(user.ThemeID),
	}, user, "default"))
}

// shopEarnings is the owner's earnings as Themis, the commerce authority,
// records them. nil when Themis cannot answer; the panel is then not drawn.
func (h *Handler) shopEarnings(ctx context.Context, owner *model.User) *model.ShopEarnings {
	status, body, err := h.themisGet(ctx, "/v1/creator/"+owner.PIALID+"/earnings", owner)
	if err != nil || status != http.StatusOK {
		log.Printf("[shop] earnings for %s: status=%d err=%v", owner.ID, status, err)
		return nil
	}
	var e struct {
		TotalDisplay      string `json:"total_display"`
		TipsDisplay       string `json:"tips_display"`
		TxCount           int    `json:"tx_count"`
		ActiveSubscribers int    `json:"active_subscribers"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		log.Printf("[shop] earnings decode for %s: %v", owner.ID, err)
		return nil
	}
	return &model.ShopEarnings{
		TotalDisplay: e.TotalDisplay,
		TxCount:      e.TxCount,
		SubsDisplay:  fmt.Sprintf("%d active", e.ActiveSubscribers),
		TipsDisplay:  e.TipsDisplay,
	}
}

func (h *Handler) enableCreatorAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	user := h.userFromRequest(w, r)
	w.Header().Set("Content-Type", "application/json")
	// Becoming a creator is the moment this account becomes payable — Themis
	// writes creator_eligibility.monetization_enabled off the back of it, and
	// every plan, PPV item and shop hangs off that row. So it asks the identity
	// predicate, not IsVerified (PIAL age_verified, which a typed-in birthday
	// sets on every signup).
	hx := r.Header.Get("HX-Request") == "true"
	// disable=true is the creator settings "Disable creator mode" intent: the
	// same route, the opposite flag. Leaving a shop is never gated on identity
	// — a gate must not be one-way — so the monetisation check applies only to
	// enabling.
	if r.FormValue("disable") == "true" {
		if h.db != nil {
			if err := dbpkg.SetCreatorMode(h.db, user.ID, false); err != nil {
				log.Printf("[creator/disable] db: %v", err)
				if hx {
					w.Header().Set("HX-Trigger", `{"f33Toast":"Could not disable creator mode — try again."}`)
				}
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"error":"db error"}`))
				return
			}
		}
		if hx {
			hxRefreshPlayground(w, r)
		}
		w.Write([]byte(`{"ok":true}`))
		return
	}
	if !user.CanMonetize() {
		if hx {
			w.Header().Set("HX-Trigger", `{"f33Toast":"Creator monetisation requires identity verification. Complete verification at /kyc."}`)
		}
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"identity_verification_required","message":"Creator monetisation requires identity verification. Complete verification at /kyc."}`))
		return
	}
	// Themis first: an account is a creator only once the commerce authority
	// will pay it. If Themis refuses, nothing local changes.
	if user.PIALID == "" {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"no_identity"}`))
		return
	}
	status, body, err := h.themisCall(r.Context(), http.MethodPost, "/v1/creator/enable", user, nil)
	if err != nil || status != http.StatusOK {
		log.Printf("[creator/enable] themis: status=%d err=%v body=%s", status, err, body)
		if hx {
			w.Header().Set("HX-Trigger", `{"f33Toast":"Could not enable creator mode — try again."}`)
		}
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"commerce_unavailable"}`))
		return
	}
	if h.db != nil {
		if err := dbpkg.SetCreatorMode(h.db, user.ID, true); err != nil {
			log.Printf("[creator/enable] db: %v", err)
			if hx {
				w.Header().Set("HX-Trigger", `{"f33Toast":"Could not enable creator mode — try again."}`)
			}
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"db error"}`))
			return
		}
	}
	if hx {
		// The page the caller is on (their shop) is now a creator's shop and the
		// nav gains its Create item: the Shell re-requests the Playground.
		hxRefreshPlayground(w, r)
	}
	w.Write([]byte(`{"ok":true}`))
}

// facetSubscribeModal — GET /facets/subscribe_modal?handle=<creator>
// The plan picker for one creator, opened into #overlay-slot by btn_subscribe.
func (h *Handler) facetSubscribeModal(w http.ResponseWriter, r *http.Request) {
	viewer := h.userFromRequest(w, r)
	if viewer == nil || viewer.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	handle := strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("handle")), "@")
	creator, err := dbpkg.GetUserByHandle(h.db, handle)
	if err != nil || creator == nil || creator.ID == viewer.ID {
		http.NotFound(w, r)
		return
	}
	plans, perr := h.shopPlans(r.Context(), creator.ID)
	notice := ""
	if perr != nil {
		log.Printf("[subscribe] plans for %s: %v", creator.ID, perr)
		notice = "Plans could not be loaded right now."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "subscribe_modal", map[string]interface{}{
		"CreatorID":     creator.ID,
		"CreatorHandle": creator.Handle,
		"Plans":         plans,
		"Notice":        notice,
	})
}

// subscribeToCreator — POST /api/subscribe {creator_id, plan_id}
//
//	triggering event  a plan's Subscribe control
//	server handler    this: Themis charges the plan's own price, read from the
//	                  plan server-side, to the session's identity
//	Facet renderer    subscribe_result on refusal; on success the overlay is
//	                  cleared and the Shell re-requests the Playground
//	stream mutation   commerce.events projects the subscription; this answer
//	                  waits for the projection so the redrawn page shows it
func (h *Handler) subscribeToCreator(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	user := h.userFromRequest(w, r)
	creatorID := strings.TrimSpace(r.FormValue("creator_id"))
	planID := strings.TrimSpace(r.FormValue("plan_id"))
	if _, err := uuid.Parse(planID); err != nil {
		h.subscribeRefused(w, r, "Choose a plan.", http.StatusBadRequest)
		return
	}
	if creatorID == "" || user.ID == creatorID {
		h.subscribeRefused(w, r, "You cannot subscribe to yourself.", http.StatusBadRequest)
		return
	}
	if user.PIALID == "" {
		h.subscribeRefused(w, r, "Your account has no identity to subscribe with.", http.StatusForbidden)
		return
	}

	// The creator on the receiving end must be identity-verified before a
	// subscription can credit them. Resolved from PIAL, the system of record.
	creator, err := dbpkg.GetUserByID(h.db, creatorID)
	if err != nil || creator == nil {
		h.subscribeRefused(w, r, "That creator was not found.", http.StatusNotFound)
		return
	}
	ps := dbpkg.LoadPIALState(h.db, creator.PIALID)
	if ps == nil || !(model.KYCTierIsIdentity(ps.KYCTier) || ps.Role == model.RoleAdmin || ps.Role == model.RoleFounder) {
		h.subscribeRefused(w, r, "This creator has not completed identity verification and cannot accept subscriptions yet.", http.StatusForbidden)
		return
	}

	status, body, err := h.themisCall(r.Context(), http.MethodPost, "/v1/subscribe", user, map[string]interface{}{
		"plan_id":            planID,
		"subscriber_account": user.ID,
	})
	if err != nil {
		log.Printf("[subscribe] themis: %v", err)
		h.subscribeRefused(w, r, "Subscriptions are unavailable right now. Nothing was charged.", http.StatusBadGateway)
		return
	}
	if status != http.StatusOK {
		// Every non-success answer means no subscription: a decline, a
		// refusal, or a payment still settling (which the reconciler finishes
		// and the projection then shows).
		h.subscribeRefused(w, r, themisRefusal(body, "The subscription did not go through."), status)
		return
	}
	var sub struct {
		SubscriptionID string `json:"subscription_id"`
		Status         string `json:"status"`
	}
	if err := json.Unmarshal(body, &sub); err != nil || sub.SubscriptionID == "" {
		log.Printf("[subscribe] themis answer unreadable: %v", err)
		h.subscribeRefused(w, r, "The subscription went through but could not be shown. Reload the page.", http.StatusBadGateway)
		return
	}
	h.awaitProjection(r.Context(), sub.SubscriptionID, sub.Status == "active", false)
	h.subscriptionChanged(w, r)
}

// unsubscribeFromCreator — POST /api/unsubscribe {creator_id}
// The subscription runs to the end of the period it paid for and is not
// renewed.
func (h *Handler) unsubscribeFromCreator(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	user := h.userFromRequest(w, r)
	creator, err := dbpkg.GetUserByID(h.db, strings.TrimSpace(r.FormValue("creator_id")))
	if err != nil || creator == nil || creator.PIALID == "" {
		h.subscribeRefused(w, r, "That creator was not found.", http.StatusNotFound)
		return
	}
	status, body, err := h.themisCall(r.Context(), http.MethodDelete, "/v1/subscribe", user, map[string]interface{}{
		"creator_pial_id": creator.PIALID,
	})
	if err != nil {
		log.Printf("[unsubscribe] themis: %v", err)
		h.subscribeRefused(w, r, "Subscriptions are unavailable right now.", http.StatusBadGateway)
		return
	}
	if status != http.StatusOK {
		h.subscribeRefused(w, r, themisRefusal(body, "The subscription could not be cancelled."), status)
		return
	}
	var sub struct {
		SubscriptionID string `json:"subscription_id"`
	}
	if json.Unmarshal(body, &sub) == nil && sub.SubscriptionID != "" {
		h.awaitProjection(r.Context(), sub.SubscriptionID, true, true)
	}
	h.subscriptionChanged(w, r)
}

// awaitProjection waits, briefly, until this brain's projection shows the
// subscription as Themis just answered, so the page redrawn next reads its own
// write. The projection follows commerce.events within moments; if it has not
// caught up in time the page is redrawn anyway and the next render shows it.
func (h *Handler) awaitProjection(ctx context.Context, subscriptionID string, active, cancelled bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		ok, err := dbpkg.SubscriptionProjected(ctx, h.db, subscriptionID, active, cancelled)
		if err == nil && ok {
			return
		}
		if err != nil {
			log.Printf("[subscribe] projection check for %s: %v", subscriptionID, err)
		}
		select {
		case <-ctx.Done():
			log.Printf("[subscribe] projection of %s did not catch up within 3s", subscriptionID)
			return
		case <-tick.C:
		}
	}
}

// subscriptionChanged answers a subscription change: the overlay is cleared and
// the Shell re-requests the Playground, whose gates and controls the server
// redraws from the projection.
func (h *Handler) subscriptionChanged(w http.ResponseWriter, r *http.Request) {
	if facetRequest(r) {
		hxRefreshPlayground(w, r)
		w.Header().Set("HX-Retarget", "#overlay-slot")
		w.Header().Set("HX-Reswap", "innerHTML")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// subscribeRefused renders the subscribe_result Fragment with Themis's (or
// this server's) reason.
func (h *Handler) subscribeRefused(w http.ResponseWriter, r *http.Request, msg string, code int) {
	if facetRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		h.renderPartial(w, "subscribe_result", map[string]interface{}{"Message": msg})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
