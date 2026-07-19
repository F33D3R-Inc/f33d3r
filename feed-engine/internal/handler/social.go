package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/realm"
)

// followEvent handles POST /events with event_type=follow|unfollow (D-070 lane).
// target_pial must be the author's PIAL UUID.
func (h *Handler) followEvent(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	targetPIAL := strings.TrimSpace(r.FormValue("target_pial"))
	if targetPIAL == "" {
		htmxError(w, r, "target_pial required", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		htmxError(w, r, "not authenticated", http.StatusUnauthorized)
		return
	}
	targetID, err := dbpkg.GetUserIDFromPIAL(h.db, targetPIAL)
	if err != nil || targetID == "" {
		htmxError(w, r, "user not found", http.StatusNotFound)
		return
	}
	if user.ID == targetID {
		htmxError(w, r, "cannot follow yourself", http.StatusBadRequest)
		return
	}

	isFollow := r.FormValue("event_type") == "follow"
	if h.db != nil {
		if isFollow {
			_ = dbpkg.FollowUser(h.db, user.ID, targetID)
			go realm.AwardXP(h.db, user.ID, "follow", targetID, realm.XPFollow)
			go func() {
				h.notifyUser(targetID, "follow", user.ID, user.ID, "user")
				go h.HeraldNotify(targetPIAL, "follow",
					"@"+user.Handle+" followed you", "",
					"", "/@"+user.Handle)
			}()
			go dbpkg.RecordFollowerEvent(h.db, user.ID, targetID, "follow")
			go dbpkg.TryAwardTrollOnFollow(h.db, targetID)
			// FA Live: push follow_accepted to the follower's SSE stream.
			// Data is just the target PIAL — JS updates all [data-pial] buttons by class.
			go PublishToUser(user.PIALID, SSEEvent{
				Type: "follow_accepted",
				Data: targetPIAL,
			})
		} else {
			_ = dbpkg.UnfollowUser(h.db, user.ID, targetID)
			go dbpkg.RecordFollowerEvent(h.db, user.ID, targetID, "unfollow")
			go dbpkg.TryAwardTrollOnFollow(h.db, targetID)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	source := r.FormValue("source")
	switch source {
	case "follow_list":
		// Followers/following list: toggle between suggested-follow-btn states.
		if isFollow {
			fmt.Fprintf(w, `<button class="suggested-follow-btn following" data-pial="%s"
				hx-post="/events" hx-target="this" hx-swap="outerHTML"
				hx-vals='{"event_type":"unfollow","target_pial":"%s","source":"follow_list"}'>Following</button>`, targetPIAL, targetPIAL)
		} else {
			fmt.Fprintf(w, `<button class="suggested-follow-btn" data-pial="%s"
				hx-post="/events" hx-target="this" hx-swap="outerHTML"
				hx-vals='{"event_type":"follow","target_pial":"%s","source":"follow_list"}'>Follow</button>`, targetPIAL, targetPIAL)
		}
	case "suggested":
		// Right-rail / who-to-follow: toggle suggested-follow-btn states.
		if isFollow {
			fmt.Fprintf(w, `<button class="suggested-follow-btn following" data-pial="%s"
				hx-post="/events" hx-target="this" hx-swap="outerHTML"
				hx-vals='{"event_type":"unfollow","target_pial":"%s","source":"suggested"}'>Following</button>`, targetPIAL, targetPIAL)
		} else {
			fmt.Fprintf(w, `<button class="suggested-follow-btn" data-pial="%s"
				hx-post="/events" hx-target="this" hx-swap="outerHTML"
				hx-vals='{"event_type":"follow","target_pial":"%s","source":"suggested"}'>Follow</button>`, targetPIAL, targetPIAL)
		}
	default:
		// Profile page: return toggled profile-follow-btn.
		if isFollow {
			fmt.Fprintf(w, `<button class="profile-follow-btn following" data-pial="%s"
				hx-post="/events" hx-target="this" hx-swap="outerHTML"
				hx-vals='{"event_type":"unfollow","target_pial":"%s"}'>Following</button>`, targetPIAL, targetPIAL)
		} else {
			fmt.Fprintf(w, `<button class="profile-follow-btn" data-pial="%s"
				hx-post="/events" hx-target="this" hx-swap="outerHTML"
				hx-vals='{"event_type":"follow","target_pial":"%s"}'>Follow</button>`, targetPIAL, targetPIAL)
		}
	}
}

func (h *Handler) followAPI(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	targetID := strings.TrimSpace(r.FormValue("target_id"))
	action   := r.FormValue("action") // "follow" | "unfollow"
	if targetID == "" {
		http.Error(w, "missing target_id", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	if user.ID == targetID {
		http.Error(w, "cannot follow yourself", http.StatusBadRequest)
		return
	}
	if h.db != nil {
		if action == "unfollow" {
			_ = dbpkg.UnfollowUser(h.db, user.ID, targetID)
		} else {
			_ = dbpkg.FollowUser(h.db, user.ID, targetID)
			go realm.AwardXP(h.db, user.ID, "follow", targetID, realm.XPFollow)
			go func() {
				h.notifyUser(targetID, "follow", user.ID, user.ID, "user")
				go h.HeraldNotify(dbpkg.GetUserPIAL(h.db, targetID), "follow",
					"@"+user.Handle+" followed you", "",
					"", "/@"+user.Handle)
			}()
		}
	}
	// Return toggled button HTML for HTMX swap
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if action == "unfollow" {
		fmt.Fprintf(w, `<button hx-post="/api/follow" hx-target="this" hx-swap="outerHTML"
			hx-vals='{"target_id":"%s","action":"follow"}' class="btn-primary follow-btn">Follow</button>`, targetID)
	} else {
		fmt.Fprintf(w, `<button hx-post="/api/follow" hx-target="this" hx-swap="outerHTML"
			hx-vals='{"target_id":"%s","action":"unfollow"}' class="btn-secondary follow-btn">Following</button>`, targetID)
	}
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
	handle  := parts[2]
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
		entries, _ = dbpkg.GetSubscribers(h.db, target.ID, 100)
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

func (h *Handler) bookmarkAPI(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	postID := strings.TrimSpace(r.FormValue("post_id"))
	action := r.FormValue("action") // "add" | "remove"
	if postID == "" {
		http.Error(w, "missing post_id", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	if h.db != nil {
		if action == "remove" {
			_ = dbpkg.RemoveBookmark(h.db, user.ID, postID)
		} else {
			_ = dbpkg.AddBookmark(h.db, user.ID, postID)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if action == "remove" {
		fmt.Fprintf(w, `<button hx-post="/api/bookmark" hx-target="this" hx-swap="outerHTML"
			hx-vals='{"post_id":"%s","action":"add"}' class="action-btn" title="Bookmark">
			<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" class="w-4 h-4"><path d="M19 21l-7-5-7 5V5a2 2 0 012-2h10a2 2 0 012 2z"/></svg></button>`, postID)
	} else {
		fmt.Fprintf(w, `<button hx-post="/api/bookmark" hx-target="this" hx-swap="outerHTML"
			hx-vals='{"post_id":"%s","action":"remove"}' class="action-btn saved" title="Remove bookmark">
			<svg viewBox="0 0 24 24" fill="currentColor" class="w-4 h-4"><path d="M19 21l-7-5-7 5V5a2 2 0 012-2h10a2 2 0 012 2z"/></svg></button>`, postID)
	}
}

func (h *Handler) feedbackAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	var req model.AethyrFeedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Persist to DB and update denormalized post_metrics asynchronously.
	// The audit trail feeds velocity scoring and offline analysis.
	go func() {
		if err := dbpkg.InsertFeedbackEvents(h.db, req.UserID, req.SessionID, req.Surface, req.Events); err != nil {
			log.Printf("[feedback] DB insert error: %v", err)
		}
		for _, ev := range req.Events {
			switch ev.EventType {
			case "impression":
				dbpkg.IncrementImpression(h.db, ev.ContentID)
			case "view_complete":
				dbpkg.IncrementImpression(h.db, ev.ContentID)
			}
		}
	}()
	h.aethyr.SendFeedback(&req)
	w.WriteHeader(http.StatusAccepted)
}
