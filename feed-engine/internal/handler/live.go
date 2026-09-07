package handler

// live.go — the live viewing and broadcast surfaces.
//
// Status, viewer tally and chat are server state, delivered as rendered
// Fragments over the existing FA Live registry. No ingest key is ever rendered.

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/live"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── presence ─────────────────────────────────────────────────────────────────

// liveViewerTTL drops a viewer after ~2 missed 20 s heartbeats.
const liveViewerTTL = 50 * time.Second

// liveSweepInterval prunes silent viewers even when no heartbeat arrives.
const liveSweepInterval = 15 * time.Second

// liveChatRingSize is the whole chat store for a stream; dropped when it ends.
const liveChatRingSize = 120

// liveViewer is one watching connection. pial is empty for a logged-out viewer,
// who is counted but gets tally updates through the heartbeat response, not SSE.
type liveViewer struct {
	pial     string
	lastSeen time.Time
}

// liveChatMessage is one chat line. Server-owned; the only shape of a line.
type liveChatMessage struct {
	ID            string
	StreamID      string
	Handle        string
	Name          string
	AvatarURL     string
	Body          string
	CreatedAt     time.Time
	TimeLabel     string
	IsBroadcaster bool
	// Kind is "chat" or "tip"; AmountUAET is the tip's size, 0 for chat.
	Kind       string
	AmountUAET int64
}

// liveState is the in-process authority for who is watching and what was said.
// Deliberately not persisted: presence and live chat end with the broadcast.
type liveState struct {
	mu       sync.Mutex
	viewers  map[string]map[string]liveViewer // streamID → viewerKey → viewer
	chat     map[string][]liveChatMessage     // streamID → ring
	tally    map[string]int                   // streamID → last published tally
	source   map[string]string                // streamID → chosen publish source
	shape    map[string]string                // streamID → last pushed player shape
	sweeping bool
}

var liveRuntime = &liveState{
	viewers: make(map[string]map[string]liveViewer),
	chat:    make(map[string][]liveChatMessage),
	tally:   make(map[string]int),
	source:  make(map[string]string),
	shape:   make(map[string]string),
}

// playerShape names which of the player's branches a stream renders into:
// its status, and whether it is blocked. Two streams with the same shape render
// the same player root; a render that leaves the shape alone has nothing to
// say to that root.
func playerShape(s *model.LiveStream) string {
	if s.IsBlocked {
		return "blocked"
	}
	return s.Status
}

// shapeMoved records the player shape about to be pushed and reports whether
// it differs from the one last pushed. The first render of a stream always
// counts as moved.
//
// This is what keeps a working decoder alive under a viewer. The player root
// holds the <video> and its decoder; replacing it restarts the decode from
// nothing. Every state render used to replace it — including a backpressure
// report that changed only the ladder — so a broadcast under load had its
// viewers re-buffering from zero on every report. Only a change of shape is
// allowed to touch the root now.
func (ls *liveState) shapeMoved(streamID, shape string) bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	prev, seen := ls.shape[streamID]
	if seen && prev == shape {
		return false
	}
	ls.shape[streamID] = shape
	return true
}

// setPublishSource records which way the broadcaster chose to publish. It is the
// server's memory of a decision the browser must not be trusted to re-assert:
// without it, reopening /golive for an OBS broadcast would land on the camera
// surface and start a second publisher into the same stream.
func (ls *liveState) setPublishSource(streamID, source string) {
	if streamID == "" || source == "" {
		return
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.source[streamID] = source
}

// publishSource returns the recorded choice, or "" when none was recorded.
func (ls *liveState) publishSource(streamID string) string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.source[streamID]
}

// tallyMoved records a new tally and reports whether it changed — without it
// every viewer's beat would rewrite the row and refan an identical Fragment.
func (ls *liveState) tallyMoved(streamID string, count int) bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	prev, seen := ls.tally[streamID]
	if seen && prev == count {
		return false
	}
	ls.tally[streamID] = count
	return true
}

// touch records a viewer as present and returns the stream's current count.
func (ls *liveState) touch(streamID, viewerKey, pial string) int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	set := ls.viewers[streamID]
	if set == nil {
		set = make(map[string]liveViewer)
		ls.viewers[streamID] = set
	}
	set[viewerKey] = liveViewer{pial: pial, lastSeen: time.Now()}
	ls.pruneLocked(streamID)
	return len(ls.viewers[streamID])
}

func (ls *liveState) count(streamID string) int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.pruneLocked(streamID)
	return len(ls.viewers[streamID])
}

// sweep prunes silent viewers and reports whether that changed the count.
func (ls *liveState) sweep(streamID string) (count int, changed bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	before := len(ls.viewers[streamID])
	ls.pruneLocked(streamID)
	after := len(ls.viewers[streamID])
	return after, before != after
}

// pruneLocked drops viewers whose last heartbeat is older than the TTL.
func (ls *liveState) pruneLocked(streamID string) {
	set := ls.viewers[streamID]
	if set == nil {
		return
	}
	cutoff := time.Now().Add(-liveViewerTTL)
	for k, v := range set {
		if v.lastSeen.Before(cutoff) {
			delete(set, k)
		}
	}
	if len(set) == 0 {
		delete(ls.viewers, streamID)
	}
}

// audiencePIALs is the set of signed-in people watching a stream — the fanout
// list for that stream's Fragments.
func (ls *liveState) audiencePIALs(streamID string) []string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	set := ls.viewers[streamID]
	out := make([]string, 0, len(set))
	seen := make(map[string]bool, len(set))
	for _, v := range set {
		if v.pial == "" || seen[v.pial] {
			continue
		}
		seen[v.pial] = true
		out = append(out, v.pial)
	}
	return out
}

// trackedStreams lists every stream with live presence or chat.
func (ls *liveState) trackedStreams() []string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	out := make([]string, 0, len(ls.viewers))
	for id := range ls.viewers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (ls *liveState) appendChat(m liveChatMessage) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ring := append(ls.chat[m.StreamID], m)
	if len(ring) > liveChatRingSize {
		ring = ring[len(ring)-liveChatRingSize:]
	}
	ls.chat[m.StreamID] = ring
}

func (ls *liveState) chatBacklog(streamID string) []liveChatMessage {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	src := ls.chat[streamID]
	out := make([]liveChatMessage, len(src))
	copy(out, src)
	return out
}

// forget releases everything held for a finished stream.
func (ls *liveState) forget(streamID string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	delete(ls.viewers, streamID)
	delete(ls.chat, streamID)
	delete(ls.tally, streamID)
	delete(ls.source, streamID)
	delete(ls.shape, streamID)
}

