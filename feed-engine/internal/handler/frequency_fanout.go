package handler

import (
	"context"
	"fmt"
	"hash/fnv"
	"html"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/f33d3r/feed-engine/internal/auralis"
)

// Frequencies: the one path a change takes to the people looking at it.
//
//	mutation in Auralis → frequencyDirty(id) → read the room once → render →
//	publish only the Facets whose rendered output changed
//
// A mutation marks its room dirty. That happens from the Sitra Achra consumer
// for every committed event, and from the HTTP handler so the person who acted
// does not wait on the event drain. Both only mark the room; one worker per room
// does the work, reading after the mutation committed. Every commit is therefore
// followed by a read that includes it, so whatever arrives last on a screen is
// current, without the browser comparing versions.
//
// Audiences:
//
//	watchers    people with an open stage who are not in the room, by data-watch-id
//	room        everyone in the room, registered by this server and by their beat
//	moderators  host and co-hosts, for the surfaces that carry moderation actions
//	one person  their controls, and the whole stage when their standing changes
//
// A shared Fragment names nobody, so a listener joining costs one tally, not one
// render per person. Facets that differ by audience carry data-surface, so a
// fragment for one audience never lands on another audience's copy.

const (
	frequencyCoalesce    = 150 * time.Millisecond
	frequencyReadTimeout = 5 * time.Second
	frequencyReadRetries = 3
	frequencyRetryDelay  = time.Second

	frequencyWatchPrefix = "frequency:"
	frequencyRoomPrefix  = "frequency-room:"

	freqSlotDockSpeakers = "dock_speakers"

	freqSurfaceAudience  = "audience"
	freqSurfaceHost      = "host"
	freqSurfaceCohost    = "cohost"
	freqSurfaceModerator = "moderator"
	freqSurfaceOutsider  = "outsider"
	freqSurfaceStage     = "stage"
	freqSurfaceDock      = "dock"

	frequencyDockMountID = "facet:f33d3r:shell:global:frequency_dock"
	frequencyStripLiveID = "facet:f33d3r:frequency:strip:live"
)

// frequencyFanout holds one entry per room with a mutation in flight or anyone
// in it. Its contents only avoid repeating output; losing them re-sends once.
type frequencyFanout struct {
	mu    sync.Mutex
	rooms map[string]*frequencyRoom
}

type frequencyRoom struct {
	mu      sync.Mutex
	dirty   bool
	running bool

	// pass serialises rendering and guards the two maps below.
	pass sync.Mutex
	// sent is the hash of the last output per audience and slot.
	sent map[string]uint64
	// standing is the last role each person in the room was rendered with.
	standing map[string]string

	reactMu    sync.Mutex
	reactSince time.Time
	reactCount int
}

func (f *frequencyFanout) room(id string) *frequencyRoom {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rooms == nil {
		f.rooms = map[string]*frequencyRoom{}
	}
	r := f.rooms[id]
	if r == nil {
		r = &frequencyRoom{sent: map[string]uint64{}, standing: map[string]string{}}
		f.rooms[id] = r
	}
	return r
}

func (f *frequencyFanout) forget(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rooms, id)
}

// frequencyDirty says a room changed. It never blocks its caller.
func (h *Handler) frequencyDirty(id string) {
	if id == "" || h.auralis == nil || !h.auralis.Configured() {
		return
	}
	room := h.freqFanout.room(id)
	room.mu.Lock()
	defer room.mu.Unlock()
	room.dirty = true
	if room.running {
		return
	}
	room.running = true
	go h.frequencyWorker(id, room)
}

func (h *Handler) frequencyWorker(id string, room *frequencyRoom) {
	for {
		time.Sleep(frequencyCoalesce)
		room.mu.Lock()
		room.dirty = false
		room.mu.Unlock()

		h.frequencyReadAndPublish(id)

		room.mu.Lock()
		if !room.dirty {
			room.running = false
			room.mu.Unlock()
			return
		}
		room.mu.Unlock()
	}
}

