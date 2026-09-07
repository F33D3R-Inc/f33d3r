package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/auralis"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// Frequencies — routes and the POST /events lane.
//
// Every handler here has the same shape as followEvent (social.go): the
// actor is the authenticated session, the mutation is asked of Auralis, the
// HTTP answer is the actor's own re-rendered Fragment, and everyone else
// present receives theirs over FA Live from PublishFrequencyState. Nothing is
// decided in this file; Auralis refuses or permits, and the refusal is
// rendered.
//
// Frequencies live under the Go Live tab: Start Frequency is a mode of the
// go-live composer, cards share the live strip with video broadcasts, and
// /frequencies is the lanes page. /spheres and /spaces, the old placeholder,
// redirect here.

// frequencyActor returns the signed-in account with a PIAL, or answers the
// request and returns nil. The DemoUser can read; it cannot act.
func (h *Handler) frequencyActor(w http.ResponseWriter, r *http.Request) *model.User {
	user := liveActor(h.userFromRequest(w, r))
	if user == nil {
		frequencyProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to take part in a Frequency.")
		return nil
	}
	if user.PIALID == "" {
		frequencyProblem(w, r, http.StatusForbidden, "identity_incomplete", "Your identity is not fully set up yet — finish onboarding first.")
		return nil
	}
	return user
}

// frequencyReady answers 503 when this deployment has no Frequencies brain.
func (h *Handler) frequencyReady(w http.ResponseWriter, r *http.Request) bool {
	if h.auralis == nil || !h.auralis.Configured() {
		frequencyProblem(w, r, http.StatusServiceUnavailable, "frequencies_unavailable", "Frequencies are not available on this deployment.")
		return false
	}
	return true
}

// frequencyRefuse renders an Auralis refusal or outage to the caller: the
// JSON envelope for a native client, the HTMX bubble for the web.
func (h *Handler) frequencyRefuse(w http.ResponseWriter, r *http.Request, id string, err error) {
	if apiWantsJSON(r) {
		h.apiFrequencyRefuse(w, err)
		return
	}
	msg, code := frequencyErrorMessage(err)
	if code >= 500 {
		log.Printf("[frequency] %s: %v", id, err)
	}
	htmxError(w, r, msg, code)
}

// frequencyProblem answers a refusal this lane raises itself — a missing
// field, a gate, a person who does not exist — in whichever form the caller
// reads: the {code,message} envelope for a native client, the inline notice
// for the web. Auralis's own refusals go through frequencyRefuse.
func frequencyProblem(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	if apiWantsJSON(r) {
		apiError(w, status, code, msg)
		return
	}
	htmxError(w, r, msg, status)
}

// frequencyVerityTier maps the PIAL's KYC tier to Auralis's 0..3 ladder.
func frequencyVerityTier(u *model.User) int {
	switch u.KYCTier {
	case model.KYCTierFull:
		return 3
	case model.KYCTierSoft:
		return 2
	case model.KYCTierBasic:
		return 1
	}
	return 0
}

// frequencyNSFWAllowed mirrors the live lane's broadcast gate (live.go).
func frequencyNSFWAllowed(u *model.User) bool {
	return u.IsAdultCreator || (u.IsAdult && u.ContentSetting == "adult_enabled")
}

// ── pages ────────────────────────────────────────────────────────────────────

// frequenciesPage is the lanes page: live now, scheduled, recently ended.
func (h *Handler) frequenciesPage(w http.ResponseWriter, r *http.Request) {
	viewer := h.userFromRequest(w, r)
	viewerPIAL := ""
	if a := liveActor(viewer); a != nil {
		viewerPIAL = a.PIALID
	}
	lanes := map[string]interface{}{
		"Live":      []FrequencyCardData{},
		"Scheduled": []FrequencyCardData{},
		"Ended":     []FrequencyCardData{},
		"Ctx":       h.workCardCtx(viewer, "frequencies"),
	}
	data := map[string]interface{}{
		"User":        viewer,
		"PageTitle":   "Frequencies · F33D3R",
		"Mode":        "frequencies",
		"Lanes":       lanes,
		"Unavailable": false,
	}
	if h.auralis != nil && h.auralis.Configured() {
		for _, lane := range []string{"live", "scheduled", "ended"} {
			ans, err := h.auralis.List(r.Context(), lane, viewerPIAL, 24)
			if err != nil {
				log.Printf("[frequency] lanes %s: %v", lane, err)
				data["Unavailable"] = true
				break
			}
			cards := h.buildFrequencyCards(ans.Items, viewer)
			switch lane {
			case "live":
				lanes["Live"] = cards
			case "scheduled":
				lanes["Scheduled"] = cards
			case "ended":
				lanes["Ended"] = cards
			}
		}
	} else {
		data["Unavailable"] = true
	}
	for k, val := range h.railData(viewer, "default") {
		data[k] = val
	}
	h.render(w, r, "frequencies.html", data)
}

