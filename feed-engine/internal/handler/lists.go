package handler

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/google/uuid"
)

func (h *Handler) listsPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		return
	}
	lists, _ := dbpkg.GetUserLists(h.db, user.ID)
	h.render(w, r, "lists.html", map[string]interface{}{
		"SessionID":         uuid.New().String(),
		"CurrentUserHandle": user.Handle,
		"User":              user,
		"Lists":             lists,
		"ShowScores":        h.cfg.ShowScores,
	})
}

// listWorksNextURL is the address of the next page of a list's works: the
// cursor is the created_at of the last work on this page, and the Facet at
// that address answers with the next works and the next sentinel only.
func listWorksNextURL(listID, before string) string {
	return "/facets/list/" + listID + "/works?before=" + url.QueryEscape(before)
}

func (h *Handler) listDetailPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		return
	}
	listID := r.PathValue("id")
	list, err := dbpkg.GetPublicList(h.db, listID)
	if err != nil || list == nil {
		http.NotFound(w, r)
		return
	}
	before := r.URL.Query().Get("before")
	works, _ := dbpkg.GetWorksByList(h.db, listID, h.cfg.FeedPageSize+1, before)
	members, _ := dbpkg.GetListMembers(h.db, listID)
	// One enrichment sequence, defined once (enrichWorks) — a list card carries
	// the same reactions, quotes, previews, repliers and ballots as a feed card.
	h.enrichWorks(works, user)
	var nextBefore string
	if len(works) > h.cfg.FeedPageSize {
		works = works[:h.cfg.FeedPageSize]
		nextBefore = works[len(works)-1].CreatedAt.Format(time.RFC3339)
	}
	works = filterAdultForViewer(user, works)
	nextURL := ""
	if nextBefore != "" {
		nextURL = listWorksNextURL(listID, nextBefore)
	}
	h.render(w, r, "list_detail.html", map[string]interface{}{
		"SessionID":         uuid.New().String(),
		"CurrentUserHandle": user.Handle,
		"User":              user,
		"List":              list,
		"Works":             works,
		"Members":           members,
		"Before":            nextBefore,
		"NextURL":           nextURL,
		"ShowScores":        h.cfg.ShowScores,
	})
}

// facetListWorks — GET /facets/list/{id}/works?before=<created_at RFC3339>
// The paging Facet of a list: the next page of work cards followed by the next
// sentinel, and nothing else. The list page itself is never re-rendered by a
// scroll.
func (h *Handler) facetListWorks(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	listID := r.PathValue("id")
	list, err := dbpkg.GetPublicList(h.db, listID)
	if err != nil || list == nil {
		http.NotFound(w, r)
		return
	}
	before := r.URL.Query().Get("before")
	if before == "" {
		// The sentinel's data-next-after cursor, as a native Shell sends it back.
		before = r.URL.Query().Get("after")
	}
	works, err := dbpkg.GetWorksByList(h.db, listID, h.cfg.FeedPageSize+1, before)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	var nextBefore string
	if len(works) > h.cfg.FeedPageSize {
		works = works[:h.cfg.FeedPageSize]
		nextBefore = works[len(works)-1].CreatedAt.Format(time.RFC3339)
	}
	// renderWorkCardSet is the one work_card entry point: adult stripping and
	// enrichment happen there, so a list page and a list scroll cannot produce
	// different cards for the same work.
	cards := h.renderWorkCardSet(works, user, h.workCardCtx(user, "list"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if len(cards) == 0 && before == "" {
		h.renderPartial(w, "stateEmpty", map[string]interface{}{
			"Title": "No posts yet",
			"Sub":   "Works from list members will appear here.",
		})
		return
	}
	for _, c := range cards {
		_, _ = w.Write([]byte(c.Fragment))
	}
	if nextBefore != "" {
		h.renderPartial(w, "feed_sentinel", map[string]interface{}{
			"URL":    listWorksNextURL(listID, nextBefore),
			"Cursor": nextBefore,
		})
	}
}

// facetListCreateModal — GET /facets/list/create_modal
// The "New list" overlay: a form whose submission is the create_list event.
func (h *Handler) facetListCreateModal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "list_create_modal", map[string]interface{}{})
}

// facetListAddMemberModal — GET /facets/list/{id}/add_member_modal
// The "Add member" overlay of a list the viewer owns.
func (h *Handler) facetListAddMemberModal(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	listID := r.PathValue("id")
	list, err := dbpkg.GetPublicList(h.db, listID)
	if err != nil || list == nil {
		http.NotFound(w, r)
		return
	}
	if list.OwnerID != user.ID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "list_add_member_modal", map[string]interface{}{
		"ListID":   list.ID,
		"ListName": list.Name,
	})
}

// renderListMembers writes the list_members Facet for listID — the member
// strip on the list page, re-rendered after a membership mutation.
func (h *Handler) renderListMembers(w http.ResponseWriter, listID string) {
	members, _ := dbpkg.GetListMembers(h.db, listID)
	h.renderPartial(w, "list_members", map[string]interface{}{
		"ListID":  listID,
		"Members": members,
	})
}

func (h *Handler) postAnalyticsPartial(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		return
	}
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
		// A list is public unless the caller says otherwise, in either of the two
		// ways a form can say it: is_public=false (scripted callers) or the
		// create modal's "Make private" box (is_private=true).
		isPublic := r.FormValue("is_public") != "false" && r.FormValue("is_private") != "true"
		id, err := dbpkg.CreateList(h.db, user.ID, name, desc, isPublic)
		if err != nil {
			htmxError(w, r, "Could not create list", http.StatusInternalServerError)
			return
		}
		if facetRequest(r) {
			// The Facet answer: the new list's row, placed at the head of
			// #lists-container by the form's swap, the empty state removed and
			// the overlay the form lived in closed — all in one response.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			h.renderPartial(w, "list_row", dbpkg.List{
				ID:          id,
				OwnerID:     user.ID,
				OwnerHandle: user.Handle,
				Name:        name,
				Description: desc,
				IsPublic:    isPublic,
				CreatedAt:   time.Now(),
			})
			_, _ = w.Write([]byte(`<div id="lists-empty" hx-swap-oob="delete"></div>`))
			writeOverlaySlotClear(w)
			return
		}
		// The new list's address: the Shell navigates its Playground there.
		hxLocationHeader(w, "/lists/"+id, true)
		w.WriteHeader(http.StatusOK)

	case "delete_list":
		listID := r.FormValue("list_id")
		if err := dbpkg.DeleteList(h.db, listID, user.ID); err != nil {
			htmxError(w, r, "Could not delete list", http.StatusForbidden)
			return
		}
		hxLocationHeader(w, "/lists", true)
		w.WriteHeader(http.StatusOK)

	case "list_add_member":
		listID := r.FormValue("list_id")
		handle := strings.TrimPrefix(strings.TrimSpace(r.FormValue("handle")), "@")
		targetUser, _ := dbpkg.GetUserByHandle(h.db, handle)
		if targetUser == nil {
			htmxError(w, r, "User not found", http.StatusNotFound)
			return
		}
		if err := dbpkg.AddListMember(h.db, listID, user.ID, targetUser.ID); err != nil {
			htmxError(w, r, "Could not add member", http.StatusForbidden)
			return
		}
		if facetRequest(r) {
			// The member strip re-rendered with the new member, and the
			// overlay the form lived in closed.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			h.renderListMembers(w, listID)
			writeOverlaySlotClear(w)
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
		if facetRequest(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			h.renderListMembers(w, listID)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "unknown list event", http.StatusBadRequest)
	}
}
