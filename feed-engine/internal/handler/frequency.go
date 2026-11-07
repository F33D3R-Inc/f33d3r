package handler

import (
	"context"
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
		h.frequencyProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to take part in a Frequency.")
		return nil
	}
	if user.PIALID == "" {
		h.frequencyProblem(w, r, http.StatusForbidden, "identity_incomplete", "Your identity is not fully set up yet — finish onboarding first.")
		return nil
	}
	return user
}

// frequencyReady answers 503 when this deployment has no Frequencies brain.
func (h *Handler) frequencyReady(w http.ResponseWriter, r *http.Request) bool {
	if h.auralis == nil || !h.auralis.Configured() {
		h.frequencyProblem(w, r, http.StatusServiceUnavailable, "frequencies_unavailable", "Frequencies are not available on this deployment.")
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
	h.frequencyTellWeb(w, r, msg, code)
}

// frequencyProblem answers a refusal this lane raises itself — a missing
// field, a gate, a person who does not exist — in whichever form the caller
// reads: the {code,message} envelope for a native client, the inline notice
// for the web. Auralis's own refusals go through frequencyRefuse.
func (h *Handler) frequencyProblem(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	if apiWantsJSON(r) {
		apiError(w, status, code, msg)
		return
	}
	h.frequencyTellWeb(w, r, msg, status)
}

// frequencyTellWeb shows a refusal to a person who acted from a page. htmx does
// not swap an error status, so a signed-in person receives it as a toast on
// their stream and the request answers 204.
func (h *Handler) frequencyTellWeb(w http.ResponseWriter, r *http.Request, msg string, status int) {
	if r.Header.Get("HX-Request") == "true" {
		if user := liveActor(h.userFromRequest(w, r)); user != nil && user.PIALID != "" {
			frequencyToast(user.PIALID, msg)
			w.WriteHeader(http.StatusNoContent)
			return
		}
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
			if lane != "ended" {
				h.frequencyFollowedFirst(ans.Items, viewer)
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
	// The link is shareable: anyone can look, and joining asks them to sign in.
	viewer := h.userFromRequest(w, r)
	viewerPIAL := viewerPIALOf(viewer)
	ans, err := h.auralis.Get(r.Context(), id, viewerPIAL)
	if err != nil {
		if auralis.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		h.frequencyRefuse(w, r, id, err)
		return
	}
	if code, msg, refused := h.frequencyGate(viewer, &ans.Frequency); refused {
		// Someone signed out may be able to see it once they sign in.
		if viewerPIAL == "" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h.frequencyProblem(w, r, http.StatusForbidden, code, msg)
		return
	}
	stage := h.buildFrequencyStage(&ans.Frequency, viewerPIAL, viewer, nil)
	data := map[string]interface{}{
		"User":      viewer,
		"PageTitle": ans.Frequency.Frequency.Title + " · Frequency · F33D3R",
		"Mode":      "frequency",
		"Stage":     stage,
	}
	for k, val := range h.railData(viewer, "default") {
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
	if code, msg, refused := h.frequencyGate(viewer, &ans.Frequency); refused {
		h.frequencyProblem(w, r, http.StatusForbidden, code, msg)
		return
	}
	var frag string
	if slot == freqSlotCard {
		people := h.frequencyPeople([]string{ans.Frequency.Frequency.HostPialID})
		frag, err = h.renderFrequencyFragment(name, h.buildFrequencyCard(&ans.Frequency.Frequency, ans.Frequency.Counts, people[ans.Frequency.Frequency.HostPialID], viewer))
	} else {
		frag, err = h.renderFrequencySlot(slot, h.buildFrequencyStage(&ans.Frequency, viewerPIAL, viewer, nil))
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
				h.frequencyProblem(w, r, http.StatusBadRequest, "invalid_scheduled_at", "scheduled_at must be an RFC 3339 time")
				return true
			}
			if problem := frequencyScheduleProblem(t); problem != "" {
				h.frequencyProblem(w, r, http.StatusBadRequest, "invalid_scheduled_at", problem)
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
			h.frequencyProblem(w, r, http.StatusBadRequest, "frequency_id_required", "frequency_id required")
			return true
		}
		ans, err := h.auralis.Leave(r.Context(), user.PIALID, id)
		if err != nil {
			h.frequencyRefuse(w, r, id, err)
			return true
		}
		h.frequencyDirty(id)
		h.frequencyDone(w, r, user, ans)
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
		h.frequencyDirty(id)
		h.frequencyMutated(w, r, user, id)
	case "frequency.upvote":
		rid := eventField(r, raw, "request_id")
		if _, err := h.auralis.Upvote(r.Context(), user.PIALID, id, rid); err != nil {
			h.frequencyRefuse(w, r, id, err)
			return true
		}
		h.frequencyDirty(id)
		h.frequencyMutated(w, r, user, id)
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
		"frequency.block", "frequency.unblock", "frequency.cohost", "frequency.uncohost", "frequency.invite",
		"frequency.promote", "frequency.transfer":
		h.frequencyParticipantAction(w, r, user, id, eventType, raw)
	case "frequency.mic_denied":
		// The browser could not open the microphone: the person is not speaking,
		// so the room stops saying they are.
		if _, err := h.auralis.Mute(r.Context(), user.PIALID, id, user.PIALID); err != nil {
			h.frequencyRefuse(w, r, id, err)
			return true
		}
		h.frequencyDirty(id)
		frequencyToast(user.PIALID, "Your browser blocked the microphone. Allow it for this site, then Unmute.")
		w.WriteHeader(http.StatusNoContent)
	case "frequency.report":
		h.frequencyReport(w, r, user, id, raw)
	case "frequency.delete_replay":
		h.frequencyAnswer(w, r, user, id, func() (*auralis.Answer, error) {
			return h.auralis.DeleteReplay(r.Context(), user.PIALID, id)
		})
	case "frequency.pin":
		h.frequencyPin(w, r, user, id, raw)
	case "frequency.unpin":
		h.frequencyUnpin(w, r, user, id, raw)
	case "frequency.clip":
		h.frequencyClip(w, r, user, id, raw)
	case "frequency.clips_enabled":
		h.frequencyClipsEnabled(w, r, user, id, raw)
	case "frequency.remind":
		h.frequencyRemind(w, r, user, id, raw)
	case "frequency.react":
		h.frequencyReact(w, r, user, id, raw)
	case "frequency.step_down":
		h.frequencyAnswer(w, r, user, id, func() (*auralis.Answer, error) {
			return h.auralis.StepDown(r.Context(), user.PIALID, id)
		})
	default:
		http.Error(w, "unknown event_type: "+eventType, http.StatusNotFound)
	}
	return true
}

// frequencyMediaToken — POST /frequencies/{id}/media-token
// The audio connection's credential. It is minted for whoever is asking at this
// moment and is never rendered into a page.
func (h *Handler) frequencyMediaToken(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token, err := h.auralis.MediaToken(r.Context(), user.PIALID, r.PathValue("id"))
	switch {
	case auralis.IsOutage(err):
		log.Printf("[frequency] media token: %v", err)
		http.Error(w, "audio unavailable", http.StatusBadGateway)
		return
	case err != nil:
		http.Error(w, "not in this Frequency", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
}

// idemKey is the caller's idempotency key, when it sent one.
func idemKey(r *http.Request, raw map[string]json.RawMessage) string {
	if k := strings.TrimSpace(r.Header.Get("Idempotency-Key")); k != "" {
		return k
	}
	return eventField(r, raw, "idempotency_key")
}

// frequencyAnswer runs one Auralis mutation. The page learns the result from FA
// Live, the same way everyone else does; a native caller gets the room DTO.
func (h *Handler) frequencyAnswer(w http.ResponseWriter, r *http.Request, user *model.User, id string, do func() (*auralis.Answer, error)) {
	if id == "" {
		h.frequencyProblem(w, r, http.StatusBadRequest, "frequency_id_required", "frequency_id required")
		return
	}
	ans, err := do()
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	h.frequencyDirty(id)
	h.frequencyDone(w, r, user, ans)
}

// frequencyDone answers an accepted mutation. An empty 200 lets a form inside an
// overlay clear it; every other form on these surfaces declares hx-swap="none".
func (h *Handler) frequencyDone(w http.ResponseWriter, r *http.Request, user *model.User, ans *auralis.Answer) {
	if apiWantsJSON(r) {
		h.writeFrequencyRoomJSON(w, user, ans)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// frequencyMutated answers a mutation whose Auralis call returned no room.
func (h *Handler) frequencyMutated(w http.ResponseWriter, r *http.Request, user *model.User, id string) {
	if !apiWantsJSON(r) {
		w.WriteHeader(http.StatusOK)
		return
	}
	ans, err := h.auralis.Get(r.Context(), id, user.PIALID)
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	h.writeFrequencyRoomJSON(w, user, ans)
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
		if problem := frequencyScheduleProblem(t); problem != "" {
			return in, problem
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
		h.frequencyGoRefused(w, r, user, http.StatusBadRequest, draft.withError(problem))
		return
	}
	created, err := h.auralis.Create(r.Context(), user.PIALID, idemKey(r, raw), in)
	if err != nil {
		msg, code := frequencyErrorMessage(err)
		if code >= 500 {
			log.Printf("[frequency] go_live create: %v", err)
		}
		h.frequencyGoRefused(w, r, user, code, draft.withError(msg))
		return
	}
	id := created.Frequency.Frequency.ID
	// With a time, the room waits for it. Otherwise it starts now.
	started := created
	if in.ScheduledAt == nil {
		started, err = h.auralis.Start(r.Context(), user.PIALID, "", id)
		if err != nil {
			msg, code := frequencyErrorMessage(err)
			if code >= 500 {
				log.Printf("[frequency] go_live start %s: %v", id, err)
			}
			// Nothing started, so the draft made a moment ago must not linger.
			if _, cerr := h.auralis.Cancel(r.Context(), user.PIALID, id); cerr != nil {
				log.Printf("[frequency] go_live cancelling draft %s: %v", id, cerr)
			}
			h.frequencyGoRefused(w, r, user, code, draft.withError(msg))
			return
		}
	}
	h.frequencyDirty(id)
	if apiWantsJSON(r) {
		h.writeFrequencyRoomJSON(w, user, started)
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/frequencies/"+id, http.StatusSeeOther)
		return
	}
	// The composer targets the inside of #main-col. The stage replaces that
	// element itself, and the stage URL is pushed so the Shell lands where a
	// document load would have. The dock arrives over FA Live.
	stage := h.buildFrequencyStage(&started.Frequency, user.PIALID, user, nil)
	frag, err := h.renderFrequencyFragment("frequency_stage", stage)
	if err != nil {
		log.Printf("[frequency] %v", err)
		hxRedirect(w, "/frequencies/"+id)
		return
	}
	w.Header().Set("HX-Push-Url", "/frequencies/"+id)
	w.Header().Set("HX-Reswap", "outerHTML")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<main class="f33d3r-main freq-main freq-main--` + stage.State + `" id="main-col">` + frag + `</main>`))
}

// frequencyGoRefused re-renders the composer with the host's words in it. htmx
// does not swap an error status and the form pushes its URL, so a page request
// answers 200 and pushes nothing.
func (h *Handler) frequencyGoRefused(w http.ResponseWriter, r *http.Request, user *model.User, status int, draft frequencyGoDraft) {
	if r.Header.Get("HX-Request") == "true" {
		status = http.StatusOK
		w.Header().Set("HX-Push-Url", "false")
	}
	h.renderGoComposerWith(w, r, user, goComposerDraft{}, "", status, draft)
}

// frequencyCreate creates without starting (a draft or a scheduled one).
func (h *Handler) frequencyCreate(w http.ResponseWriter, r *http.Request, user *model.User, raw map[string]json.RawMessage, _ bool) {
	in, problem := frequencyCreateInput(r, user, raw)
	if problem != "" {
		h.frequencyProblem(w, r, http.StatusBadRequest, "invalid_frequency", problem)
		return
	}
	ans, err := h.auralis.Create(r.Context(), user.PIALID, idemKey(r, raw), in)
	if err != nil {
		h.frequencyRefuse(w, r, "", err)
		return
	}
	h.frequencyDirty(ans.Frequency.Frequency.ID)
	h.frequencyDone(w, r, user, ans)
}

// frequencyTuneIn admits the actor. Their stage and dock arrive over FA Live.
func (h *Handler) frequencyTuneIn(w http.ResponseWriter, r *http.Request, user *model.User, id string, raw map[string]json.RawMessage) {
	if id == "" {
		h.frequencyProblem(w, r, http.StatusBadRequest, "frequency_id_required", "frequency_id required")
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
		h.frequencyProblem(w, r, http.StatusForbidden, "adult_content_disabled", "This Frequency is for adult-enabled accounts.")
		return
	}
	facts := h.frequencyAudienceFacts(user, pre.Frequency.Frequency.HostPialID)
	ans, err := h.auralis.TuneIn(r.Context(), user.PIALID, idemKey(r, raw), id, facts)
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	h.frequencyDirty(id)
	h.frequencyDone(w, r, user, ans)
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

// frequencyHeartbeat is the presence beat. On the web it comes from the dock,
// swaps nothing, and renews the person's place in the room's audience. A beat
// that finds the person gone is how a page learns it after any missed update:
// the room re-renders for them and they are told.
func (h *Handler) frequencyHeartbeat(w http.ResponseWriter, r *http.Request, user *model.User, id string) {
	if id == "" {
		h.frequencyProblem(w, r, http.StatusBadRequest, "frequency_id_required", "frequency_id required")
		return
	}
	hb, err := h.auralis.Heartbeat(r.Context(), user.PIALID, id)
	if err != nil && auralis.Code(err) != "not_joined" {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	if apiWantsJSON(r) {
		if err != nil {
			h.frequencyRefuse(w, r, id, err)
			return
		}
		apiJSON(w, http.StatusOK, frequencyHeartbeatDTO(hb))
		return
	}
	if err == nil && hb.Present {
		AddPostWatcher(user.PIALID, frequencyRoomPrefix+id)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.frequencyDeparted(r.Context(), user, id)
	w.WriteHeader(http.StatusNoContent)
}

// frequencyDeparted brings one person's page in line after they were found to
// be out of a room their dock still shows.
func (h *Handler) frequencyDeparted(ctx context.Context, user *model.User, id string) {
	RemovePostWatcher(user.PIALID, frequencyRoomPrefix+id)
	PublishFacetToUser(user.PIALID, FacetMutation{FacetID: frequencyFacetID(id, freqSlotDock), Fragment: frequencyDockGone(id)})
	ans, err := h.auralis.Get(ctx, id, user.PIALID)
	if err != nil {
		log.Printf("[frequency] departed %s: read: %v", id, err)
		return
	}
	stage := h.buildFrequencyStage(&ans.Frequency, user.PIALID, user, nil)
	if frag, rerr := h.renderFrequencySlot(freqSlotStage, stage); rerr == nil {
		PublishFacetToUser(user.PIALID, FacetMutation{FacetID: frequencyFacetID(id, freqSlotStage), Fragment: frag})
	} else {
		log.Printf("[frequency] %v", rerr)
	}
	if !ans.Frequency.Frequency.IsOver() {
		frequencyToast(user.PIALID, "You were disconnected from this Frequency. Tune In to come back.")
	}
}

// frequencyRequestMic queues the actor's request with the reason they typed.
func (h *Handler) frequencyRequestMic(w http.ResponseWriter, r *http.Request, user *model.User, id string, raw map[string]json.RawMessage) {
	if id == "" {
		h.frequencyProblem(w, r, http.StatusBadRequest, "frequency_id_required", "frequency_id required")
		return
	}
	reason := truncate(strings.TrimSpace(eventField(r, raw, "reason")), 140)
	if _, err := h.auralis.RequestMic(r.Context(), user.PIALID, id, reason, frequencyVerityTier(user)); err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	h.frequencyDirty(id)
	h.frequencyMutated(w, r, user, id)
}

// frequencyParticipantAction is every host/co-host control aimed at a person.
// The target is a handle from the button; it is resolved to a PIAL here,
// because Auralis speaks PIAL and never handle.
func (h *Handler) frequencyParticipantAction(w http.ResponseWriter, r *http.Request, user *model.User, id, eventType string, raw map[string]json.RawMessage) {
	if id == "" {
		h.frequencyProblem(w, r, http.StatusBadRequest, "frequency_id_required", "frequency_id required")
		return
	}
	target, ok := h.frequencyTargetPIAL(w, r, raw)
	if !ok {
		return
	}
	reason := truncate(strings.TrimSpace(eventField(r, raw, "reason")), 140)
	ctx := r.Context()
	me := user.PIALID
	// An open people sheet refreshes at once instead of on its next tick.
	w.Header().Set("HX-Trigger", "fa:people-changed")
	h.frequencyAnswer(w, r, user, id, func() (*auralis.Answer, error) {
		switch eventType {
		case "frequency.mute":
			return h.auralis.Mute(ctx, me, id, target)
		case "frequency.unmute":
			return h.auralis.Unmute(ctx, me, id, target)
		case "frequency.demote":
			return h.auralis.Demote(ctx, me, id, target)
		case "frequency.remove":
			ans, err := h.auralis.Remove(ctx, me, id, target, reason)
			if err == nil {
				frequencyToast(target, "A host removed you from this Frequency.")
			}
			return ans, err
		case "frequency.block":
			ans, err := h.auralis.Block(ctx, me, id, target, reason)
			if err == nil {
				frequencyToast(target, "A host blocked you from this Frequency.")
			}
			return ans, err
		case "frequency.unblock":
			return h.auralis.Unblock(ctx, me, id, target)
		case "frequency.invite":
			ans, err := h.auralis.Invite(ctx, me, id, target)
			if err == nil {
				publishToast(me, "frequency-invite", "Invite sent.")
			}
			return ans, err
		case "frequency.cohost":
			return h.auralis.AddCohost(ctx, me, id, target)
		case "frequency.promote":
			ans, err := h.auralis.Promote(ctx, me, id, target)
			if err == nil {
				publishToast(target, "frequency-promote", "A host gave you the mic. Unmute when you are ready.")
			}
			return ans, err
		case "frequency.transfer":
			ans, err := h.auralis.TransferHost(ctx, me, id, target)
			if err == nil {
				publishToast(target, "frequency-transfer", "You are now the host of this Frequency.")
			}
			return ans, err
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
		h.frequencyProblem(w, r, http.StatusBadRequest, "target_handle_required", "target_handle required")
		return "", false
	}
	// The viewer naming themself (self mute/unmute posts their own handle):
	// no lookup, and it works before this brain's user rows are consulted.
	if me := liveActor(h.userFromRequest(w, r)); me != nil && (handle == "me" || handle == strings.ToLower(me.Handle)) {
		return me.PIALID, true
	}
	if h.db == nil {
		h.frequencyProblem(w, r, http.StatusServiceUnavailable, "db_unavailable", "database unavailable")
		return "", false
	}
	target, err := dbpkg.GetUserByHandle(h.db, handle)
	if err != nil || target == nil || target.PIALID == "" {
		h.frequencyProblem(w, r, http.StatusNotFound, "not_found", "No such person.")
		return "", false
	}
	return target.PIALID, true
}

// frequencyReport files a report on a Frequency or on one of its participants
// through the same funnel every other report takes.
func (h *Handler) frequencyReport(w http.ResponseWriter, r *http.Request, user *model.User, id string, raw map[string]json.RawMessage) {
	if id == "" {
		h.frequencyProblem(w, r, http.StatusBadRequest, "frequency_id_required", "frequency_id required")
		return
	}
	reason := eventField(r, raw, "reason")
	switch reason {
	case "spam", "harassment", "hate", "nsfw", "violence", "illegal", "other":
	default:
		h.frequencyProblem(w, r, http.StatusBadRequest, "reason_required", "reason required")
		return
	}
	detail := truncate(eventField(r, raw, "detail"), 500)
	contentID, contentType := id, "frequency"
	if handle := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(eventField(r, raw, "target_handle"))), "@"); handle != "" && h.db != nil {
		target, err := dbpkg.GetUserByHandle(h.db, handle)
		if err != nil || target == nil {
			h.frequencyProblem(w, r, http.StatusNotFound, "not_found", "No such person.")
			return
		}
		if target.ID == user.ID {
			h.frequencyProblem(w, r, http.StatusBadRequest, "report_self", "You cannot report yourself.")
			return
		}
		contentID, contentType = id+":"+target.ID, "frequency_participant"
	}
	if err := h.fileContentReport(user, contentID, contentType, reason, detail); err != nil {
		h.frequencyProblem(w, r, http.StatusInternalServerError, "server_error", "Could not file the report.")
		return
	}
	// The report sheet swaps this answer into the overlay, which closes it.
	if r.Header.Get("HX-Request") == "true" {
		publishToast(user.PIALID, "frequency-report", "Report sent. Thanks for telling us.")
		w.WriteHeader(http.StatusOK)
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
