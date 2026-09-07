package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/auralis"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── /api/v1/frequencies — the JSON twin of the Frequency Facets ──────────────
//
// The native clients draw the Frequency lane, the room and the dock from
// these DTOs, the way they draw live video from api_v1_live.go. Nothing here
// decides anything: every field is Auralis's read model, with PIALs resolved
// to public people and the viewer's standing computed by the same
// frequencyStanding the Facets use. One Auralis read, two projections.
//
// Keys are snake_case and pinned by the iOS models in
// F33D3RKit/Sources/F33D3RKit/Models/Frequency.swift.

type FrequencyDTO struct {
	ID            string        `json:"id"`
	Host          WorkAuthorDTO `json:"host"`
	Title         string        `json:"title"`
	Description   string        `json:"description"`
	State         string        `json:"state"`
	ScheduledAt   *time.Time    `json:"scheduled_at"`
	StartedAt     *time.Time    `json:"started_at"`
	EndedAt       *time.Time    `json:"ended_at"`
	EndReason     *string       `json:"end_reason"`
	ListenerCount int           `json:"listener_count"`
	SpeakerCount  int           `json:"speaker_count"`
	PeopleCount   int           `json:"people_count"`
	IsNSFW        bool          `json:"is_nsfw"`
	Recording     bool          `json:"recording"`
	RequestsOpen  bool          `json:"requests_open"`
	Locked        bool          `json:"locked"`
	Visibility    string        `json:"visibility"`
}

type FrequencyListDTO struct {
	Frequencies []FrequencyDTO `json:"frequencies"`
	Count       int            `json:"count"`
	Own         *FrequencyDTO  `json:"own"`
}

type FrequencySpeakerDTO struct {
	Author   WorkAuthorDTO `json:"author"`
	Role     string        `json:"role"`
	Muted    bool          `json:"muted"`
	Present  bool          `json:"present"`
	Speaking bool          `json:"speaking"`
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
	InFrequency   bool                 `json:"in_frequency"`
	Muted         bool                 `json:"muted"`
	Blocked       bool                 `json:"blocked"`
	CanRequestMic bool                 `json:"can_request_mic"`
	CanModerate   bool                 `json:"can_moderate"`
	CanEnd        bool                 `json:"can_end"`
	CanSpeak      bool                 `json:"can_speak"`
	Request       *FrequencyRequestDTO `json:"request"`
}

// FrequencySessionDTO is the media bootstrap for native WebRTC (Phase 3).
// Present only on the answer to the Tune In or Start that minted it.
type FrequencySessionDTO struct {
	Token       string   `json:"token"`
	SignalPath  string   `json:"signal_path"`
	ExpiresAt   int64    `json:"expires_at"`
	Permissions []string `json:"permissions"`
}

// FrequencyEndedDTO is the data of the `frequency_ended` frame.
type FrequencyEndedDTO struct {
	Reason string `json:"reason"`
}

// FrequencyHeartbeatDTO answers a native `frequency.heartbeat` on POST
// /events: this person's standing in the room after the beat. Present=false
// is the signal to stop beating.
type FrequencyHeartbeatDTO struct {
	State   string              `json:"state"`
	Present bool                `json:"present"`
	Role    string              `json:"role"`
	Muted   bool                `json:"muted"`
	Counts  *FrequencyCountsDTO `json:"counts"`
}

// FrequencyCountsDTO is the room's tally as the beat reports it.
type FrequencyCountsDTO struct {
	Listeners    int `json:"listeners"`
	Speakers     int `json:"speakers"`
	Participants int `json:"participants"`
}

type FrequencyRoomDTO struct {
	Frequency FrequencyDTO          `json:"frequency"`
	Speakers  []FrequencySpeakerDTO `json:"speakers"`
	Cohosts   []WorkAuthorDTO       `json:"cohosts"`
	Requests  []FrequencyRequestDTO `json:"requests"`
	Viewer    FrequencyViewerDTO    `json:"viewer"`
	Session   *FrequencySessionDTO  `json:"session"`
}

// SSE event names on /api/v1/frequencies/{id}/events.
const (
	sseFrequencyJSON  = "frequency_json"
	sseFrequencyEnded = "frequency_ended"
)

// frequencyJSONFrame is what PublishFrequencyState puts on a person's SSE
// channel for the native stream: the room as that person sees it. The
// stream filters by frequency id, because one channel carries every room a
// person may have a stream open on.
type frequencyJSONFrame struct {
	FrequencyID string          `json:"frequency_id"`
	Event       string          `json:"event"`
	Data        json.RawMessage `json:"data"`
}

// ── DTO builders ─────────────────────────────────────────────────────────────

