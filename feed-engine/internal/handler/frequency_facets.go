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
	userID       string
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
	ScheduledISO   string
	IsNSFW         bool
	Ctx            PageCtx
}

type FrequencySpeakerData struct {
	ID string
	// MediaIdentity is the name this person goes by in the room's audio.
	MediaIdentity string
	Person        FrequencyPerson
	Role          string
	Muted         bool
	Present       bool
	IsHost        bool
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
	// ScheduledISO is the scheduled time for the page to show in local time.
	ScheduledISO  string
	ReminderCount int
	Pins          []FrequencyPinData
	Viewer        FrequencyViewer
	TimeLabel     string
	EndedLabel    string
	// EndReason vocabulary: host_ended | host_lost | moderation | "".
	EndReason string
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
	freqSlotPins          = "pins"
)

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
	case freqSlotDockSpeakers:
		return "frequency_dock_speakers", true
	case freqSlotPins:
		return "frequency_pins", true
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
			"Recording": st.Recording, "IsNSFW": st.IsNSFW, "Ctx": st.Ctx,
		}
	case freqSlotListenerCount:
		return map[string]interface{}{"ID": st.ID, "Count": st.ListenerCount, "Label": st.ListenersLabel}
	case freqSlotLiveBadge:
		return map[string]interface{}{"ID": st.ID, "State": st.State, "ScheduledLabel": ""}
	case freqSlotRecording:
		return map[string]interface{}{"ID": st.ID, "Recording": st.Recording}
	case freqSlotStatus:
		return map[string]interface{}{
			"ID": st.ID, "State": st.State, "TimeLabel": st.TimeLabel, "EndedLabel": st.EndedLabel, "EndReason": st.EndReason,
			"Locked": st.Locked, "RequestsOpen": st.RequestsOpen, "ScheduledISO": st.ScheduledISO, "ReminderCount": st.ReminderCount,
		}
	case freqSlotSpeakers:
		return map[string]interface{}{"ID": st.ID, "Speakers": st.Speakers, "Surface": frequencyGridSurface(v), "Ctx": st.Ctx}
	case freqSlotDockSpeakers:
		return map[string]interface{}{"ID": st.ID, "Speakers": st.Speakers}
	case freqSlotPins:
		return map[string]interface{}{"ID": st.ID, "Pins": st.Pins, "Surface": frequencyQueueSurface(v)}
	case freqSlotRequests:
		return map[string]interface{}{"ID": st.ID, "Requests": st.Requests, "CanModerate": v.CanModerate, "Surface": frequencyQueueSurface(v), "Ctx": st.Ctx}
	case freqSlotControls:
		return map[string]interface{}{"ID": st.ID, "State": st.State, "Viewer": v, "RequestsOpen": st.RequestsOpen, "Locked": st.Locked, "Ctx": st.Ctx, "InDock": false, "Surface": frequencyControlsSurface(v), "Recording": st.Recording}
	case freqSlotRequestButton:
		return map[string]interface{}{"ID": st.ID, "CanRequestMic": v.CanRequestMic, "Request": v.Request, "RequestsOpen": st.RequestsOpen, "Live": live}
	}
	// stage, card (handled by caller), preview, dock, dock_mini, captions take
	// the whole stage.
	return st
}

// frequencyMayWatch applies the gates of the stage page to watching one.
func (h *Handler) frequencyMayWatch(ctx context.Context, user *model.User, id string) bool {
	if h.auralis == nil || !h.auralis.Configured() {
		return false
	}
	ans, err := h.auralis.Get(ctx, id, user.PIALID)
	if err != nil {
		if !auralis.IsNotFound(err) {
			log.Printf("[frequency] watch %s: %v", id, err)
		}
		return false
	}
	_, _, refused := h.frequencyGate(user, &ans.Frequency)
	return !refused
}

// frequencyAdultAllowed says whether an account has opted into adult rooms. Audio has
// no blur to fall back on, so being merely not a minor is not enough: a person must be
// a confirmed adult who turned adult content on.
func frequencyAdultAllowed(u *model.User) bool {
	return u != nil && !excludeNSFW(u) && u.IsAdult && u.ContentSetting == "adult_enabled"
}

