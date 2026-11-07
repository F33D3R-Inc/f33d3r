package handler

import (
	"encoding/json"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/f33d3r/feed-engine/internal/model"
)

// FrequencyReaction is one of the fixed reactions a person in a room can send.
type FrequencyReaction struct {
	Key   string
	Glyph string
	Label string
}

var frequencyReactionSet = []FrequencyReaction{
	{"clap", "👏", "Clap"},
	{"heart", "❤️", "Heart"},
	{"laugh", "😂", "Laugh"},
	{"hundred", "💯", "Agree"},
	{"fire", "🔥", "Fire"},
	{"wave", "👋", "Wave"},
}

func frequencyReactions() []FrequencyReaction { return frequencyReactionSet }

const (
	frequencyReactionLanes = 5
	// Each room shows at most this many reactions a second. Past that a big room
	// already looks full, so the rest are dropped instead of sent to everyone.
	frequencyReactionsPerSecond = 8
)

// allowReaction spends one of the room's reactions for the current second.
func (r *frequencyRoom) allowReaction(now time.Time) bool {
	r.reactMu.Lock()
	defer r.reactMu.Unlock()
	if now.Sub(r.reactSince) >= time.Second {
		r.reactSince, r.reactCount = now, 0
	}
	if r.reactCount >= frequencyReactionsPerSecond {
		return false
	}
	r.reactCount++
	return true
}