func frequencyAuthorDTO(p FrequencyPerson) WorkAuthorDTO {
	name := p.DisplayName
	if name == "" {
		name = p.Handle
	}
	d := WorkAuthorDTO{Handle: p.Handle, DisplayName: name, AvatarURL: strPtr(p.AvatarURL), IsVerified: p.IsVerified, Role: model.RoleUser, Realm: p.Realm}
	if p.Realm == 0 {
		d.Realm = 1
	}
	if p.VerifiedType != "" {
		d.OfficialType = strPtr(p.VerifiedType)
	}
	return d
}

func frequencyDTO(s *auralis.Summary, counts auralis.Counts, host FrequencyPerson) FrequencyDTO {
	d := FrequencyDTO{
		ID:            s.ID,
		Host:          frequencyAuthorDTO(host),
		Title:         s.Title,
		Description:   s.Description,
		State:         s.State,
		ScheduledAt:   s.ScheduledAt,
		StartedAt:     s.StartedAt,
		EndedAt:       s.EndedAt,
		ListenerCount: counts.Listeners,
		SpeakerCount:  counts.Speakers,
		PeopleCount:   counts.Participants,
		IsNSFW:        s.AdultContent,
		Recording:     s.RecordingEnabled && s.IsLive(),
		RequestsOpen:  s.RequestsOpen,
		Locked:        s.Locked,
		Visibility:    frequencyVisibility(s.Visibility),
	}
	if reason := frequencyEndReason(s); reason != "" {
		d.EndReason = strPtr(reason)
	}
	return d
}

func frequencyRequestDTO(r *auralis.Request, people map[string]FrequencyPerson) FrequencyRequestDTO {
	return FrequencyRequestDTO{
		ID:        r.ID,
		Author:    frequencyAuthorDTO(people[r.PialID]),
		Reason:    r.Reason,
		Upvotes:   r.Upvotes,
		CreatedAt: r.CreatedAt,
	}
}

// frequencyRoomDTO shapes the room for one viewer from an already-fetched
// view. The request queue is the moderators' to see; a listener gets an
// empty list, never a redacted one. The viewer's own pending request rides on
// `viewer.request` whatever their role.
func (h *Handler) frequencyRoomDTO(v *auralis.View, viewerPIAL string, session *auralis.Session, people map[string]FrequencyPerson) FrequencyRoomDTO {
	if people == nil {
		people = h.frequencyPeople(frequencyPIALs(v, viewerPIAL))
	}
	standing := frequencyStanding(v, viewerPIAL)

	speakers := make([]FrequencySpeakerDTO, 0, len(v.Speakers))
	for _, sp := range v.Speakers {
		speakers = append(speakers, FrequencySpeakerDTO{
			Author:  frequencyAuthorDTO(people[sp.PialID]),
			Role:    sp.Role,
			Muted:   sp.Muted,
			Present: sp.Present,
		})
	}
	cohosts := make([]WorkAuthorDTO, 0, len(v.CohostPialIDs))
	for _, c := range v.CohostPialIDs {
		cohosts = append(cohosts, frequencyAuthorDTO(people[c]))
	}
	requests := make([]FrequencyRequestDTO, 0)
	if standing.CanModerate {
		for i := range v.Requests {
			requests = append(requests, frequencyRequestDTO(&v.Requests[i], people))
		}
	}

	viewer := FrequencyViewerDTO{
		InFrequency:   standing.InFrequency,
		Muted:         standing.Muted,
		Blocked:       standing.Blocked,
		CanRequestMic: standing.CanRequestMic,
		CanModerate:   standing.CanModerate,
		CanEnd:        standing.CanEnd,
		CanSpeak:      standing.CanSpeak,
	}
	if standing.Role != "" {
		viewer.Role = strPtr(standing.Role)
	}
	if standing.Request != nil {
		for i := range v.Requests {
			if v.Requests[i].ID == standing.Request.RequestID {
				req := frequencyRequestDTO(&v.Requests[i], people)
				viewer.Request = &req
			}
		}
		if viewer.Request == nil {
			// Auralis's viewer block carried the request but the queue is
			// not in this read (a non-moderator's view): shape it from the
			// standing, whose author is the viewer.
			viewer.Request = &FrequencyRequestDTO{
				ID:      standing.Request.RequestID,
				Author:  frequencyAuthorDTO(people[viewerPIAL]),
				Reason:  standing.Request.Reason,
				Upvotes: standing.Request.Upvotes,
			}
			if v.Viewer != nil && v.Viewer.Request != nil {
				viewer.Request.CreatedAt = v.Viewer.Request.CreatedAt
			}
		}
	}

	room := FrequencyRoomDTO{
		Frequency: frequencyDTO(&v.Frequency, v.Counts, people[v.Frequency.HostPialID]),
		Speakers:  speakers,
		Cohosts:   cohosts,
		Requests:  requests,
		Viewer:    viewer,
	}
	if session != nil && session.Token != "" {
		room.Session = &FrequencySessionDTO{
			Token:       session.Token,
			SignalPath:  session.SignalPath,
			ExpiresAt:   session.ExpiresAt,
			Permissions: session.Permissions,
		}
	}
	return room
}

