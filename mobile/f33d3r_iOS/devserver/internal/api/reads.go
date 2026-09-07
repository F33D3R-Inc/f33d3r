package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"f33d3r.com/ios/devserver/internal/store"
)

const (
	defaultPageSize = 20
	maxPageSize     = 50
)

// feed — GET /api/v1/feed?surface=&limit=&cursor=
//
// The surface ids are the ones the web's lane table uses: following, foryou,
// trending, music, visions, live. Anything else is `400 unknown_surface`.
func (s *Server) feed(w http.ResponseWriter, r *http.Request, u *store.User) {
	surface := r.URL.Query().Get("surface")
	if surface == "" {
		surface = "following"
	}
	limit := limitParam(r, defaultPageSize, maxPageSize)
	cursor := r.URL.Query().Get("cursor")
	ctx := r.Context()

	var (
		works []*store.Work
		next  string
		err   error
		prov  func(*store.Work) *ProvenanceDTO
		live  *int
	)
	switch surface {
	case "following":
		works, next, err = s.store.FeedFollowing(ctx, u.ID, limit, cursor)
		prov = followingProvenance(u)
	case "foryou", "for_you":
		works, next, err = s.store.FeedForYou(ctx, u.ID, limit, cursor)
		prov = discoveryProvenance(u)
	case "trending":
		works, next, err = s.store.FeedTrending(ctx, u.ID, limit, cursor)
		prov = trendingProvenance()
	case "music":
		works, next, err = s.store.FeedBySurface(ctx, u.ID, []string{"music"}, "audio", limit, cursor)
	case "visions":
		works, next, err = s.store.FeedVisions(ctx, u.ID, limit, cursor)
	case "tag":
		tag := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(r.URL.Query().Get("tag"), "#")))
		if tag == "" {
			writeError(w, http.StatusBadRequest, "tag_required", "Which tag?")
			return
		}
		works, next, err = s.store.FeedBySurface(ctx, u.ID, []string{tag}, "", limit, cursor)
	case "live":
		// Live rooms are not works; the lane reads them from /api/v1/live.
		// The page is empty by design and carries the count for the badge.
		n := s.store.LiveCount(ctx)
		works, next, live = []*store.Work{}, "", &n
	default:
		writeError(w, http.StatusBadRequest, "unknown_surface", fmt.Sprintf("No surface named %q.", surface))
		return
	}
	if err != nil {
		if strings.HasPrefix(err.Error(), "bad cursor") {
			writeError(w, http.StatusBadRequest, "bad_cursor", "That page cursor is not valid.")
			return
		}
		serverError(w, err)
		return
	}
	if live == nil {
		n := s.store.LiveCount(ctx)
		live = &n
	}
	writeJSON(w, http.StatusOK, WorkPageDTO{Works: workDTOs(works, prov), NextCursor: next, LiveCount: live})
}

// Provenance is the surface's one-line account of why a row is on it. Only
// what the server actually knows: a follow, a repost by someone followed, a
// rank. Never a description of how ranking works.

func followingProvenance(viewer *store.User) func(*store.Work) *ProvenanceDTO {
	return func(w *store.Work) *ProvenanceDTO {
		if w.RepostedBy != nil && w.RepostedBy.ID != viewer.ID {
			return &ProvenanceDTO{Kind: "reposted", Text: "Reposted by @" + w.RepostedBy.Handle, Handle: strPtr(w.RepostedBy.Handle)}
		}
		if w.Author != nil && w.Author.ID != viewer.ID {
			return &ProvenanceDTO{Kind: "you_follow", Text: "You follow @" + w.Author.Handle, Handle: strPtr(w.Author.Handle)}
		}
		return nil
	}
}

func discoveryProvenance(viewer *store.User) func(*store.Work) *ProvenanceDTO {
	return func(w *store.Work) *ProvenanceDTO {
		if w.RepostedBy != nil && w.RepostedBy.ID != viewer.ID {
			return &ProvenanceDTO{Kind: "reposted", Text: "Reposted by @" + w.RepostedBy.Handle, Handle: strPtr(w.RepostedBy.Handle)}
		}
		if w.ViewerFollowsAuthor {
			return &ProvenanceDTO{Kind: "you_follow", Text: "You follow @" + w.Author.Handle, Handle: strPtr(w.Author.Handle)}
		}
		return nil
	}
}

func trendingProvenance() func(*store.Work) *ProvenanceDTO {
	return func(w *store.Work) *ProvenanceDTO {
		if w.ScoreBand == "trending" || w.ScoreBand == "rising" {
			return &ProvenanceDTO{Kind: "trending", Text: "Picking up on F33D3R"}
		}
		return nil
	}
}

// work — GET /api/v1/works/{id}: the work, its ancestors, the first page of
// replies.
func (s *Server) work(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	if !store.IsUUID(id) {
		writeError(w, http.StatusNotFound, "not_found", "That work doesn't exist.")
		return
	}
	ctx := r.Context()
	work, err := s.store.GetWorkByID(ctx, id, u.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "That work doesn't exist.")
			return
		}
		serverError(w, err)
		return
	}
	s.store.RecordView(ctx, id)
	ancestors, err := s.store.ParentChain(ctx, id, u.ID, 10)
	if err != nil {
		serverError(w, err)
		return
	}
	replies, next, err := s.store.Replies(ctx, id, u.ID, defaultPageSize, "")
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, WorkThreadDTO{
		Work:          workDTO(work, nil),
		Ancestors:     workDTOs(ancestors, nil),
		Replies:       workDTOs(replies, nil),
		RepliesCursor: next,
	})
}