// ── formatting ───────────────────────────────────────────────────────────────

// livePresenceBeats is the one rule for whether a surface showing this stream
// keeps asking to be counted.
//
// Presence is a property of a running broadcast: the server only records and
// only persists a viewer while status is live. A surface must therefore be
// handed the beat only while that is true, and the moment it stops being true
// the next Fragment the server renders carries no beat — which is how the beat
// ends. The browser is never told to remember anything; it is handed an element
// that either asks for the next beat or does not.
//
// want is what the calling surface would like: the watch page asks for presence,
// a feed card and the broadcaster's own stage never do.
func livePresenceBeats(s *model.LiveStream, want bool) bool {
	return want && s != nil && s.Status == model.LiveStatusLive
}

// fmtViewerCount formats a tally server-side so no surface formats it itself.
func fmtViewerCount(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 10000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "K"
	case n < 1000000:
		return fmt.Sprintf("%dK", n/1000)
	default:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000000), ".0") + "M"
	}
}

// fmtLiveElapsed renders how long a stream has been running.
func fmtLiveElapsed(s *model.LiveStream) string {
	if s.Status == model.LiveStatusEnded {
		return "ended"
	}
	if s.StartedAt == nil {
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

// liveVerifiedType picks which verified mark an author carries.
func liveVerifiedType(u *model.User) string {
	if u != nil && (u.IsFoundingCreator || u.OfficialType != "") {
		return "gold"
	}
	return "blue"
}

// liveQualityLadder is the selector's rungs, highest first.
//
// The ladder actually being produced is preferred over the one the source could
// support, and under load those are different: the capacity governor sheds
// rungs from the top rather than let a new broadcast damage the ones already
// running, and it records what it settled on. Offering a rung the platform
// decided not to encode is offering a selection that resolves to nothing at the
// edge — the server knows better and should say so.
//
// Before the first plan is issued there is no recorded ladder, and the full one
// derived from the source geometry is the right thing to offer until there is.
func liveQualityLadder(s *model.LiveStream) []int {
	if produced := live.RungHeightsFromSummary(s.Ladder); len(produced) > 0 {
		out := make([]int, 0, len(produced))
		for i := len(produced) - 1; i >= 0; i-- {
			out = append(out, produced[i])
		}
		return out
	}
	rungs := live.LadderFor(s.SourceHeight)
	if len(rungs) == 0 {
		return []int{2160, 1440, 1080, 720, 480}
	}
	out := make([]int, 0, len(rungs))
	for i := len(rungs) - 1; i >= 0; i-- {
		out = append(out, rungs[i].Height)
	}
	return out
}

// ── view assembly ────────────────────────────────────────────────────────────

// liveStreamView is everything the live Facets read — built once per render.
type liveStreamView struct {
	S            *model.LiveStream
	AuthorHandle string
	AuthorName   string
	AvatarURL    string
	AuthorPIAL   string
	AuthorRealm  int
	IsVerified   bool
	VerifiedType string
	Description  string
	Viewers      string
	ViewerCount  int
	TimeLabel    string
	Ladder       []int
	Heartbeat    bool
	ViewerToken  string
	IsOwner      bool
	IsAnon       bool
	IsFollowing  bool
	CanChat      bool
	ViewerHandle string
	Ctx          map[string]interface{}
}

// buildLiveView resolves a stream into the shape the Facets consume.
func (h *Handler) buildLiveView(s *model.LiveStream, viewer *model.User, heartbeat bool) *liveStreamView {
	author, err := dbpkg.GetUserByID(h.db, s.AuthorID)
	if err != nil {
		log.Printf("[live] author %s for stream %s: %v", s.AuthorID, s.ID, err)
	}

	v := &liveStreamView{
		S:            s,
		AuthorHandle: s.AuthorHandle,
		AuthorName:   s.AuthorName,
		AvatarURL:    s.AvatarURL,
		AuthorPIAL:   s.AuthorPIAL,
		// The broadcaster's own words. live_author_row has always rendered this;
		// nothing had ever filled it, so every stream read as untitled prose.
		Description:  s.Description,
		VerifiedType: "blue",
		ViewerCount:  liveRuntime.count(s.ID),
		TimeLabel:    fmtLiveElapsed(s),
		Ladder:       liveQualityLadder(s),
		Heartbeat:    livePresenceBeats(s, heartbeat),
	}
	if author != nil {
		if v.AuthorHandle == "" {
			v.AuthorHandle = author.Handle
		}
		if v.AuthorName == "" {
			v.AuthorName = author.DisplayName
		}
		if v.AvatarURL == "" {
			v.AvatarURL = author.AvatarURL
		}
		v.AuthorRealm = author.Realm
		v.IsVerified = author.IsVerified
		v.VerifiedType = liveVerifiedType(author)
	}
	if v.AuthorName == "" {
		v.AuthorName = v.AuthorHandle
	}
	v.Viewers = fmtViewerCount(v.ViewerCount)

	v.IsAnon = viewer == nil || viewer.ID == "" || viewer.ID == "demo_user"
	if !v.IsAnon {
		v.ViewerHandle = viewer.Handle
		v.IsOwner = viewer.ID == s.AuthorID
		v.CanChat = s.Status != model.LiveStatusEnded && !s.IsBlocked
		if !v.IsOwner && author != nil {
			following, ferr := dbpkg.IsFollowing(h.db, viewer.ID, author.ID)
			if ferr != nil {
				log.Printf("[live] follow state %s→%s: %v", viewer.ID, author.ID, ferr)
			}
			v.IsFollowing = following
		}
	}
	v.Ctx = h.workCardCtx(viewer, "live")
	return v
}

// liveActor returns the signed-in account, or nil. DemoUser can watch, no more.
func liveActor(u *model.User) *model.User {
	if u == nil || u.ID == "" || u.ID == "demo_user" {
		return nil
	}
	return u
}

// liveViewerKey identifies one watching connection: PIAL when signed in (two
// devices are one viewer), otherwise the page's opaque viewer token.
func liveViewerKey(r *http.Request, viewer *model.User) (key, pial string) {
	if viewer != nil && viewer.PIALID != "" {
		return "pial:" + viewer.PIALID, viewer.PIALID
	}
	tok := strings.TrimSpace(r.FormValue("vt"))
	if tok == "" {
		tok = strings.TrimSpace(r.URL.Query().Get("vt"))
	}
	if tok == "" {
		return "", ""
	}
	return "anon:" + tok, ""
}

// ── Fragment rendering + FA Live delivery ────────────────────────────────────

// renderLiveFragment renders one live Facet. Failures are returned, never
// swallowed — a half-built Fragment must not reach the stream.
func (h *Handler) renderLiveFragment(name string, data interface{}) (string, error) {
	if h.partial == nil {
		return "", errors.New("live: partial template set unavailable")
	}
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("live: render %s: %w", name, err)
	}
	if buf.Len() == 0 {
		return "", fmt.Errorf("live: %s produced an empty fragment", name)
	}
	return buf.String(), nil
}

// publishLiveFacet delivers one rendered Facet to everyone holding a surface for
// a stream. The browser replaces the element carrying the same data-facet-id.
//
// The audience is every signed-in watcher plus the broadcaster, and the
// broadcaster is not optional. Presence is built from heartbeats, and the
// broadcaster's own stage deliberately does not beat — it is not a viewer of its
// own broadcast and must never be tallied as one. Fanning out to the heartbeat
// set alone therefore reaches every surface except the one belonging to the
// person whose broadcast it is, which is how a broadcaster ended by the media
// server was left looking at a live stage until they reloaded. The stream row
// carries its author's PIAL, so the server knows exactly who that is without
// asking the browser to announce itself.
func (h *Handler) publishLiveFacet(s *model.LiveStream, fragment string) {
	if s == nil {
		return
	}
	sent := make(map[string]bool)
	for _, pial := range liveRuntime.audiencePIALs(s.ID) {
		if pial == "" || sent[pial] {
			continue
		}
		sent[pial] = true
		PublishToUser(pial, SSEEvent{Type: "live_facet", Data: fragment})
	}
	if s.AuthorPIAL != "" && !sent[s.AuthorPIAL] {
		PublishToUser(s.AuthorPIAL, SSEEvent{Type: "live_facet", Data: fragment})
	}
}

// publishViewerCount persists the tally and pushes the re-rendered Facet, so the
// durable record and the Fragment come from the same number.
func (h *Handler) publishViewerCount(s *model.LiveStream, count int) {
	if !liveRuntime.tallyMoved(s.ID, count) {
		return
	}
	if s.Status == model.LiveStatusLive {
		if err := dbpkg.UpdateViewerCount(h.db, s.ID, count); err != nil &&
			!errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
			log.Printf("[live] persist viewer count for %s: %v", s.ID, err)
		}
	}
	// One tally, three surfaces. Each carries its own sub_id so the watch page's
	// beating element and a feed card's passive one are distinct Facets: pushing
	// a beating tally into the Playground would make reading the feed register
	// the reader as a viewer.
	display := fmtViewerCount(count)
	slots := []struct {
		slot string
		beat bool
	}{
		{"viewers", livePresenceBeats(s, true)},
		{"card_viewers", false},
		{"bcast_viewers", false},
	}
	for _, sl := range slots {
		frag, err := h.renderLiveFragment("live_viewer_count", map[string]interface{}{
			"StreamID": s.ID, "Slot": sl.slot, "Display": display, "Count": count,
			"Heartbeat": sl.beat, "Token": "",
		})
		if err != nil {
			log.Printf("[live] %v", err)
			continue
		}
		h.publishLiveFacet(s, frag)
	}
	h.publishLiveJSON(s, "viewers", map[string]int{"count": count})
}

