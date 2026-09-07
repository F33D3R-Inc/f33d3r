package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"f33d3r.com/ios/devserver/internal/store"
)

// ── DTOs ──────────────────────────────────────────────────────────────────────

type LiveStreamDTO struct {
	ID           string        `json:"id"`
	Author       WorkAuthorDTO `json:"author"`
	Title        string        `json:"title"`
	Description  string        `json:"description,omitempty"`
	Status       string        `json:"status"`
	StartedAt    *time.Time    `json:"started_at,omitempty"`
	EndedAt      *time.Time    `json:"ended_at,omitempty"`
	ViewerCount  int           `json:"viewer_count"`
	IsNSFW       bool          `json:"is_nsfw"`
	Audience     string        `json:"audience"`
	Lane         string        `json:"lane,omitempty"`
	TipGoalUAET  int64         `json:"tip_goal_uaet"`
	TipTotalUAET int64         `json:"tip_total_uaet"`
	PinnedBody   string        `json:"pinned_body,omitempty"`
	HeartCount   int           `json:"heart_count"`
	// Whether video is actually flowing. Always false here: this server has
	// no media path, and a viewer is told so rather than shown a spinner.
	HasVideo         bool   `json:"has_video"`
	VideoUnavailable string `json:"video_unavailable,omitempty"`
	// Owner-only.
	TopTippers []TipperDTO `json:"top_tippers,omitempty"`
}

type TipperDTO struct {
	Author     WorkAuthorDTO `json:"author"`
	AmountUAET int64         `json:"amount_uaet"`
}

