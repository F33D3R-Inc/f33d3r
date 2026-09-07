package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/auralis"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// Frequencies — the render and fan-out layer.
//
// Auralis owns the Frequency. This file turns Auralis's read model into the
// data the Facets consume and pushes re-rendered Fragments over FA Live. It
// holds no Frequency state: every mutation asks Auralis again, and every
// Fragment is rendered from that answer.
//
// Identities arrive as bare PIAL uuids and leave as people. No PIAL is ever
// rendered — see frequencyPeople.

// PageCtx is the per-viewer context every card and stage Facet receives,
// built by workCardCtx: CurrentUserHandle, User, SessionID, Surface,
// ShowScores.
type PageCtx = map[string]interface{}

// FrequencyPerson is a public representation of a participant.
type FrequencyPerson struct {
	Handle       string
	DisplayName  string
	AvatarURL    string
	Realm        int
	IsVerified   bool
	VerifiedType string
}

// FrequencyCardData is the discovery card: the live strip, the lanes page,
// the go-live page's "live now" row.
type FrequencyCardData struct {
	ID             string
	Title          string
	State          string
	Host           FrequencyPerson
	SpeakerCount   int
	ListenerCount  int
	ListenersLabel string
	TimeLabel      string
	ScheduledLabel string
	IsNSFW         bool
	Ctx            PageCtx
}

type FrequencySpeakerData struct {
	ID      string
	Person  FrequencyPerson
	Role    string
	Muted   bool
	Present bool
	IsHost  bool
	// The viewer's standing, carried on each tile because Ctx is a map and
	// the tile decides whether to draw a moderation menu.
	ViewerCanModerate bool
	ViewerIsHost      bool
	Ctx               PageCtx
}

type FrequencyRequestData struct {
	ID           string
	RequestID    string
	Person       FrequencyPerson
	Reason       string
	Upvotes      int
	CreatedLabel string
	CanModerate  bool
	IsMine       bool
	Ctx          PageCtx
}

// FrequencyViewer is the viewer's own standing, computed by Auralis from the
// same role matrix it enforces. Nantar renders controls from it and never
// decides a rule itself.
type FrequencyViewer struct {
	Role          string
	InFrequency   bool
	Muted         bool
	Blocked       bool
	CanRequestMic bool
	CanModerate   bool
	CanEnd        bool
	CanSpeak      bool
	Request       *FrequencyRequestData
}

// FrequencySession is the media bootstrap handed to the browser after Tune
// In or Start. Short-lived; minted by Auralis; never stored here.
type FrequencySession struct {
	Token       string
	SignalPath  string
	ExpiresAt   int64
	Permissions []string
}

// FrequencyStageData is everything the stage renders.
type FrequencyStageData struct {
	ID             string
	Title          string
	Description    string
	State          string
	Host           FrequencyPerson
	Speakers       []FrequencySpeakerData
	Cohosts        []FrequencyPerson
	ListenerCount  int
	SpeakerCount   int
	ListenersLabel string
	Requests       []FrequencyRequestData
	RequestsOpen   bool
	Locked         bool
	Recording      bool
	IsNSFW         bool
	Viewer         FrequencyViewer
	Session        *FrequencySession
	TimeLabel      string
	EndedLabel     string
	// EndReason vocabulary: host_ended | host_lost | moderation | "".
	EndReason string
	// Heartbeat: the surface keeps beating — viewer is a participant of a live
	// Frequency.
	Heartbeat bool
	// RequestCount is the pending queue depth; PeopleCount everyone present,
	// host included. Both are what the dock and the preview show.
	RequestCount int
	PeopleCount  int
	// Captions are rendered caption lines for the dock (Phase 5); empty now.
	Captions []string
	Ctx      PageCtx
}

// frequencyFacetID is the ONE builder for a Frequency facet id. It is
// registered as a template func so markup addresses a facet through it and a
// typo cannot silently mint an unreachable id.
func frequencyFacetID(id, slot string) string {
	return "facet:f33d3r:frequency:" + id + ":" + slot
}