// frequencyReadAndPublish retries a failed read, because a change nobody
// re-read would otherwise stay off screens until the room next changes.
func (h *Handler) frequencyReadAndPublish(id string) {
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), frequencyReadTimeout)
		ans, err := h.auralis.Get(ctx, id, "")
		cancel()
		if err == nil {
			h.publishFrequencyView(&ans.Frequency)
			return
		}
		if auralis.IsNotFound(err) {
			h.freqFanout.forget(id)
			return
		}
		log.Printf("[frequency] publish %s: read %d of %d: %v", id, attempt, frequencyReadRetries, err)
		if attempt == frequencyReadRetries {
			return
		}
		time.Sleep(frequencyRetryDelay)
	}
}

// PublishFrequencyState reads a room and publishes it before returning.
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

// FrequencyChanged is what the Sitra Achra consumer calls for each event.
func (h *Handler) FrequencyChanged(frequencyID string) {
	h.frequencyDirty(frequencyID)
}

func fragmentHash(frag string) uint64 {
	sum := fnv.New64a()
	_, _ = sum.Write([]byte(frag))
	return sum.Sum64()
}

// changed records output for a key and reports whether it differs from the last.
func (r *frequencyRoom) changed(key, frag string) bool {
	next := fragmentHash(frag)
	if last, ok := r.sent[key]; ok && last == next {
		return false
	}
	r.sent[key] = next
	return true
}

// frequencyDockGone takes one room's dock off a person's Shell.
func frequencyDockGone(id string) string {
	return `<div hidden data-facet-id="` + frequencyFacetID(id, freqSlotDock) + `"></div>`
}

