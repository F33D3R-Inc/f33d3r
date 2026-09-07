package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── /api/v1/live — the room around a broadcast, for the native clients ───────
//
// The broadcast itself (ingest, ladder, HLS) is live.go's and untouched. What
// this file adds is the room the phone draws: the list of who is on air, one
// room with its chat backlog, and a JSON event stream of what happens in it.
// Presence, chat and hearts flow through the same liveState and the same
// SSE registry the web uses; a JSON twin rides on the fragment the web gets,
// so one chat line reaches every viewer whichever client they hold.
//
// Shape is F33D3RKit/Sources/F33D3RKit/Models/Live.swift.

const liveNoVideoNote = "This broadcast has no video feed. Chat, tips and presence are live."

type LiveStreamDTO struct {
	ID               string        `json:"id"`
	Author           WorkAuthorDTO `json:"author"`
	Title            string        `json:"title"`
	Description      *string       `json:"description,omitempty"`
	Status           string        `json:"status"`
	StartedAt        *time.Time    `json:"started_at,omitempty"`
	EndedAt          *time.Time    `json:"ended_at,omitempty"`
	ViewerCount      int           `json:"viewer_count"`
	IsNSFW           bool          `json:"is_nsfw"`
	Audience         string        `json:"audience"`
	Lane             *string       `json:"lane,omitempty"`
	TipGoalUAET      int64         `json:"tip_goal_uaet"`
	TipTotalUAET     int64         `json:"tip_total_uaet"`
	PinnedBody       *string       `json:"pinned_body,omitempty"`
	HeartCount       int           `json:"heart_count"`
	HasVideo         bool          `json:"has_video"`
	VideoUnavailable *string       `json:"video_unavailable,omitempty"`
	TopTippers       []TipperDTO   `json:"top_tippers,omitempty"`
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
	AmountUAET *int64         `json:"amount_uaet,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

// LiveListDTO is the live lane: everything on air the viewer may watch, and
// the viewer's own open broadcast when there is one.
type LiveListDTO struct {
	Streams []LiveStreamDTO `json:"streams"`
	Count   int             `json:"count"`
	Own     *LiveStreamDTO  `json:"own,omitempty"`
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

// liveAuthorDTO is the author strip from the fields the live query joins in.
func liveAuthorDTO(s *model.LiveStream) WorkAuthorDTO {
	name := s.AuthorName
	if name == "" {
		name = s.AuthorHandle
	}
	d := WorkAuthorDTO{Handle: s.AuthorHandle, DisplayName: name, AvatarURL: strPtr(s.AvatarURL), Role: model.RoleUser, Realm: 1}
	return d
}

func (h *Handler) liveStreamDTO(s *model.LiveStream, viewer *model.User) LiveStreamDTO {
	d := LiveStreamDTO{
		ID:           s.ID,
		Author:       liveAuthorDTO(s),
		Title:        s.Title,
		Description:  strPtr(s.Description),
		Status:       s.Status,
		StartedAt:    s.StartedAt,
		EndedAt:      s.EndedAt,
		IsNSFW:       s.IsNSFW,
		Audience:     s.Audience,
		Lane:         strPtr(s.Lane),
		TipGoalUAET:  s.TipGoalUAET,
		TipTotalUAET: s.TipTotalUAET,
		PinnedBody:   strPtr(s.PinnedBody),
		HeartCount:   s.HeartCount,
	}
	if d.Audience == "" {
		d.Audience = "everyone"
	}
	if s.Status == model.LiveStatusLive {
		d.ViewerCount = liveRuntime.count(s.ID)
		d.HasVideo = s.SourceWidth > 0
	}
	if !d.HasVideo {
		d.VideoUnavailable = strPtr(liveNoVideoNote)
	}
	if h.db != nil && viewer != nil && viewer.ID == s.AuthorID {
		tippers, err := dbpkg.TopStreamTippers(h.db, s.ID, 3)
		if err != nil {
			log.Printf("[api/v1] top tippers for %s: %v", s.ID, err)
		}
		for _, t := range tippers {
			handle := dbpkg.GetHandleForPIAL(h.db, t.PIAL)
			if handle == "" {
				continue
			}
			u, err := dbpkg.GetUserByHandle(h.db, handle)
			if err != nil || u == nil {
				continue
			}
			d.TopTippers = append(d.TopTippers, TipperDTO{Author: userAuthorDTO(u), AmountUAET: t.AmountUAET})
		}
	}
	return d
}

func liveChatDTO(m liveChatMessage) LiveChatDTO {
	kind := m.Kind
	if kind == "" {
		kind = "chat"
	}
	d := LiveChatDTO{ID: m.ID, Kind: kind, Body: m.Body, CreatedAt: m.CreatedAt}
	if m.Handle != "" {
		name := m.Name
		if name == "" {
			name = m.Handle
		}
		d.Author = &WorkAuthorDTO{Handle: m.Handle, DisplayName: name, AvatarURL: strPtr(m.AvatarURL), Role: model.RoleUser, Realm: 1}
	}
	if m.AmountUAET > 0 {
		amt := m.AmountUAET
		d.AmountUAET = &amt
	}
	return d
}

// liveMayWatch is who a broadcast admits: the author, everyone, the author's
// followers, or the author's subscribers, and never past a block or an adult
// gate the viewer is behind.
func (h *Handler) liveMayWatch(s *model.LiveStream, u *model.User) bool {
	if s == nil || u == nil {
		return false
	}
	if s.AuthorID == u.ID {
		return true
	}
	if s.IsBlocked || (s.IsNSFW && excludeNSFW(u)) {
		return false
	}
	switch s.Audience {
	case "followers":
		ok, _ := dbpkg.IsFollowing(h.db, u.ID, s.AuthorID)
		return ok
	case "subscribers":
		return dbpkg.IsSubscribed(h.db, u.ID, s.AuthorID)
	default:
		return true
	}
}

func (h *Handler) liveWallError(w http.ResponseWriter, s *model.LiveStream) {
	switch s.Audience {
	case "followers":
		apiError(w, http.StatusForbidden, "followers_only", "This broadcast is for @"+s.AuthorHandle+"'s followers.")
	case "subscribers":
		apiError(w, http.StatusForbidden, "subscribers_only", "This broadcast is for @"+s.AuthorHandle+"'s subscribers.")
	default:
		apiError(w, http.StatusForbidden, "restricted", "This broadcast is not available to you.")
	}
}

// apiV1Live — GET /api/v1/live: who is on air, and the viewer's own open
// broadcast if any.
func (h *Handler) apiV1Live(w http.ResponseWriter, r *http.Request, u *model.User) {
	streams, err := dbpkg.GetActiveStreams(h.db, 50)
	if err != nil {
		apiServerError(w, err)
		return
	}
	out := make([]LiveStreamDTO, 0, len(streams))
	for _, s := range streams {
		if s == nil || !h.liveMayWatch(s, u) {
			continue
		}
		out = append(out, h.liveStreamDTO(s, u))
	}
	var own *LiveStreamDTO
	if mine, err := dbpkg.GetStreamForAuthor(h.db, u.ID); err == nil && mine != nil {
		d := h.liveStreamDTO(mine, u)
		own = &d
	} else if err != nil && !errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
		log.Printf("[api/v1] own stream for %s: %v", u.ID, err)
	}
	apiJSON(w, http.StatusOK, LiveListDTO{Streams: out, Count: len(out), Own: own})
}

// apiLoadLive resolves a stream id for a viewer, answering 404 or the wall.
func (h *Handler) apiLoadLive(w http.ResponseWriter, id string, u *model.User) (*model.LiveStream, bool) {
	if _, err := uuid.Parse(id); err != nil {
		apiError(w, http.StatusNotFound, "not_found", "That stream doesn't exist.")
		return nil, false
	}
	s, err := dbpkg.GetLiveStreamByID(h.db, id)
	if err != nil {
		if errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
			apiError(w, http.StatusNotFound, "not_found", "That stream doesn't exist.")
			return nil, false
		}
		apiServerError(w, err)
		return nil, false
	}
	if !h.liveMayWatch(s, u) {
		h.liveWallError(w, s)
		return nil, false
	}
	return s, true
}

// apiV1LiveRoom — GET /api/v1/live/{id}: the stream and its chat backlog.
func (h *Handler) apiV1LiveRoom(w http.ResponseWriter, r *http.Request, u *model.User) {
	s, ok := h.apiLoadLive(w, r.PathValue("id"), u)
	if !ok {
		return
	}
	backlog := liveRuntime.chatBacklog(s.ID)
	out := LiveRoomDTO{Stream: h.liveStreamDTO(s, u), Chat: make([]LiveChatDTO, 0, len(backlog))}
	for _, m := range backlog {
		out.Chat = append(out.Chat, liveChatDTO(m))
	}
	apiJSON(w, http.StatusOK, out)
}

// liveJSONFrame is what publishLiveJSON puts on the registry: one room
// event, addressed by stream so a viewer's stream handler can pick out the
// room it is watching from everything its account receives.
type liveJSONFrame struct {
	StreamID string          `json:"stream_id"`
	Event    string          `json:"event"`
	Data     json.RawMessage `json:"data"`
}

// publishLiveJSON sends one room event to everyone watching s, and to the
// broadcaster, as the JSON twin of the fragments publishLiveFacet sends.
func (h *Handler) publishLiveJSON(s *model.LiveStream, event string, data interface{}) {
	if s == nil {
		return
	}
	raw, err := json.Marshal(data)
	if err != nil {
		log.Printf("[live] json %s for %s: %v", event, s.ID, err)
		return
	}
	frame, err := json.Marshal(liveJSONFrame{StreamID: s.ID, Event: event, Data: raw})
	if err != nil {
		return
	}
	ev := SSEEvent{Type: "live_json", JSON: string(frame)}
	sent := make(map[string]bool)
	for _, pial := range liveRuntime.audiencePIALs(s.ID) {
		if pial == "" || sent[pial] {
			continue
		}
		sent[pial] = true
		PublishToUser(pial, ev)
	}
	if s.AuthorPIAL != "" && !sent[s.AuthorPIAL] {
		PublishToUser(s.AuthorPIAL, ev)
	}
}

// apiV1LiveEvents — GET /api/v1/live/{id}/events: the room as a JSON event
// stream. Holding the stream is being in the room: the connection is a
// heartbeat, so the viewer counts and is swept when it drops.
func (h *Handler) apiV1LiveEvents(w http.ResponseWriter, r *http.Request, u *model.User) {
	s, ok := h.apiLoadLive(w, r.PathValue("id"), u)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		apiError(w, http.StatusInternalServerError, "no_stream", "SSE not supported.")
		return
	}
	// A room stream, not an account stream: being in this room must not cost the
	// viewer the /api/v1/events connection their notifications arrive on.
	ch, done, cleanup := RegisterRoomSSESession(u.ID, u.PIALID)
	defer cleanup()
	h.startLiveSweeper()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	beat := func() {
		if u.PIALID == "" {
			return
		}
		count := liveRuntime.touch(s.ID, "pial:"+u.PIALID, u.PIALID)
		go h.publishViewerCount(s, count)
	}
	beat()
	writeSSEFrame(w, "viewers", fmt.Sprintf(`{"count":%d}`, liveRuntime.count(s.ID)))
	flusher.Flush()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-done:
			return
		case <-ping.C:
			beat()
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Type != "live_json" || ev.JSON == "" {
				continue
			}
			var frame liveJSONFrame
			if err := json.Unmarshal([]byte(ev.JSON), &frame); err != nil || frame.StreamID != s.ID {
				continue
			}
			writeSSEFrame(w, frame.Event, string(frame.Data))
			flusher.Flush()
		}
	}
}

// eventBoolField reads a boolean the native clients send as true/false or
// the web sends as 1/0. Absent means def.
func eventBoolField(r *http.Request, rawBody map[string]json.RawMessage, def bool, names ...string) bool {
	v := strings.ToLower(eventField(r, rawBody, names...))
	switch v {
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	}
	return def
}

// apiV1LiveEvent is the event lane's live vocabulary for the native clients:
// live_start, live_end, live_chat, live_pin, live_heart. Returns false for
// any other event type so the caller carries on.
func (h *Handler) apiV1LiveEvent(w http.ResponseWriter, r *http.Request, eventType string, rawBodyMap map[string]json.RawMessage) bool {
	switch eventType {
	case "live_start", "live_end", "live_chat", "live_pin", "live_heart":
	default:
		return false
	}
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return true
	}
	user := liveActor(h.userFromRequest(w, r))
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return true
	}
	switch eventType {
	case "live_start":
		h.liveStartEvent(w, r, user, rawBodyMap)
	case "live_end":
		h.liveEndEvent(w, r, user, rawBodyMap)
	case "live_chat":
		h.liveChatEvent(w, r, user, rawBodyMap)
	case "live_pin":
		h.livePinEvent(w, r, user, rawBodyMap)
	case "live_heart":
		h.liveHeartEvent(w, r, user, rawBodyMap)
	}
	return true
}

func (h *Handler) liveStartEvent(w http.ResponseWriter, r *http.Request, user *model.User, raw map[string]json.RawMessage) {
	if user.PIALID == "" {
		http.Error(w, "your identity is not fully set up yet — finish onboarding to broadcast", http.StatusForbidden)
		return
	}
	title := strings.TrimSpace(eventField(r, raw, "title"))
	if title == "" {
		http.Error(w, "title required", http.StatusBadRequest)
		return
	}
	if len([]rune(title)) > 120 {
		title = string([]rune(title)[:120])
	}
	description := strings.TrimSpace(eventField(r, raw, "description"))
	if len([]rune(description)) > 600 {
		description = string([]rune(description)[:600])
	}
	isNSFW := eventBoolField(r, raw, false, "is_nsfw", "nsfw")
	if isNSFW && !(user.IsAdultCreator || (user.IsAdult && user.ContentSetting == "adult_enabled")) {
		http.Error(w, "your account is not cleared to broadcast 18+ content", http.StatusForbidden)
		return
	}
	audience := strings.ToLower(strings.TrimSpace(eventField(r, raw, "audience")))
	switch audience {
	case "":
		audience = "everyone"
	case "everyone", "followers", "subscribers":
	default:
		http.Error(w, "unknown audience", http.StatusBadRequest)
		return
	}
	lane := strings.ToLower(strings.TrimSpace(eventField(r, raw, "lane")))
	if len([]rune(lane)) > 40 {
		lane = string([]rune(lane)[:40])
	}
	var tipGoalUAET int64
	if goal, ok := eventIntField(r, raw, "tip_goal_aet"); ok && goal > 0 {
		tipGoalUAET = int64(goal) * 1_000_000
	}
	notify := eventBoolField(r, raw, true, "notify_followers")
	saveReplay := eventBoolField(r, raw, true, "save_replay")

	if open, err := dbpkg.GetStreamForAuthor(h.db, user.ID); err == nil && open != nil {
		http.Error(w, "you already have an open broadcast", http.StatusConflict)
		return
	} else if err != nil && !errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
		log.Printf("[live] open stream lookup for %s: %v", user.ID, err)
		http.Error(w, "the broadcast could not be opened", http.StatusInternalServerError)
		return
	}

	s, _, err := dbpkg.CreateStream(h.db, user.ID, user.PIALID, title, isNSFW)
	if err != nil {
		log.Printf("[live] create stream for %s: %v", user.ID, err)
		http.Error(w, "the broadcast could not be opened", http.StatusInternalServerError)
		return
	}
	if err := dbpkg.SetStreamMeta(h.db, s.ID, title, description); err != nil {
		log.Printf("[live] stream meta for %s: %v", s.ID, err)
		http.Error(w, "the broadcast details could not be saved", http.StatusInternalServerError)
		return
	}
	if err := dbpkg.SetStreamRoom(h.db, s.ID, audience, lane, tipGoalUAET, saveReplay); err != nil {
		log.Printf("[live] stream room for %s: %v", s.ID, err)
		http.Error(w, "the broadcast settings could not be saved", http.StatusInternalServerError)
		return
	}
	// A native broadcast opens its room at once: there is no media ingest to
	// wait for, and a room nobody can find is not open.
	if err := dbpkg.StartStream(h.db, s.ID, 0, 0, "native"); err != nil {
		log.Printf("[live] open native room %s: %v", s.ID, err)
		http.Error(w, "the broadcast could not be opened", http.StatusInternalServerError)
		return
	}
	s, err = dbpkg.GetLiveStreamByID(h.db, s.ID)
	if err != nil {
		log.Printf("[live] reload %s: %v", s.ID, err)
		http.Error(w, "the broadcast could not be read back", http.StatusInternalServerError)
		return
	}
	h.startLiveSweeper()
	h.publishStreamStatus(s)

	if notify {
		go h.notifyFollowersLive(user, s)
	}
	apiJSON(w, http.StatusCreated, h.liveStreamDTO(s, user))
}

// notifyFollowersLive tells the broadcaster's followers a room opened: an
// inbox row each, and the live_start signal on the user stream.
func (h *Handler) notifyFollowersLive(user *model.User, s *model.LiveStream) {
	rows, err := h.db.Query(`
		SELECT f.follower_id::text, COALESCE(u.pial_id::text, '')
		  FROM follows f JOIN users u ON u.id = f.follower_id
		 WHERE f.following_id = $1::uuid`, user.ID)
	if err != nil {
		log.Printf("[live] followers of %s: %v", user.ID, err)
		return
	}
	defer rows.Close()
	twin := fmt.Sprintf(`{"stream_id":%q}`, s.ID)
	for rows.Next() {
		var fid, pial string
		if err := rows.Scan(&fid, &pial); err != nil {
			continue
		}
		h.notifyUserWithPayload(fid, "live_start", user.ID, s.ID, "live", map[string]interface{}{"preview": s.Title})
		if pial != "" {
			PublishToUser(pial, SSEEvent{Type: "live_start", JSON: twin})
		}
	}
}

func (h *Handler) liveEndEvent(w http.ResponseWriter, r *http.Request, user *model.User, raw map[string]json.RawMessage) {
	streamID := eventField(r, raw, "stream_id")
	if streamID == "" {
		http.Error(w, "stream_id required", http.StatusBadRequest)
		return
	}
	s, err := dbpkg.GetLiveStreamByID(h.db, streamID)
	if err != nil {
		if errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
			http.Error(w, "stream not found", http.StatusNotFound)
			return
		}
		log.Printf("[live] end load %s: %v", streamID, err)
		http.Error(w, "stream unavailable", http.StatusInternalServerError)
		return
	}
	if s.AuthorID != user.ID && !user.IsAdmin() {
		http.Error(w, "this is not your broadcast", http.StatusForbidden)
		return
	}
	if err := dbpkg.EndStream(h.db, s.ID); err != nil {
		log.Printf("[live] end %s: %v", s.ID, err)
		http.Error(w, "the broadcast could not be ended", http.StatusInternalServerError)
		return
	}
	ended, err := dbpkg.GetLiveStreamByID(h.db, s.ID)
	if err != nil {
		log.Printf("[live] reload ended stream %s: %v", s.ID, err)
		ended = s
		ended.Status = model.LiveStatusEnded
	}
	h.publishStreamStatus(ended)
	liveRuntime.forget(ended.ID)

	var duration int64
	if ended.StartedAt != nil {
		end := time.Now()
		if ended.EndedAt != nil {
			end = *ended.EndedAt
		}
		duration = int64(end.Sub(*ended.StartedAt).Seconds())
		if duration < 0 {
			duration = 0
		}
	}
	apiJSON(w, http.StatusOK, LiveSummaryDTO{
		DurationSecs: duration,
		PeakViewers:  ended.PeakViewers,
		TipsUAET:     ended.TipTotalUAET,
		NewFollowers: dbpkg.StreamNewFollowers(h.db, ended),
		ChatLines:    ended.ChatLines,
		Hearts:       ended.HeartCount,
		ReplaySaved:  ended.SaveReplay && ended.SourceWidth > 0,
	})
}

// liveRoomFor loads an open room the user may take part in, answering the
// plain-text errors the event lane speaks.
func (h *Handler) liveRoomFor(w http.ResponseWriter, user *model.User, streamID string) (*model.LiveStream, bool) {
	if streamID == "" {
		http.Error(w, "stream_id required", http.StatusBadRequest)
		return nil, false
	}
	s, err := dbpkg.GetLiveStreamByID(h.db, streamID)
	if err != nil {
		if errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
			http.Error(w, "stream not found", http.StatusNotFound)
			return nil, false
		}
		log.Printf("[live] room load %s: %v", streamID, err)
		http.Error(w, "stream unavailable", http.StatusInternalServerError)
		return nil, false
	}
	if s.Status == model.LiveStatusEnded {
		http.Error(w, "this stream has ended", http.StatusConflict)
		return nil, false
	}
	if s.IsBlocked {
		http.Error(w, "this stream is unavailable", http.StatusForbidden)
		return nil, false
	}
	if !h.liveMayWatch(s, user) {
		switch s.Audience {
		case "followers":
			http.Error(w, "followers only", http.StatusForbidden)
		case "subscribers":
			http.Error(w, "subscribers only", http.StatusForbidden)
		default:
			http.Error(w, "not available for this account", http.StatusForbidden)
		}
		return nil, false
	}
	return s, true
}

func (h *Handler) liveChatEvent(w http.ResponseWriter, r *http.Request, viewer *model.User, raw map[string]json.RawMessage) {
	s, ok := h.liveRoomFor(w, viewer, eventField(r, raw, "stream_id"))
	if !ok {
		return
	}
	body := strings.TrimSpace(eventField(r, raw, "body"))
	if body == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}
	if len([]rune(body)) > 280 {
		body = string([]rune(body)[:280])
	}
	msg := h.appendLiveChat(s, viewer, "chat", body, 0)
	apiJSON(w, http.StatusOK, liveChatDTO(msg))
}

// appendLiveChat is one chat line landing in a room, whichever client sent
// it: stored in the ring, the sender kept present, the fragment sent to every
// web viewer, the JSON twin to every native viewer, the tally bumped.
func (h *Handler) appendLiveChat(s *model.LiveStream, from *model.User, kind, body string, amountUAET int64) liveChatMessage {
	now := time.Now()
	msg := liveChatMessage{
		ID:            uuid.New().String(),
		StreamID:      s.ID,
		Handle:        from.Handle,
		Name:          from.DisplayName,
		AvatarURL:     from.AvatarURL,
		Body:          body,
		CreatedAt:     now,
		TimeLabel:     now.Format("15:04"),
		IsBroadcaster: from.ID == s.AuthorID,
		Kind:          kind,
		AmountUAET:    amountUAET,
	}
	if msg.Name == "" {
		msg.Name = msg.Handle
	}
	liveRuntime.appendChat(msg)
	if from.PIALID != "" {
		liveRuntime.touch(s.ID, "pial:"+from.PIALID, from.PIALID)
	}
	if kind == "chat" {
		if frag, err := h.renderLiveFragment("live_chat_row", map[string]interface{}{"M": msg}); err != nil {
			log.Printf("[live] %v", err)
		} else {
			h.publishLiveFacet(s, frag)
		}
		dbpkg.CountStreamChatLine(h.db, s.ID)
	}
	h.publishLiveJSON(s, kind, liveChatDTO(msg))
	return msg
}

func (h *Handler) livePinEvent(w http.ResponseWriter, r *http.Request, user *model.User, raw map[string]json.RawMessage) {
	s, ok := h.liveRoomFor(w, user, eventField(r, raw, "stream_id"))
	if !ok {
		return
	}
	if s.AuthorID != user.ID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	body := strings.TrimSpace(eventField(r, raw, "body"))
	if len([]rune(body)) > 140 {
		body = string([]rune(body)[:140])
	}
	if err := dbpkg.PinStreamLine(h.db, s.ID, body); err != nil {
		if errors.Is(err, dbpkg.ErrLiveNotOpen) {
			http.Error(w, "this stream has ended", http.StatusConflict)
			return
		}
		log.Printf("[live] pin %s: %v", s.ID, err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	h.publishLiveJSON(s, "pinned", map[string]string{"body": body})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) liveHeartEvent(w http.ResponseWriter, r *http.Request, user *model.User, raw map[string]json.RawMessage) {
	s, ok := h.liveRoomFor(w, user, eventField(r, raw, "stream_id"))
	if !ok {
		return
	}
	n, err := dbpkg.HeartStream(h.db, s.ID)
	if err != nil {
		if errors.Is(err, dbpkg.ErrLiveNotOpen) {
			http.Error(w, "this stream has ended", http.StatusConflict)
			return
		}
		log.Printf("[live] heart %s: %v", s.ID, err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	if user.PIALID != "" {
		liveRuntime.touch(s.ID, "pial:"+user.PIALID, user.PIALID)
	}
	h.publishLiveJSON(s, "hearts", map[string]int{"count": n})
	// The JSON twin above reaches native clients over their event stream; web
	// viewers are HTMX/Facet-driven and never parse that JSON, so without this
	// they never learn a heart landed. live_heart_burst is the Fragment twin,
	// same pattern as appendLiveChat's chat_row push.
	for _, slot := range []string{"hearts", "bcast_hearts"} {
		if frag, err := h.renderLiveFragment("live_heart_burst", map[string]interface{}{
			"StreamID": s.ID, "Slot": slot, "Count": n,
		}); err != nil {
			log.Printf("[live] %v", err)
		} else {
			h.publishLiveFacet(s, frag)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// recordLiveTip is what a settled tip does to a room: the row, the running
// total, a tip line in chat, the totals frame, and the broadcaster's inbox.
func (h *Handler) recordLiveTip(s *model.LiveStream, from *model.User, amountUAET int64) {
	total, err := dbpkg.RecordStreamTip(h.db, s.ID, from.PIALID, amountUAET)
	if err != nil {
		log.Printf("[live] record tip on %s from %s: %v", s.ID, from.ID, err)
		return
	}
	h.appendLiveChat(s, from, "tip", "", amountUAET)
	h.publishLiveJSON(s, "tips", map[string]int64{"total_uaet": total, "goal_uaet": s.TipGoalUAET})
	h.notifyUserWithPayload(s.AuthorID, "tip", from.ID, s.ID, "live", map[string]interface{}{"amount_uaet": amountUAET})
}
