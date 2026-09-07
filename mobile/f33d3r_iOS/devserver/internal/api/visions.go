package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"f33d3r.com/ios/devserver/internal/store"
)

// ── DTOs ──────────────────────────────────────────────────────────────────────

type VisionArtboardDTO struct {
	Background string `json:"background"`
	Typeface   string `json:"typeface"`
	TypeScale  string `json:"type_scale"`
	Align      string `json:"align"`
}

type VisionDTO struct {
	ID           string            `json:"id"`
	Author       WorkAuthorDTO     `json:"author"`
	ContentType  string            `json:"content_type"`
	Body         string            `json:"body"`
	Artboard     VisionArtboardDTO `json:"artboard"`
	MediaURLs    []string          `json:"media_urls,omitempty"`
	Poll         *PollDTO          `json:"poll,omitempty"`
	IsNSFW       bool              `json:"is_nsfw"`
	Audience     string            `json:"audience"`
	AllowReplies bool              `json:"allow_replies"`
	CreatedAt    time.Time         `json:"created_at"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Seen         bool              `json:"seen"`
	// Owner-only; omitted for everyone else.
	ViewCount *int `json:"view_count,omitempty"`
}

type VisionRingDTO struct {
	Author      WorkAuthorDTO  `json:"author"`
	State       string         `json:"state"`
	Count       int            `json:"count"`
	UnseenCount int            `json:"unseen_count"`
	LatestAt    time.Time      `json:"latest_at"`
	IsLive      bool           `json:"is_live"`
	Provenance  *ProvenanceDTO `json:"provenance,omitempty"`
	Visions     []VisionDTO    `json:"visions"`
}

type VisionTrayDTO struct {
	Own   *VisionRingDTO  `json:"own,omitempty"`
	Rings []VisionRingDTO `json:"rings"`
}

func visionDTO(f *store.Vision, viewer *store.User) VisionDTO {
	d := VisionDTO{
		ID:          f.ID,
		Author:      authorDTO(f.Author),
		ContentType: f.ContentType,
		Body:        f.Body,
		Artboard: VisionArtboardDTO{
			Background: f.ArtboardBackground,
			Typeface:   f.ArtboardTypeface,
			TypeScale:  resolveTypeScale(f.ArtboardTypeScale, f.Body),
			Align:      f.ArtboardAlign,
		},
		MediaURLs:    f.MediaURLs,
		Poll:         pollDTO(f.Poll),
		IsNSFW:       f.IsNSFW,
		Audience:     f.Audience,
		AllowReplies: f.AllowReplies,
		CreatedAt:    f.CreatedAt,
		ExpiresAt:    f.ExpiresAt,
		Seen:         f.ViewedByViewer,
	}
	if f.AuthorID == viewer.ID {
		n := f.ViewCount
		d.ViewCount = &n
	}
	return d
}

// resolveTypeScale turns "auto" into a concrete rung by body length, the
// way the web's server-side renderer does.
func resolveTypeScale(scale, body string) string {
	if scale != "auto" {
		return scale
	}
	n := len([]rune(body))
	switch {
	case n <= 40:
		return "xl"
	case n <= 90:
		return "l"
	case n <= 160:
		return "m"
	default:
		return "s"
	}
}

func ringDTO(r *store.VisionRing, viewer *store.User) VisionRingDTO {
	d := VisionRingDTO{
		Author:      authorDTO(r.Author),
		State:       r.State,
		Count:       r.Count,
		UnseenCount: r.UnseenCount,
		LatestAt:    r.LatestAt,
		IsLive:      r.IsLive,
		Visions:     make([]VisionDTO, 0, len(r.Visions)),
	}
	if r.Author != nil && r.Author.ID != viewer.ID {
		d.Provenance = &ProvenanceDTO{Kind: "you_follow", Text: "You follow @" + r.Author.Handle, Handle: strPtr(r.Author.Handle)}
	}
	for _, f := range r.Visions {
		d.Visions = append(d.Visions, visionDTO(f, viewer))
	}
	return d
}

// ── Reads ─────────────────────────────────────────────────────────────────────

// visions — GET /api/v1/visions: the tray, with every vision inline.
func (s *Server) visions(w http.ResponseWriter, r *http.Request, u *store.User) {
	own, rings, err := s.store.VisionTray(r.Context(), u)
	if err != nil {
		serverError(w, err)
		return
	}
	out := VisionTrayDTO{Rings: make([]VisionRingDTO, 0, len(rings))}
	if own != nil {
		d := ringDTO(own, u)
		out.Own = &d
	}
	for _, ring := range rings {
		out.Rings = append(out.Rings, ringDTO(ring, u))
	}
	writeJSON(w, http.StatusOK, out)
}

// visionViewers — GET /api/v1/visions/{id}/viewers (owner only).
func (s *Server) visionViewers(w http.ResponseWriter, r *http.Request, u *store.User) {
	f, err := s.store.GetVision(r.Context(), r.PathValue("id"), u)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "That vision is gone.")
		return
	}
	if f.AuthorID != u.ID {
		writeError(w, http.StatusForbidden, "forbidden", "Only the author can see who watched.")
		return
	}
	users, _, err := s.store.VisionViewers(r.Context(), f.ID, 200)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]WorkAuthorDTO, 0, len(users))
	for _, v := range users {
		out = append(out, authorDTO(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"viewers": out, "count": f.ViewCount})
}

// ── Events ────────────────────────────────────────────────────────────────────

// visionCreateEvent — {event_type:"vision", content_type, body, artboard_*,
// media_url, audience, ttl_hours, allow_replies, poll_options}. Unsigned, as
// feed-engine's POST /visions is. Answers 201 {"vision_id"}.
func (s *Server) visionCreateEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	in := store.VisionInput{
		AuthorID:     u.ID,
		AuthorPIAL:   u.PIALID,
		ContentType:  body.get("content_type"),
		Body:         strings.TrimSpace(body.get("body")),
		Background:   body.get("artboard_background"),
		Typeface:     body.get("artboard_typeface"),
		TypeScale:    body.get("artboard_type_scale"),
		Align:        body.get("artboard_align"),
		Audience:     body.get("audience"),
		AllowReplies: body.get("allow_replies") != "0" && body.get("allow_replies") != "false",
		Source:       body.get("source"),
	}
	if len([]rune(in.Body)) > store.VisionBodyMax {
		in.Body = string([]rune(in.Body)[:store.VisionBodyMax])
	}
	if m := body.get("media_url"); m != "" {
		in.MediaURLs = []string{m}
	}
	if hrs, err := strconv.Atoi(body.get("ttl_hours")); err == nil && hrs > 0 {
		in.TTL = time.Duration(hrs) * time.Hour
	}
	if opts := body.getStrings("poll_options"); opts != nil {
		in.PollOptions = opts
	}
	if body.get("nsfw") == "1" || body.get("is_nsfw") == "true" {
		if !u.IsAdult {
			plainError(w, http.StatusForbidden, "Your account is not cleared to post 18+ content.")
			return
		}
		in.IsNSFW = true
	}
	id, err := s.store.InsertVision(r.Context(), in)
	if err != nil {
		plainError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"vision_id": id})
}

func (s *Server) visionSeenEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	id := body.get("vision_id")
	if id == "" {
		plainError(w, http.StatusBadRequest, "vision_id required")
		return
	}
	if err := s.store.MarkVisionViewed(r.Context(), id, u); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			plainError(w, http.StatusNotFound, "vision not found")
			return
		}
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) visionDeleteEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	id := body.get("vision_id")
	if err := s.store.SoftDeleteVision(r.Context(), id, u.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			plainError(w, http.StatusForbidden, "Vision not found or you do not own it")
			return
		}
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// visionReplyEvent — a private reply. Lands in the author's inbox as a
// `vision_reply` with the text as preview; nobody else sees it.
func (s *Server) visionReplyEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	id := body.get("vision_id")
	text := strings.TrimSpace(body.get("body"))
	if id == "" || text == "" {
		plainError(w, http.StatusBadRequest, "vision_id and body required")
		return
	}
	if len([]rune(text)) > 500 {
		text = string([]rune(text)[:500])
	}
	f, err := s.store.ReplyToVision(r.Context(), id, u, text)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			plainError(w, http.StatusNotFound, "vision not found")
			return
		}
		plainError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.store.Notify(r.Context(), f.AuthorID, "vision_reply", u.ID, f.ID, "vision", map[string]any{"preview": text})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) visionPollVoteEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	id := body.get("vision_id")
	idx, err := strconv.Atoi(body.get("option_idx"))
	if id == "" || err != nil {
		plainError(w, http.StatusBadRequest, "vision_id and option_idx required")
		return
	}
	if err := s.store.CastVisionPollVote(r.Context(), id, u.ID, idx); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			plainError(w, http.StatusNotFound, "vision not found")
			return
		}
		plainError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) visionMuteEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	target, err := s.targetUser(r, body)
	if err != nil {
		plainError(w, http.StatusNotFound, "user not found")
		return
	}
	if err := s.store.SetVisionMuted(r.Context(), u.ID, target.ID, body.eventType == "vision_mute"); err != nil {
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