// broadcastStageData hydrates live_broadcast_stage from server state alone.
//
// The stage is one Facet with one id, and every state it can be in — waiting,
// live, ended — is a re-render of that same Facet. Building its inputs here
// rather than only in the page handler is what lets the server push a state
// change into a stage that is already on screen.
func (h *Handler) broadcastStageData(s *model.LiveStream, facing string) map[string]interface{} {
	if facing != "user" {
		facing = "environment"
	}
	count := liveRuntime.count(s.ID)
	return map[string]interface{}{
		"S":            s,
		"AuthorHandle": s.AuthorHandle,
		"Facing":       facing,
		"Source":       resolvePublishSource("", s),
		"Viewers":      fmtViewerCount(count),
		"ViewerCount":  count,
		"ShareURL":     "/live/" + s.ID,
		"Encoder":      encoderSetupData(s.ID, nil),
	}
}

// publishStreamStatus re-renders every surface a stream appears on and pushes
// them, so a stream that ends goes dark everywhere at once.
//
// The broadcaster's own stage is one of those surfaces. It is rendered here and
// not only in liveEndHandler because the owner pressing "End broadcast" is the
// rarest way a broadcast ends: the publisher drops, the media server restarts,
// the ladder dies, moderation blocks it, or the reconcile loop finds no
// publisher — and in every one of those the decision is taken on the server with
// no request from the broadcaster to answer. Without this the stage keeps
// showing a live broadcast that stopped existing, and the stage does not beat,
// so nothing else would ever correct it.
func (h *Handler) publishStreamStatus(s *model.LiveStream) {
	// Heartbeat stays on in the pushed player: only signed-in watchers receive
	// FA Live, and they are keyed by PIAL, so the replacement element needs no
	// viewer token to keep their presence beat alive.
	v := h.buildLiveView(s, nil, true)

	// A render that leaves the player's shape alone — a ladder reduced under
	// pressure, a capacity note recorded — must not replace the player root: the
	// root holds the decoder, and a viewer who is watching would be sent back
	// to an empty buffer for a change they cannot see. What such a render
	// changes is the ladder, and the ladder has a Facet of its own.
	if !liveRuntime.shapeMoved(s.ID, playerShape(s)) {
		frag, err := h.renderLiveFragment("live_quality_selector", map[string]interface{}{
			"StreamID": s.ID, "Ladder": v.Ladder,
		})
		if err != nil {
			log.Printf("[live] %v", err)
		} else {
			h.publishLiveFacet(s, frag)
		}
		h.publishLiveJSON(s, "status", map[string]string{"status": s.Status})
		return
	}

	for _, name := range []string{"live_player", "live_card"} {
		frag, err := h.renderLiveFragment(name, v)
		if err != nil {
			log.Printf("[live] %v", err)
			continue
		}
		h.publishLiveFacet(s, frag)
	}
	// The standalone badges live outside the player and the card — the title bar
	// above the stage and the broadcaster's own top row — so they are pushed too.
	for _, slot := range []string{"title_badge", "bcast_badge"} {
		// OOB is false: FA Live delivers one Fragment per event and the client
		// runtime addresses it by the data-facet-id the Fragment carries. The
		// out-of-band address is only for a Fragment riding alongside another
		// one in a single HTTP answer.
		frag, err := h.renderLiveFragment("live_badge", map[string]interface{}{
			"StreamID": s.ID, "Slot": slot, "Status": s.Status, "OOB": false,
		})
		if err != nil {
			log.Printf("[live] %v", err)
			continue
		}
		h.publishLiveFacet(s, frag)
	}
	// The broadcaster's stage, and only on the transition that the stage's own
	// children cannot carry.
	//
	// Going live is already carried by the atomic Facets inside the stage — the
	// bcast_badge and bcast_viewers pushed just above — and those are the right
	// size for it: the composite around them holds the camera preview and the
	// live MediaStream bound to it, and replacing that element mid-broadcast
	// tears the broadcaster's own preview out from under a broadcast that is
	// working. Ending is the one transition the children cannot express, because
	// the whole surface has to stop being a publisher: no capture, no controls,
	// no publish credential.
	h.publishLiveJSON(s, "status", map[string]string{"status": s.Status})
	if s.Status != model.LiveStatusEnded {
		return
	}
	if frag, err := h.renderLiveFragment("live_broadcast_stage",
		h.broadcastStageData(s, "")); err != nil {
		log.Printf("[live] %v", err)
	} else {
		h.publishLiveFacet(s, frag)
	}
}