// replies — GET /api/v1/works/{id}/replies?cursor=&limit=
func (s *Server) replies(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	if !store.IsUUID(id) {
		writeError(w, http.StatusNotFound, "not_found", "That work doesn't exist.")
		return
	}
	replies, next, err := s.store.Replies(r.Context(), id, u.ID, limitParam(r, defaultPageSize, maxPageSize), r.URL.Query().Get("cursor"))
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, WorkPageDTO{Works: workDTOs(replies, nil), NextCursor: next})
}

// quotes — GET /api/v1/works/{id}/quotes?cursor=&limit=
//
// The works that quote this one. An ordinary page of works, the same shape the
// replies route answers with, because a quote is an ordinary work — the list
// behind "View quotes" draws with the same card as any feed.
func (s *Server) quotes(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	if !store.IsUUID(id) {
		writeError(w, http.StatusNotFound, "not_found", "That work doesn't exist.")
		return
	}
	quotes, next, err := s.store.Quotes(r.Context(), id, u.ID, limitParam(r, defaultPageSize, maxPageSize), r.URL.Query().Get("cursor"))
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, WorkPageDTO{Works: workDTOs(quotes, nil), NextCursor: next})
}

// profile — GET /api/v1/users/{handle}
func (s *Server) profile(w http.ResponseWriter, r *http.Request, viewer *store.User) {
	ctx := r.Context()
	target, err := s.store.GetUserByHandle(ctx, r.PathValue("handle"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No one by that handle.")
			return
		}
		serverError(w, err)
		return
	}
	viewerFollows, _ := s.store.IsFollowing(ctx, viewer.ID, target.ID)
	followsViewer, _ := s.store.IsFollowing(ctx, target.ID, viewer.ID)
	viewerIsBlocked, _ := s.store.IsBlocked(ctx, target.ID, viewer.ID)
	dto := ProfileDTO{
		User:            userDTO(target),
		ViewerFollows:   viewerFollows,
		FollowsViewer:   followsViewer,
		ViewerIsBlocked: viewerIsBlocked,
	}
	if target.PinnedWorkID != "" {
		if pinned, err := s.store.GetWorkByID(ctx, target.PinnedWorkID, viewer.ID); err == nil {
			pinned.IsPinned = true
			p := workDTO(pinned, nil)
			dto.PinnedWork = &p
		}
	}
	writeJSON(w, http.StatusOK, dto)
}

// profileWorks — GET /api/v1/users/{handle}/works?tab=works|replies|media|likes
//
// Likes are the owner's alone: anyone else asking gets `403 likes_private`,
// which the client renders as a rule rather than a failure.
func (s *Server) profileWorks(w http.ResponseWriter, r *http.Request, viewer *store.User) {
	ctx := r.Context()
	target, err := s.store.GetUserByHandle(ctx, r.PathValue("handle"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No one by that handle.")
			return
		}
		serverError(w, err)
		return
	}
	tab := store.ProfileTab(r.URL.Query().Get("tab"))
	switch tab {
	case "", store.TabWorks:
		tab = store.TabWorks
	case store.TabReplies, store.TabMedia:
	case store.TabLikes:
		if target.ID != viewer.ID {
			writeError(w, http.StatusForbidden, "likes_private", "Nobody but @"+target.Handle+" can see what they have liked.")
			return
		}
	case "saves":
		if target.ID != viewer.ID {
			writeError(w, http.StatusForbidden, "saves_private", "Saves are private.")
			return
		}
		works, next, err := s.store.SavedWorks(ctx, viewer.ID, limitParam(r, defaultPageSize, maxPageSize), r.URL.Query().Get("cursor"))
		if err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, WorkPageDTO{Works: workDTOs(works, nil), NextCursor: next})
		return
	default:
		writeError(w, http.StatusBadRequest, "unknown_tab", fmt.Sprintf("No tab named %q.", tab))
		return
	}
	works, next, err := s.store.ProfileWorks(ctx, target.ID, viewer.ID, tab, limitParam(r, defaultPageSize, maxPageSize), r.URL.Query().Get("cursor"))
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, WorkPageDTO{Works: workDTOs(works, nil), NextCursor: next})
}

// search — GET /api/v1/search?q=&type=works|people|all
func (s *Server) search(w http.ResponseWriter, r *http.Request, viewer *store.User) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, SearchDTO{Works: []WorkDTO{}, People: []UserDTO{}, Tags: []TagDTO{}})
		return
	}
	kind := r.URL.Query().Get("type")
	out := SearchDTO{Works: []WorkDTO{}, People: []UserDTO{}, Tags: []TagDTO{}}
	ctx := r.Context()
	if kind == "" || kind == "all" || kind == "tags" {
		tags, err := s.store.SearchTags(ctx, q, 10)
		if err != nil {
			serverError(w, err)
			return
		}
		for _, t := range tags {
			out.Tags = append(out.Tags, TagDTO{Tag: t.Tag, Count: t.Count})
		}
	}
	if kind == "" || kind == "all" || kind == "works" {
		works, err := s.store.SearchWorks(ctx, q, viewer.ID, defaultPageSize)
		if err != nil {
			serverError(w, err)
			return
		}
		out.Works = workDTOs(works, nil)
	}
	if kind == "" || kind == "all" || kind == "people" {
		people, err := s.store.SearchUsers(ctx, q, defaultPageSize)
		if err != nil {
			serverError(w, err)
			return
		}
		for _, p := range people {
			out.People = append(out.People, userDTO(p))
		}
	}
	writeJSON(w, http.StatusOK, out)
}
