package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

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

	isOwner := user != nil && user.Handle == shopUser.Handle

	// Fetch Themis listings for this creator's shop
	var themisListings []PublicListing
	if h.themisURL != "" && shopUser.PIALID != "" {
		// Build the Themis request — include viewer PIAL for subscription visibility
		tReq, terr := http.NewRequestWithContext(r.Context(), "GET",
			h.themisURL+"/v1/shop/"+shopUser.PIALID, nil)
		if terr == nil {
			if user != nil && user.PIALID != "" {
				tReq.Header.Set("X-Pial-Identity", user.PIALID)
			}
			tResp, terr2 := h.httpClient.Do(tReq)
			if terr2 == nil && tResp.StatusCode == http.StatusOK {
				var tResult struct {
					Listings []PublicListing `json:"listings"`
				}
				if json.NewDecoder(tResp.Body).Decode(&tResult) == nil {
					for _, l := range tResult.Listings {
						if l.IsActive || isOwner {
							themisListings = append(themisListings, l)
						}
					}
				}
				tResp.Body.Close()
			}
		}
	}

	rawPlans, _ := dbpkg.GetCreatorPlans(h.db, shopUser.ID)
	plans := make([]model.ShopPlan, 0, len(rawPlans))
	for _, p := range rawPlans {
		plans = append(plans, model.ShopPlan{
			ID:           p.ID,
			Name:         p.Name,
			Description:  p.Description,
			PriceAET:     p.PriceAet,
			PriceDisplay: fmt.Sprintf("%.2f", float64(p.PriceAet)/100.0),
			IsActive:     true,
		})
	}

	isEnabled := shopUser.IsCreator || len(plans) > 0
	isSubscribed := user != nil && h.db != nil && dbpkg.IsSubscribed(h.db, user.ID, shopUser.ID)

	var earnings *model.ShopEarnings
	if isOwner && h.db != nil {
		subCount := dbpkg.GetSubscriberCount(h.db, shopUser.ID)
		totalAet := 0
		for _, p := range rawPlans {
			totalAet += p.PriceAet * subCount
		}
		earnings = &model.ShopEarnings{
			TotalDisplay: fmt.Sprintf("%.2f", float64(totalAet)/100.0),
			TxCount:      subCount,
			SubsDisplay:  fmt.Sprintf("%d active", subCount),
			TipsDisplay:  "–",
		}
	}

	h.render(w, r, "shop.html", h.withRail(map[string]interface{}{
		"User":           user,
		"ShopUser":       shopUser,
		"IsOwner":        isOwner,
		"IsEnabled":      isEnabled,
		"Plans":          plans,
		"Earnings":       earnings,
		"IsSubscribed":   isSubscribed,
		"PPVItems":       []model.PPVItem{},
		"ThemisListings": themisListings,
		"Title":          "@" + shopUser.Handle + " · Shop · F33D3R",
		"SessionID":      uuid.New().String(),
		"ShowScores":     h.cfg.ShowScores,
		"Themes":         ThemesWithActive(user.ThemeID),
	}, user, "default"))
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
			// The creator dashboard the caller is on no longer applies: the
			// Shell re-requests the Playground and the server draws what a
			// non-creator sees there.
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
	// Fire-and-forget: notify Themis commerce so creator_eligibility stays in sync
	if h.cfg.ThemisURL != "" && user.PIALID != "" {
		go func() {
			body, _ := json.Marshal(map[string]string{"pial_id": user.PIALID})
			req, _ := http.NewRequest(http.MethodPost, h.cfg.ThemisURL+"/v1/creator/enable", bytes.NewReader(body))
			if req != nil {
				req.Header.Set("Content-Type", "application/json")
				// Themis now requires proof that the caller IS the identity it is
				// acting for — a body-supplied PIAL is a claim, not an authority.
				// This call is made server-side for the authenticated viewer, so
				// it carries their session-derived PIAL exactly as the /themis
				// proxy does.
				req.Header.Set("X-Pial-Identity", user.PIALID)
				req.Header.Set("X-Pial-Handle", user.Handle)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				h.httpClient.Do(req.WithContext(ctx))
			}
		}()
	}
	w.Write([]byte(`{"ok":true}`))
}

