package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"f33d3r.com/ios/devserver/internal/store"
)

// Frequencies — the live-audio surface, as Nantar will serve it.
//
// In production this is a proxy: feed-engine calls Auralis over
// `X-Internal-Key` + `X-Pial-Identity`, flattens the brain's double-nested
// view, and resolves every bare PIAL to a handle. Here the brain runs
// in-process against SQLite, which changes two things and only two:
//
//   - Mutations do not have to be diffed back out of a poll. This server
//     performs them, so the SSE frames it pushes are the exact ones the change
//     produced rather than a guess made by comparing two snapshots.
//   - Presence is a participant row in state `joined`, not a Redis TTL key
//     refreshed by heartbeats. Every count the client draws comes from rows.
//
// What does not change: the wire. The routes, the JSON keys, the event_types,
// the error codes and the frame vocabulary are the contract's, and no response
// here carries a PIAL. The `can_*` booleans are computed from the same matrix
// in internal/store that refuses the write, so a client that renders its
// controls from them can never offer a button the server would reject.

// audioUnavailableNote is the server's own sentence for why there is no sound
// yet, the twin of LiveStreamDTO.VideoUnavailable. Audio transport is Phase 3
// of the brain; the room, the roles, the queue and the moderation are real.
const audioUnavailableNote = "Live audio needs the media path, which this deployment does not run. The room, the stage and the queue are live."

// ── DTOs ─────────────────────────────────────────────────────────────────────
//
// Every nullable field is written out as an explicit null rather than omitted:
// the Swift models decode with decodeIfPresent, but the contract lists the key,
// and a key that is sometimes absent is a key somebody eventually forgets.

type FrequencySummaryDTO struct {
	ID               string        `json:"id"`
	Host             WorkAuthorDTO `json:"host"`
	Title            string        `json:"title"`
	Description      *string       `json:"description"`
	State            string        `json:"state"`
	Visibility       string        `json:"visibility"`
	Language         string        `json:"language"`
	IsNSFW           bool          `json:"is_nsfw"`
	ScheduledAt      *time.Time    `json:"scheduled_at"`
	StartedAt        *time.Time    `json:"started_at"`
	EndedAt          *time.Time    `json:"ended_at"`
	EndReason        *string       `json:"end_reason"`
	RecordingEnabled bool          `json:"recording_enabled"`
	ReplayStatus     string        `json:"replay_status"`
	MaxSpeakers      int           `json:"max_speakers"`
	MaxListeners     int           `json:"max_listeners"`
	RequestsOpen     bool          `json:"requests_open"`
	Locked           bool          `json:"locked"`
	ListenerCount    int           `json:"listener_count"`
	SpeakerCount     int           `json:"speaker_count"`
	ParticipantCount int           `json:"participant_count"`
	AudioUnavailable *string       `json:"audio_unavailable"`
}

type FrequencyParticipantDTO struct {
	Author   WorkAuthorDTO `json:"author"`
	Role     string        `json:"role"`
	Muted    bool          `json:"muted"`
	Present  bool          `json:"present"`
	JoinedAt time.Time     `json:"joined_at"`
}

type FrequencyRequestDTO struct {
	ID        string        `json:"id"`
	Author    WorkAuthorDTO `json:"author"`
	Reason    string        `json:"reason"`
	Upvotes   int           `json:"upvotes"`
	CreatedAt time.Time     `json:"created_at"`
}

type FrequencyViewerDTO struct {
	Role          *string              `json:"role"`
	Muted         bool                 `json:"muted"`
	Present       bool                 `json:"present"`
	Blocked       bool                 `json:"blocked"`
	Request       *FrequencyRequestDTO `json:"request"`
	CanRequestMic bool                 `json:"can_request_mic"`
	CanModerate   bool                 `json:"can_moderate"`
	CanEnd        bool                 `json:"can_end"`
	CanSpeak      bool                 `json:"can_speak"`
	CanListen     bool                 `json:"can_listen"`
}

type FrequencyRoomDTO struct {
	Frequency FrequencySummaryDTO       `json:"frequency"`
	Speakers  []FrequencyParticipantDTO `json:"speakers"`
	Cohosts   []WorkAuthorDTO           `json:"cohosts"`
	Requests  []FrequencyRequestDTO     `json:"requests"`
	Viewer    *FrequencyViewerDTO       `json:"viewer"`
}

type FrequencyListDTO struct {
	Lane        string                `json:"lane"`
	Frequencies []FrequencySummaryDTO `json:"frequencies"`
	Count       int                   `json:"count"`
	Own         *FrequencySummaryDTO  `json:"own"`
}

// ── Mappers ──────────────────────────────────────────────────────────────────

func frequencySummaryDTO(f *store.Frequency, listeners, speakers int) FrequencySummaryDTO {
	d := FrequencySummaryDTO{
		ID:               f.ID,
		Host:             authorDTO(f.Host),
		Title:            f.Title,
		Description:      strPtr(f.Description),
		State:            f.State,
		Visibility:       f.Visibility,
		Language:         f.Language,
		IsNSFW:           f.AdultContent,
		ScheduledAt:      f.ScheduledAt,
		StartedAt:        f.StartedAt,
		EndedAt:          f.EndedAt,
		EndReason:        strPtr(f.EndReason),
		RecordingEnabled: f.RecordingEnabled,
		ReplayStatus:     f.ReplayStatus,
		MaxSpeakers:      f.MaxSpeakers,
		MaxListeners:     f.MaxListeners,
		RequestsOpen:     f.RequestsOpen,
		Locked:           f.Locked,
		ListenerCount:    listeners,
		SpeakerCount:     speakers,
		ParticipantCount: listeners + speakers,
	}
	if store.FreqStateOpen(f.State) {
		note := audioUnavailableNote
		d.AudioUnavailable = &note
	}
	return d
}