// Slots this brain renders and pushes. Every one is a partial of the same
// name prefixed frequency_ (frequency_header, ...), except card/stage which
// are frequency_card and frequency_stage.
const (
	freqSlotCard          = "card"
	freqSlotStage         = "stage"
	freqSlotHeader        = "header"
	freqSlotSpeakers      = "speakers"
	freqSlotListenerCount = "listener_count"
	freqSlotLiveBadge     = "live_badge"
	freqSlotControls      = "controls"
	freqSlotRequestButton = "request_button"
	freqSlotRequests      = "requests"
	freqSlotStatus        = "status"
	freqSlotRecording     = "recording"
	freqSlotPreview       = "preview"
	freqSlotDock          = "dock"
	freqSlotDockMini      = "dock_mini"
	freqSlotCaptions      = "captions"
)

// frequencyDockEmpty is the Shell's dock mount with nothing in it: what a
// person who is in no Frequency sees, and what replaces the dock when they
// leave or the Frequency ends.
const frequencyDockEmpty = `<div id="frequency-dock" class="freq-dock-slot" data-facet-id="facet:f33d3r:frequency:dock"></div>`

// frequencySlotPartial maps a slot to the partial that renders it.
func frequencySlotPartial(slot string) (string, bool) {
	switch slot {
	case freqSlotCard:
		return "frequency_card", true
	case freqSlotStage:
		return "frequency_stage", true
	case freqSlotHeader:
		return "frequency_header", true
	case freqSlotSpeakers:
		return "frequency_speaker_grid", true
	case freqSlotListenerCount:
		return "frequency_listener_count", true
	case freqSlotLiveBadge:
		return "frequency_live_badge", true
	case freqSlotControls:
		return "frequency_controls", true
	case freqSlotRequestButton:
		return "frequency_request_button", true
	case freqSlotRequests:
		return "frequency_request_queue", true
	case freqSlotStatus:
		return "frequency_status", true
	case freqSlotRecording:
		return "frequency_recording_badge", true
	case freqSlotPreview:
		return "frequency_preview", true
	case freqSlotDock:
		return "frequency_dock", true
	case freqSlotDockMini:
		return "frequency_dock_mini", true
	case freqSlotCaptions:
		return "frequency_captions", true
	}
	return "", false
}

// frequencySlotData shapes the data one slot's partial expects. The stage
// composes its children with dict literals; a slot pushed on its own over FA
// Live must receive the same shape, so this is the one place those shapes are
// written down. Keep it in step with _frequency_stage.html.
func frequencySlotData(slot string, st FrequencyStageData) interface{} {
	v := st.Viewer
	live := st.State == "live"
	switch slot {
	case freqSlotHeader:
		return map[string]interface{}{
			"ID": st.ID, "Title": st.Title, "Description": st.Description, "State": st.State, "Host": st.Host,
			"ListenerCount": st.ListenerCount, "ListenersLabel": st.ListenersLabel,
			"Recording": st.Recording, "IsNSFW": st.IsNSFW,
			"Heartbeat": st.Heartbeat && live, "Ctx": st.Ctx,
		}
	case freqSlotListenerCount:
		return map[string]interface{}{"ID": st.ID, "Count": st.ListenerCount, "Label": st.ListenersLabel, "Heartbeat": st.Heartbeat && live}
	case freqSlotLiveBadge:
		return map[string]interface{}{"ID": st.ID, "State": st.State, "ScheduledLabel": ""}
	case freqSlotRecording:
		return map[string]interface{}{"ID": st.ID, "Recording": st.Recording}
	case freqSlotStatus:
		return map[string]interface{}{
			"ID": st.ID, "State": st.State, "TimeLabel": st.TimeLabel, "EndedLabel": st.EndedLabel, "EndReason": st.EndReason,
			"Locked": st.Locked, "RequestsOpen": st.RequestsOpen,
		}
	case freqSlotSpeakers:
		return map[string]interface{}{"ID": st.ID, "Speakers": st.Speakers, "Ctx": st.Ctx}
	case freqSlotRequests:
		return map[string]interface{}{"ID": st.ID, "Requests": st.Requests, "CanModerate": v.CanModerate, "Ctx": st.Ctx}
	case freqSlotControls:
		return map[string]interface{}{"ID": st.ID, "State": st.State, "Viewer": v, "RequestsOpen": st.RequestsOpen, "Locked": st.Locked, "Ctx": st.Ctx, "InDock": false}
	case freqSlotRequestButton:
		return map[string]interface{}{"ID": st.ID, "CanRequestMic": v.CanRequestMic, "Request": v.Request, "RequestsOpen": st.RequestsOpen, "Live": live}
	}
	// stage, card (handled by caller), preview, dock, dock_mini, captions take
	// the whole stage.
	return st
}