type LiveChatDTO struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"`
	Author     *WorkAuthorDTO `json:"author,omitempty"`
	Body       string         `json:"body"`
	AmountUAET int64          `json:"amount_uaet,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

type LiveRoomDTO struct {
	Stream LiveStreamDTO `json:"stream"`
	Chat   []LiveChatDTO `json:"chat"`
}

type LiveSummaryDTO struct {
	DurationSecs int64 `json:"duration_secs"`
	PeakViewers  int   `json:"peak_viewers"`
	TipsUAET     int64 `json:"tips_uaet"`
	NewFollowers int   `json:"new_followers"`
	ChatLines    int   `json:"chat_lines"`
	Hearts       int   `json:"hearts"`
	ReplaySaved  bool  `json:"replay_saved"`
}

const videoUnavailableNote = "Live video needs the media server, which this deployment does not run. Chat, tips and presence are live."

func (s *Server) liveDTO(r *http.Request, l *store.LiveStream, viewer *store.User) LiveStreamDTO {
	d := LiveStreamDTO{
		ID:               l.ID,
		Author:           authorDTO(l.Author),
		Title:            l.Title,
		Description:      l.Description,
		Status:           l.Status,
		StartedAt:        l.StartedAt,
		EndedAt:          l.EndedAt,
		ViewerCount:      s.hub.presence(liveTopic(l.ID), l.AuthorID),
		IsNSFW:           l.IsNSFW,
		Audience:         l.Audience,
		Lane:             l.Lane,
		TipGoalUAET:      l.TipGoalUAET,
		TipTotalUAET:     l.TipTotalUAET,
		PinnedBody:       l.PinnedBody,
		HeartCount:       l.HeartCount,
		HasVideo:         false,
		VideoUnavailable: videoUnavailableNote,
	}
	if l.Status != "live" {
		d.ViewerCount = 0
	}
	if viewer != nil && viewer.ID == l.AuthorID {
		tippers, _ := s.store.TopTippers(r.Context(), l.ID, 3)
		for _, t := range tippers {
			d.TopTippers = append(d.TopTippers, TipperDTO{Author: authorDTO(t.User), AmountUAET: t.AmountUAET})
		}
	}
	return d
}

func chatDTO(m *store.LiveChatMessage) LiveChatDTO {
	d := LiveChatDTO{ID: m.ID, Kind: m.Kind, Body: m.Body, AmountUAET: m.AmountUAET, CreatedAt: m.CreatedAt}
	if m.User != nil {
		a := authorDTO(m.User)
		d.Author = &a
	}
	return d
}

func liveTopic(id string) string { return "live:" + id }

// ── Reads ─────────────────────────────────────────────────────────────────────

// live — GET /api/v1/live: rooms live now.
func (s *Server) live(w http.ResponseWriter, r *http.Request, u *store.User) {
	streams, err := s.store.ActiveLive(r.Context(), u)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]LiveStreamDTO, 0, len(streams))
	for _, l := range streams {
		out = append(out, s.liveDTO(r, l, u))
	}
	var own *LiveStreamDTO
	if mine, err := s.store.OpenLiveFor(r.Context(), u.ID); err == nil {
		d := s.liveDTO(r, mine, u)
		own = &d
	}
	writeJSON(w, http.StatusOK, map[string]any{"streams": out, "count": len(out), "own": own})
}

// liveRoom — GET /api/v1/live/{id}: the room and its chat backlog.
func (s *Server) liveRoom(w http.ResponseWriter, r *http.Request, u *store.User) {
	l, err := s.store.GetLive(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "That stream doesn't exist.")
		return
	}
	if !s.mayWatch(r, l, u) {
		writeError(w, http.StatusForbidden, "subscribers_only", "This broadcast is for @"+l.Author.Handle+"'s subscribers.")
		return
	}
	chat, err := s.store.ChatBacklog(r.Context(), l.ID, 60)
	if err != nil {
		serverError(w, err)
		return
	}
	out := LiveRoomDTO{Stream: s.liveDTO(r, l, u), Chat: make([]LiveChatDTO, 0, len(chat))}
	for _, m := range chat {
		out.Chat = append(out.Chat, chatDTO(m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) mayWatch(r *http.Request, l *store.LiveStream, u *store.User) bool {
	if l.AuthorID == u.ID || l.Audience == "everyone" {
		return true
	}
	var n int
	s.store.DB().QueryRowContext(r.Context(), `SELECT COUNT(*) FROM subscriptions WHERE subscriber_id = ? AND creator_id = ? AND status = 'active'`, u.ID, l.AuthorID).Scan(&n)
	return n > 0
}

// liveEvents — GET /api/v1/live/{id}/events: the room's SSE stream.
//
// Frames, each with a JSON body:
//
//	chat     LiveChatDTO
//	viewers  {"count": N}
//	tip      LiveChatDTO (kind "tip")
//	pinned   {"body": "..."}
//	hearts   {"count": N}
//	status   {"status": "ended", "summary": ...}
//
// The connection is the viewer's presence: opening it counts them, closing it
// uncounts them, and the count is pushed to everyone each time it changes.
func (s *Server) liveEvents(w http.ResponseWriter, r *http.Request, u *store.User) {
	l, err := s.store.GetLive(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "That stream doesn't exist.")
		return
	}
	if !s.mayWatch(r, l, u) {
		writeError(w, http.StatusForbidden, "subscribers_only", "Subscribers only.")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_stream", "SSE not supported.")
		return
	}
	topic := liveTopic(l.ID)
	sub, cancel := s.hub.subscribe(topic, u.ID)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	s.publishViewers(r.Context(), l)
	defer func() {
		// The request context is gone by now; the departure is published on a
		// fresh one so the room learns the viewer left.
		cancel()
		ctx, done := context.WithTimeout(context.Background(), 2*time.Second)
		defer done()
		if fresh, err := s.store.GetLive(ctx, l.ID); err == nil && fresh.Status == "live" {
			s.publishViewers(ctx, fresh)
		}
	}()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case f := <-sub.ch:
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.Event, f.Data)
			flusher.Flush()
		}
	}
}

func (s *Server) publishViewers(ctx context.Context, l *store.LiveStream) {
	n := s.hub.presence(liveTopic(l.ID), l.AuthorID)
	s.store.SetViewerCount(ctx, l.ID, n)
	s.hub.publish(liveTopic(l.ID), "viewers", map[string]int{"count": n})
}

// ── Events ────────────────────────────────────────────────────────────────────

// liveStartEvent — {event_type:"live_start", title, description, audience,
// lane, tip_goal_aet, notify_followers, save_replay, nsfw} → 201 the room.
func (s *Server) liveStartEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	in := store.LiveInput{
		AuthorID:    u.ID,
		AuthorPIAL:  u.PIALID,
		Title:       strings.TrimSpace(body.get("title")),
		Description: strings.TrimSpace(body.get("description")),
		Audience:    body.get("audience"),
		Lane:        body.get("lane"),
		Notify:      body.get("notify_followers") != "0" && body.get("notify_followers") != "false",
		SaveReplay:  body.get("save_replay") != "0" && body.get("save_replay") != "false",
	}
	if goal, err := strconv.ParseFloat(body.get("tip_goal_aet"), 64); err == nil && goal > 0 {
		in.TipGoalUAET = int64(goal * store.UAETPerAET)
	}
	if body.get("nsfw") == "1" || body.get("is_nsfw") == "true" {
		if !u.IsAdult {
			plainError(w, http.StatusForbidden, "Your account is not cleared to broadcast 18+ content.")
			return
		}
		in.IsNSFW = true
	}
	l, err := s.store.StartLive(r.Context(), in)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyLive) {
			plainError(w, http.StatusConflict, err.Error())
			return
		}
		plainError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Notify {
		// Followers hear about it in their inbox; the SSE catalog's live_start
		// would carry the same fact on the web.
		// Collect first, notify after: the store has one connection, and a
		// write issued while this cursor is open would wait on itself.
		var followers []string
		rows, err := s.store.DB().QueryContext(r.Context(), `SELECT follower_id FROM follows WHERE following_id = ?`, u.ID)
		if err == nil {
			for rows.Next() {
				var fid string
				rows.Scan(&fid)
				followers = append(followers, fid)
			}
			rows.Close()
		}
		for _, fid := range followers {
			s.store.Notify(r.Context(), fid, "live_start", u.ID, l.ID, "live", map[string]any{"preview": l.Title})
		}
	}
	writeJSON(w, http.StatusCreated, s.liveDTO(r, l, u))
}

func (s *Server) liveEndEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	id := body.get("stream_id")
	sum, err := s.store.EndLive(r.Context(), id, u.ID)
	if err != nil {
		if errors.Is(err, store.ErrLiveNotFound) {
			plainError(w, http.StatusNotFound, "stream not found")
			return
		}
		plainError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := LiveSummaryDTO{
		DurationSecs: sum.DurationSecs, PeakViewers: sum.PeakViewers, TipsUAET: sum.TipsUAET,
		NewFollowers: sum.NewFollowers, ChatLines: sum.ChatLines, Hearts: sum.Hearts, ReplaySaved: sum.ReplaySaved,
	}
	s.hub.publish(liveTopic(id), "status", map[string]any{"status": "ended"})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) liveRoomFor(w http.ResponseWriter, r *http.Request, u *store.User, id string) (*store.LiveStream, bool) {
	l, err := s.store.GetLive(r.Context(), id)
	if err != nil {
		plainError(w, http.StatusNotFound, "stream not found")
		return nil, false
	}
	if l.Status != "live" {
		plainError(w, http.StatusConflict, "this stream has ended")
		return nil, false
	}
	if !s.mayWatch(r, l, u) {
		plainError(w, http.StatusForbidden, "subscribers only")
		return nil, false
	}
	return l, true
}

// liveChatEvent — {event_type:"live_chat", stream_id, body}. 280 runes.
func (s *Server) liveChatEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	l, ok := s.liveRoomFor(w, r, u, body.get("stream_id"))
	if !ok {
		return
	}
	text := strings.TrimSpace(body.get("body"))
	if text == "" {
		plainError(w, http.StatusBadRequest, "message required")
		return
	}
	if len([]rune(text)) > 280 {
		text = string([]rune(text)[:280])
	}
	m, err := s.store.AppendChat(r.Context(), l.ID, u, "chat", text, 0)
	if err != nil {
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	s.hub.publish(liveTopic(l.ID), "chat", chatDTO(m))
	writeJSON(w, http.StatusOK, chatDTO(m))
}

// livePinEvent — broadcaster pins a line (or clears it with "").
func (s *Server) livePinEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	l, ok := s.liveRoomFor(w, r, u, body.get("stream_id"))
	if !ok {
		return
	}
	if l.AuthorID != u.ID {
		plainError(w, http.StatusForbidden, "forbidden")
		return
	}
	text := strings.TrimSpace(body.get("body"))
	if len([]rune(text)) > 140 {
		text = string([]rune(text)[:140])
	}
	if err := s.store.PinLive(r.Context(), l.ID, text); err != nil {
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	s.hub.publish(liveTopic(l.ID), "pinned", map[string]string{"body": text})
	w.WriteHeader(http.StatusNoContent)
}

// liveHeartEvent — a viewer's ♥. Counted, never ranked.
func (s *Server) liveHeartEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	l, ok := s.liveRoomFor(w, r, u, body.get("stream_id"))
	if !ok {
		return
	}
	n, err := s.store.Heart(r.Context(), l.ID)
	if err != nil {
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	s.hub.publish(liveTopic(l.ID), "hearts", map[string]int{"count": n})
	w.WriteHeader(http.StatusNoContent)
}

// liveTip is the tip event's stream branch: the ledger move plus a tip line
// in the room's chat for everyone to see.
func (s *Server) liveTip(w http.ResponseWriter, r *http.Request, u *store.User, streamID string, amountUAET int64) {
	l, ok := s.liveRoomFor(w, r, u, streamID)
	if !ok {
		return
	}
	if l.AuthorID == u.ID {
		plainError(w, http.StatusBadRequest, "cannot tip yourself")
		return
	}
	if err := s.store.TipStream(r.Context(), u.PIALID, l.AuthorPIAL, l.ID, amountUAET); err != nil {
		if errors.Is(err, store.ErrInsufficientBalance) {
			plainError(w, http.StatusPaymentRequired, "insufficient balance")
			return
		}
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	m, err := s.store.AppendChat(r.Context(), l.ID, u, "tip", "", amountUAET)
	if err == nil {
		s.hub.publish(liveTopic(l.ID), "tip", chatDTO(m))
	}
	if fresh, err := s.store.GetLive(r.Context(), l.ID); err == nil {
		s.hub.publish(liveTopic(l.ID), "tips", map[string]int64{"total_uaet": fresh.TipTotalUAET, "goal_uaet": fresh.TipGoalUAET})
	}
	s.store.Notify(r.Context(), l.AuthorID, "tip", u.ID, l.ID, "live", map[string]any{"amount_uaet": amountUAET})
	w.WriteHeader(http.StatusNoContent)
}
