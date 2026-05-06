package handler

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/realm"
)

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
				_ = dbpkg.CreateNotification(h.db, targetID, "follow", user.ID, user.ID, "user")
				dbpkg.IncrementUnreadCount(h.db, targetID)
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

	if listType != "followers" && listType != "following" {
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

	var entries []dbpkg.FollowListEntry
	if listType == "followers" {
		entries, _ = dbpkg.GetFollowers(h.db, target.ID, 100)
	} else {
		entries, _ = dbpkg.GetFollowing(h.db, target.ID, 100)
	}

	title := "Followers"
	if listType == "following" {
		title = "Following"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<div style="padding-bottom:4px;margin-bottom:14px;border-bottom:1px solid var(--border);font-size:14px;font-weight:600;color:var(--text-primary)">%s — @%s</div>`, title, template.HTMLEscapeString(handle))
	if len(entries) == 0 {
		fmt.Fprintf(w, `<p style="font-size:13px;color:var(--text-muted)">None yet.</p>`)
		return
	}
	for _, e := range entries {
		av := ""
		if e.AvatarURL != "" {
			av = fmt.Sprintf(`<img src="%s" style="width:100%%;height:100%%;object-fit:cover;border-radius:50%%">`, template.HTMLEscapeString(e.AvatarURL))
		} else {
			colors := AvatarColors(e.Handle)
			av = fmt.Sprintf(`<span style="width:100%%;height:100%%;border-radius:50%%;background:linear-gradient(135deg,%s,%s);display:flex;align-items:center;justify-content:center;font-size:13px;font-weight:700;color:#fff">%s</span>`,
				colors[0], colors[1], strings.ToUpper(string([]rune(e.DisplayName)[0:1])))
		}
		fmt.Fprintf(w, `<a href="/u/%s" style="display:flex;align-items:center;gap:10px;padding:10px 0;border-bottom:1px solid color-mix(in srgb,var(--border) 50%%,transparent);text-decoration:none">
			<div class="avatar-sm" style="flex-shrink:0">%s</div>
			<div><p style="font-size:13px;font-weight:500;color:var(--text-primary)">%s</p>
			<p style="font-size:11px;color:var(--text-muted)">@%s</p></div>
		</a>`, template.HTMLEscapeString(e.Handle), av, template.HTMLEscapeString(e.DisplayName), template.HTMLEscapeString(e.Handle))
	}
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
			}
		}
	}()
	h.aethyr.SendFeedback(&req)
	w.WriteHeader(http.StatusAccepted)
}