// ── reads ────────────────────────────────────────────────────────────────────

// apiV1Frequencies — GET /api/v1/frequencies?lane=live|scheduled|ended.
func (h *Handler) apiV1Frequencies(w http.ResponseWriter, r *http.Request, u *model.User) {
	if h.auralis == nil || !h.auralis.Configured() {
		apiError(w, http.StatusServiceUnavailable, "frequencies_unavailable", "Frequencies are not available on this deployment.")
		return
	}
	lane := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("lane")))
	switch lane {
	case "":
		lane = "live"
	case "live", "scheduled", "ended":
	default:
		apiError(w, http.StatusBadRequest, "bad_lane", "lane must be live, scheduled or ended.")
		return
	}
	list, err := h.auralis.List(r.Context(), lane, "", apiV1MaxPage)
	if err != nil {
		h.apiFrequencyRefuse(w, err)
		return
	}
	pials := make([]string, 0, len(list.Items)+1)
	for i := range list.Items {
		pials = append(pials, list.Items[i].Frequency.HostPialID)
	}
	var own *auralis.Summary
	if u.PIALID != "" {
		mine, err := h.auralis.HostOpen(r.Context(), u.PIALID)
		if err != nil {
			log.Printf("[api/v1] own frequency for %s: %v", u.Handle, err)
		} else if mine != nil {
			own = mine
			pials = append(pials, mine.HostPialID)
		}
	}
	people := h.frequencyPeople(pials)
	hideAdult := excludeNSFW(u)
	out := make([]FrequencyDTO, 0, len(list.Items))
	for i := range list.Items {
		s := &list.Items[i].Frequency
		if s.AdultContent && hideAdult {
			continue
		}
		out = append(out, frequencyDTO(s, list.Items[i].Counts, people[s.HostPialID]))
	}
	dto := FrequencyListDTO{Frequencies: out, Count: len(out)}
	if own != nil {
		// Counts for the host's own Frequency are in the live lane when it is
		// live; a scheduled or draft one has none yet.
		var counts auralis.Counts
		for i := range list.Items {
			if list.Items[i].Frequency.ID == own.ID {
				counts = list.Items[i].Counts
			}
		}
		d := frequencyDTO(own, counts, people[own.HostPialID])
		dto.Own = &d
	}
	apiJSON(w, http.StatusOK, dto)
}

// apiLoadFrequency reads a room for a viewer, answering the JSON envelope on
// refusal. The adult wall is Nantar's: Auralis does not know the viewer's
// content setting.
func (h *Handler) apiLoadFrequency(w http.ResponseWriter, r *http.Request, id string, u *model.User) (*auralis.Answer, bool) {
	if h.auralis == nil || !h.auralis.Configured() {
		apiError(w, http.StatusServiceUnavailable, "frequencies_unavailable", "Frequencies are not available on this deployment.")
		return nil, false
	}
	if id == "" {
		apiError(w, http.StatusNotFound, "not_found", "That Frequency doesn't exist.")
		return nil, false
	}
	ans, err := h.auralis.Get(r.Context(), id, u.PIALID)
	if err != nil {
		h.apiFrequencyRefuse(w, err)
		return nil, false
	}
	if ans.Frequency.Frequency.AdultContent && excludeNSFW(u) {
		apiError(w, http.StatusForbidden, "adult_content", "This Frequency is for adult-enabled accounts.")
		return nil, false
	}
	return ans, true
}

// apiV1FrequencyRoom — GET /api/v1/frequencies/{id}: the room as this viewer
// sees it. No session: a read has no side effects and mints nothing.
func (h *Handler) apiV1FrequencyRoom(w http.ResponseWriter, r *http.Request, u *model.User) {
	ans, ok := h.apiLoadFrequency(w, r, r.PathValue("id"), u)
	if !ok {
		return
	}
	apiJSON(w, http.StatusOK, h.frequencyRoomDTO(&ans.Frequency, u.PIALID, nil, nil))
}