// PublishStreamState re-reads a stream and pushes its rendered surfaces. It is
// the seam the live lane's own owner uses: the media server's lifecycle hooks,
// the reconcile loop and the moderation block all decide on the server, and all
// of them have to end in rendered Fragments rather than a silently updated row.
func (h *Handler) PublishStreamState(streamID string) {
	if h.db == nil || streamID == "" {
		return
	}
	s, err := dbpkg.GetLiveStreamByID(h.db, streamID)
	if err != nil {
		if !errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
			log.Printf("[live] publish state for %s: %v", streamID, err)
		}
		return
	}
	h.publishStreamStatus(s)
	if s.Status == model.LiveStatusEnded {
		liveRuntime.forget(s.ID)
	}
}

// startLiveSweeper runs the presence reaper once, for the life of the process.
func (h *Handler) startLiveSweeper() {
	liveRuntime.mu.Lock()
	if liveRuntime.sweeping {
		liveRuntime.mu.Unlock()
		return
	}
	liveRuntime.sweeping = true
	liveRuntime.mu.Unlock()

	go func() {
		ticker := time.NewTicker(liveSweepInterval)
		defer ticker.Stop()
		for range ticker.C {
			for _, streamID := range liveRuntime.trackedStreams() {
				count, changed := liveRuntime.sweep(streamID)
				if !changed {
					continue
				}
				s, err := dbpkg.GetLiveStreamByID(h.db, streamID)
				if err != nil {
					if !errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
						log.Printf("[live] sweep load %s: %v", streamID, err)
					}
					liveRuntime.forget(streamID)
					continue
				}
				h.publishViewerCount(s, count)
			}
		}
	}()
}

// ── watch surface ────────────────────────────────────────────────────────────

// liveWatchPage — GET /live/{id}
// Public. The stream plays for a logged-out visitor; only chat is gated.
func (h *Handler) liveWatchPage(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	streamID := r.PathValue("id")
	viewer := h.userFromRequest(w, r)

	s, err := dbpkg.GetLiveStreamByID(h.db, streamID)
	if err != nil {
		if errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
			h.renderLiveNotFound(w, r, viewer)
			return
		}
		log.Printf("[live] load %s: %v", streamID, err)
		http.Error(w, "stream unavailable", http.StatusInternalServerError)
		return
	}

	// Adult wall: an 18+ broadcast is never rendered to a viewer whose account
	// is not cleared for it, and never to the public.
	if s.IsNSFW && excludeNSFW(viewer) {
		h.renderLiveGate(w, r, viewer, s)
		return
	}

	h.startLiveSweeper()

	v := h.buildLiveView(s, viewer, true)

	// Count this viewer immediately so the page they load already shows them.
	viewerToken := uuid.New().String()
	key, pial := liveViewerKey(r, viewer)
	if key == "" {
		key = "anon:" + viewerToken
	}
	count := liveRuntime.touch(s.ID, key, pial)
	v.ViewerCount = count
	v.Viewers = fmtViewerCount(count)
	v.ViewerToken = viewerToken
	go h.publishViewerCount(s, count)

	title := s.Title
	if strings.TrimSpace(title) == "" {
		title = "@" + v.AuthorHandle + " is live"
	}
	poster := s.PosterURL
	if poster == "" {
		poster = "/static/brand/og-image.png"
	}
	if strings.HasPrefix(poster, "/") {
		poster = "https://f33d3r.com" + poster
	}

	data := map[string]interface{}{
		"User":         viewer,
		"Mode":         "watch",
		"V":            v,
		"ChatMessages": h.chatBacklogFor(s, v),
		"ViewerToken":  viewerToken,
		"PageTitle":    title + " · F33D3R",
		"OGTitle":      title,
		"OGDesc":       "@" + v.AuthorHandle + " is live on f33d3r",
		"OGImage":      poster,
		"OGUrl":        "https://f33d3r.com/live/" + s.ID,
	}
	for k, val := range h.railData(viewer, "default") {
		data[k] = val
	}
	h.render(w, r, "live.html", data)
}

// chatBacklogFor returns the backlog a viewer is entitled to. A logged-out
// viewer sees the gate instead of a log, so they are handed nothing.
func (h *Handler) chatBacklogFor(s *model.LiveStream, v *liveStreamView) []liveChatMessage {
	if !v.CanChat {
		return nil
	}
	return liveRuntime.chatBacklog(s.ID)
}

func (h *Handler) renderLiveNotFound(w http.ResponseWriter, r *http.Request, viewer *model.User) {
	data := map[string]interface{}{
		"User":      viewer,
		"Mode":      "missing",
		"PageTitle": "Stream not found · F33D3R",
	}
	for k, val := range h.railData(viewer, "default") {
		data[k] = val
	}
	w.WriteHeader(http.StatusNotFound)
	h.render(w, r, "live.html", data)
}

// renderLiveGate answers an 18+ broadcast a viewer's account is not cleared for.
// The title names the broadcaster and nothing else: no title, poster or body of
// a walled stream reaches a viewer who may not see it.
func (h *Handler) renderLiveGate(w http.ResponseWriter, r *http.Request, viewer *model.User, s *model.LiveStream) {
	data := map[string]interface{}{
		"User":       viewer,
		"Mode":       "missing",
		"PageTitle":  "18+ stream · F33D3R",
		"GateHandle": s.AuthorHandle,
	}
	for k, val := range h.railData(viewer, "default") {
		data[k] = val
	}
	w.WriteHeader(http.StatusForbidden)
	h.render(w, r, "live.html", data)
}

