package nexus

import (
	"database/sql"
	"fmt"
	"net/http"
)

// HandleEvent processes nexus.* event types dispatched from the main /events handler.
// user is the authenticated user making the request — required for persona switch validation.
func HandleEvent(w http.ResponseWriter, r *http.Request, eventType string, client *Client, pialID string, db *sql.DB) bool {
	switch eventType {
	case "nexus.persona.switched":
		handlePersonaSwitched(w, r, client, pialID)
	case "nexus.persona.link.initiated":
		handleLinkInitiated(w, r, client, pialID, db)
	case "nexus.persona.link.confirmed":
		handleLinkConfirmed(w, r, client, pialID)
	case "nexus.persona.link.declined":
		handleLinkDeclined(w, r, client, pialID)
	case "nexus.unified_view.toggled":
		handleUnifiedViewToggled(w, r)
	default:
		return false
	}
	return true
}

// handleLinkInitiated resolves the target handle to a pial_shard_id and sends
// a pending link request to Verity. The target must confirm — no self-approval.
func handleLinkInitiated(w http.ResponseWriter, r *http.Request, client *Client, pialID string, db *sql.DB) {
	targetHandle := r.FormValue("target_handle")
	if targetHandle == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<span class="nexus-link-error">Handle required.</span>`)
		return
	}
	// Strip leading @ if present.
	if len(targetHandle) > 0 && targetHandle[0] == '@' {
		targetHandle = targetHandle[1:]
	}

	// Resolve target handle → pial_id.
	var targetPIALID string
	err := db.QueryRow(`SELECT pial_id FROM users WHERE handle = $1 AND pial_id IS NOT NULL`, targetHandle).Scan(&targetPIALID)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err == sql.ErrNoRows {
			fmt.Fprintf(w, `<span class="nexus-link-error">Account @%s not found.</span>`, targetHandle)
		} else {
			fmt.Fprint(w, `<span class="nexus-link-error">Something went wrong — try again.</span>`)
		}
		return
	}

	sourceShard := PersonaShardFromPIAL(pialID)
	targetShard := PersonaShardFromPIAL(targetPIALID)

	res, err := client.InitiateLink(sourceShard, targetShard)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<span class="nexus-link-error">Could not send request — try again.</span>`)
		return
	}
	if res.Result == "conflict" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<span class="nexus-link-error">@%s is already linked to another identity.</span>`, targetHandle)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<span class="nexus-link-success">
  Link request sent to @%s — they'll see it in their account settings.
</span>`, targetHandle)
}

// handlePersonaSwitched validates that target_pial_shard_id belongs to the
// current user's NEXUS, then redirects to reload the feed under the new persona.
func handlePersonaSwitched(w http.ResponseWriter, r *http.Request, client *Client, pialID string) {
	targetShard := r.FormValue("target_pial_shard_id")
	if targetShard == "" {
		http.Error(w, "target_pial_shard_id required", http.StatusBadRequest)
		return
	}

	// Validate the caller's current shard and the target are in the same NEXUS.
	callerShard := PersonaShardFromPIAL(pialID)
	if callerShard == targetShard {
		// Already on this persona — redirect cleanly.
		w.Header().Set("HX-Redirect", "/")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Ask Verity for the persona list for the caller's shard.
	// If target is in the list, the switch is authorised.
	personas, err := client.ListPersonas(callerShard)
	if err != nil || personas == nil {
		http.Error(w, "not in a nexus", http.StatusForbidden)
		return
	}
	authorised := false
	for _, p := range personas.Personas {
		if p.PIALShardID == targetShard {
			authorised = true
			break
		}
	}
	if !authorised {
		http.Error(w, "target persona not in your nexus", http.StatusForbidden)
		return
	}

	// Invalidate in-process session cache for both shards so the next
	// request gets a fresh session reflecting the active persona change.
	InvalidateSession(pialID)

	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}

// handleLinkConfirmed calls Verity to confirm a pending link request,
// then dismisses the modal and refreshes the persona switcher.
func handleLinkConfirmed(w http.ResponseWriter, r *http.Request, client *Client, pialID string) {
	requestID := r.FormValue("request_id")
	if requestID == "" {
		http.Error(w, "request_id required", http.StatusBadRequest)
		return
	}

	callerShard := PersonaShardFromPIAL(pialID)
	if err := client.ConfirmLink(requestID, callerShard); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div class="inline-error" role="alert">
  <span class="inline-error__icon">⚠</span>
  <span class="inline-error__msg">Could not confirm link — request may have expired.</span>
</div>`)
		return
	}

	// Bust session cache so the persona switcher picks up the new persona.
	InvalidateSession(pialID)

	// Close modal + trigger persona switcher refresh via HTMX out-of-band swap.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<div id="modal-root"></div>
<div id="persona-switcher"
     hx-get="/partials/nexus/persona-switcher"
     hx-trigger="load"
     hx-swap="outerHTML"
     hx-swap-oob="true"></div>`)
}

// handleLinkDeclined calls Verity to decline a pending link request.
func handleLinkDeclined(w http.ResponseWriter, r *http.Request, client *Client, pialID string) {
	requestID := r.FormValue("request_id")
	if requestID == "" {
		http.Error(w, "request_id required", http.StatusBadRequest)
		return
	}

	callerShard := PersonaShardFromPIAL(pialID)
	client.DeclineLink(requestID, callerShard) // best-effort; errors are non-fatal

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<div id="modal-root"></div>`)
}

// handleUnifiedViewToggled renders the unified view for the requested mode.
func handleUnifiedViewToggled(w http.ResponseWriter, r *http.Request) {
	mode := r.FormValue("mode")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	switch mode {
	case "notifications":
		fmt.Fprint(w, `<div class="unified-view" data-mode="notifications">
  <div class="unified-view-header">
    <span class="unified-view-title">All Notifications</span>
  </div>
  <div class="unified-view-body"
       hx-get="/partials/nexus/unified-notifications"
       hx-trigger="load"
       hx-swap="innerHTML">
    <div class="post-card post-card--skeleton"></div>
    <div class="post-card post-card--skeleton"></div>
  </div>
</div>`)
	case "messages":
		fmt.Fprint(w, `<div class="unified-view" data-mode="messages">
  <div class="unified-view-header">
    <span class="unified-view-title">All Messages</span>
  </div>
  <div class="unified-view-body"
       hx-get="/partials/nexus/unified-messages"
       hx-trigger="load"
       hx-swap="innerHTML">
    <div class="post-card post-card--skeleton"></div>
    <div class="post-card post-card--skeleton"></div>
  </div>
</div>`)
	case "earnings":
		fmt.Fprint(w, `<div class="unified-view" data-mode="earnings">
  <div class="unified-view-header">
    <span class="unified-view-title">All Earnings</span>
  </div>
  <div class="unified-view-body"
       hx-get="/partials/nexus/unified-earnings"
       hx-trigger="load"
       hx-swap="innerHTML">
    <div class="post-card post-card--skeleton"></div>
    <div class="post-card post-card--skeleton"></div>
  </div>
</div>`)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}