// frequencyGate decides whether a person may look at a room at all: the stage,
// its Facets and its stream. Followers-only and subscriber-only rooms can be
// looked at, and Auralis refuses their Tune In with the reason. An invite-only
// room shows nothing to anyone it does not include.
func (h *Handler) frequencyGate(viewer *model.User, v *auralis.View) (code, message string, refused bool) {
	s := &v.Frequency
	viewerPIAL := ""
	if a := liveActor(viewer); a != nil {
		viewerPIAL = a.PIALID
	}
	if viewerPIAL != "" && viewerPIAL == s.HostPialID {
		return "", "", false
	}
	if s.AdultContent && !frequencyAdultAllowed(viewer) {
		return "adult_content_disabled", "This Frequency is for adult-enabled accounts.", true
	}
	const unavailable = "This Frequency isn't available."
	if viewerPIAL != "" && h.frequencyAudienceFacts(viewer, s.HostPialID).BlockedEitherWay {
		return "frequency_unavailable", unavailable, true
	}
	if s.Visibility == "private" {
		inside := false
		if v.Viewer != nil && v.Viewer.PialID == viewerPIAL && viewerPIAL != "" {
			inside = !v.Viewer.Blocked && (v.Viewer.Invited || v.Viewer.Role != nil)
		}
		if !inside {
			return "frequency_unavailable", unavailable, true
		}
	}
	return "", "", false
}

// frequencyAudienceFacts reads the social graph for one viewer and host. A
// failed read answers false, which is the narrowest admission.
func (h *Handler) frequencyAudienceFacts(viewer *model.User, hostPIAL string) auralis.AudienceFacts {
	var facts auralis.AudienceFacts
	a := liveActor(viewer)
	if a == nil || h.db == nil {
		return facts
	}
	hostID, err := dbpkg.GetUserIDFromPIAL(h.db, hostPIAL)
	if err != nil {
		log.Printf("[frequency] host %s: %v", hostPIAL, err)
		return facts
	}
	if following, ferr := dbpkg.IsFollowing(h.db, a.ID, hostID); ferr == nil {
		facts.FollowsHost = following
	} else {
		log.Printf("[frequency] follow state: %v", ferr)
	}
	facts.Subscribed = dbpkg.IsSubscribed(h.db, a.ID, hostID)
	facts.BlockedEitherWay = dbpkg.IsBlocked(h.db, hostID, a.ID) || dbpkg.IsBlocked(h.db, a.ID, hostID)
	return facts
}

// frequencyGridSurface names which copy of the speaker grid a viewer holds.
func frequencyGridSurface(v FrequencyViewer) string {
	switch {
	case v.Role == "host":
		return freqSurfaceHost
	case v.CanModerate:
		return freqSurfaceCohost
	}
	return freqSurfaceAudience
}

// frequencyQueueSurface names which copy of the request queue a viewer holds.
func frequencyQueueSurface(v FrequencyViewer) string {
	if v.CanModerate {
		return freqSurfaceModerator
	}
	return freqSurfaceAudience
}

// frequencyControlsSurface names which copy of the controls a stage holds.
func frequencyControlsSurface(v FrequencyViewer) string {
	if v.InFrequency {
		return freqSurfaceStage
	}
	return freqSurfaceOutsider
}

// renderFrequencyDockControls renders the controls as the dock shows them.
func (h *Handler) renderFrequencyDockControls(st FrequencyStageData) string {
	frag, err := h.renderFrequencyFragment("frequency_controls", map[string]interface{}{
		"ID": st.ID, "State": st.State, "Viewer": st.Viewer, "RequestsOpen": st.RequestsOpen,
		"Locked": st.Locked, "Ctx": st.Ctx, "InDock": true, "Surface": freqSurfaceDock, "Recording": st.Recording,
	})
	if err != nil {
		log.Printf("[frequency] %v", err)
		return ""
	}
	return frag
}

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
		userID:       u.ID,
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
		return frequencyScheduledLabel(s)
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

// frequencyDisplayState is the state a person sees. A room archived with no replay,
// or whose replay was deleted, has simply ended.
func frequencyDisplayState(s *auralis.Summary) string {
	if s.State == "archived" && s.ReplayStatus != "ready" {
		return "ended"
	}
	return s.State
}