// liveHeartbeat — POST /live/{id}/heartbeat
// The watch surface's presence beat, in both of its shapes.
//
// While a broadcast runs, the beat comes from live_viewer_count and is answered
// with the freshly rendered tally, so a logged-out viewer (who holds no FA Live
// session) still sees a live number; the same Fragment is pushed to every
// signed-in watcher.
//
// Before it runs, the beat comes from live_wait_beat inside the idle wall, and
// carries the status the wall was rendered for. It is answered with nothing at
// all while that is still the truth, and with the whole player the moment it
// is not. That answer is the only way a surface opened before the broadcast
// started can ever learn that it did: a logged-out watcher has no other
// channel, and a signed-in one is only pushed to while their presence is
// fresh — which this beat is what keeps.
func (h *Handler) liveHeartbeat(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	streamID := r.PathValue("id")
	viewer := h.userFromRequest(w, r)

	s, err := dbpkg.GetLiveStreamByID(h.db, streamID)
	if err != nil {
		if errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
			http.Error(w, "stream not found", http.StatusNotFound)
			return
		}
		log.Printf("[live] heartbeat load %s: %v", streamID, err)
		http.Error(w, "stream unavailable", http.StatusInternalServerError)
		return
	}

	key, pial := liveViewerKey(r, viewer)
	token := strings.TrimPrefix(key, "anon:")
	if strings.HasPrefix(key, "pial:") {
		token = ""
	}
	// The status the beating surface was rendered for. Only the wait beat
	// states one; the running tally's beat never has to, because the tally is
	// only ever rendered into a running broadcast.
	surfaceStatus := strings.TrimSpace(r.FormValue("status"))

	// A surface waiting for a broadcast to start is part of its audience: it
	// is kept present so the "went live" Fragment reaches it over FA Live as
	// well as over this beat's own answer. It is not tallied to anyone — no
	// idle surface renders a tally — and the row's count is only ever written
	// while the broadcast runs.
	if s.Status == model.LiveStatusIdle {
		if key != "" {
			liveRuntime.touch(s.ID, key, pial)
		}
		if surfaceStatus == model.LiveStatusIdle {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.answerBeatWithPlayer(w, r, s, viewer, token)
		return
	}

	// The stream is not running. A beat against it is a surface asking to be
	// counted in a broadcast that is over — and the server is the only party
	// that knows it is over, because a logged-out watcher holds no FA Live
	// session and has no other channel. So the beat is answered with the truth
	// instead of another tally: the re-rendered player, which for an ended
	// stream is the terminal live_ended Facet. It carries no tally, therefore
	// no beat, and the surface goes quiet on the server's word. Nothing is
	// counted: presence belongs to a running broadcast.
	if !livePresenceBeats(s, true) {
		h.answerBeatWithPlayer(w, r, s, viewer, token)
		return
	}

	// The broadcast is running and the surface that beat was rendered for a
	// different state: it is holding the idle wall over a stream that has
	// video. It is answered with the player, which carries the running beat.
	if surfaceStatus != "" && surfaceStatus != s.Status {
		h.answerBeatWithPlayer(w, r, s, viewer, token)
		return
	}

	count := 0
	if key == "" {
		// No identity and no viewer token: the beat cannot be attributed, so it
		// is not counted. The current tally is still answered honestly.
		count = liveRuntime.count(s.ID)
	} else {
		count = liveRuntime.touch(s.ID, key, pial)
	}

	go h.publishViewerCount(s, count)

	h.renderPartial(w, "live_viewer_count", map[string]interface{}{
		"StreamID": s.ID, "Slot": "viewers", "Display": fmtViewerCount(count), "Count": count,
		"Heartbeat": livePresenceBeats(s, true), "Token": token,
	})
}

// liveWatchSurfaceBeat reports whether a presence beat was sent by the watch
// surface at /live/{id} — the one surface that carries live_title_bar, and so
// the one surface whose title badge exists to be addressed.
//
// The same beating Facet (live_viewer_count inside live_player) is rendered on
// two surfaces: the watch page, and the author's live hero at the top of their
// profile. They hold different Facets around the player, so the answer to a beat
// is not the same on both, and the server has to know which one it is answering.
// htmx states the surface on every request it makes (HX-Current-URL), which is
// request context in exactly the way the request path is — the browser reports
// where it is and the server alone decides what that surface is owed. Addressing
// a Facet that is not on the asking surface would have htmx look for a target
// that cannot exist and raise an error against a page that is otherwise correct;
// the server does not send an address it knows to be wrong.
func liveWatchSurfaceBeat(r *http.Request, streamID string) bool {
	raw := strings.TrimSpace(r.Header.Get("HX-Current-URL"))
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	path := strings.TrimSuffix(u.Path, "/")
	return path == "/live/"+streamID
}