// publishFrequencyView renders one read of a room and sends what changed.
func (h *Handler) publishFrequencyView(v *auralis.View) {
	s := &v.Frequency
	room := h.freqFanout.room(s.ID)
	room.pass.Lock()
	defer room.pass.Unlock()

	people := h.frequencyPeople(frequencyPIALs(v, ""))
	watchKey, roomKey := frequencyWatchPrefix+s.ID, frequencyRoomPrefix+s.ID
	toAudience := func(m FacetMutation) {
		PublishFacetToPostWatchers(watchKey, m)
		PublishFacetToPostWatchers(roomKey, m)
	}
	render := func(slot string, st FrequencyStageData) string {
		frag, err := h.renderFrequencySlot(slot, st)
		if err != nil {
			log.Printf("[frequency] %v", err)
			return ""
		}
		return frag
	}

	defer h.publishFrequencyCard(room, v, people, toAudience)

	if s.IsOver() {
		h.publishFrequencyOver(room, v, people, toAudience, roomKey, render)
		return
	}

	nobody := h.buildFrequencyStage(v, "", nil, people)
	shared := func(slot string, priority FacetPriority) {
		frag := render(slot, nobody)
		if frag != "" && room.changed("shared:"+slot, frag) {
			toAudience(FacetMutation{FacetID: frequencyFacetID(s.ID, slot), Fragment: frag, Priority: priority})
		}
	}
	shared(freqSlotListenerCount, FacetPriorityLow)
	shared(freqSlotLiveBadge, FacetPriorityNormal)
	shared(freqSlotRecording, FacetPriorityNormal)
	shared(freqSlotStatus, FacetPriorityNormal)
	shared(freqSlotDockSpeakers, FacetPriorityNormal)
	shared(freqSlotSpeakers, FacetPriorityNormal)
	shared(freqSlotRequests, FacetPriorityNormal)
	shared(freqSlotControls, FacetPriorityNormal)
	shared(freqSlotPins, FacetPriorityNormal)

	// Host and co-hosts: their copies of the grid and queue carry actions.
	var cohost FrequencyStageData
	if len(v.CohostPialIDs) > 0 {
		cohost = h.buildFrequencyStage(v, v.CohostPialIDs[0], nil, people)
	}
	host := h.buildFrequencyStage(v, s.HostPialID, nil, people)
	moderated := func(slot, key string, st FrequencyStageData, to []string) {
		frag := render(slot, st)
		if frag == "" || !room.changed(key, frag) {
			return
		}
		for _, pial := range to {
			PublishFacetToUser(pial, FacetMutation{FacetID: frequencyFacetID(s.ID, slot), Fragment: frag})
		}
	}
	moderated(freqSlotSpeakers, "host:speakers", host, []string{s.HostPialID})
	moderated(freqSlotRequests, "host:requests", host, []string{s.HostPialID})
	moderated(freqSlotPins, "host:pins", host, []string{s.HostPialID})
	if len(v.CohostPialIDs) > 0 {
		moderated(freqSlotSpeakers, "cohost:speakers", cohost, v.CohostPialIDs)
		moderated(freqSlotRequests, "cohost:requests", cohost, v.CohostPialIDs)
		moderated(freqSlotPins, "cohost:pins", cohost, v.CohostPialIDs)
	}

	// Each person: their own controls, rendered once per distinct standing.
	here := map[string]bool{s.HostPialID: true}
	for _, p := range v.PresentPialIDs {
		here[p] = true
	}
	type rendered struct {
		st           FrequencyStageData
		stage        string
		dock         string
		stageControl string
		dockControl  string
	}
	byStanding := map[string]*rendered{}
	for pial := range here {
		standing := frequencyStanding(v, pial)
		class := frequencyStandingClass(standing)
		out := byStanding[class]
		if out == nil {
			st := h.buildFrequencyStage(v, pial, nil, people)
			out = &rendered{st: st, stage: render(freqSlotStage, st), stageControl: render(freqSlotControls, st)}
			if st.State == "live" && st.Viewer.InFrequency {
				out.dock = render(freqSlotDock, st)
				out.dockControl = h.renderFrequencyDockControls(st)
			}
			byStanding[class] = out
		}

		// A room going live changes everything on a person's screen, including
		// the dock that carries its audio, even when their role stays the same.
		transition := standing.Role + "|" + s.State
		last, seen := room.standing[pial]
		if !seen {
			AddPostWatcher(pial, roomKey)
		}
		if !seen || last != transition {
			room.standing[pial] = transition
			if out.stage != "" {
				PublishFacetToUser(pial, FacetMutation{FacetID: frequencyFacetID(s.ID, freqSlotStage), Fragment: out.stage})
			}
			if out.dock != "" {
				PublishFacetToUser(pial, FacetMutation{
					FacetID: frequencyFacetID(s.ID, freqSlotDock), Fragment: out.dock,
					Mode: FacetPrepend, ParentFacetID: frequencyDockMountID,
				})
			}
		} else {
			if out.stageControl != "" && room.changed(pial+":controls", out.stageControl) {
				PublishFacetToUser(pial, FacetMutation{FacetID: frequencyFacetID(s.ID, freqSlotControls), Fragment: out.stageControl})
			}
			if out.dockControl != "" && room.changed(pial+":dock_controls", out.dockControl) {
				PublishFacetToUser(pial, FacetMutation{FacetID: frequencyFacetID(s.ID, freqSlotControls), Fragment: out.dockControl})
			}
		}
		h.publishFrequencyJSON(v, pial, people)
	}

	// Anyone gone since the last read: back to a stranger's stage, dock removed.
	for pial := range room.standing {
		if here[pial] {
			continue
		}
		delete(room.standing, pial)
		delete(room.sent, pial+":controls")
		delete(room.sent, pial+":dock_controls")
		RemovePostWatcher(pial, roomKey)
		st := h.buildFrequencyStage(v, pial, nil, people)
		if frag := render(freqSlotStage, st); frag != "" {
			PublishFacetToUser(pial, FacetMutation{FacetID: frequencyFacetID(s.ID, freqSlotStage), Fragment: frag})
		}
		PublishFacetToUser(pial, FacetMutation{FacetID: frequencyFacetID(s.ID, freqSlotDock), Fragment: frequencyDockGone(s.ID)})
		h.publishFrequencyJSON(v, pial, people)
	}
}