func frequencyParticipantDTO(p *store.FrequencyParticipant) FrequencyParticipantDTO {
	return FrequencyParticipantDTO{
		Author:   authorDTO(p.User),
		Role:     p.Role,
		Muted:    p.Muted,
		Present:  p.Present(),
		JoinedAt: p.JoinedAt,
	}
}

func frequencyRequestDTO(r *store.FrequencySpeakerRequest) FrequencyRequestDTO {
	return FrequencyRequestDTO{
		ID:        r.ID,
		Author:    authorDTO(r.User),
		Reason:    r.Reason,
		Upvotes:   r.Upvotes,
		CreatedAt: r.CreatedAt,
	}
}

// ── Reading the room ─────────────────────────────────────────────────────────

// frequencyRoom builds the read model for one viewer: the row, the counts, the
// stage, the co-host grants, the queue (moderators only) and the viewer's own
// standing. It is the brain's `view::build`, with `joined` for presence.
func (s *Server) frequencyRoom(ctx context.Context, f *store.Frequency, viewer *store.User) (*FrequencyRoomDTO, error) {
	participants, err := s.store.FrequencyParticipants(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	listeners, speakers, _ := store.FrequencyCounts(participants)

	out := &FrequencyRoomDTO{
		Frequency: frequencySummaryDTO(f, listeners, speakers),
		Speakers:  []FrequencyParticipantDTO{},
		Cohosts:   []WorkAuthorDTO{},
		Requests:  []FrequencyRequestDTO{},
	}
	var me *store.FrequencyParticipant
	for _, p := range participants {
		if store.FreqSpeaks(p.Role) {
			out.Speakers = append(out.Speakers, frequencyParticipantDTO(p))
		}
		if viewer != nil && p.PIAL == viewer.PIALID {
			me = p
		}
	}

	cohostPIALs, err := s.store.FrequencyCohosts(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	cohostUsers, err := s.store.GetUsersByPIAL(ctx, cohostPIALs)
	if err != nil {
		return nil, err
	}
	for _, p := range cohostPIALs {
		if u := cohostUsers[p]; u != nil {
			out.Cohosts = append(out.Cohosts, authorDTO(u))
		}
	}

	if viewer == nil {
		return out, nil
	}

	role := ""
	if me != nil {
		role = me.Role
	}
	blocked, err := s.store.FrequencyBlocked(ctx, f.ID, viewer.PIALID)
	if err != nil {
		return nil, err
	}
	var own *FrequencyRequestDTO
	if pending, err := s.store.PendingFrequencyRequestFor(ctx, f.ID, viewer.PIALID); err != nil {
		return nil, err
	} else if pending != nil {
		d := frequencyRequestDTO(pending)
		own = &d
	}

	v := &FrequencyViewerDTO{
		Role:    strPtr(role),
		Blocked: blocked,
		Request: own,
		// Every one of these comes from the matrix in internal/store — the same
		// function the mutation asks before it writes.
		CanRequestMic: store.FreqMay(role, store.FreqActRequestMic) && f.RequestsOpen &&
			f.State == store.FreqStateLive && own == nil,
		CanModerate: store.FreqMay(role, store.FreqActApprove),
		CanEnd:      store.FreqMay(role, store.FreqActEnd),
		CanSpeak:    store.FreqMay(role, store.FreqActSpeak),
		CanListen:   store.FreqMay(role, store.FreqActListen),
	}
	if me != nil {
		v.Muted = me.Muted
		v.Present = me.Present()
	}
	out.Viewer = v

	// The queue is the moderators' to read: a listener never sees who else
	// has their hand up, or why.
	if v.CanModerate && store.FreqStateOpen(f.State) {
		requests, err := s.store.PendingFrequencyRequests(ctx, f.ID)
		if err != nil {
			return nil, err
		}
		for _, r := range requests {
			out.Requests = append(out.Requests, frequencyRequestDTO(r))
		}
	}
	return out, nil
}

// mayViewFrequency answers the visibility column. The host always sees their
// own; everyone else has to clear the audience the host chose.
func (s *Server) mayViewFrequency(ctx context.Context, f *store.Frequency, u *store.User) (bool, error) {
	if f.Host != nil && f.Host.ID == u.ID {
		return true, nil
	}
	switch f.Visibility {
	case "public":
		return true, nil
	case "followers":
		if f.Host == nil {
			return false, nil
		}
		return s.store.IsFollowing(ctx, u.ID, f.Host.ID)
	case "subscribers":
		if f.Host == nil {
			return false, nil
		}
		var n int
		err := s.store.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM subscriptions WHERE subscriber_id = ? AND creator_id = ? AND status = 'active'`,
			u.ID, f.Host.ID).Scan(&n)
		return n > 0, err
	}
	// private: invitees only, and there are no invites in v1.
	return false, nil
}

// ── Routes ───────────────────────────────────────────────────────────────────

// frequencies — GET /api/v1/frequencies?lane=live|scheduled|ended|mine.
func (s *Server) frequencies(w http.ResponseWriter, r *http.Request, u *store.User) {
	lane := store.FrequencyLane(r.URL.Query().Get("lane"))
	if lane == "" {
		lane = store.FrequencyLaneLive
	}
	switch lane {
	case store.FrequencyLaneLive, store.FrequencyLaneScheduled, store.FrequencyLaneEnded, store.FrequencyLaneMine:
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "Unknown lane.")
		return
	}
	list, err := s.store.ListFrequencies(r.Context(), lane, u.PIALID, limitParam(r, 24, 100))
	if err != nil {
		serverError(w, err)
		return
	}
	out := FrequencyListDTO{Lane: string(lane), Frequencies: []FrequencySummaryDTO{}}
	for _, f := range list {
		visible, err := s.mayViewFrequency(r.Context(), f, u)
		if err != nil {
			serverError(w, err)
			return
		}
		if !visible {
			continue
		}
		summary, err := s.frequencySummary(r.Context(), f)
		if err != nil {
			serverError(w, err)
			return
		}
		out.Frequencies = append(out.Frequencies, summary)
	}
	out.Count = len(out.Frequencies)

	// "Return to your Frequency": whatever the caller has open right now.
	if own, err := s.store.OpenFrequencyFor(r.Context(), u.PIALID); err == nil {
		summary, err := s.frequencySummary(r.Context(), own)
		if err != nil {
			serverError(w, err)
			return
		}
		out.Own = &summary
	} else if !errors.Is(err, store.ErrFreqNotFound) {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// frequencySummary counts the room from its rows and projects the card.
func (s *Server) frequencySummary(ctx context.Context, f *store.Frequency) (FrequencySummaryDTO, error) {
	participants, err := s.store.FrequencyParticipants(ctx, f.ID)
	if err != nil {
		return FrequencySummaryDTO{}, err
	}
	listeners, speakers, _ := store.FrequencyCounts(participants)
	return frequencySummaryDTO(f, listeners, speakers), nil
}

// frequency — GET /api/v1/frequencies/{id}.
func (s *Server) frequency(w http.ResponseWriter, r *http.Request, u *store.User) {
	f, ok := s.frequencyForRead(w, r, u, r.PathValue("id"))
	if !ok {
		return
	}
	room, err := s.frequencyRoom(r.Context(), f, u)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

// frequencyForRead loads a Frequency and applies the visibility rule.
func (s *Server) frequencyForRead(w http.ResponseWriter, r *http.Request, u *store.User, id string) (*store.Frequency, bool) {
	f, err := s.store.GetFrequency(r.Context(), id)
	if err != nil {
		if writeFreqError(w, err) {
			return nil, false
		}
		serverError(w, err)
		return nil, false
	}
	visible, err := s.mayViewFrequency(r.Context(), f, u)
	if err != nil {
		serverError(w, err)
		return nil, false
	}
	if !visible {
		writeError(w, http.StatusForbidden, "forbidden", "This Frequency isn't open to you.")
		return nil, false
	}
	return f, true
}

// ── SSE ──────────────────────────────────────────────────────────────────────

func freqTopic(id string) string { return "freq:" + id }

// frequencyEvents — GET /api/v1/frequencies/{id}/events.
//
// The first frame is `room`, and every mutation pushes the specific frame it
// produced. `room` is rendered per connection rather than fanned out
// pre-encoded, because a room carries the reader's own `viewer` block: one
// shared copy would hand every listener the host's controls.
func (s *Server) frequencyEvents(w http.ResponseWriter, r *http.Request, u *store.User) {
	f, ok := s.frequencyForRead(w, r, u, r.PathValue("id"))
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_stream", "SSE not supported.")
		return
	}
	sub, cancel := s.hub.subscribe(freqTopic(f.ID), u.ID)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	write := func(event string, data []byte) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	}
	// The opening frame is the whole room, so a reconnect never renders from
	// a diff it missed.
	if room, err := s.frequencyRoom(r.Context(), f, u); err == nil {
		data, _ := json.Marshal(room)
		write("room", data)
	}

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case fr := <-sub.ch:
			if fr.Event == "room" {
				fresh, err := s.store.GetFrequency(r.Context(), f.ID)
				if err != nil {
					continue
				}
				room, err := s.frequencyRoom(r.Context(), fresh, u)
				if err != nil {
					continue
				}
				data, _ := json.Marshal(room)
				write("room", data)
				continue
			}
			write(fr.Event, fr.Data)
		}
	}
}

// publishFrequencyRoom asks every open connection to re-render for its own
// reader. Used for changes no narrower frame covers.
func (s *Server) publishFrequencyRoom(id string) {
	s.hub.publish(freqTopic(id), "room", nil)
}

// publishFrequencyCounts pushes presence after any membership change.
func (s *Server) publishFrequencyCounts(ctx context.Context, id string) {
	participants, err := s.store.FrequencyParticipants(ctx, id)
	if err != nil {
		return
	}
	listeners, speakers, total := store.FrequencyCounts(participants)
	s.hub.publish(freqTopic(id), "counts", map[string]int{
		"listeners": listeners, "speakers": speakers, "participants": total,
	})
}

// publishFrequencyRequest pushes a raised hand to the moderators only: the
// queue is theirs to read, and the hub delivers by account.
func (s *Server) publishFrequencyRequest(ctx context.Context, f *store.Frequency, req *store.FrequencySpeakerRequest) {
	mods, err := s.frequencyModeratorIDs(ctx, f)
	if err != nil || len(mods) == 0 {
		return
	}
	s.hub.publishTo(freqTopic(f.ID), "request", frequencyRequestDTO(req), mods)
}

// frequencyModeratorIDs is the host plus every active co-host, as account ids.
func (s *Server) frequencyModeratorIDs(ctx context.Context, f *store.Frequency) ([]string, error) {
	pials, err := s.store.FrequencyCohosts(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	pials = append(pials, f.HostPIAL)
	users, err := s.store.GetUsersByPIAL(ctx, pials)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, u.ID)
	}
	return out, nil
}

// ── The write lane ───────────────────────────────────────────────────────────

// writeFreqError turns one of the brain's machine codes into the JSON error
// envelope the client branches on. Reports whether it handled err.
func writeFreqError(w http.ResponseWriter, err error) bool {
	var fe *store.FreqError
	if errors.As(err, &fe) {
		writeError(w, fe.Status, fe.Code, fe.Message)
		return true
	}
	return false
}

// frequencyEvent dispatches every `frequency_*` event_type.
func (s *Server) frequencyEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	switch body.eventType {
	case "frequency_create":
		s.frequencyCreateEvent(w, r, u, body)
	case "frequency_update":
		s.frequencyUpdateEvent(w, r, u, body)
	case "frequency_schedule":
		s.frequencyScheduleEvent(w, r, u, body)
	case "frequency_start":
		s.frequencyStartEvent(w, r, u, body)
	case "frequency_end":
		s.frequencyEndEvent(w, r, u, body)
	case "frequency_cancel":
		s.frequencyCancelEvent(w, r, u, body)
	case "frequency_join":
		s.frequencyJoinEvent(w, r, u, body)
	case "frequency_leave":
		s.frequencyLeaveEvent(w, r, u, body)
	case "frequency_request_mic":
		s.frequencyRequestMicEvent(w, r, u, body)
	case "frequency_request_withdraw":
		s.frequencyRequestWithdrawEvent(w, r, u, body)
	case "frequency_request_upvote":
		s.frequencyRequestUpvoteEvent(w, r, u, body)
	case "frequency_request_approve", "frequency_request_decline":
		s.frequencyRequestResolveEvent(w, r, u, body)
	case "frequency_mute":
		s.frequencyMuteEvent(w, r, u, body)
	case "frequency_demote":
		s.frequencyDemoteEvent(w, r, u, body)
	case "frequency_remove":
		s.frequencyRemoveEvent(w, r, u, body)
	case "frequency_block", "frequency_unblock":
		s.frequencyBlockEvent(w, r, u, body)
	case "frequency_cohost":
		s.frequencyCohostEvent(w, r, u, body)
	case "frequency_lock":
		s.frequencyLockEvent(w, r, u, body)
	case "frequency_requests_open":
		s.frequencyRequestsOpenEvent(w, r, u, body)
	default:
		plainError(w, http.StatusNotFound, "unknown event_type: "+body.eventType)
	}
}

// frequencyActor loads the Frequency named by `frequency_id` and works out
// what the caller is inside it. The role is the joined row's when they are in
// the room, otherwise the one their standing would give them on entry — a
// co-host who has dropped is still a co-host.
func (s *Server) frequencyActor(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) (*store.Frequency, string, bool) {
	id := body.get("frequency_id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "frequency_id required")
		return nil, "", false
	}
	f, err := s.store.GetFrequency(r.Context(), id)
	if err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return nil, "", false
	}
	me, err := s.store.FrequencyParticipantFor(r.Context(), f.ID, u.PIALID)
	if err != nil {
		serverError(w, err)
		return nil, "", false
	}
	if me != nil {
		return f, me.Role, true
	}
	granted, err := s.store.FrequencyGrant(r.Context(), f.ID, u.PIALID)
	if err != nil {
		serverError(w, err)
		return nil, "", false
	}
	return f, store.FrequencyRoleOnEntry(f, u.PIALID, granted), true
}

// requireFrequencyAction is the matrix, on the wire. A caller who may not
// attempt the action is refused before anything is read or written.
func requireFrequencyAction(w http.ResponseWriter, role, action string) bool {
	if store.FreqMay(role, action) {
		return true
	}
	writeError(w, http.StatusForbidden, "forbidden", "Only the "+frequencyActionAuthority(action)+" can do that.")
	return false
}

func frequencyActionAuthority(action string) string {
	switch action {
	case store.FreqActEnd, store.FreqActCohost, store.FreqActLock, store.FreqActToggleRequests:
		return "host"
	case store.FreqActRequestMic:
		return "listeners"
	case store.FreqActMuteSelf, store.FreqActSpeak:
		return "speakers"
	}
	return "host or a co-host"
}

// frequencyTarget resolves the `handle` a moderation event names to an
// account, and refuses a target the caller does not outrank.
func (s *Server) frequencyTarget(w http.ResponseWriter, r *http.Request, f *store.Frequency, actorRole string, body *eventBody) (*store.User, string, bool) {
	handle := body.get("handle")
	if handle == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "handle required")
		return nil, "", false
	}
	target, err := s.store.GetUserByHandle(r.Context(), handle)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No account with that handle.")
		return nil, "", false
	}
	role := store.FreqRoleListener
	if p, err := s.store.FrequencyParticipantFor(r.Context(), f.ID, target.PIALID); err != nil {
		serverError(w, err)
		return nil, "", false
	} else if p != nil {
		role = p.Role
	} else {
		granted, err := s.store.FrequencyGrant(r.Context(), f.ID, target.PIALID)
		if err != nil {
			serverError(w, err)
			return nil, "", false
		}
		role = store.FrequencyRoleOnEntry(f, target.PIALID, granted)
	}
	if role == store.FreqRoleHost {
		writeError(w, http.StatusConflict, store.ErrFreqIsHost.Code, store.ErrFreqIsHost.Message)
		return nil, "", false
	}
	if !store.FreqOutranks(actorRole, role) {
		writeError(w, http.StatusForbidden, "forbidden", "You can only act on people below you in the room.")
		return nil, "", false
	}
	return target, role, true
}

// answerFrequencyRoom re-reads and writes the room, which is what every
// room-returning event answers.
func (s *Server) answerFrequencyRoom(w http.ResponseWriter, r *http.Request, u *store.User, id string) {
	f, err := s.store.GetFrequency(r.Context(), id)
	if err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	room, err := s.frequencyRoom(r.Context(), f, u)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

// frequencyDraftFrom reads the create/update fields off the event body.
func frequencyDraftFrom(body *eventBody) (store.FrequencyInput, *store.FreqError) {
	in := store.FrequencyInput{
		Title:       body.get("title"),
		Description: body.get("description"),
		Visibility:  body.get("visibility"),
		Language:    body.get("language"),
	}
	in.AdultContent = frequencyBool(body, "is_nsfw", false)
	in.RecordingEnabled = frequencyBool(body, "recording_enabled", false)
	if v := body.get("max_speakers"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return in, freqBadRequestAPI("max_speakers must be a number")
		}
		in.MaxSpeakers = n
	}
	if v := body.get("max_listeners"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return in, freqBadRequestAPI("max_listeners must be a number")
		}
		in.MaxListeners = n
	}
	if v := strings.TrimSpace(body.get("scheduled_at")); v != "" {
		t, err := parseFrequencyTime(v)
		if err != nil {
			return in, freqBadRequestAPI("scheduled_at must be an RFC 3339 timestamp")
		}
		in.ScheduledAt = &t
	}
	return in, nil
}

func freqBadRequestAPI(msg string) *store.FreqError {
	return &store.FreqError{Status: http.StatusBadRequest, Code: "bad_request", Message: msg}
}

func parseFrequencyTime(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, nil
	}
	return time.Parse(store.TimeLayout, v)
}

// frequencyBool reads a flag the way the write lane's other events do: absent
// keeps the default, and "0"/"false" are false whichever encoding they arrive
// in.
func frequencyBool(body *eventBody, key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(body.get(key)))
	if v == "" {
		return def
	}
	return v != "0" && v != "false" && v != "no"
}

// frequencyCreateEvent — 201 with the summary.
func (s *Server) frequencyCreateEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	in, bad := frequencyDraftFrom(body)
	if bad != nil {
		writeError(w, bad.Status, bad.Code, bad.Message)
		return
	}
	if in.AdultContent && !u.IsAdult {
		writeError(w, http.StatusForbidden, "forbidden", "Your account is not cleared to host 18+ rooms.")
		return
	}
	in.HostPIAL = u.PIALID
	f, err := s.store.CreateFrequency(r.Context(), in)
	if err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, frequencySummaryDTO(f, 0, 0))
}

// frequencyUpdateEvent — the host edits the room's own fields.
func (s *Server) frequencyUpdateEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, role, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	if role != store.FreqRoleHost {
		writeError(w, http.StatusForbidden, "forbidden", "Only the host can change a Frequency.")
		return
	}
	in, bad := frequencyDraftFrom(body)
	if bad != nil {
		writeError(w, bad.Status, bad.Code, bad.Message)
		return
	}
	patch := store.FrequencyPatch{}
	if body.has("title") {
		patch.Title = &in.Title
	}
	if body.has("description") {
		patch.Description = &in.Description
	}
	if body.has("visibility") {
		patch.Visibility = &in.Visibility
	}
	if body.has("language") {
		patch.Language = &in.Language
	}
	if body.has("is_nsfw") {
		patch.AdultContent = &in.AdultContent
	}
	if body.has("recording_enabled") {
		patch.RecordingEnabled = &in.RecordingEnabled
	}
	if body.has("max_speakers") {
		patch.MaxSpeakers = &in.MaxSpeakers
	}
	if body.has("max_listeners") {
		patch.MaxListeners = &in.MaxListeners
	}
	if err := s.store.UpdateFrequency(r.Context(), f, patch); err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	s.publishFrequencyRoom(f.ID)
	s.answerFrequencyRoom(w, r, u, f.ID)
}

// frequencyScheduleEvent — set or clear the time. Clearing returns to draft.
func (s *Server) frequencyScheduleEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, role, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	if role != store.FreqRoleHost {
		writeError(w, http.StatusForbidden, "forbidden", "Only the host can schedule a Frequency.")
		return
	}
	var at *time.Time
	if v := strings.TrimSpace(body.get("scheduled_at")); v != "" {
		t, err := parseFrequencyTime(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "scheduled_at must be an RFC 3339 timestamp")
			return
		}
		at = &t
	}
	if err := s.store.ScheduleFrequency(r.Context(), f, at); err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	s.publishFrequencyRoom(f.ID)
	s.answerFrequencyRoom(w, r, u, f.ID)
}

// frequencyStartEvent — the host opens the room, and their followers hear it.
func (s *Server) frequencyStartEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, role, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	if role != store.FreqRoleHost {
		writeError(w, http.StatusForbidden, "forbidden", "Only the host can start a Frequency.")
		return
	}
	if err := s.store.StartFrequency(r.Context(), f); err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	s.notifyFollowersFrequency(r, u, f)
	s.publishFrequencyRoom(f.ID)
	s.answerFrequencyRoom(w, r, u, f.ID)
}

// notifyFollowersFrequency is `notifyFollowersLive`'s twin: an inbox row and a
// signal frame on each follower's own stream.
//
// Collect first, notify after: the store has one connection, and a write
// issued while a cursor is open would wait on itself.
func (s *Server) notifyFollowersFrequency(r *http.Request, u *store.User, f *store.Frequency) {
	followers, err := s.store.FollowerIDs(r.Context(), u.ID)
	if err != nil {
		return
	}
	for _, fid := range followers {
		s.store.Notify(r.Context(), fid, "frequency_start", u.ID, f.ID, "frequency", map[string]any{"preview": f.Title})
		s.hub.publish(userTopic(fid), "frequency_start", map[string]string{"frequency_id": f.ID})
		s.signalNotify(r, fid)
	}
}

// frequencyEndEvent — host only, and the room learns through `state`.
func (s *Server) frequencyEndEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, role, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	if !requireFrequencyAction(w, role, store.FreqActEnd) {
		return
	}
	if err := s.store.EndFrequency(r.Context(), f, "host_ended"); err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	s.publishFrequencyState(r.Context(), f.ID)
	s.answerFrequencyRoom(w, r, u, f.ID)
}

// frequencyCancelEvent — calling off something that never started.
func (s *Server) frequencyCancelEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, role, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	if role != store.FreqRoleHost {
		writeError(w, http.StatusForbidden, "forbidden", "Only the host can cancel a Frequency.")
		return
	}
	if err := s.store.CancelFrequency(r.Context(), f); err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	s.publishFrequencyState(r.Context(), f.ID)
	s.answerFrequencyRoom(w, r, u, f.ID)
}

// publishFrequencyState pushes the frame that closes a room.
func (s *Server) publishFrequencyState(ctx context.Context, id string) {
	f, err := s.store.GetFrequency(ctx, id)
	if err != nil {
		return
	}
	s.hub.publish(freqTopic(id), "state", map[string]any{
		"state": f.State, "end_reason": strPtr(f.EndReason),
	})
}

// frequencyJoinEvent — Tune In.
func (s *Server) frequencyJoinEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, _, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	visible, err := s.mayViewFrequency(r.Context(), f, u)
	if err != nil {
		serverError(w, err)
		return
	}
	if !visible {
		writeError(w, http.StatusForbidden, "forbidden", "This Frequency isn't open to you.")
		return
	}
	role, err := s.store.JoinFrequency(r.Context(), f, u.PIALID)
	if err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	if store.FreqSpeaks(role) {
		s.publishFrequencyParticipant(r.Context(), f.ID, u.PIALID, "speaker_joined")
	}
	s.publishFrequencyCounts(r.Context(), f.ID)
	s.answerFrequencyRoom(w, r, u, f.ID)
}

// publishFrequencyParticipant pushes one person's row under the named frame.
func (s *Server) publishFrequencyParticipant(ctx context.Context, id, pial, event string) {
	p, err := s.store.FrequencyParticipantFor(ctx, id, pial)
	if err != nil || p == nil {
		return
	}
	s.hub.publish(freqTopic(id), event, frequencyParticipantDTO(p))
}

// frequencyLeaveEvent — 204, and the stage loses a face if they were on it.
func (s *Server) frequencyLeaveEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, role, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	// Capture the row before it closes: the frame carries who left.
	var left *FrequencyParticipantDTO
	if store.FreqSpeaks(role) {
		if p, err := s.store.FrequencyParticipantFor(r.Context(), f.ID, u.PIALID); err == nil && p != nil {
			d := frequencyParticipantDTO(p)
			d.Present = false
			left = &d
		}
	}
	if err := s.store.LeaveFrequency(r.Context(), f.ID, u.PIALID); err != nil {
		serverError(w, err)
		return
	}
	if left != nil {
		s.hub.publish(freqTopic(f.ID), "speaker_left", *left)
	}
	s.publishFrequencyCounts(r.Context(), f.ID)
	w.WriteHeader(http.StatusNoContent)
}

// frequencyRequestMicEvent — a listener raises a hand. 201 with the request.
func (s *Server) frequencyRequestMicEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, role, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	if role == store.FreqRoleListener {
		// A listener who is not actually in the room has nothing to be called
		// up from.
		if p, err := s.store.FrequencyParticipantFor(r.Context(), f.ID, u.PIALID); err != nil {
			serverError(w, err)
			return
		} else if p == nil {
			writeError(w, http.StatusConflict, store.ErrFreqNotJoined.Code, store.ErrFreqNotJoined.Message)
			return
		}
	}
	if !requireFrequencyAction(w, role, store.FreqActRequestMic) {
		return
	}
	req, err := s.store.CreateSpeakerRequest(r.Context(), f, u.PIALID, body.get("reason"))
	if err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	s.publishFrequencyRequest(r.Context(), f, req)
	s.publishFrequencyRoom(f.ID)
	writeJSON(w, http.StatusCreated, frequencyRequestDTO(req))
}

// frequencyRequestWithdrawEvent — taking your own hand down. 204.
func (s *Server) frequencyRequestWithdrawEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, _, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	req, ok := s.frequencyRequestFrom(w, r, f, body)
	if !ok {
		return
	}
	if req.PIAL != u.PIALID {
		writeError(w, http.StatusForbidden, "forbidden", "That isn't your request.")
		return
	}
	if err := s.store.ResolveSpeakerRequest(r.Context(), f.ID, req.ID, "withdrawn", u.PIALID); err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	s.publishFrequencyRequestResolved(f.ID, req.ID, "withdrawn")
	s.publishFrequencyRoom(f.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) frequencyRequestFrom(w http.ResponseWriter, r *http.Request, f *store.Frequency, body *eventBody) (*store.FrequencySpeakerRequest, bool) {
	id := body.get("request_id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "request_id required")
		return nil, false
	}
	req, err := s.store.GetFrequencyRequest(r.Context(), f.ID, id)
	if err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return nil, false
	}
	return req, true
}

func (s *Server) publishFrequencyRequestResolved(id, requestID, outcome string) {
	s.hub.publish(freqTopic(id), "request_resolved", map[string]string{
		"request_id": requestID, "outcome": outcome,
	})
}

// frequencyRequestUpvoteEvent — the room votes a question up the queue.
func (s *Server) frequencyRequestUpvoteEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, _, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	req, ok := s.frequencyRequestFrom(w, r, f, body)
	if !ok {
		return
	}
	if req.PIAL == u.PIALID {
		writeError(w, http.StatusConflict, store.ErrFreqOwnRequest.Code, store.ErrFreqOwnRequest.Message)
		return
	}
	counted, total, err := s.store.UpvoteSpeakerRequest(r.Context(), req.ID, u.PIALID)
	if err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	if counted {
		fresh, err := s.store.GetFrequencyRequest(r.Context(), f.ID, req.ID)
		if err == nil {
			s.publishFrequencyRequest(r.Context(), f, fresh)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"counted": counted, "upvotes": total})
}

// frequencyRequestResolveEvent — a moderator calls someone up, or doesn't.
func (s *Server) frequencyRequestResolveEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, role, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	approve := body.eventType == "frequency_request_approve"
	action := store.FreqActDecline
	if approve {
		action = store.FreqActApprove
	}
	if !requireFrequencyAction(w, role, action) {
		return
	}
	req, ok := s.frequencyRequestFrom(w, r, f, body)
	if !ok {
		return
	}
	if req.Status != "pending" {
		writeError(w, http.StatusConflict, "not_found", "That request has already been answered.")
		return
	}
	outcome := "declined"
	if approve {
		outcome = "approved"
		participants, err := s.store.FrequencyParticipants(r.Context(), f.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		if _, speakers, _ := store.FrequencyCounts(participants); speakers >= f.MaxSpeakers {
			writeError(w, http.StatusConflict, store.ErrFreqSpeakersFull.Code, store.ErrFreqSpeakersFull.Message)
			return
		}
		// The grant outlives the connection; the joined row carries the role now.
		if err := s.store.GrantFrequencyRole(r.Context(), f.ID, req.PIAL, store.FreqRoleSpeaker, u.PIALID); err != nil {
			serverError(w, err)
			return
		}
		if err := s.store.SetFrequencyParticipantRole(r.Context(), f.ID, req.PIAL, store.FreqRoleSpeaker); err != nil {
			serverError(w, err)
			return
		}
	}
	if err := s.store.ResolveSpeakerRequest(r.Context(), f.ID, req.ID, outcome, u.PIALID); err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	s.publishFrequencyRequestResolved(f.ID, req.ID, outcome)
	if approve {
		s.publishFrequencyParticipant(r.Context(), f.ID, req.PIAL, "speaker_joined")
		s.publishFrequencyCounts(r.Context(), f.ID)
	}
	s.publishFrequencyRoom(f.ID)
	s.answerFrequencyRoom(w, r, u, f.ID)
}

// frequencyMuteEvent — muting yourself needs no rank; muting anyone else does.
func (s *Server) frequencyMuteEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, role, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	muted := frequencyBool(body, "muted", true)
	handle := store.NormalizeHandle(body.get("handle"))
	self := handle == "" || handle == u.Handle
	targetPIAL := u.PIALID
	targetHandle := u.Handle
	if self {
		if !requireFrequencyAction(w, role, store.FreqActMuteSelf) {
			return
		}
	} else {
		if !requireFrequencyAction(w, role, store.FreqActMuteOther) {
			return
		}
		target, _, ok := s.frequencyTarget(w, r, f, role, body)
		if !ok {
			return
		}
		targetPIAL, targetHandle = target.PIALID, target.Handle
	}
	if err := s.store.SetFrequencyMuted(r.Context(), f.ID, targetPIAL, u.PIALID, muted); err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	s.hub.publish(freqTopic(f.ID), "muted", map[string]any{"handle": targetHandle, "muted": muted})
	s.answerFrequencyRoom(w, r, u, f.ID)
}

// frequencyDemoteEvent — a speaker goes back to the audience.
func (s *Server) frequencyDemoteEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, role, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	if !requireFrequencyAction(w, role, store.FreqActDemote) {
		return
	}
	target, targetRole, ok := s.frequencyTarget(w, r, f, role, body)
	if !ok {
		return
	}
	if !store.FreqSpeaks(targetRole) {
		writeError(w, http.StatusConflict, store.ErrFreqNotSpeaker.Code, store.ErrFreqNotSpeaker.Message)
		return
	}
	s.publishFrequencyParticipantLeft(r.Context(), f.ID, target.PIALID)
	if err := s.store.RevokeFrequencyRole(r.Context(), f.ID, target.PIALID, u.PIALID); err != nil {
		serverError(w, err)
		return
	}
	if err := s.store.SetFrequencyParticipantRole(r.Context(), f.ID, target.PIALID, store.FreqRoleListener); err != nil {
		serverError(w, err)
		return
	}
	s.publishFrequencyCounts(r.Context(), f.ID)
	s.publishFrequencyRoom(f.ID)
	s.answerFrequencyRoom(w, r, u, f.ID)
}

// publishFrequencyParticipantLeft pushes `speaker_left` for a row that is
// about to stop speaking, while it can still be read.
func (s *Server) publishFrequencyParticipantLeft(ctx context.Context, id, pial string) {
	p, err := s.store.FrequencyParticipantFor(ctx, id, pial)
	if err != nil || p == nil || !store.FreqSpeaks(p.Role) {
		return
	}
	d := frequencyParticipantDTO(p)
	d.Present = false
	s.hub.publish(freqTopic(id), "speaker_left", d)
}

// frequencyRemoveEvent — out of the room, but not barred from returning.
func (s *Server) frequencyRemoveEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, role, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	if !requireFrequencyAction(w, role, store.FreqActRemove) {
		return
	}
	target, _, ok := s.frequencyTarget(w, r, f, role, body)
	if !ok {
		return
	}
	s.publishFrequencyParticipantLeft(r.Context(), f.ID, target.PIALID)
	if err := s.store.RemoveFromFrequency(r.Context(), f.ID, target.PIALID, u.PIALID, body.get("reason")); err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	s.publishFrequencyCounts(r.Context(), f.ID)
	s.publishFrequencyRoom(f.ID)
	s.answerFrequencyRoom(w, r, u, f.ID)
}

// frequencyBlockEvent — barred for the life of the room, or let back in.
func (s *Server) frequencyBlockEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, role, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	if !requireFrequencyAction(w, role, store.FreqActBlock) {
		return
	}
	target, _, ok := s.frequencyTarget(w, r, f, role, body)
	if !ok {
		return
	}
	if body.eventType == "frequency_unblock" {
		if err := s.store.UnblockFromFrequency(r.Context(), f.ID, target.PIALID, u.PIALID); err != nil {
			serverError(w, err)
			return
		}
		s.publishFrequencyRoom(f.ID)
		s.answerFrequencyRoom(w, r, u, f.ID)
		return
	}
	s.publishFrequencyParticipantLeft(r.Context(), f.ID, target.PIALID)
	if err := s.store.BlockFromFrequency(r.Context(), f.ID, target.PIALID, u.PIALID, body.get("reason")); err != nil {
		serverError(w, err)
		return
	}
	s.publishFrequencyCounts(r.Context(), f.ID)
	s.publishFrequencyRoom(f.ID)
	s.answerFrequencyRoom(w, r, u, f.ID)
}

// frequencyCohostEvent — the host delegates, or takes it back. Removing a
// co-host demotes them to speaker rather than throwing them off the stage.
func (s *Server) frequencyCohostEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, role, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	if !requireFrequencyAction(w, role, store.FreqActCohost) {
		return
	}
	target, targetRole, ok := s.frequencyTarget(w, r, f, role, body)
	if !ok {
		return
	}
	makeCohost := frequencyBool(body, "cohost", true)
	if makeCohost {
		if err := s.store.GrantFrequencyRole(r.Context(), f.ID, target.PIALID, store.FreqRoleCohost, u.PIALID); err != nil {
			serverError(w, err)
			return
		}
		if err := s.store.SetFrequencyParticipantRole(r.Context(), f.ID, target.PIALID, store.FreqRoleCohost); err != nil {
			serverError(w, err)
			return
		}
		s.store.LogFrequencyModeration(r.Context(), f.ID, u.PIALID, target.PIALID, "cohost_added", "")
	} else {
		if targetRole != store.FreqRoleCohost {
			writeError(w, http.StatusConflict, store.ErrFreqNotCohost.Code, store.ErrFreqNotCohost.Message)
			return
		}
		// Uncohost demotes to speaker: they keep the microphone they were
		// already using.
		if err := s.store.GrantFrequencyRole(r.Context(), f.ID, target.PIALID, store.FreqRoleSpeaker, u.PIALID); err != nil {
			serverError(w, err)
			return
		}
		if err := s.store.SetFrequencyParticipantRole(r.Context(), f.ID, target.PIALID, store.FreqRoleSpeaker); err != nil {
			serverError(w, err)
			return
		}
		s.store.LogFrequencyModeration(r.Context(), f.ID, u.PIALID, target.PIALID, "cohost_removed", "")
	}
	s.hub.publish(freqTopic(f.ID), "cohost", map[string]any{"handle": target.Handle, "is_cohost": makeCohost})
	s.publishFrequencyRoom(f.ID)
	s.answerFrequencyRoom(w, r, u, f.ID)
}

// frequencyLockEvent — the host closes the door to new listeners.
func (s *Server) frequencyLockEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, role, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	if !requireFrequencyAction(w, role, store.FreqActLock) {
		return
	}
	locked := frequencyBool(body, "locked", true)
	if err := s.store.SetFrequencyLock(r.Context(), f, locked); err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	s.publishFrequencyFlags(r.Context(), f.ID)
	s.answerFrequencyRoom(w, r, u, f.ID)
}

// frequencyRequestsOpenEvent — the host opens or closes the queue.
func (s *Server) frequencyRequestsOpenEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	f, role, ok := s.frequencyActor(w, r, u, body)
	if !ok {
		return
	}
	if !requireFrequencyAction(w, role, store.FreqActToggleRequests) {
		return
	}
	open := frequencyBool(body, "open", true)
	if err := s.store.SetFrequencyRequestsOpen(r.Context(), f, open); err != nil {
		if !writeFreqError(w, err) {
			serverError(w, err)
		}
		return
	}
	s.publishFrequencyFlags(r.Context(), f.ID)
	s.answerFrequencyRoom(w, r, u, f.ID)
}

func (s *Server) publishFrequencyFlags(ctx context.Context, id string) {
	f, err := s.store.GetFrequency(ctx, id)
	if err != nil {
		return
	}
	s.hub.publish(freqTopic(id), "flags", map[string]bool{"locked": f.Locked, "requests_open": f.RequestsOpen})
	// A closed queue changes what the Raise Hand button may do, and that lives
	// in the viewer block.
	s.publishFrequencyRoom(id)
}