// answerBeatWithPlayer answers a presence beat with the whole truth of the
// surface that asked, not only its player.
//
// A logged-out watcher holds no FA Live session. The 20 s presence beat is their
// entire channel, so whatever the beat does not carry, they never learn. That
// makes this answer — not the player alone — the convergence path for a surface
// with no stream behind it.
//
// The answer is one HTTP response carrying every Facet of that surface whose
// rendering the status change actually altered, each one complete and each one
// addressed by its own data-facet-id:
//
//   - the player, as the primary swap. The element that beats is the tally
//     burned into the player's corner, so the response says it replaces
//     something larger than the element that asked, through the two response
//     headers htmx reads for exactly this: the Facet to swap and the swap to
//     perform. For an ended stream the player is the terminal live_ended Facet,
//     which holds no tally and therefore asks for no further beat.
//   - the title bar's badge, out of band. It sits outside the player, above the
//     stage, and is the Facet the owner watched read LIVE over a stage that said
//     the stream had ended. The atomic badge is what is sent and not the
//     live_title_bar composite around it: the bar's other content — the back
//     control and the stream's title — is identical before and after the
//     transition, and the badge is the smallest unit that fully expresses it.
//     Sending the bar would re-render unchanged markup and replace a sticky
//     element for no reason.
//
// Out of band means hx-swap-oob, and it means the selector form of it. These
// Facets are addressed by data-facet-id; htmx's plain hx-swap-oob="true" matches
// the id ATTRIBUTE, finds nothing here and swaps nothing at all, silently. The
// selector form carries the Facet's own address, which is why the badge Fragment
// is rendered with OOB set rather than being marked up by hand here.
//
// The browser gains no rule from any of this. It is handed finished HTML and,
// for each piece, the server's own name for where it goes — the same contract as
// an FA Live push, carried on the one channel a logged-out watcher has.
//
// Cost: this is the transition answer, not the steady-state one. While the
// broadcast runs, a beat is still answered with one live_viewer_count Fragment
// and nothing else; while it waits, with nothing at all. This path is reached
// at most twice per surface per broadcast — once going live, once ending —
// because the Fragment it returns either carries the next beat or does not.
//
// token is the page's viewer token, handed back into the player so a
// logged-out watcher who was waiting keeps beating as a watcher under the same
// identity. Going live, the watcher is counted here at once, exactly as the
// watch page counts them on load, so the tally the player arrives with already
// includes them.
func (h *Handler) answerBeatWithPlayer(w http.ResponseWriter, r *http.Request, s *model.LiveStream, viewer *model.User, token string) {
	v := h.buildLiveView(s, viewer, true)
	v.ViewerToken = token
	if v.Heartbeat {
		key, pial := liveViewerKey(r, viewer)
		if key != "" {
			count := liveRuntime.touch(s.ID, key, pial)
			v.ViewerCount = count
			v.Viewers = fmtViewerCount(count)
			go h.publishViewerCount(s, count)
		}
	}
	frag, err := h.renderLiveFragment("live_player", v)
	if err != nil {
		log.Printf("[live] %v", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}

	body := frag
	if liveWatchSurfaceBeat(r, s.ID) {
		badge, berr := h.renderLiveFragment("live_badge", map[string]interface{}{
			"StreamID": s.ID, "Slot": "title_badge", "Status": s.Status, "OOB": true,
		})
		if berr != nil {
			// A half-answered surface is the defect this path exists to close:
			// the player would go terminal and the title bar would keep reading
			// LIVE, which is exactly the stale state. Refuse the whole answer
			// instead, and let the next beat carry a complete one.
			log.Printf("[live] %v", berr)
			http.Error(w, "render error", http.StatusInternalServerError)
			return
		}
		body += badge
	}

	w.Header().Set("HX-Retarget", `[data-facet-id="facet:f33d3r:live:`+s.ID+`:player"]`)
	w.Header().Set("HX-Reswap", "outerHTML")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

// liveChatSend — POST /live/{id}/chat
// One render: the sender gets the line as the HTTP response, every other watcher
// gets the identical Fragment over FA Live.
func (h *Handler) liveChatSend(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	viewer := liveActor(h.userFromRequest(w, r))
	if viewer == nil {
		http.Error(w, "log in to chat", http.StatusUnauthorized)
		return
	}
	streamID := r.PathValue("id")
	s, err := dbpkg.GetLiveStreamByID(h.db, streamID)
	if err != nil {
		if errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
			http.Error(w, "stream not found", http.StatusNotFound)
			return
		}
		log.Printf("[live] chat load %s: %v", streamID, err)
		http.Error(w, "stream unavailable", http.StatusInternalServerError)
		return
	}
	if s.Status == model.LiveStatusEnded {
		http.Error(w, "this stream has ended", http.StatusConflict)
		return
	}
	if s.IsBlocked {
		http.Error(w, "this stream is unavailable", http.StatusForbidden)
		return
	}
	if s.IsNSFW && excludeNSFW(viewer) {
		http.Error(w, "not available for this account", http.StatusForbidden)
		return
	}

	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}
	if len([]rune(body)) > 280 {
		body = string([]rune(body)[:280])
	}

	now := time.Now()
	msg := liveChatMessage{
		ID:            uuid.New().String(),
		StreamID:      s.ID,
		Handle:        viewer.Handle,
		Name:          viewer.DisplayName,
		AvatarURL:     viewer.AvatarURL,
		Body:          body,
		CreatedAt:     now,
		TimeLabel:     now.Format("15:04"),
		IsBroadcaster: viewer.ID == s.AuthorID,
		Kind:          "chat",
	}
	if msg.Name == "" {
		msg.Name = msg.Handle
	}

	// Render before publishing: a line that cannot be rendered is not a line,
	// and must not be recorded or fanned out.
	frag, err := h.renderLiveFragment("live_chat_row", map[string]interface{}{"M": msg})
	if err != nil {
		log.Printf("[live] %v", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}

	liveRuntime.appendChat(msg)
	// Keep the sender present without waiting for their next beat.
	liveRuntime.touch(s.ID, "pial:"+viewer.PIALID, viewer.PIALID)

	// One line, every watcher — and the broadcaster, who is deliberately absent
	// from the audience set: their stage never beats, so audiencePIALs can never
	// contain them, and without this the author of the broadcast receives no
	// chat at all. Same shape as publishLiveFacet, which already solves exactly
	// this for Facet mutations.
	sent := make(map[string]bool)
	for _, pial := range liveRuntime.audiencePIALs(s.ID) {
		if pial == "" || sent[pial] {
			continue
		}
		sent[pial] = true
		if pial == viewer.PIALID {
			continue // the sender gets this line as the HTTP response
		}
		PublishToUser(pial, SSEEvent{Type: "live_chat", Data: frag})
	}
	if s.AuthorPIAL != "" && !sent[s.AuthorPIAL] && s.AuthorPIAL != viewer.PIALID {
		PublishToUser(s.AuthorPIAL, SSEEvent{Type: "live_chat", Data: frag})
	}
	h.publishLiveJSON(s, "chat", liveChatDTO(msg))
	dbpkg.CountStreamChatLine(h.db, s.ID)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// ── go live ──────────────────────────────────────────────────────────────────

// liveGoPage — GET /golive
// The composer, or the broadcast surface when the author already holds a stream.
func (h *Handler) liveGoPage(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	user := liveActor(h.userFromRequest(w, r))
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// A host with a Frequency running belongs on its stage, not on a form.
	if fid := h.frequencyHostOpen(r, user); fid != "" {
		http.Redirect(w, r, "/frequencies/"+fid, http.StatusSeeOther)
		return
	}

	if s, err := dbpkg.GetStreamForAuthor(h.db, user.ID); err == nil && s != nil {
		// The broadcaster already holds an open stream: send them to the surface
		// for the way they chose to publish it, not to whichever one is default.
		http.Redirect(w, r, "/live/"+s.ID+"/broadcast?source="+resolvePublishSource("", s),
			http.StatusSeeOther)
		return
	} else if err != nil && !errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
		log.Printf("[live] open stream for %s: %v", user.ID, err)
		http.Error(w, "stream lookup failed", http.StatusInternalServerError)
		return
	}

	h.renderGoComposer(w, r, user, goComposerDraft{}, "", http.StatusOK)
}

// goComposerDraft is what the broadcaster typed. A rejected form is answered
// with their own words still in it — the server re-renders the composer, so it
// has to carry the draft back rather than leave the browser to remember it.
type goComposerDraft struct {
	Title       string
	Description string
	Source      string
}

func (h *Handler) renderGoComposer(w http.ResponseWriter, r *http.Request, user *model.User, draft goComposerDraft, errMsg string, status int) {
	h.renderGoComposerWith(w, r, user, draft, errMsg, status, frequencyGoDraft{})
}

// renderGoComposerWith renders the go-live composer with both of its modes:
// the video broadcast form (draft, errMsg) and the Frequency form (freq). The
// composer is one Facet with two modes because going live is one act; which
// medium is a server input, and the server hands back the surface for it.
// FrequenciesLive is the "live now" row of audio cards under the form.
func (h *Handler) renderGoComposerWith(w http.ResponseWriter, r *http.Request, user *model.User, draft goComposerDraft, errMsg string, status int, freq frequencyGoDraft) {
	data := map[string]interface{}{
		// Frequency mode is chosen by ?mode=frequency, or forced when the
		// audio form was the one rejected.
		"FrequencyMode":   freq.Error != "" || r.URL.Query().Get("mode") == "frequency",
		"FrequencyGo":     frequencyGoData(user, freq),
		"FrequenciesLive": h.frequencyGoLiveStrip(r, user),
		"User":            user,
		"Mode":            "compose",
		"ComposerHandle":  user.Handle,
		"CanNSFW":         user.IsAdultCreator || (user.IsAdult && user.ContentSetting == "adult_enabled"),
		"Error":           errMsg,
		"Title":           draft.Title,
		"Description":     draft.Description,
		"Source":          normalizePublishSource(draft.Source),
		"PageTitle":       "Go live · F33D3R",
	}
	for k, val := range h.railData(user, "default") {
		data[k] = val
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	h.render(w, r, "live.html", data)
}

// ── publish source ───────────────────────────────────────────────────────────

// Publish sources. A broadcast is carried either by this device's camera over
// WebRTC or by a desktop/hardware encoder over RTMP or SRT — never both, since
// the media server accepts one publisher per stream.
const (
	liveSourceBrowser = "browser"
	liveSourceEncoder = "encoder"
)

// normalizePublishSource accepts only the two sources that exist. Anything else
// — including nothing — is the browser camera, which is the phone-first default.
func normalizePublishSource(raw string) string {
	if strings.TrimSpace(raw) == liveSourceEncoder {
		return liveSourceEncoder
	}
	return liveSourceBrowser
}

// resolvePublishSource decides which broadcast surface an author gets, in the
// order the evidence is trustworthy: what they just asked for, then what they
// chose when they opened the stream, then what the media server actually
// observed arriving. Only the last of those survives a restart of this process,
// which is why the observed protocol is consulted at all.
func resolvePublishSource(requested string, s *model.LiveStream) string {
	switch strings.TrimSpace(requested) {
	case liveSourceBrowser:
		return liveSourceBrowser
	case liveSourceEncoder:
		return liveSourceEncoder
	}
	if recorded := liveRuntime.publishSource(s.ID); recorded != "" {
		return recorded
	}
	if s.IngestProto != "" && s.IngestProto != "webrtc" {
		return liveSourceEncoder
	}
	return liveSourceBrowser
}

// liveStartHandler — POST /live/start
// Opens the author's stream idle; it turns live only when the media server
// reports frames arriving, never because a client said so.
func (h *Handler) liveStartHandler(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	user := liveActor(h.userFromRequest(w, r))
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if user.PIALID == "" {
		h.renderGoComposer(w, r, user, goComposerDraft{}, "Your identity is not fully set up yet. Finish onboarding to broadcast.", http.StatusForbidden)
		return
	}

	source := normalizePublishSource(r.FormValue("source"))
	description := strings.TrimSpace(r.FormValue("description"))
	if len([]rune(description)) > 600 {
		description = string([]rune(description)[:600])
	}
	title := strings.TrimSpace(r.FormValue("title"))
	draft := goComposerDraft{Title: title, Description: description, Source: source}

	if title == "" {
		h.renderGoComposer(w, r, user, draft, "Give the broadcast a title so people know what they are joining.", http.StatusBadRequest)
		return
	}
	if len([]rune(title)) > 120 {
		title = string([]rune(title)[:120])
		draft.Title = title
	}
	isNSFW := r.FormValue("nsfw") == "1"
	if isNSFW && !(user.IsAdultCreator || (user.IsAdult && user.ContentSetting == "adult_enabled")) {
		h.renderGoComposer(w, r, user, draft, "Your account is not cleared to broadcast 18+ content.", http.StatusForbidden)
		return
	}

	// The plaintext ingest key returned here is deliberately dropped: it is a
	// publish credential and never travels to a Facet, and never rides a redirect.
	// A broadcaster who needs it for an encoder asks for it on their own
	// broadcast surface, over a POST that proves they own the stream.
	s, _, err := dbpkg.CreateStream(h.db, user.ID, user.PIALID, title, isNSFW)
	if err != nil {
		log.Printf("[live] create stream for %s: %v", user.ID, err)
		h.renderGoComposer(w, r, user, draft, "The broadcast could not be opened. Try again.", http.StatusInternalServerError)
		return
	}

	// CreateStream persists the title but knows nothing of the description, and
	// returns the author's existing open stream unchanged when they already hold
	// one. Writing the submitted text here is what makes the composer's two text
	// inputs both land, on a fresh stream and a re-submitted one alike.
	if err := dbpkg.SetStreamMeta(h.db, s.ID, title, description); err != nil {
		log.Printf("[live] stream meta for %s: %v", s.ID, err)
		h.renderGoComposer(w, r, user, draft, "The broadcast details could not be saved. Try again.", http.StatusInternalServerError)
		return
	}

	liveRuntime.setPublishSource(s.ID, source)

	facing := r.FormValue("facing")
	if facing != "user" {
		facing = "environment"
	}
	http.Redirect(w, r, "/live/"+s.ID+"/broadcast?facing="+facing+"&source="+source, http.StatusSeeOther)
}

// liveBroadcastPage — GET /live/{id}/broadcast
// The broadcaster's own surface. Owner only.
func (h *Handler) liveBroadcastPage(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	user := liveActor(h.userFromRequest(w, r))
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	streamID := r.PathValue("id")
	s, err := dbpkg.GetLiveStreamByID(h.db, streamID)
	if err != nil {
		if errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
			h.renderLiveNotFound(w, r, user)
			return
		}
		log.Printf("[live] broadcast load %s: %v", streamID, err)
		http.Error(w, "stream unavailable", http.StatusInternalServerError)
		return
	}
	if s.AuthorID != user.ID {
		http.Error(w, "this is not your broadcast", http.StatusForbidden)
		return
	}

	h.startLiveSweeper()

	facing := r.URL.Query().Get("facing")
	if facing != "user" {
		facing = "environment"
	}
	// A source named in the URL is the broadcaster changing their mind on their
	// own surface: remember it so the next visit lands here too, and so the
	// pushed re-render of this stage resolves to the same one.
	liveRuntime.setPublishSource(s.ID, resolvePublishSource(r.URL.Query().Get("source"), s))

	// The stage is built from the same server state whether it is rendered into
	// a page or pushed into one that is already open. Two builders would be two
	// answers to what this broadcast is doing.
	stage := h.broadcastStageData(s, facing)
	stage["AuthorHandle"] = user.Handle

	title := "You are live · F33D3R"
	if s.Status == model.LiveStatusEnded {
		title = "Broadcast ended · F33D3R"
	}
	data := map[string]interface{}{
		"User":      user,
		"Mode":      "broadcast",
		"B":         stage,
		"PageTitle": title,
		// The owner of a stream is entitled to its whole ring — no CanChat gate
		// to check, unlike a viewer. Without this a broadcaster who reloads
		// mid-stream opens an empty log until the next line arrives.
		"ChatMessages": liveRuntime.chatBacklog(s.ID),
	}
	for k, val := range h.railData(user, "default") {
		data[k] = val
	}
	h.render(w, r, "live.html", data)
}

// ── encoder credentials ──────────────────────────────────────────────────────

// livePublishHost is the host every publish URL points at, and whether that host
// only means anything on the machine running this process. A broadcaster whose
// encoder sits on another device needs to be told that before they spend ten
// minutes wondering why OBS cannot connect, not after.
func livePublishHost() (host string, isLocal bool) {
	host = "localhost"
	if u, err := url.Parse(live.RTMPServerURL()); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return host, true
	}
	return host, false
}