// frequencyReact shows a reaction to everyone looking at the room. Auralis checks
// the person is in the room and within their limit; a reaction is not stored.
func (h *Handler) frequencyReact(w http.ResponseWriter, r *http.Request, user *model.User, id string, raw map[string]json.RawMessage) {
	key := eventField(r, raw, "reaction")
	glyph := ""
	for _, reaction := range frequencyReactionSet {
		if reaction.Key == key {
			glyph = reaction.Glyph
		}
	}
	if id == "" || glyph == "" {
		h.frequencyProblem(w, r, http.StatusBadRequest, "reaction_required", "Choose a reaction.")
		return
	}
	if err := h.auralis.React(r.Context(), user.PIALID, id); err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	if h.freqFanout.room(id).allowReaction(time.Now()) {
		h.publishFrequencyReaction(id, glyph)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) publishFrequencyReaction(id, glyph string) {
	facetID := frequencyFacetID(id, "reaction_"+strings.ReplaceAll(uuid.NewString(), "-", ""))
	frag, err := h.renderFrequencyFragment("frequency_reaction", map[string]interface{}{
		"FacetID": facetID,
		"Glyph":   glyph,
		"Lane":    rand.Intn(frequencyReactionLanes),
	})
	if err != nil {
		log.Printf("[frequency] %v", err)
		return
	}
	m := FacetMutation{
		FacetID:       facetID,
		Fragment:      frag,
		Priority:      FacetPriorityLow,
		Mode:          FacetAppend,
		ParentFacetID: frequencyFacetID(id, "reactions"),
	}
	PublishFacetToPostWatchers(frequencyWatchPrefix+id, m)
	PublishFacetToPostWatchers(frequencyRoomPrefix+id, m)
}

// facetFrequencyPeople — GET /facets/frequency/{id}/people
// The sheet of who is listening. Moderators also get their actions and the blocked
// list. It refreshes itself while open, so a room nobody is inspecting costs nothing.
func (h *Handler) facetFrequencyPeople(w http.ResponseWriter, r *http.Request) {
	if !h.frequencyReady(w, r) {
		return
	}
	id := r.PathValue("id")
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ans, err := h.auralis.Get(r.Context(), id, user.PIALID)
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	if code, msg, refused := h.frequencyGate(user, &ans.Frequency); refused {
		h.frequencyProblem(w, r, http.StatusForbidden, code, msg)
		return
	}
	list, err := h.auralis.People(r.Context(), user.PIALID, id)
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	people := h.frequencyPeople(append(append([]string{}, list.Listeners...), list.Blocked...))
	named := func(pials []string) []FrequencyPerson {
		out := make([]FrequencyPerson, 0, len(pials))
		for _, p := range pials {
			out = append(out, people[p])
		}
		return out
	}
	name := "frequency_people"
	if r.URL.Query().Get("part") == "list" {
		name = "frequency_people_list"
	}
	frag, err := h.renderFrequencyFragment(name, map[string]interface{}{
		"ID":               id,
		"Total":            list.Total,
		"More":             list.Total - len(list.Listeners),
		"Listeners":        named(list.Listeners),
		"Blocked":          named(list.Blocked),
		"CanModerate":      list.CanModerate,
		"RequestsPossible": ans.Frequency.Frequency.IsLive(),
	})
	if err != nil {
		log.Printf("[frequency] %v", err)
		http.Error(w, "fragment unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// facetFrequencyReport — GET /facets/frequency/{id}/report
// The report sheet, for the room or for one person in it.
func (h *Handler) facetFrequencyReport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		http.NotFound(w, r)
		return
	}
	frag, err := h.renderFrequencyFragment("frequency_report", map[string]interface{}{
		"ID":     id,
		"Handle": strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("handle")), "@"),
	})
	if err != nil {
		log.Printf("[frequency] %v", err)
		http.Error(w, "fragment unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

type adminFrequencyRow struct {
	ID         string
	Title      string
	HostHandle string
	People     int
}

// adminFrequencyRemoveReplay — POST /admin/frequencies/{id}/remove_replay
// Takes a replay down for good, whatever its host wants.
func (h *Handler) adminFrequencyRemoveReplay(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	if _, err := h.auralis.RemoveReplay(r.Context(), id); err != nil {
		log.Printf("[frequency] remove replay %s by %s: %v", id, user.Handle, err)
		publishToast(user.PIALID, "frequency-remove-replay", "Could not remove that replay. Try again.")
	} else {
		log.Printf("[frequency] replay %s removed by %s", id, user.Handle)
		h.frequencyDirty(id)
	}
	h.renderAdminFrequencies(w, r)
}

// adminPanelFrequencies — GET /admin/partials/frequencies
func (h *Handler) adminPanelFrequencies(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	h.renderAdminFrequencies(w, r)
}

func (h *Handler) renderAdminFrequencies(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{"Rooms": []adminFrequencyRow{}, "Unavailable": false}
	list, err := h.auralis.List(r.Context(), "live", "", 100)
	if err != nil {
		log.Printf("[frequency] admin list: %v", err)
		data["Unavailable"] = true
	} else {
		rows := make([]adminFrequencyRow, 0, len(list.Items))
		for _, item := range list.Items {
			host := h.frequencyPeople([]string{item.Frequency.HostPialID})[item.Frequency.HostPialID]
			rows = append(rows, adminFrequencyRow{
				ID:         item.Frequency.ID,
				Title:      item.Frequency.Title,
				HostHandle: host.Handle,
				People:     item.Counts.Participants,
			})
		}
		data["Rooms"] = rows
	}
	replays := []adminFrequencyRow{}
	if ended, err := h.auralis.List(r.Context(), "ended", "", 100); err != nil {
		log.Printf("[frequency] admin replays: %v", err)
	} else {
		for _, item := range ended.Items {
			if item.Frequency.ReplayStatus != "ready" {
				continue
			}
			host := h.frequencyPeople([]string{item.Frequency.HostPialID})[item.Frequency.HostPialID]
			replays = append(replays, adminFrequencyRow{ID: item.Frequency.ID, Title: item.Frequency.Title, HostHandle: host.Handle})
		}
	}
	data["Replays"] = replays
	frag, err := h.renderFrequencyFragment("admin_panel_frequencies", data)
	if err != nil {
		log.Printf("[frequency] %v", err)
		http.Error(w, "fragment unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// adminFrequencyTerminate — POST /admin/frequencies/{id}/terminate
// Ends a room on policy grounds. Everyone in it is told through the ended stage,
// and the reason goes on the Frequency's event record.
func (h *Handler) adminFrequencyTerminate(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	reason := truncate(strings.TrimSpace(r.FormValue("reason")), 140)
	if reason == "" {
		http.Error(w, "reason required", http.StatusBadRequest)
		return
	}
	if _, err := h.auralis.Terminate(r.Context(), id, reason); err != nil {
		log.Printf("[frequency] terminate %s by %s: %v", id, user.Handle, err)
		publishToast(user.PIALID, "frequency-terminate", "Could not end that Frequency. Try again.")
	} else {
		log.Printf("[frequency] terminated %s by %s: %s", id, user.Handle, reason)
		h.frequencyDirty(id)
	}
	h.renderAdminFrequencies(w, r)
}