// publishFrequencyOver tells everyone the room reached. A finished room has
// nobody in it by the time it is read, so this uses who was last seen, and the
// room key for anyone this replica has not seen.
func (h *Handler) publishFrequencyOver(room *frequencyRoom, v *auralis.View, people map[string]FrequencyPerson, toAudience func(FacetMutation), roomKey string, render func(string, FrequencyStageData) string) {
	s := &v.Frequency
	dockGone := FacetMutation{FacetID: frequencyFacetID(s.ID, freqSlotDock), Fragment: frequencyDockGone(s.ID)}
	var stage FacetMutation
	if frag := render(freqSlotStage, h.buildFrequencyStage(v, "", nil, people)); frag != "" {
		stage = FacetMutation{FacetID: frequencyFacetID(s.ID, freqSlotStage), Fragment: frag, Priority: FacetPriorityHigh}
		toAudience(stage)
	}
	toAudience(dockGone)

	known := map[string]bool{s.HostPialID: true}
	for pial := range room.standing {
		known[pial] = true
	}
	for _, p := range v.PresentPialIDs {
		known[p] = true
	}
	for pial := range known {
		if stage.Fragment != "" {
			PublishFacetToUser(pial, stage)
		}
		PublishFacetToUser(pial, dockGone)
		RemovePostWatcher(pial, roomKey)
		h.publishFrequencyJSON(v, pial, people)
	}
	h.freqFanout.forget(s.ID)
}

// publishFrequencyCard follows the room on every surface that shows its card.
// Only a public room that is safe for everyone goes to every session; any other
// room's card reaches only those already admitted to look at it.
func (h *Handler) publishFrequencyCard(room *frequencyRoom, v *auralis.View, people map[string]FrequencyPerson, toAudience func(FacetMutation)) {
	s := &v.Frequency
	card := h.buildFrequencyCard(s, v.Counts, people[s.HostPialID], nil)
	frag, err := h.renderFrequencyFragment("frequency_card", card)
	if err != nil {
		log.Printf("[frequency] %v", err)
		return
	}
	if !room.changed("card", frag) {
		return
	}
	m := FacetMutation{FacetID: frequencyFacetID(s.ID, freqSlotCard), Fragment: frag, Priority: FacetPriorityLow}
	if s.Visibility != "public" || s.AdultContent {
		toAudience(m)
		return
	}
	if s.IsLive() {
		m.Mode = FacetPrepend
		m.ParentFacetID = frequencyStripLiveID
	}
	PublishFacetToAll(m)
}

// frequencyStandingClass groups people who see identical personal controls.
func frequencyStandingClass(v FrequencyViewer) string {
	requestID := ""
	if v.Request != nil {
		requestID = v.Request.RequestID
	}
	return fmt.Sprintf("%s|%t|%t|%t|%t|%t|%t|%t|%s", v.Role, v.InFrequency, v.Muted, v.Blocked,
		v.CanRequestMic, v.CanModerate, v.CanEnd, v.CanSpeak, requestID)
}

// frequencyGoneToast tells one person something over FA Live.
func frequencyToast(pial, message string) {
	if pial == "" {
		return
	}
	facetID := "facet:f33d3r:toast:" + html.EscapeString(pial) + ":" + uuid.NewString()
	PublishFacetToUser(pial, FacetMutation{
		FacetID: facetID,
		Fragment: `<div class="toast toast-show toast--error" role="alert" data-expires-ms="6000" data-facet-id="` +
			facetID + `">` + html.EscapeString(message) + `</div>`,
		Mode:          FacetPrepend,
		ParentFacetID: toastsFacetID,
		Priority:      FacetPriorityHigh,
	})
}