// encoderSetupData hydrates live_encoder_setup. Passing nil targets builds the
// sealed state, which carries no credential at all: the revealed state exists
// only as the answer to the owner's own POST.
func encoderSetupData(streamID string, targets *live.PublishTargets) map[string]interface{} {
	host, isLocal := livePublishHost()
	data := map[string]interface{}{
		"StreamID":    streamID,
		"Revealed":    targets != nil,
		"PublicHost":  host,
		"HostIsLocal": isLocal,
	}
	if targets != nil {
		data["RTMPServer"] = targets.RTMPServer
		data["RTMPStreamKey"] = targets.RTMPStreamKey
		data["SRTURL"] = targets.SRTURL
	}
	return data
}

// liveEncoderReveal — POST /live/{id}/encoder
//
// The one place in the application where an ingest key reaches a rendered
// surface. Four things make that safe and all four are enforced here:
//
//  1. publishableLiveStream proves the session is the stream's author and that
//     the broadcast can still be published to. A viewer, a logged-out visitor
//     and any other account are refused before anything is minted.
//  2. It is a POST. The key never appears in a URL, so it cannot reach an
//     access log, a Referer header, browser history or a pasted link.
//  3. The Fragment is written to this one response. It is never handed to
//     publishLiveFacet, so it cannot travel over FA Live to a watcher.
//  4. The answer is marked no-store, so no shared cache or back-button restore
//     keeps a copy of it.
//
// The stored key is a one-way hash and cannot be read back, so revealing means
// minting: RotateIngestKey issues a fresh secret and retires the old one. The
// Facet says so before the broadcaster presses the button.
func (h *Handler) liveEncoderReveal(w http.ResponseWriter, r *http.Request) {
	s, ok := h.publishableLiveStream(w, r)
	if !ok {
		return
	}

	key, err := dbpkg.RotateIngestKey(h.db, s.ID)
	if err != nil {
		if errors.Is(err, dbpkg.ErrLiveStreamEnded) {
			http.Error(w, "this broadcast has ended", http.StatusConflict)
			return
		}
		if errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
			http.Error(w, "stream not found", http.StatusNotFound)
			return
		}
		log.Printf("[live] rotate ingest key for %s: %v", s.ID, err)
		http.Error(w, "the stream key could not be issued", http.StatusInternalServerError)
		return
	}

	targets := live.TargetsFor(s.ID, key)
	log.Printf("[live] encoder credentials issued for stream %s to author %s", s.ID, s.AuthorID)

	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Referrer-Policy", "no-referrer")
	h.renderPartial(w, "live_encoder_setup", encoderSetupData(s.ID, &targets))
}

