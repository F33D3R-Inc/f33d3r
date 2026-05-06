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
	user   := h.userFromRequest(w, r)
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

	isEnabled    := shopUser.IsCreator || len(plans) > 0
	isSubscribed := user != nil && h.db != nil && dbpkg.IsSubscribed(h.db, user.ID, shopUser.ID)

	var earnings *model.ShopEarnings
	if isOwner && h.db != nil {
		subCount := dbpkg.GetSubscriberCount(h.db, shopUser.ID)
		totalAet  := 0
		for _, p := range rawPlans { totalAet += p.PriceAet * subCount }
		earnings = &model.ShopEarnings{
			TotalDisplay: fmt.Sprintf("%.2f", float64(totalAet)/100.0),
			TxCount:      subCount,
			SubsDisplay:  fmt.Sprintf("%d active", subCount),
			TipsDisplay:  "–",
		}
	}

	h.render(w, "shop.html", map[string]interface{}{
		"User":         user,
		"ShopUser":     shopUser,
		"IsOwner":      isOwner,
		"IsEnabled":    isEnabled,
		"Plans":        plans,
		"Earnings":     earnings,
		"IsSubscribed": isSubscribed,
		"PPVItems":     []model.PPVItem{},
		"Title":        "@" + shopUser.Handle + " · Shop · F33D3R",
		"SessionID":    uuid.New().String(),
		"ShowScores":   h.cfg.ShowScores,
		"Themes":       ThemesWithActive(user.ThemeID),
	})
}

func (h *Handler) enableCreatorAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	user := h.userFromRequest(w, r)
	w.Header().Set("Content-Type", "application/json")
	if h.db != nil {
		if err := dbpkg.SetCreatorMode(h.db, user.ID, true); err != nil {
			log.Printf("[creator/enable] db: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"db error"}`))
			return
		}
	}
	// Fire-and-forget: also notify Thessalon so commerce DB stays in sync
	if h.cfg.ThessalonURL != "" && user.PIALID != "" {
		go func() {
			body, _ := json.Marshal(map[string]string{"pial_id": user.PIALID})
			req, _ := http.NewRequest(http.MethodPost, h.cfg.ThessalonURL+"/v1/creator/enable", bytes.NewReader(body))
			if req != nil {
				req.Header.Set("Content-Type", "application/json")
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
	if creatorHandle == "" { http.Error(w, "handle required", 400); return }
	creator, _ := dbpkg.GetUserByHandle(h.db, creatorHandle)
	if creator == nil { http.Error(w, "not found", 404); return }
	plans, err := dbpkg.GetCreatorPlans(h.db, creator.ID)
	if err != nil { http.Error(w, "error", 500); return }
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(plans)
}

func (h *Handler) subscribeToCreator(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	_ = r.ParseForm()
	user        := h.userFromRequest(w, r)
	creatorID   := r.FormValue("creator_id")
	planID      := r.FormValue("plan_id")
	priceStr    := r.FormValue("price_aet")
	if creatorID == "" { http.Error(w, "creator_id required", 400); return }
	if user.ID == creatorID { http.Error(w, "cannot subscribe to yourself", 400); return }

	priceAet := 100 // default 1 AET
	if priceStr != "" {
		if v, err := strconv.Atoi(priceStr); err == nil { priceAet = v }
	}

	// Deduct from wallet via ain-soph
	if h.cfg.AinSophURL != "" && user.PIALID != "" {
		creator, _ := dbpkg.GetUserByID(h.db, creatorID)
		var creatorPIAL string
		if creator != nil { creatorPIAL = creator.PIALID }
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
				resp2, err2 := http.DefaultClient.Do(req2)
				if err2 != nil || resp2.StatusCode == 422 {
					http.Error(w, `{"error":"insufficient_balance","message":"Not enough AET to subscribe."}`, 422)
					return
				}
				if resp2 != nil { resp2.Body.Close() }
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
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	_ = r.ParseForm()
	user      := h.userFromRequest(w, r)
	creatorID := r.FormValue("creator_id")
	if creatorID == "" { http.Error(w, "creator_id required", 400); return }
	if err := dbpkg.CancelSubscription(h.db, user.ID, creatorID); err != nil {
		http.Error(w, "unsubscribe failed", 500)
		return
	}
	w.WriteHeader(200)
	fmt.Fprint(w, `{"status":"cancelled"}`)
}