// apiV1FrequencyEvents — GET /api/v1/frequencies/{id}/events: the room as
// JSON on connect and on every change, `frequency_ended` when it is over.
// While the viewer is joined, this connection is their presence: it beats
// Auralis every 15 s, the cadence the Facets use, so a phone with the room
// open needs no timer of its own.
func (h *Handler) apiV1FrequencyEvents(w http.ResponseWriter, r *http.Request, u *model.User) {
	ans, ok := h.apiLoadFrequency(w, r, r.PathValue("id"), u)
	if !ok {
		return
	}
	id := ans.Frequency.Frequency.ID
	flusher, ok := w.(http.Flusher)
	if !ok {
		apiError(w, http.StatusInternalServerError, "no_stream", "SSE not supported.")
		return
	}
	ch, done, cleanup := RegisterSSESession(u.ID, u.PIALID)
	defer cleanup()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")

	room := h.frequencyRoomDTO(&ans.Frequency, u.PIALID, nil, nil)
	if raw, err := json.Marshal(room); err == nil {
		writeSSEFrame(w, sseFrequencyJSON, string(raw))
	}
	if ans.Frequency.Frequency.IsOver() {
		writeSSEFrame(w, sseFrequencyEnded, frequencyEndedJSON(&ans.Frequency.Frequency))
	}
	flusher.Flush()

	joined := room.Viewer.InFrequency
	beat := func() {
		if !joined || u.PIALID == "" {
			return
		}
		hb, err := h.auralis.Heartbeat(r.Context(), u.PIALID, id)
		if err != nil {
			log.Printf("[api/v1] frequency beat %s for %s: %v", id, u.Handle, err)
			return
		}
		if !hb.Present {
			joined = false
		}
	}

	ping := time.NewTicker(15 * time.Second)
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
			if ev.Type != sseFrequencyJSON || ev.JSON == "" {
				continue
			}
			var frame frequencyJSONFrame
			if err := json.Unmarshal([]byte(ev.JSON), &frame); err != nil || frame.FrequencyID != id {
				continue
			}
			writeSSEFrame(w, frame.Event, string(frame.Data))
			flusher.Flush()
			if frame.Event == sseFrequencyEnded {
				joined = false
			} else if frame.Event == sseFrequencyJSON {
				var v struct {
					Viewer struct {
						InFrequency bool `json:"in_frequency"`
					} `json:"viewer"`
				}
				if json.Unmarshal(frame.Data, &v) == nil {
					joined = v.Viewer.InFrequency
				}
			}
		}
	}
}

func frequencyEndedJSON(s *auralis.Summary) string {
	raw, _ := json.Marshal(FrequencyEndedDTO{Reason: frequencyEndReason(s)})
	return string(raw)
}

// publishFrequencyJSON puts the native projection of a room on one person's
// channel: the room as they see it, and `frequency_ended` when it is over.
// Called by publishFrequencyView beside the fragments, from the same
// already-fetched view — no extra brain calls.
func (h *Handler) publishFrequencyJSON(v *auralis.View, pial string, people map[string]FrequencyPerson) {
	room := h.frequencyRoomDTO(v, pial, nil, people)
	if raw, err := json.Marshal(room); err == nil {
		h.pushFrequencyFrame(v.Frequency.ID, pial, sseFrequencyJSON, raw)
	} else {
		log.Printf("[frequency] json room for %s: %v", v.Frequency.ID, err)
	}
	if v.Frequency.IsOver() {
		h.pushFrequencyFrame(v.Frequency.ID, pial, sseFrequencyEnded, json.RawMessage(frequencyEndedJSON(&v.Frequency)))
	}
}

func (h *Handler) pushFrequencyFrame(frequencyID, pial, event string, data json.RawMessage) {
	frame, err := json.Marshal(frequencyJSONFrame{FrequencyID: frequencyID, Event: event, Data: data})
	if err != nil {
		return
	}
	PublishToUser(pial, SSEEvent{Type: sseFrequencyJSON, JSON: string(frame)})
}

// ── native answers on the event lane ─────────────────────────────────────────

// apiFrequencyRefuse renders an Auralis refusal in the JSON envelope, with
// Auralis's machine code so the client can branch on it.
func (h *Handler) apiFrequencyRefuse(w http.ResponseWriter, err error) {
	msg, status := frequencyErrorMessage(err)
	code := auralis.Code(err)
	if code == "" {
		code = "frequency_error"
	}
	if status >= 500 {
		log.Printf("[api/v1] frequency: %v", err)
	}
	apiError(w, status, code, msg)
}

// writeFrequencyRoomJSON answers a native mutation with the room the actor
// now sees, carrying the session Auralis minted when there is one.
func (h *Handler) writeFrequencyRoomJSON(w http.ResponseWriter, user *model.User, ans *auralis.Answer) {
	apiJSON(w, http.StatusOK, h.frequencyRoomDTO(&ans.Frequency, user.PIALID, ans.Session, nil))
}