// frequencyPage is the stage. It renders the stage as Auralis sees it for
// this viewer; joining is a POST /events frequency.tune_in, never a side
// effect of loading a page.
func (h *Handler) frequencyPage(w http.ResponseWriter, r *http.Request) {
	if !h.frequencyReady(w, r) {
		return
	}
	id := r.PathValue("id")
	user := h.frequencyActor(w, r)
	if user == nil {
		return
	}
	ans, err := h.auralis.Get(r.Context(), id, user.PIALID)
	if err != nil {
		if auralis.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		h.frequencyRefuse(w, r, id, err)
		return
	}
	if ans.Frequency.Frequency.AdultContent && excludeNSFW(user) {
		frequencyProblem(w, r, http.StatusForbidden, "adult_content_disabled", "This Frequency is for adult-enabled accounts.")
		return
	}
	stage := h.buildFrequencyStage(&ans.Frequency, user.PIALID, user, nil, nil)
	data := map[string]interface{}{
		"User":      user,
		"PageTitle": ans.Frequency.Frequency.Title + " · Frequency · F33D3R",
		"Mode":      "frequency",
		"Stage":     stage,
	}
	for k, val := range h.railData(user, "default") {
		data[k] = val
	}
	h.render(w, r, "frequency.html", data)
}

// ── read lane ────────────────────────────────────────────────────────────────

// facetFrequencyCards writes the live Frequency cards for the head lane.
// Empty body when nothing is live, like facetLiveCards.
func (h *Handler) facetFrequencyCards(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	viewer := h.userFromRequest(w, r)
	for _, frag := range h.frequencyCardFragments(r, viewer, 12) {
		_, _ = w.Write([]byte(frag))
	}
}

// frequencyCardFragments renders the live cards, or nothing when Auralis is
// unreachable — the strip must never break the surface it sits in.
func (h *Handler) frequencyCardFragments(r *http.Request, viewer *model.User, limit int) []string {
	if h.auralis == nil || !h.auralis.Configured() {
		return nil
	}
	ans, err := h.auralis.List(r.Context(), "live", "", limit)
	if err != nil {
		log.Printf("[frequency] live cards: %v", err)
		return nil
	}
	cards := h.buildFrequencyCards(ans.Items, viewer)
	out := make([]string, 0, len(cards))
	for i := range cards {
		frag, rerr := h.renderFrequencyFragment("frequency_card", cards[i])
		if rerr != nil {
			log.Printf("[frequency] %v", rerr)
			continue
		}
		out = append(out, frag)
	}
	return out
}

