package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/realm"
)

// followEvent handles POST /events with event_type=follow|unfollow (D-070 lane).
//
// Triggering event : POST /events {event_type: follow|unfollow, target_pial|target_handle, source}
// Server handler   : this function — resolves the target, writes the follows row
// Facet renderer   : renderFollowButton (the single btn_follow entry point)
// Produced fragment: btn_follow for the acting viewer, in the surface they clicked
// Stream mutation  : follow_state, one per surface, onto the acting viewer's own stream
//
// The target is addressed by PIAL when the caller has one and by handle otherwise
// (the user list sheet only ever knew a handle, and used to POST target_handle to
// a handler that read only target_pial — every follow from that panel 400'd).
func (h *Handler) followEvent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		htmxError(w, r, "malformed request", http.StatusBadRequest)
		return
	}
	targetPIAL := strings.TrimSpace(r.FormValue("target_pial"))
	targetHandle := strings.TrimSpace(strings.TrimPrefix(r.FormValue("target_handle"), "@"))
	if targetPIAL == "" && targetHandle == "" {
		htmxError(w, r, "target_pial or target_handle required", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		htmxError(w, r, "not authenticated", http.StatusUnauthorized)
		return
	}
	if h.db == nil {
		htmxError(w, r, "database unavailable", http.StatusServiceUnavailable)
		return
	}

	// Resolve to the full identity triple. Both the button that comes back and
	// the fragments that go on the stream need PIAL *and* handle, so whichever
	// address the caller supplied, the other is looked up here.
	var target *model.User
	var err error
	if targetPIAL != "" {
		var targetID string
		targetID, err = dbpkg.GetUserIDFromPIAL(h.db, targetPIAL)
		if err == nil && targetID != "" {
			target, err = dbpkg.GetUserByID(h.db, targetID)
		}
	} else {
		target, err = dbpkg.GetUserByHandle(h.db, targetHandle)
	}
	if err != nil || target == nil || target.ID == "" {
		log.Printf("[follow] target lookup failed (pial=%q handle=%q): %v", targetPIAL, targetHandle, err)
		htmxError(w, r, "user not found", http.StatusNotFound)
		return
	}
	targetPIAL = target.PIALID
	targetHandle = target.Handle
	if user.ID == target.ID {
		htmxError(w, r, "cannot follow yourself", http.StatusBadRequest)
		return
	}

	surface := normalizeFollowSurface(r.FormValue("source"))
	isFollow := r.FormValue("event_type") == "follow"

	// The write decides what the button says. It used to be ignored (`_ =`), so a
	// failed follow still returned a "Following" button: the click looked like it
	// worked, the next page load said otherwise, and that is the "follow does not
	// stick" the owner reported. A failed write is an error to the caller, never a
	// button that lies about server state.
	var changed bool
	if isFollow {
		changed, err = dbpkg.FollowUser(h.db, user.ID, target.ID)
	} else {
		changed, err = dbpkg.UnfollowUser(h.db, user.ID, target.ID)
	}
	if err != nil {
		log.Printf("[follow] %s %s -> %s: %v", r.FormValue("event_type"), user.ID, target.ID, err)
		htmxError(w, r, "could not save that — try again", http.StatusInternalServerError)
		return
	}

	// Side effects only when the relationship actually changed. A repeat follow
	// (ON CONFLICT DO NOTHING) must not award XP again or re-notify the target.
	if changed {
		go dbpkg.RecordFollowerEvent(h.db, user.ID, target.ID, r.FormValue("event_type"))
		go dbpkg.TryAwardTrollOnFollow(h.db, target.ID)
		if isFollow {
			go realm.AwardXP(h.db, user.ID, "follow", target.ID, realm.XPFollow)
			go func() {
				h.notifyUser(target.ID, "follow", user.ID, user.ID, "user")
				h.HeraldNotify(targetPIAL, "follow",
					"@"+user.Handle+" followed you", "",
					"", "/@"+user.Handle)
			}()
		}
	}

	// FA Live: the acting viewer's other open panels showing this person are
	// re-rendered server-side and pushed as finished fragments — on unfollow as
	// well as follow, which the old follow-only, PIAL-string event could not do.
	go h.publishFollowState(user.PIALID, targetPIAL, targetHandle, isFollow)

	fragment, err := h.renderFollowButton(targetPIAL, targetHandle, isFollow, surface)
	if err != nil {
		log.Printf("[follow] %v", err)
		htmxError(w, r, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(fragment))
}

// followAPI is the REST twin of the follow lane: POST /api/follow with target_id
// and action. It exists for callers holding an account id rather than a PIAL. It
// is converged onto the same two owners as the /events lane — dbpkg.FollowUser /
// UnfollowUser for the write, renderFollowButton for the control — so it cannot
// hand back a differently-classed button or a button that lies about a failed
// write, which is exactly what its two hand-written HTML strings used to do.
func (h *Handler) followAPI(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	targetID := strings.TrimSpace(r.FormValue("target_id"))
	action := r.FormValue("action") // "follow" | "unfollow"
	if targetID == "" {
		http.Error(w, "missing target_id", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	if user.ID == targetID {
		http.Error(w, "cannot follow yourself", http.StatusBadRequest)
		return
	}
	if h.db == nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	target, err := dbpkg.GetUserByID(h.db, targetID)
	if err != nil || target == nil || target.ID == "" {
		log.Printf("[follow-api] target lookup failed (%s): %v", targetID, err)
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	isFollow := action != "unfollow"
	var changed bool
	if isFollow {
		changed, err = dbpkg.FollowUser(h.db, user.ID, target.ID)
	} else {
		changed, err = dbpkg.UnfollowUser(h.db, user.ID, target.ID)
	}
	if err != nil {
		log.Printf("[follow-api] %s %s -> %s: %v", action, user.ID, target.ID, err)
		http.Error(w, "could not save that", http.StatusInternalServerError)
		return
	}
	if changed {
		go dbpkg.RecordFollowerEvent(h.db, user.ID, target.ID, map[bool]string{true: "follow", false: "unfollow"}[isFollow])
		go dbpkg.TryAwardTrollOnFollow(h.db, target.ID)
		if isFollow {
			go realm.AwardXP(h.db, user.ID, "follow", target.ID, realm.XPFollow)
			go func() {
				h.notifyUser(target.ID, "follow", user.ID, user.ID, "user")
				h.HeraldNotify(target.PIALID, "follow",
					"@"+user.Handle+" followed you", "",
					"", "/@"+user.Handle)
			}()
		}
	}
	go h.publishFollowState(user.PIALID, target.PIALID, target.Handle, isFollow)

	fragment, err := h.renderFollowButton(target.PIALID, target.Handle, isFollow, r.FormValue("source"))
	if err != nil {
		log.Printf("[follow-api] %v", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(fragment))
}

// userListAPI serves /api/user/{handle}/followers and /api/user/{handle}/following
// as HTML fragments for HTMX modal rendering.
func (h *Handler) userListAPI(w http.ResponseWriter, r *http.Request) {
	// Path: /api/user/{handle}/followers|following
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 {
		http.NotFound(w, r)
		return
	}
	handle := parts[2]
	listType := parts[3] // "followers" or "following"

	if listType != "followers" && listType != "following" && listType != "subscribers" {
		http.NotFound(w, r)
		return
	}

	if h.db == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<p style="color:var(--text-muted);font-size:13px">Database unavailable.</p>`))
		return
	}

	target, err := dbpkg.GetUserByHandle(h.db, handle)
	if err != nil || target == nil {
		http.NotFound(w, r)
		return
	}

	viewer := h.userFromRequest(w, r)
	var entries []dbpkg.FollowListEntry
	var title string
	switch listType {
	case "followers":
		entries, _ = dbpkg.GetFollowers(h.db, target.ID, viewer.ID, 100)
		title = "Followers"
	case "following":
		entries, _ = dbpkg.GetFollowing(h.db, target.ID, viewer.ID, 100)
		title = "Following"
	case "subscribers":
		entries, _ = dbpkg.GetSubscribers(h.db, target.ID, viewer.ID, 100)
		title = "Subscribers"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "follow_list_popup", map[string]interface{}{
		"Title":        title,
		"Handle":       handle,
		"Users":        entries,
		"ViewerHandle": viewer.Handle,
	})
}

func (h *Handler) mentionSearchAPI(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 1 || h.db == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}
	results, _ := dbpkg.SearchHandles(h.db, q, 8)
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	if results == nil {
		w.Write([]byte("[]"))
		return
	}
	enc.Encode(results)
}

// NOTE: /api/bookmark (bookmarkAPI) was removed with the posts lane. It wrote the
// bookmarks table, which is foreign-keyed to posts and which nothing reads: the
// saved surface is GetWorksSaves over work_reactions. The only markup that ever
// pointed at that route was the markup the handler printed back to itself, so no
// page could reach it — it was a loop with no entrance.

func (h *Handler) feedbackAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	var req model.AethyrFeedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Persist the audit trail asynchronously — it feeds velocity scoring and
	// offline analysis.
	//
	// This used to also bump post_metrics.impressions per event. That counter had
	// no reader: the impression number a work card shows is works.view_count, and
	// that column already has exactly one writer (the view_time event). Reviving
	// the increment against works here would have given it a second one, which is
	// how counters start disagreeing. The signal AethyrRank consumes is the
	// feedback_events row, which is still written.
	go func() {
		if err := dbpkg.InsertFeedbackEvents(h.db, req.UserID, req.SessionID, req.Surface, req.Events); err != nil {
			log.Printf("[feedback] DB insert error: %v", err)
		}
	}()
	h.aethyr.SendFeedback(&req)
	w.WriteHeader(http.StatusAccepted)
}