func (h *Handler) getCreatorPlans(w http.ResponseWriter, r *http.Request) {
	creatorHandle := r.URL.Query().Get("handle")
	if creatorHandle == "" {
		http.Error(w, "handle required", 400)
		return
	}
	creator, _ := dbpkg.GetUserByHandle(h.db, creatorHandle)
	if creator == nil {
		http.Error(w, "not found", 404)
		return
	}
	plans, err := dbpkg.GetCreatorPlans(h.db, creator.ID)
	if err != nil {
		http.Error(w, "error", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(plans)
}

func (h *Handler) subscribeToCreator(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	_ = r.ParseMultipartForm(32 << 20)
	user := h.userFromRequest(w, r)
	creatorID := r.FormValue("creator_id")
	planID := r.FormValue("plan_id")
	priceStr := r.FormValue("price_aet")
	if creatorID == "" {
		http.Error(w, "creator_id required", 400)
		return
	}
	if user.ID == creatorID {
		http.Error(w, "cannot subscribe to yourself", 400)
		return
	}

	// The creator on the receiving end must be identity-verified before a
	// subscription can credit them. The gate belongs here and not on the
	// subscriber: the subscriber is paying, and the person the platform has to
	// be able to name is the one being paid. Resolved from PIAL, the system of
	// record, so it cannot be stale on a projection column.
	if h.db != nil {
		creator, _ := dbpkg.GetUserByID(h.db, creatorID)
		if creator == nil {
			http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
			return
		}
		ps := dbpkg.LoadPIALState(h.db, creator.PIALID)
		if ps == nil || !(model.KYCTierIsIdentity(ps.KYCTier) || ps.Role == model.RoleAdmin || ps.Role == model.RoleFounder) {
			http.Error(w, `{"error":"creator_not_verified","message":"This creator has not completed identity verification and cannot accept subscriptions yet."}`, http.StatusForbidden)
			return
		}
	}

	priceAet := 100 // default 1 AET
	if priceStr != "" {
		if v, err := strconv.Atoi(priceStr); err == nil {
			priceAet = v
		}
	}

	// Deduct from wallet via ain-soph
	if h.cfg.AinSophURL != "" && user.PIALID != "" {
		creator, _ := dbpkg.GetUserByID(h.db, creatorID)
		var creatorPIAL string
		if creator != nil {
			creatorPIAL = creator.PIALID
		}
		if creatorPIAL != "" {
			body := fmt.Sprintf(`{"from_pial_id":%q,"to_pial_id":%q,"amount_aet":%s,"idempotency_key":%q}`,
				user.PIALID, creatorPIAL,
				fmt.Sprintf("%.2f", float64(priceAet)/100.0),
				fmt.Sprintf("sub_%s_%s", user.ID, creatorID),
			)
			ctx2, cancel2 := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel2()
			req2, _ := http.NewRequestWithContext(ctx2, http.MethodPost, h.cfg.AinSophURL+"/v1/tip",
				strings.NewReader(body))
			if req2 != nil {
				req2.Header.Set("Content-Type", "application/json")
				// The subscriber is the payer. Ain Soph takes the payer from this
				// header, never from from_pial_id in the body.
				req2.Header.Set("X-Pial-Identity", user.PIALID)
				resp2, err2 := http.DefaultClient.Do(req2)
				if err2 != nil || resp2.StatusCode == 422 {
					http.Error(w, `{"error":"insufficient_balance","message":"Not enough AET to subscribe."}`, 422)
					return
				}
				if resp2 != nil {
					resp2.Body.Close()
				}
			}
		}
	}

	if err := dbpkg.CreateSubscription(h.db, user.ID, creatorID, planID, priceAet, 30); err != nil {
		log.Printf("subscribe: %v", err)
		http.Error(w, "subscription failed", 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	fmt.Fprintf(w, `{"status":"subscribed","expires_in_days":30}`)
}

func (h *Handler) unsubscribeFromCreator(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	_ = r.ParseMultipartForm(32 << 20)
	user := h.userFromRequest(w, r)
	creatorID := r.FormValue("creator_id")
	if creatorID == "" {
		http.Error(w, "creator_id required", 400)
		return
	}
	if err := dbpkg.CancelSubscription(h.db, user.ID, creatorID); err != nil {
		http.Error(w, "unsubscribe failed", 500)
		return
	}
	w.WriteHeader(200)
	fmt.Fprint(w, `{"status":"cancelled"}`)
}