// facetFrequencySlot serves one slot of one Frequency for this viewer.
func (h *Handler) facetFrequencySlot(w http.ResponseWriter, r *http.Request) {
	if !h.frequencyReady(w, r) {
		return
	}
	id, slot := r.PathValue("id"), r.PathValue("slot")
	name, ok := frequencySlotPartial(slot)
	if !ok {
		http.NotFound(w, r)
		return
	}
	viewer := h.userFromRequest(w, r)
	viewerPIAL := ""
	if a := liveActor(viewer); a != nil {
		viewerPIAL = a.PIALID
	}
	ans, err := h.auralis.Get(r.Context(), id, viewerPIAL)
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	var frag string
	if slot == freqSlotCard {
		people := h.frequencyPeople([]string{ans.Frequency.Frequency.HostPialID})
		frag, err = h.renderFrequencyFragment(name, h.buildFrequencyCard(&ans.Frequency.Frequency, ans.Frequency.Counts, people[ans.Frequency.Frequency.HostPialID], viewer))
	} else {
		frag, err = h.renderFrequencySlot(slot, h.buildFrequencyStage(&ans.Frequency, viewerPIAL, viewer, nil, nil))
	}
	if err != nil {
		log.Printf("[frequency] %v", err)
		http.Error(w, "fragment unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// ── POST /events lane ────────────────────────────────────────────────────────

// apiV1FrequencyEvent answers every frequency.* event and returns true; false
// for any other event type so the caller carries on.
func (h *Handler) apiV1FrequencyEvent(w http.ResponseWriter, r *http.Request, eventType string, raw map[string]json.RawMessage) bool {
	if !strings.HasPrefix(eventType, "frequency.") {
		return false
	}
	if !h.frequencyReady(w, r) {
		return true
	}
	user := h.frequencyActor(w, r)
	if user == nil {
		return true
	}
	id := eventField(r, raw, "frequency_id", "id")

	switch eventType {
	case "frequency.go_live":
		h.frequencyGoLive(w, r, user, raw)
	case "frequency.create":
		h.frequencyCreate(w, r, user, raw, false)
	case "frequency.start":
		h.frequencyAnswer(w, r, user, id, func() (*auralis.Answer, error) {
			return h.auralis.Start(r.Context(), user.PIALID, idemKey(r, raw), id)
		})
	case "frequency.end":
		h.frequencyAnswer(w, r, user, id, func() (*auralis.Answer, error) {
			return h.auralis.End(r.Context(), user.PIALID, idemKey(r, raw), id)
		})
	case "frequency.cancel":
		h.frequencyAnswer(w, r, user, id, func() (*auralis.Answer, error) {
			return h.auralis.Cancel(r.Context(), user.PIALID, id)
		})
	case "frequency.schedule":
		var at *time.Time
		if s := eventField(r, raw, "scheduled_at"); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				frequencyProblem(w, r, http.StatusBadRequest, "invalid_scheduled_at", "scheduled_at must be an RFC 3339 time")
				return true
			}
			at = &t
		}
		h.frequencyAnswer(w, r, user, id, func() (*auralis.Answer, error) {
			return h.auralis.Schedule(r.Context(), user.PIALID, id, at)
		})
	case "frequency.lock":
		locked := eventBoolField(r, raw, false, "locked")
		h.frequencyAnswer(w, r, user, id, func() (*auralis.Answer, error) {
			return h.auralis.Lock(r.Context(), user.PIALID, id, locked)
		})
	case "frequency.requests_open":
		open := eventBoolField(r, raw, false, "open")
		h.frequencyAnswer(w, r, user, id, func() (*auralis.Answer, error) {
			return h.auralis.RequestsOpen(r.Context(), user.PIALID, id, open)
		})
	case "frequency.tune_in":
		h.frequencyTuneIn(w, r, user, id, raw)
	case "frequency.leave":
		if id == "" {
			frequencyProblem(w, r, http.StatusBadRequest, "frequency_id_required", "frequency_id required")
			return true
		}
		ans, err := h.auralis.Leave(r.Context(), user.PIALID, id)
		if err != nil {
			h.frequencyRefuse(w, r, id, err)
			return true
		}
		go h.publishFrequencyView(&ans.Frequency)
		h.writeDockOrStage(w, r, user, ans, true)
	case "frequency.heartbeat":
		h.frequencyHeartbeat(w, r, user, id)
	case "frequency.request_mic":
		h.frequencyRequestMic(w, r, user, id, raw)
	case "frequency.withdraw":
		rid := eventField(r, raw, "request_id")
		if err := h.auralis.Withdraw(r.Context(), user.PIALID, id, rid); err != nil {
			h.frequencyRefuse(w, r, id, err)
			return true
		}
		h.frequencyRespondSlots(w, r, user, id, freqSlotRequestButton)
	case "frequency.upvote":
		rid := eventField(r, raw, "request_id")
		if _, err := h.auralis.Upvote(r.Context(), user.PIALID, id, rid); err != nil {
			h.frequencyRefuse(w, r, id, err)
			return true
		}
		h.frequencyRespondSlots(w, r, user, id, freqSlotRequests)
	case "frequency.approve":
		rid := eventField(r, raw, "request_id")
		h.frequencyAnswer(w, r, user, id, func() (*auralis.Answer, error) {
			return h.auralis.Approve(r.Context(), user.PIALID, id, rid)
		})
	case "frequency.decline":
		rid := eventField(r, raw, "request_id")
		h.frequencyAnswer(w, r, user, id, func() (*auralis.Answer, error) {
			return h.auralis.Decline(r.Context(), user.PIALID, id, rid)
		})
	case "frequency.mute", "frequency.unmute", "frequency.demote", "frequency.remove",
		"frequency.block", "frequency.unblock", "frequency.cohost", "frequency.uncohost":
		h.frequencyParticipantAction(w, r, user, id, eventType, raw)
	case "frequency.report":
		h.frequencyReport(w, r, user, id, raw)
	default:
		http.Error(w, "unknown event_type: "+eventType, http.StatusNotFound)
	}
	return true
}

// idemKey is the caller's idempotency key, when it sent one.
func idemKey(r *http.Request, raw map[string]json.RawMessage) string {
	if k := strings.TrimSpace(r.Header.Get("Idempotency-Key")); k != "" {
		return k
	}
	return eventField(r, raw, "idempotency_key")
}

// frequencyAnswer runs one Auralis mutation and answers with the actor's
// stage, then fans the new state out to everyone else.
func (h *Handler) frequencyAnswer(w http.ResponseWriter, r *http.Request, user *model.User, id string, do func() (*auralis.Answer, error)) {
	if id == "" {
		frequencyProblem(w, r, http.StatusBadRequest, "frequency_id_required", "frequency_id required")
		return
	}
	ans, err := do()
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	go h.publishFrequencyView(&ans.Frequency)
	h.writeFrequencyStage(w, r, user, ans)
}

// writeFrequencyStage answers with the whole stage for this actor — the
// simplest correct Fragment after a mutation that can change several slots.
// A native caller gets the room DTO instead, session included.
func (h *Handler) writeFrequencyStage(w http.ResponseWriter, r *http.Request, user *model.User, ans *auralis.Answer) {
	if apiWantsJSON(r) {
		h.writeFrequencyRoomJSON(w, user, ans)
		return
	}
	stage := h.buildFrequencyStage(&ans.Frequency, user.PIALID, user, ans.Session, nil)
	frag, err := h.renderFrequencyFragment("frequency_stage", stage)
	if err != nil {
		log.Printf("[frequency] %v", err)
		http.Error(w, "fragment unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// frequencyRespondSlots re-reads and answers with the named slots for this
// actor, and fans out.
func (h *Handler) frequencyRespondSlots(w http.ResponseWriter, r *http.Request, user *model.User, id string, slots ...string) {
	ans, err := h.auralis.Get(r.Context(), id, user.PIALID)
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	go h.publishFrequencyView(&ans.Frequency)
	if apiWantsJSON(r) {
		h.writeFrequencyRoomJSON(w, user, ans)
		return
	}
	stage := h.buildFrequencyStage(&ans.Frequency, user.PIALID, user, nil, nil)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	for _, slot := range slots {
		frag, err := h.renderFrequencySlot(slot, stage)
		if err != nil {
			log.Printf("[frequency] %v", err)
			continue
		}
		_, _ = w.Write([]byte(frag))
	}
}

// frequencyCreateInput reads the creation fields. adult_content is refused
// for an account the live lane would refuse an 18+ broadcast to.
func frequencyCreateInput(r *http.Request, user *model.User, raw map[string]json.RawMessage) (auralis.CreateInput, string) {
	in := auralis.CreateInput{
		Title:            strings.TrimSpace(eventField(r, raw, "title")),
		Description:      strings.TrimSpace(eventField(r, raw, "description")),
		Visibility:       frequencyVisibility(eventField(r, raw, "visibility", "audience")),
		Language:         strings.TrimSpace(eventField(r, raw, "language")),
		AdultContent:     eventBoolField(r, raw, false, "adult_content", "is_nsfw", "nsfw"),
		RecordingEnabled: eventBoolField(r, raw, false, "recording_enabled", "recording", "save_replay"),
	}
	if in.Title == "" {
		return in, "Give your Frequency a title."
	}
	if in.AdultContent && !frequencyNSFWAllowed(user) {
		return in, "Your account is not cleared to host an 18+ Frequency."
	}
	if n, ok := eventIntField(r, raw, "max_speakers"); ok {
		in.MaxSpeakers = &n
	}
	if n, ok := eventIntField(r, raw, "max_listeners"); ok {
		in.MaxListeners = &n
	}
	if n, ok := eventIntField(r, raw, "speaker_verity_min_tier"); ok {
		in.SpeakerVerityMinTier = &n
	}
	if s := eventField(r, raw, "scheduled_at"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return in, "scheduled_at must be an RFC 3339 time"
		}
		in.ScheduledAt = &t
	}
	return in, ""
}

// frequencyGoLive is the go-live composer's audio mode: create and start in
// one press, then send the host to their stage. A refusal re-renders the
// composer with their words still in it.
func (h *Handler) frequencyGoLive(w http.ResponseWriter, r *http.Request, user *model.User, raw map[string]json.RawMessage) {
	in, problem := frequencyCreateInput(r, user, raw)
	draft := frequencyGoDraft{Title: in.Title, Description: in.Description, AdultContent: in.AdultContent, Recording: in.RecordingEnabled}
	if problem != "" {
		h.renderGoComposerWith(w, r, user, goComposerDraft{}, "", http.StatusBadRequest, draft.withError(problem))
		return
	}
	created, err := h.auralis.Create(r.Context(), user.PIALID, idemKey(r, raw), in)
	if err != nil {
		msg, code := frequencyErrorMessage(err)
		if code >= 500 {
			log.Printf("[frequency] go_live create: %v", err)
		}
		h.renderGoComposerWith(w, r, user, goComposerDraft{}, "", code, draft.withError(msg))
		return
	}
	id := created.Frequency.Frequency.ID
	started, err := h.auralis.Start(r.Context(), user.PIALID, "", id)
	if err != nil {
		msg, code := frequencyErrorMessage(err)
		if code >= 500 {
			log.Printf("[frequency] go_live start %s: %v", id, err)
		}
		h.renderGoComposerWith(w, r, user, goComposerDraft{}, "", code, draft.withError(msg))
		return
	}
	go h.publishFrequencyView(&started.Frequency)
	if apiWantsJSON(r) {
		h.writeFrequencyRoomJSON(w, user, started)
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/frequencies/"+id, http.StatusSeeOther)
		return
	}
	// The composer targets #main-col: answer with the stage body and push the
	// stage URL so the Shell lands where a document load would have.
	stage := h.buildFrequencyStage(&started.Frequency, user.PIALID, user, started.Session, nil)
	frag, err := h.renderFrequencyFragment("frequency_stage", stage)
	if err != nil {
		log.Printf("[frequency] %v", err)
		hxRedirect(w, "/frequencies/"+id)
		return
	}
	w.Header().Set("HX-Push-Url", "/frequencies/"+id)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<main class="f33d3r-main freq-main freq-main--` + stage.State + `" id="main-col">` + frag + `</main>`))
	// The host is in their Frequency now: the Shell's dock follows.
	if dock, derr := h.renderFrequencySlot(freqSlotDock, stage); derr == nil {
		_, _ = w.Write([]byte(oobSwap(dock)))
	} else {
		log.Printf("[frequency] %v", derr)
	}
}

// fromStage reports whether the caller is looking at this Frequency's stage
// page (HX-Current-URL), which decides whether an answer is the stage or the
// Shell's dock.
func fromStage(r *http.Request, id string) bool {
	return strings.HasPrefix(hxCurrentPath(r), "/frequencies/"+id)
}

// writeDockOrStage answers a join/leave. On the stage page the primary
// target is the stage (its forms swap `closest .freq-stage`), and the dock
// rides along out-of-band by its id. Anywhere else — the preview overlay, a
// card — the target is #frequency-dock and the dock is the answer. `over`
// forces the empty mount.
func (h *Handler) writeDockOrStage(w http.ResponseWriter, r *http.Request, user *model.User, ans *auralis.Answer, over bool) {
	if apiWantsJSON(r) {
		h.writeFrequencyRoomJSON(w, user, ans)
		return
	}
	stage := h.buildFrequencyStage(&ans.Frequency, user.PIALID, user, ans.Session, nil)
	dock := frequencyDockEmpty
	if !over && stage.Viewer.InFrequency && stage.State == "live" {
		if frag, err := h.renderFrequencySlot(freqSlotDock, stage); err == nil {
			dock = frag
		} else {
			log.Printf("[frequency] %v", err)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if fromStage(r, stage.ID) {
		frag, err := h.renderFrequencyFragment("frequency_stage", stage)
		if err != nil {
			log.Printf("[frequency] %v", err)
			http.Error(w, "fragment unavailable", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(frag))
		_, _ = w.Write([]byte(oobSwap(dock)))
		return
	}
	_, _ = w.Write([]byte(dock))
}

// frequencyCreate creates without starting (a draft or a scheduled one).
func (h *Handler) frequencyCreate(w http.ResponseWriter, r *http.Request, user *model.User, raw map[string]json.RawMessage, _ bool) {
	in, problem := frequencyCreateInput(r, user, raw)
	if problem != "" {
		frequencyProblem(w, r, http.StatusBadRequest, "invalid_frequency", problem)
		return
	}
	ans, err := h.auralis.Create(r.Context(), user.PIALID, idemKey(r, raw), in)
	if err != nil {
		h.frequencyRefuse(w, r, "", err)
		return
	}
	go h.publishFrequencyView(&ans.Frequency)
	h.writeFrequencyStage(w, r, user, ans)
}

// frequencyTuneIn admits the actor and answers with the stage that carries
// their session token.
func (h *Handler) frequencyTuneIn(w http.ResponseWriter, r *http.Request, user *model.User, id string, raw map[string]json.RawMessage) {
	if id == "" {
		frequencyProblem(w, r, http.StatusBadRequest, "frequency_id_required", "frequency_id required")
		return
	}
	// The adult gate is this brain's to apply before asking: Auralis does not
	// know the viewer's content setting.
	pre, err := h.auralis.Get(r.Context(), id, user.PIALID)
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	if pre.Frequency.Frequency.AdultContent && excludeNSFW(user) {
		frequencyProblem(w, r, http.StatusForbidden, "adult_content_disabled", "This Frequency is for adult-enabled accounts.")
		return
	}
	ans, err := h.auralis.TuneIn(r.Context(), user.PIALID, idemKey(r, raw), id)
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	go h.publishFrequencyView(&ans.Frequency)
	h.writeDockOrStage(w, r, user, ans, false)
}

// frequencyHeartbeatDTO is the beat's answer in the native envelope: the
// brain's HeartbeatAnswer re-stated as the DTO the contract pins, so a field
// Auralis adds does not reach a phone unannounced.
func frequencyHeartbeatDTO(hb *auralis.HeartbeatAnswer) FrequencyHeartbeatDTO {
	out := FrequencyHeartbeatDTO{State: hb.State, Present: hb.Present, Role: hb.Role, Muted: hb.Muted}
	if hb.Counts != nil {
		out.Counts = &FrequencyCountsDTO{Listeners: hb.Counts.Listeners, Speakers: hb.Counts.Speakers, Participants: hb.Counts.Participants}
	}
	return out
}

// frequencyHeartbeat is the presence beat. The tally Facet posts it and swaps
// itself for the answer, on the stage header and in the Shell's dock alike,
// so the answer is always that tally: beating again while the person is
// present, still when they are not.
func (h *Handler) frequencyHeartbeat(w http.ResponseWriter, r *http.Request, user *model.User, id string) {
	if id == "" {
		frequencyProblem(w, r, http.StatusBadRequest, "frequency_id_required", "frequency_id required")
		return
	}
	hb, err := h.auralis.Heartbeat(r.Context(), user.PIALID, id)
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	if apiWantsJSON(r) {
		apiJSON(w, http.StatusOK, frequencyHeartbeatDTO(hb))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !hb.Present {
		// The Frequency is over, or this person is no longer in it. The beat
		// stops: the tally is answered without one, the Shell's dock leaves
		// out-of-band by its id, and on the stage the status, badge and
		// controls follow so the page goes dark with it.
		stage := FrequencyStageData{ID: id, State: hb.State, ListenersLabel: frequencyListenersLabel(0), Ctx: h.workCardCtx(user, "frequency")}
		onStage := fromStage(r, id)
		if ans, gerr := h.auralis.Get(r.Context(), id, user.PIALID); gerr == nil {
			stage = h.buildFrequencyStage(&ans.Frequency, user.PIALID, user, nil, nil)
		} else if onStage {
			log.Printf("[frequency] heartbeat %s: read after departure: %v", id, gerr)
		}
		stage.Heartbeat = false
		frag, rerr := h.renderFrequencySlot(freqSlotListenerCount, stage)
		if rerr != nil {
			log.Printf("[frequency] %v", rerr)
			http.Error(w, "fragment unavailable", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(frag))
		_, _ = w.Write([]byte(oobSwap(frequencyDockEmpty)))
		if onStage {
			for _, slot := range []string{freqSlotStatus, freqSlotLiveBadge, freqSlotControls} {
				if sf, serr := h.renderFrequencySlot(slot, stage); serr == nil {
					_, _ = w.Write([]byte(oobSwap(sf)))
				}
			}
		}
		return
	}
	count := 0
	if hb.Counts != nil {
		count = hb.Counts.Listeners
	}
	// The listener count is the only slot a beat can change; the host's
	// requests, the speaker grid and the rest arrive over FA Live.
	stage := FrequencyStageData{ID: id, State: hb.State, ListenerCount: count, ListenersLabel: frequencyListenersLabel(count), Heartbeat: true, Viewer: FrequencyViewer{InFrequency: true}, Ctx: h.workCardCtx(user, "frequency")}
	frag, err := h.renderFrequencySlot(freqSlotListenerCount, stage)
	if err != nil {
		log.Printf("[frequency] %v", err)
		http.Error(w, "fragment unavailable", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(frag))
}

// frequencyRequestMic queues the actor's request with the reason they typed.
func (h *Handler) frequencyRequestMic(w http.ResponseWriter, r *http.Request, user *model.User, id string, raw map[string]json.RawMessage) {
	if id == "" {
		frequencyProblem(w, r, http.StatusBadRequest, "frequency_id_required", "frequency_id required")
		return
	}
	reason := truncate(strings.TrimSpace(eventField(r, raw, "reason")), 140)
	if _, err := h.auralis.RequestMic(r.Context(), user.PIALID, id, reason, frequencyVerityTier(user)); err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	h.frequencyRespondSlots(w, r, user, id, freqSlotRequestButton)
}

// frequencyParticipantAction is every host/co-host control aimed at a person.
// The target is a handle from the button; it is resolved to a PIAL here,
// because Auralis speaks PIAL and never handle.
func (h *Handler) frequencyParticipantAction(w http.ResponseWriter, r *http.Request, user *model.User, id, eventType string, raw map[string]json.RawMessage) {
	if id == "" {
		frequencyProblem(w, r, http.StatusBadRequest, "frequency_id_required", "frequency_id required")
		return
	}
	target, ok := h.frequencyTargetPIAL(w, r, raw)
	if !ok {
		return
	}
	reason := truncate(strings.TrimSpace(eventField(r, raw, "reason")), 140)
	ctx := r.Context()
	me := user.PIALID
	if eventType == "frequency.remove" || eventType == "frequency.block" {
		// The fan-out cannot reach someone who is no longer present; their
		// dock is cleared here, after the answer.
		defer frequencyDepartedDock(target)
	}
	h.frequencyAnswer(w, r, user, id, func() (*auralis.Answer, error) {
		switch eventType {
		case "frequency.mute":
			return h.auralis.Mute(ctx, me, id, target)
		case "frequency.unmute":
			return h.auralis.Unmute(ctx, me, id, target)
		case "frequency.demote":
			return h.auralis.Demote(ctx, me, id, target)
		case "frequency.remove":
			return h.auralis.Remove(ctx, me, id, target, reason)
		case "frequency.block":
			return h.auralis.Block(ctx, me, id, target, reason)
		case "frequency.unblock":
			return h.auralis.Unblock(ctx, me, id, target)
		case "frequency.cohost":
			return h.auralis.AddCohost(ctx, me, id, target)
		default:
			return h.auralis.RemoveCohost(ctx, me, id, target)
		}
	})
}

// frequencyTargetPIAL resolves target_handle (or target_pial, from the
// native clients) to a bare PIAL uuid. "me" names the actor.
func (h *Handler) frequencyTargetPIAL(w http.ResponseWriter, r *http.Request, raw map[string]json.RawMessage) (string, bool) {
	if p := strings.TrimSpace(eventField(r, raw, "target_pial")); p != "" {
		return p, true
	}
	handle := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(eventField(r, raw, "target_handle", "handle"))), "@")
	if handle == "" {
		frequencyProblem(w, r, http.StatusBadRequest, "target_handle_required", "target_handle required")
		return "", false
	}
	// The viewer naming themself (self mute/unmute posts their own handle):
	// no lookup, and it works before this brain's user rows are consulted.
	if me := liveActor(h.userFromRequest(w, r)); me != nil && (handle == "me" || handle == strings.ToLower(me.Handle)) {
		return me.PIALID, true
	}
	if h.db == nil {
		frequencyProblem(w, r, http.StatusServiceUnavailable, "db_unavailable", "database unavailable")
		return "", false
	}
	target, err := dbpkg.GetUserByHandle(h.db, handle)
	if err != nil || target == nil || target.PIALID == "" {
		frequencyProblem(w, r, http.StatusNotFound, "not_found", "No such person.")
		return "", false
	}
	return target.PIALID, true
}

// frequencyReport files a report on a Frequency or on one of its participants
// through the same funnel every other report takes.
func (h *Handler) frequencyReport(w http.ResponseWriter, r *http.Request, user *model.User, id string, raw map[string]json.RawMessage) {
	if id == "" {
		frequencyProblem(w, r, http.StatusBadRequest, "frequency_id_required", "frequency_id required")
		return
	}
	reason := eventField(r, raw, "reason")
	switch reason {
	case "spam", "harassment", "hate", "nsfw", "violence", "illegal", "other":
	default:
		frequencyProblem(w, r, http.StatusBadRequest, "reason_required", "reason required")
		return
	}
	detail := truncate(eventField(r, raw, "detail"), 500)
	contentID, contentType := id, "frequency"
	if handle := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(eventField(r, raw, "target_handle"))), "@"); handle != "" && h.db != nil {
		target, err := dbpkg.GetUserByHandle(h.db, handle)
		if err != nil || target == nil {
			frequencyProblem(w, r, http.StatusNotFound, "not_found", "No such person.")
			return
		}
		contentID, contentType = id+":"+target.ID, "frequency_participant"
	}
	if err := h.fileContentReport(user, contentID, contentType, reason, detail); err != nil {
		frequencyProblem(w, r, http.StatusInternalServerError, "server_error", "Could not file the report.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// frequencyGoDraft is what the host typed into the audio mode of the go-live
// composer, carried back when the server refuses it.
type frequencyGoDraft struct {
	Title        string
	Description  string
	AdultContent bool
	Recording    bool
	Error        string
}

func (d frequencyGoDraft) withError(msg string) frequencyGoDraft {
	d.Error = msg
	return d
}

// frequencyGoData is the composer's audio-mode data: the draft plus what the
// host is cleared for.
func frequencyGoData(user *model.User, d frequencyGoDraft) map[string]interface{} {
	if d.Error == "" {
		// No rejection: the composer renders a blank audio form.
		return nil
	}
	return map[string]interface{}{
		"Error":        d.Error,
		"Title":        d.Title,
		"Description":  d.Description,
		"AdultContent": d.AdultContent,
		"Recording":    d.Recording,
		"CanNSFW":      frequencyNSFWAllowed(user),
		"Active":       d.Error != "" || d.Title != "",
	}
}

// frequencyGoLiveStrip is the "Frequencies live now" row on the go-live page.
// An unreachable Auralis yields an empty row and a log line, never a broken
// page.
func (h *Handler) frequencyGoLiveStrip(r *http.Request, user *model.User) []FrequencyCardData {
	if h.auralis == nil || !h.auralis.Configured() {
		return nil
	}
	ans, err := h.auralis.List(r.Context(), "live", "", 6)
	if err != nil {
		log.Printf("[frequency] go-live strip: %v", err)
		return nil
	}
	return h.buildFrequencyCards(ans.Items, user)
}

// frequencyHostOpen returns the id of the host's open Frequency, or "".
// Used by /golive to send a host straight back to their running stage.
func (h *Handler) frequencyHostOpen(r *http.Request, user *model.User) string {
	if h.auralis == nil || !h.auralis.Configured() || user == nil || user.PIALID == "" {
		return ""
	}
	open, err := h.auralis.HostOpen(r.Context(), user.PIALID)
	if err != nil {
		log.Printf("[frequency] host open %s: %v", user.Handle, err)
		return ""
	}
	if open == nil {
		return ""
	}
	return open.ID
}
