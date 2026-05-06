package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

func (h *Handler) healthAPI(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	engineOK := h.aethyr.Health(ctx)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"service": "feed-engine",
		"engine":  engineOK,
	})
}