// sseFrequencyFacet is the FA Live event type every Frequency Fragment rides.
// Distinct from live_facet: the two lanes must never replace each other's
// nodes.
const sseFrequencyFacet = "frequency_facet"

// ── people ───────────────────────────────────────────────────────────────────

// frequencyPeople resolves bare PIAL uuids to public people through this
// brain's own user rows (users.pial_id). A PIAL that names nobody here yields
// a placeholder person with an empty handle — rendered as "someone", never as
// the uuid.
func (h *Handler) frequencyPeople(pials []string) map[string]FrequencyPerson {
	out := make(map[string]FrequencyPerson, len(pials))
	if h.db == nil {
		return out
	}
	for _, p := range pials {
		if p == "" {
			continue
		}
		if _, seen := out[p]; seen {
			continue
		}
		uid, err := dbpkg.GetUserIDFromPIAL(h.db, p)
		if err != nil {
			continue
		}
		u, err := dbpkg.GetUserByID(h.db, uid)
		if err != nil || u == nil {
			continue
		}
		out[p] = personOf(u)
	}
	return out
}

func personOf(u *model.User) FrequencyPerson {
	name := u.DisplayName
	if name == "" {
		name = u.Handle
	}
	return FrequencyPerson{
		Handle:       u.Handle,
		DisplayName:  name,
		AvatarURL:    u.AvatarURL,
		Realm:        u.Realm,
		IsVerified:   u.IsVerified,
		VerifiedType: liveVerifiedType(u),
	}
}

// ── labels ───────────────────────────────────────────────────────────────────

func frequencyListenersLabel(n int) string {
	switch n {
	case 0:
		return "nobody listening yet"
	case 1:
		return "1 listening"
	default:
		return fmtViewerCount(n) + " listening"
	}
}

