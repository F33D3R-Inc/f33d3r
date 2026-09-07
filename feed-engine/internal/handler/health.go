package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
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
		// The applied schema version. Migrations are ordered and forward-only,
		// so this number says exactly what shape the database is in — which a
		// replayed idempotent DDL blob could never answer.
		"schema_version": dbpkg.SchemaVersion(h.db),
		// Whether this deployment can reach the naming plane. Queued-but-
		// undelivered graph registrations are visible here rather than only in
		// the logs.
		"manhattan":        h.manhattan.Configured(),
		"manhattan_queued": dbpkg.ManhattanOutboxPending(h.db),
		// The sports lane's request rate, per league. It is here because the
		// lane polls a public upstream on the platform's behalf and the only
		// honest way to run that is to be able to read, at any moment, how often
		// each league is being asked and when it last answered. A rate that is
		// only visible in a log nobody reads is a rate nobody is respecting.
		"sports": h.sportsHealthLine(),
	})
}