// liveEndHandler — POST /live/{id}/end
// Ends the broadcast. Owner only. Every open surface is told by Fragment.
func (h *Handler) liveEndHandler(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	user := liveActor(h.userFromRequest(w, r))
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	streamID := r.PathValue("id")
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

	// The owner's own click gets the same Fragment every other surface was just
	// pushed: the stage re-rendered in its terminal state, under the same facet
	// id. Answering with a different shape here would mean the broadcaster who
	// pressed the button and the broadcaster whose publisher dropped end up
	// looking at two different pages for the same fact.
	h.renderPartial(w, "live_broadcast_stage", h.broadcastStageData(ended, ""))
}

// ── Playground surface ───────────────────────────────────────────────────────

// facetLiveCards — GET /facets/live/cards
// The Playground's live strip. Writes nothing when nobody is live.
func (h *Handler) facetLiveCards(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	viewer := h.userFromRequest(w, r)
	streams, err := dbpkg.GetActiveStreams(h.db, 12)
	if err != nil {
		log.Printf("[live] active streams: %v", err)
		http.Error(w, "live streams unavailable", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	hideAdult := excludeNSFW(viewer)
	for _, s := range streams {
		if s == nil || (s.IsNSFW && hideAdult) {
			continue
		}
		v := h.buildLiveView(s, viewer, false)
		frag, rerr := h.renderLiveFragment("live_card", v)
		if rerr != nil {
			log.Printf("[live] %v", rerr)
			continue
		}
		_, _ = w.Write([]byte(frag))
	}
	// Live Frequencies share this strip: one lane for everything that is live
	// right now, video and audio. An unreachable Auralis writes nothing here.
	for _, frag := range h.frequencyCardFragments(r, viewer, 12) {
		_, _ = w.Write([]byte(frag))
	}
}