func frequencyTimeLabel(s *auralis.Summary) string {
	switch {
	case s == nil:
		return ""
	case s.IsOver():
		return "ended"
	case s.State == "scheduled":
		return "scheduled"
	case s.StartedAt == nil:
		return "starting"
	}
	d := time.Since(*s.StartedAt)
	switch {
	case d < time.Minute:
		return "live now"
	case d < time.Hour:
		return fmt.Sprintf("live %dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("live %dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func frequencyScheduledLabel(s *auralis.Summary) string {
	if s == nil || s.ScheduledAt == nil {
		return ""
	}
	return s.ScheduledAt.UTC().Format("Mon 2 Jan · 15:04 UTC")
}

// frequencyEndReason maps Auralis's end_reason onto the templates' vocabulary:
// host_ended | host_lost | moderation | "".
func frequencyEndReason(s *auralis.Summary) string {
	if s == nil {
		return ""
	}
	if s.State == "moderation_terminated" {
		return "moderation"
	}
	if s.EndReason == nil {
		return ""
	}
	switch *s.EndReason {
	case "host_ended", "host_lost":
		return *s.EndReason
	case "moderation":
		return "moderation"
	case "cancelled", "":
		return ""
	}
	return *s.EndReason
}

func frequencyEndedLabel(s *auralis.Summary) string {
	if s == nil || s.EndedAt == nil {
		return ""
	}
	return "ended " + TimeAgo(*s.EndedAt) + " ago"
}

// ── builders ─────────────────────────────────────────────────────────────────

// buildFrequencyCard shapes one discovery card.
func (h *Handler) buildFrequencyCard(s *auralis.Summary, counts auralis.Counts, host FrequencyPerson, viewer *model.User) FrequencyCardData {
	return FrequencyCardData{
		ID:             s.ID,
		Title:          s.Title,
		State:          s.State,
		Host:           host,
		SpeakerCount:   counts.Speakers,
		ListenerCount:  counts.Listeners,
		ListenersLabel: frequencyListenersLabel(counts.Listeners),
		TimeLabel:      frequencyTimeLabel(s),
		ScheduledLabel: frequencyScheduledLabel(s),
		IsNSFW:         s.AdultContent,
		Ctx:            h.workCardCtx(viewer, "frequencies"),
	}
}

// buildFrequencyCards resolves every host once and shapes a list.
func (h *Handler) buildFrequencyCards(items []auralis.ListItem, viewer *model.User) []FrequencyCardData {
	pials := make([]string, 0, len(items))
	for i := range items {
		pials = append(pials, items[i].Frequency.HostPialID)
	}
	people := h.frequencyPeople(pials)
	hideAdult := excludeNSFW(viewer)
	out := make([]FrequencyCardData, 0, len(items))
	for i := range items {
		s := &items[i].Frequency
		if s.AdultContent && hideAdult {
			continue
		}
		out = append(out, h.buildFrequencyCard(s, items[i].Counts, people[s.HostPialID], viewer))
	}
	return out
}

// buildFrequencyStage shapes the stage from Auralis's read model.
//
// viewerPIAL is the person the stage is rendered for ("" for nobody). When
// the read model carries a Viewer for that person it is used as-is; when it
// does not (the fan-out path, which reads the model once without a viewer),
// the standing is derived locally from the host, co-host and speaker lists
// and the request queue. That derivation mirrors Auralis's rules exactly and
// exists only to avoid one Auralis read per present participant; Auralis
// still enforces every action.
func (h *Handler) buildFrequencyStage(v *auralis.View, viewerPIAL string, viewer *model.User, session *auralis.Session, people map[string]FrequencyPerson) FrequencyStageData {
	s := &v.Frequency
	if people == nil {
		people = h.frequencyPeople(frequencyPIALs(v, viewerPIAL))
	}
	ctx := h.workCardCtx(viewer, "frequency")

	standing := frequencyStanding(v, viewerPIAL)
	canModerate := standing.CanModerate

	speakers := make([]FrequencySpeakerData, 0, len(v.Speakers))
	for _, sp := range v.Speakers {
		speakers = append(speakers, FrequencySpeakerData{
			ID:                s.ID,
			Person:            people[sp.PialID],
			Role:              sp.Role,
			Muted:             sp.Muted,
			Present:           sp.Present,
			IsHost:            sp.Role == "host",
			ViewerCanModerate: canModerate,
			ViewerIsHost:      standing.Role == "host",
			Ctx:               ctx,
		})
	}

	cohosts := make([]FrequencyPerson, 0, len(v.CohostPialIDs))
	for _, c := range v.CohostPialIDs {
		cohosts = append(cohosts, people[c])
	}

	requests := make([]FrequencyRequestData, 0, len(v.Requests))
	for i := range v.Requests {
		requests = append(requests, h.requestData(s.ID, &v.Requests[i], people, viewerPIAL, canModerate, ctx))
	}

	if standing.Request != nil {
		standing.Request.Ctx = ctx
	}

	data := FrequencyStageData{
		ID:             s.ID,
		Title:          s.Title,
		Description:    s.Description,
		State:          s.State,
		Host:           people[s.HostPialID],
		Speakers:       speakers,
		Cohosts:        cohosts,
		ListenerCount:  v.Counts.Listeners,
		SpeakerCount:   v.Counts.Speakers,
		ListenersLabel: frequencyListenersLabel(v.Counts.Listeners),
		Requests:       requests,
		RequestsOpen:   s.RequestsOpen,
		Locked:         s.Locked,
		Recording:      s.RecordingEnabled && s.IsLive(),
		IsNSFW:         s.AdultContent,
		Viewer:         standing,
		TimeLabel:      frequencyTimeLabel(s),
		EndedLabel:     frequencyEndedLabel(s),
		Ctx:            ctx,
	}
	data.EndReason = frequencyEndReason(s)
	data.Heartbeat = standing.InFrequency && s.IsLive()
	data.RequestCount = len(v.Requests)
	data.PeopleCount = v.Counts.Participants
	data.Captions = []string{}
	if session != nil && session.Token != "" {
		data.Session = &FrequencySession{
			Token:       session.Token,
			SignalPath:  session.SignalPath,
			ExpiresAt:   session.ExpiresAt,
			Permissions: session.Permissions,
		}
	}
	return data
}

func (h *Handler) requestData(freqID string, r *auralis.Request, people map[string]FrequencyPerson, viewerPIAL string, canModerate bool, ctx PageCtx) FrequencyRequestData {
	return FrequencyRequestData{
		ID:           freqID,
		RequestID:    r.ID,
		Person:       people[r.PialID],
		Reason:       r.Reason,
		Upvotes:      r.Upvotes,
		CreatedLabel: TimeAgo(r.CreatedAt),
		CanModerate:  canModerate,
		IsMine:       viewerPIAL != "" && r.PialID == viewerPIAL,
		Ctx:          ctx,
	}
}

// frequencyPIALs lists every identity a stage names, so they resolve in one
// pass.
func frequencyPIALs(v *auralis.View, extra string) []string {
	out := []string{v.Frequency.HostPialID}
	for _, sp := range v.Speakers {
		out = append(out, sp.PialID)
	}
	out = append(out, v.CohostPialIDs...)
	for _, r := range v.Requests {
		out = append(out, r.PialID)
	}
	if extra != "" {
		out = append(out, extra)
	}
	return out
}

// frequencyStanding is the viewer's standing. Auralis's own answer wins when
// present; otherwise it is derived from the lists, by the same rules Auralis
// applies (domain/role.rs, domain/permissions.rs).
func frequencyStanding(v *auralis.View, viewerPIAL string) FrequencyViewer {
	if viewerPIAL == "" {
		return FrequencyViewer{}
	}
	s := &v.Frequency
	if v.Viewer != nil && v.Viewer.PialID == viewerPIAL {
		fv := FrequencyViewer{
			InFrequency:   v.Viewer.Role != nil,
			Muted:         v.Viewer.Muted,
			Blocked:       v.Viewer.Blocked,
			CanRequestMic: v.Viewer.CanRequestMic,
			CanModerate:   v.Viewer.CanModerate,
			CanEnd:        v.Viewer.CanEnd,
			CanSpeak:      v.Viewer.CanSpeak,
		}
		if v.Viewer.Role != nil {
			fv.Role = *v.Viewer.Role
		}
		if v.Viewer.Request != nil {
			fv.Request = &FrequencyRequestData{ID: s.ID, RequestID: v.Viewer.Request.ID, Reason: v.Viewer.Request.Reason, Upvotes: v.Viewer.Request.Upvotes, CreatedLabel: TimeAgo(v.Viewer.Request.CreatedAt), IsMine: true}
		}
		return fv
	}

	// Derived standing (fan-out path).
	role := ""
	muted := false
	in := false
	if s.HostPialID == viewerPIAL {
		role, in = "host", true
	}
	for _, sp := range v.Speakers {
		if sp.PialID == viewerPIAL {
			role, muted, in = sp.Role, sp.Muted, true
		}
	}
	if role == "" {
		for _, c := range v.CohostPialIDs {
			if c == viewerPIAL {
				role, in = "cohost", true
			}
		}
	}
	if role == "" {
		for _, p := range v.PresentPialIDs {
			if p == viewerPIAL {
				role, in = "listener", true
			}
		}
	}
	var pending *FrequencyRequestData
	for i := range v.Requests {
		if v.Requests[i].PialID == viewerPIAL {
			r := &v.Requests[i]
			pending = &FrequencyRequestData{ID: s.ID, RequestID: r.ID, Reason: r.Reason, Upvotes: r.Upvotes, CreatedLabel: TimeAgo(r.CreatedAt), IsMine: true}
		}
	}
	speaks := role == "host" || role == "cohost" || role == "speaker"
	moderates := role == "host" || role == "cohost"
	return FrequencyViewer{
		Role:          role,
		InFrequency:   in,
		Muted:         muted,
		CanRequestMic: role == "listener" && s.RequestsOpen && s.IsLive() && pending == nil,
		CanModerate:   moderates,
		CanEnd:        role == "host",
		CanSpeak:      speaks,
		Request:       pending,
	}
}

// ── rendering ────────────────────────────────────────────────────────────────

// renderFrequencyFragment renders one Frequency Facet. Failures are returned,
// never swallowed — a half-built Fragment must not reach the stream.
func (h *Handler) renderFrequencyFragment(name string, data interface{}) (string, error) {
	if h.partial == nil {
		return "", errors.New("frequency: partial template set unavailable")
	}
	if h.partial.Lookup(name) == nil {
		return "", fmt.Errorf("frequency: partial %q is not defined", name)
	}
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("frequency: render %s: %w", name, err)
	}
	if buf.Len() == 0 {
		return "", fmt.Errorf("frequency: %s produced an empty fragment", name)
	}
	return buf.String(), nil
}

// frequencySharedSlots are rendered once per mutation and pushed to everyone
// present; they carry no viewer-specific state.
var frequencySharedSlots = []string{freqSlotSpeakers, freqSlotLiveBadge, freqSlotStatus, freqSlotRecording}

// frequencyViewerSlots depend on who is looking. The dock is one of them: it
// lives in the Shell and follows the person around the site. The tally, and
// the header that embeds it, are here because a participant's tally carries
// their presence beat: pushed as a shared render they would land without one
// and silence the beat of everyone who received them.
var frequencyViewerSlots = []string{freqSlotHeader, freqSlotListenerCount, freqSlotControls, freqSlotRequestButton, freqSlotRequests, freqSlotDock}

// renderFrequencySlot renders one slot for a stage.
func (h *Handler) renderFrequencySlot(slot string, st FrequencyStageData) (string, error) {
	name, ok := frequencySlotPartial(slot)
	if !ok {
		return "", fmt.Errorf("frequency: unknown slot %q", slot)
	}
	return h.renderFrequencyFragment(name, frequencySlotData(slot, st))
}

// PublishFrequencyState re-renders a Frequency's Facets from Auralis's
// authoritative state and pushes them over FA Live to every present
// participant.
//
// Cost model: one Auralis read (no viewer) plus one people lookup. Shared
// slots render once. Viewer slots render once per present PIAL from a
// locally derived standing (see buildFrequencyStage) — N template executions,
// not N brain calls. The card is re-rendered and broadcast to every session
// so the live strip on the home Playground and the go-live page follow.
//
// When the Frequency is over, everyone it still knows about — the host, and
// anyone present at the last read — receives the empty dock mount so the
// panel leaves their Shell. A participant removed or blocked receives it
// through frequencyDepartedDock. A participant whose presence expired learns
// it on their next heartbeat, which answers the empty mount when the
// Frequency has ended.
//
// This is the seam the Sitra Achra consumer and every event handler call.
func (h *Handler) PublishFrequencyState(ctx context.Context, frequencyID string) {
	if h.auralis == nil || !h.auralis.Configured() {
		return
	}
	ans, err := h.auralis.Get(ctx, frequencyID, "")
	if err != nil {
		log.Printf("[frequency] publish %s: read: %v", frequencyID, err)
		return
	}
	h.publishFrequencyView(&ans.Frequency)
}

func (h *Handler) publishFrequencyView(v *auralis.View) {
	people := h.frequencyPeople(frequencyPIALs(v, ""))
	for _, p := range v.PresentPialIDs {
		if _, ok := people[p]; !ok {
			for k, person := range h.frequencyPeople([]string{p}) {
				people[k] = person
			}
		}
	}

	// Shared slots: rendered once for nobody in particular.
	shared := h.buildFrequencyStage(v, "", nil, nil, people)
	sharedFrags := make([]string, 0, len(frequencySharedSlots))
	for _, slot := range frequencySharedSlots {
		frag, err := h.renderFrequencySlot(slot, shared)
		if err != nil {
			log.Printf("[frequency] %v", err)
			continue
		}
		sharedFrags = append(sharedFrags, frag)
	}

	// Everyone present, plus the host, who is present by definition while
	// their stage is open but may be between heartbeats.
	recipients := map[string]bool{v.Frequency.HostPialID: true}
	for _, p := range v.PresentPialIDs {
		recipients[p] = true
	}
	over := v.Frequency.IsOver()
	for pial := range recipients {
		for _, frag := range sharedFrags {
			PublishToUser(pial, SSEEvent{Type: sseFrequencyFacet, Data: frag})
		}
		mine := h.buildFrequencyStage(v, pial, nil, nil, people)
		// The native projection of the same state, for a phone with the room
		// or the dock open.
		h.publishFrequencyJSON(v, pial, people)
		for _, slot := range frequencyViewerSlots {
			if slot == freqSlotDock && (over || !mine.Viewer.InFrequency) {
				PublishToUser(pial, SSEEvent{Type: sseFrequencyFacet, Data: frequencyDockEmpty})
				continue
			}
			frag, err := h.renderFrequencySlot(slot, mine)
			if err != nil {
				log.Printf("[frequency] %v", err)
				continue
			}
			PublishToUser(pial, SSEEvent{Type: sseFrequencyFacet, Data: frag})
		}
	}

	// The card follows the object everywhere it is shown.
	card := h.buildFrequencyCard(&v.Frequency, v.Counts, people[v.Frequency.HostPialID], nil)
	if frag, err := h.renderFrequencyFragment("frequency_card", card); err == nil {
		PublishToAllSessions(SSEEvent{Type: sseFrequencyFacet, Data: frag})
	} else {
		log.Printf("[frequency] %v", err)
	}
}

// frequencyDepartedDock clears the dock of someone a moderator removed or
// blocked: they are no longer present, so the fan-out above will not reach
// them.
func frequencyDepartedDock(pial string) {
	if pial != "" {
		PublishToUser(pial, SSEEvent{Type: sseFrequencyFacet, Data: frequencyDockEmpty})
	}
}

// frequencyDock is the Shell's dock for a signed-in person: their live
// Frequency's dock, or the empty mount. Asked once per full page render.
// Fail-soft: an unreachable Auralis is a WARN line and an empty mount, never
// a broken page — and nothing is cached, so the next page asks again.
func (h *Handler) frequencyDock(ctx context.Context, user *model.User) template.HTML {
	empty := template.HTML(frequencyDockEmpty)
	if h.auralis == nil || !h.auralis.Configured() || user == nil || user.PIALID == "" || user.ID == "demo_user" {
		return empty
	}
	v, err := h.auralis.Current(ctx, user.PIALID)
	if err != nil {
		log.Printf("[frequency] WARN dock for %s: %v", user.Handle, err)
		return empty
	}
	if v == nil || !v.Frequency.IsLive() {
		return empty
	}
	st := h.buildFrequencyStage(v, user.PIALID, user, nil, nil)
	frag, err := h.renderFrequencySlot(freqSlotDock, st)
	if err != nil {
		log.Printf("[frequency] WARN dock render: %v", err)
		return empty
	}
	return template.HTML(frag)
}

// oobSwap marks a fragment for an htmx out-of-band swap by its own id, so a
// response whose primary target is the stage can also replace the Shell's
// dock in the same answer.
func oobSwap(frag string) string {
	frag = strings.TrimSpace(frag)
	end := strings.IndexByte(frag, '>')
	if !strings.HasPrefix(frag, "<") || end < 0 {
		return frag
	}
	head := frag[:end]
	if strings.HasSuffix(head, "/") {
		head = strings.TrimSuffix(head, "/")
		return head + ` hx-swap-oob="true"/>` + frag[end+1:]
	}
	return head + ` hx-swap-oob="true">` + frag[end+1:]
}

// frequencyErrorMessage turns an Auralis refusal into the sentence a person
// reads. Codes are Auralis's machine codes; the words are Nantar's.
func frequencyErrorMessage(err error) (string, int) {
	if auralis.IsOutage(err) {
		return "Frequencies are unavailable right now. Try again in a moment.", 503
	}
	if auralis.IsRateLimited(err) {
		return "Slow down a little — try that again in a minute.", 429
	}
	if auralis.IsForbidden(err) {
		return "You are not allowed to do that here.", 403
	}
	if auralis.IsNotFound(err) {
		return "That Frequency no longer exists.", 404
	}
	if auralis.IsBadRequest(err) {
		var s *auralis.StatusError
		if errors.As(err, &s) && s.Message != "" {
			return s.Message, 400
		}
		return "That request could not be understood.", 400
	}
	switch auralis.Code(err) {
	case "not_live":
		return "This Frequency is not live.", 409
	case "over":
		return "This Frequency has ended.", 409
	case "locked":
		return "The host has locked this Frequency.", 409
	case "blocked":
		return "You cannot join this Frequency.", 409
	case "full":
		return "This Frequency is full.", 409
	case "speakers_full":
		return "Every microphone is taken right now.", 409
	case "requests_closed":
		return "The host is not taking speaker requests right now.", 409
	case "verity_tier_too_low":
		return "This host only takes speaker requests from verified accounts.", 409
	case "host_already_live":
		return "You already have a Frequency running. End it before starting another.", 409
	case "not_joined":
		return "Tune In first.", 409
	case "not_a_speaker", "not_a_cohost", "is_host", "own_request":
		return "That does not apply to this person.", 409
	case "media_unavailable":
		return "No audio node could take this Frequency. Try again shortly.", 409
	case "recording_locked_while_live":
		return "Recording cannot be changed while the Frequency is live.", 409
	case "already_cancelled":
		return "Already cancelled.", 409
	}
	if auralis.IsConflict(err) {
		return "That is not possible right now.", 409
	}
	return "Something went wrong with that Frequency.", 502
}

// frequencyVisibility normalises what the form sent.
func frequencyVisibility(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "followers", "subscribers", "private":
		return strings.ToLower(strings.TrimSpace(raw))
	}
	return "public"
}
