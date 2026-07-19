package handler

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

func (h *Handler) listsPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil { return }
	lists, _ := dbpkg.GetUserLists(h.db, user.ID)
	h.render(w, "lists.html", map[string]interface{}{
		"SessionID":          uuid.New().String(),
		"CurrentUserHandle":  user.Handle,
		"User":               user,
		"Lists":              lists,
		"ShowScores":         h.cfg.ShowScores,
	})
}

func (h *Handler) listDetailPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil { return }
	listID := r.PathValue("id")
	list, err := dbpkg.GetPublicList(h.db, listID)
	if err != nil || list == nil {
		http.NotFound(w, r)
		return
	}
	before := r.URL.Query().Get("before")
	works, _ := dbpkg.GetWorksByList(h.db, listID, h.cfg.FeedPageSize+1, before)
	members, _ := dbpkg.GetListMembers(h.db, listID)
	dbpkg.EnrichWorksWithReactions(h.db, works, user.ID)
	dbpkg.EnrichWorksWithQuotes(h.db, works)
	dbpkg.EnrichWorksWithLinkPreviews(h.db, works)
	var nextBefore string
	if len(works) > h.cfg.FeedPageSize {
		works = works[:h.cfg.FeedPageSize]
		nextBefore = works[len(works)-1].CreatedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	works = filterAdultForViewer(user, works)
	h.render(w, "list_detail.html", map[string]interface{}{
		"SessionID":         uuid.New().String(),
		"CurrentUserHandle": user.Handle,
		"User":              user,
		"List":              list,
		"Works":             works,
		"Members":           members,
		"Before":            nextBefore,
		"ShowScores":        h.cfg.ShowScores,
	})
}

func (h *Handler) postAnalyticsPartial(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil { return }
	postID := r.PathValue("id")
	a, err := dbpkg.GetPostAnalytics(h.db, postID, user.ID)
	if err != nil {
		htmxError(w, r, "Analytics not available", http.StatusForbidden)
		return
	}
	h.renderPartial(w, "post_analytics", a)
}

// listEvent handles list-related mutations via POST /events:
// create_list, delete_list, list_add_member, list_remove_member
func (h *Handler) listEvent(w http.ResponseWriter, r *http.Request, eventType string) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch eventType {
	case "create_list":
		name := strings.TrimSpace(r.FormValue("name"))
		if name == "" {
			htmxError(w, r, "List name required", http.StatusBadRequest)
			return
		}
		desc := strings.TrimSpace(r.FormValue("description"))
		isPublic := r.FormValue("is_public") != "false"
		id, err := dbpkg.CreateList(h.db, user.ID, name, desc, isPublic)
		if err != nil {
			htmxError(w, r, "Could not create list", http.StatusInternalServerError)
			return
		}
		w.Header().Set("HX-Redirect", "/lists/"+id)
		w.WriteHeader(http.StatusOK)

	case "delete_list":
		listID := r.FormValue("list_id")
		if err := dbpkg.DeleteList(h.db, listID, user.ID); err != nil {
			htmxError(w, r, "Could not delete list", http.StatusForbidden)
			return
		}
		w.Header().Set("HX-Redirect", "/lists")
		w.WriteHeader(http.StatusOK)

	case "list_add_member":
		listID := r.FormValue("list_id")
		handle := strings.TrimPrefix(r.FormValue("handle"), "@")
		targetUser, _ := dbpkg.GetUserByHandle(h.db, handle)
		if targetUser == nil {
			htmxError(w, r, "User not found", http.StatusNotFound)
			return
		}
		if err := dbpkg.AddListMember(h.db, listID, user.ID, targetUser.ID); err != nil {
			htmxError(w, r, "Could not add member", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case "list_remove_member":
		listID := r.FormValue("list_id")
		handle := strings.TrimPrefix(r.FormValue("handle"), "@")
		targetUser, _ := dbpkg.GetUserByHandle(h.db, handle)
		if targetUser == nil {
			htmxError(w, r, "User not found", http.StatusNotFound)
			return
		}
		if err := dbpkg.RemoveListMember(h.db, listID, user.ID, targetUser.ID); err != nil {
			htmxError(w, r, "Could not remove member", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "unknown list event", http.StatusBadRequest)
	}
}