func frequencyScheduledISO(s *auralis.Summary) string {
	if s == nil || s.ScheduledAt == nil {
		return ""
	}
	return s.ScheduledAt.UTC().Format(time.RFC3339)
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
		State:          frequencyDisplayState(s),
		Host:           host,
		SpeakerCount:   counts.Speakers,
		ListenerCount:  counts.Listeners,
		ListenersLabel: frequencyListenersLabel(counts.Listeners),
		TimeLabel:      frequencyTimeLabel(s),
		ScheduledLabel: frequencyScheduledLabel(s),
		ScheduledISO:   frequencyScheduledISO(s),
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
	viewerPIAL := ""
	blocked := map[string]bool{}
	if a := liveActor(viewer); a != nil {
		viewerPIAL = a.PIALID
		if h.db != nil {
			ids, err := dbpkg.GetBlockedUserIDs(h.db, a.ID)
			if err != nil {
				log.Printf("[frequency] blocks for %s: %v", a.ID, err)
			} else {
				blocked = ids
			}
		}
	}
	out := make([]FrequencyCardData, 0, len(items))
	for i := range items {
		s := &items[i].Frequency
		if s.AdultContent && (hideAdult || !frequencyAdultAllowed(viewer)) {
			continue
		}
		// An invite-only room is reached through its invitation, not a lane.
		if s.Visibility == "private" && s.HostPialID != viewerPIAL {
			continue
		}
		if blocked[people[s.HostPialID].userID] {
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
func (h *Handler) buildFrequencyStage(v *auralis.View, viewerPIAL string, viewer *model.User, people map[string]FrequencyPerson) FrequencyStageData {
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
			MediaIdentity:     sp.MediaIdentity,
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
		State:          frequencyDisplayState(s),
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
	data.ScheduledISO = frequencyScheduledISO(s)
	data.ReminderCount = v.ReminderCount
	data.Pins = h.frequencyPinData(v, canModerate)
	data.RequestCount = len(v.Requests)
	data.PeopleCount = v.Counts.Participants
	data.Captions = []string{}
	return data
}

func (h *Handler) requestData(freqID string, r *auralis.Request, people map[string]FrequencyPerson, viewerPIAL string, canModerate bool, ctx PageCtx) FrequencyRequestData {
	// Why someone asked for the mic is for the host and for them, not the audience.
	reason := ""
	if canModerate || (viewerPIAL != "" && r.PialID == viewerPIAL) {
		reason = r.Reason
	}
	return FrequencyRequestData{
		ID:           freqID,
		RequestID:    r.ID,
		Person:       people[r.PialID],
		Reason:       reason,
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

// renderFrequencySlot renders one slot for a stage.
func (h *Handler) renderFrequencySlot(slot string, st FrequencyStageData) (string, error) {
	name, ok := frequencySlotPartial(slot)
	if !ok {
		return "", fmt.Errorf("frequency: unknown slot %q", slot)
	}
	return h.renderFrequencyFragment(name, frequencySlotData(slot, st))
}

// frequencyDock is what the Shell's dock mount holds for a signed-in person:
// their live Frequency's dock, or nothing. Asked once per full page render.
// Fail-soft: an unreachable Auralis is a WARN line and an empty dock, never
// a broken page — and nothing is cached, so the next page asks again.
func (h *Handler) frequencyDock(ctx context.Context, user *model.User) template.HTML {
	empty := template.HTML("")
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
	st := h.buildFrequencyStage(v, user.PIALID, user, nil)
	frag, err := h.renderFrequencySlot(freqSlotDock, st)
	if err != nil {
		log.Printf("[frequency] WARN dock render: %v", err)
		return empty
	}
	return template.HTML(frag)
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
	case "followers_only":
		return "Only people who follow this host can tune in.", 409
	case "subscribers_only":
		return "Only this host's subscribers can tune in.", 409
	case "invite_only", "blocked_by_host":
		return "This Frequency isn't available.", 409
	case "in_another_room":
		return "You just joined another Frequency. Try again.", 409
	case "hosting_elsewhere":
		return "You're hosting a Frequency. End it before tuning in to another.", 409
	case "muted_by_moderator":
		return "A host muted you. Only a host can lift that.", 409
	case "requests_closed":
		return "The host is not taking speaker requests right now.", 409
	case "verity_tier_too_low":
		return "This host only takes speaker requests from verified accounts.", 409
	case "host_already_live":
		return "You already have a Frequency running. End it before starting another.", 409
	case "not_joined":
		return "Tune In first.", 409
	case "not_a_speaker", "not_a_cohost", "not_a_listener", "is_host", "own_request":
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
