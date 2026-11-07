package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/prometheus/client_golang/prometheus"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/observ"
	"github.com/f33d3r/feed-engine/internal/sitra"
)

// sseFacetMutate is the one general FA Live event. One event carries one Facet's
// complete rendered fragment, and the runtime applies it by data-facet-id. A new
// surface needs a renderer and a publish, not a new event name and listener.
const sseFacetMutate = "facet.mutate"

// FacetMode says how a fragment lands. Replace is the normal case. Prepend and
// Append add a new row to the Facet named by ParentFacetID.
type FacetMode string

const (
	FacetReplace FacetMode = "replace"
	FacetPrepend FacetMode = "prepend"
	FacetAppend  FacetMode = "append"
)

// FacetPriority says how much a mutation matters when a connection is under
// pressure.
type FacetPriority string

const (
	FacetPriorityLow    FacetPriority = "low"
	FacetPriorityNormal FacetPriority = "normal"
	FacetPriorityHigh   FacetPriority = "high"
)

// FacetMutation is the Facet Architecture event model.
type FacetMutation struct {
	EventID       string        `json:"event_id"`
	Timestamp     string        `json:"timestamp"`
	Type          string        `json:"type"`
	FacetID       string        `json:"facet_id"`
	Fragment      string        `json:"fragment"`
	Priority      FacetPriority `json:"priority"`
	Mode          FacetMode     `json:"mode"`
	ParentFacetID string        `json:"parent_facet_id,omitempty"`
}

// validFacetID enforces facet:<namespace>:<type>:<entity_id>:<sub_id>.
func validFacetID(id string) bool {
	parts := strings.Split(id, ":")
	if len(parts) != 5 || parts[0] != "facet" {
		return false
	}
	for _, p := range parts[1:] {
		if p == "" {
			return false
		}
	}
	return true
}

// newFacetEvent checks a mutation and builds the event for it. A fragment that
// does not carry its own Facet ID could never be applied, so it is refused here
// rather than disappearing silently in the browser.
func newFacetEvent(m FacetMutation) (SSEEvent, error) {
	if !validFacetID(m.FacetID) {
		return SSEEvent{}, fmt.Errorf("facet mutation: invalid facet id %q", m.FacetID)
	}
	if !strings.Contains(m.Fragment, `data-facet-id="`+m.FacetID+`"`) {
		return SSEEvent{}, fmt.Errorf("facet mutation: fragment does not carry %q", m.FacetID)
	}
	switch m.Mode {
	case "":
		m.Mode = FacetReplace
	case FacetReplace:
	case FacetPrepend, FacetAppend:
		if !validFacetID(m.ParentFacetID) {
			return SSEEvent{}, fmt.Errorf("facet mutation: %s of %q has invalid parent %q", m.Mode, m.FacetID, m.ParentFacetID)
		}
	default:
		return SSEEvent{}, fmt.Errorf("facet mutation: unknown mode %q", m.Mode)
	}
	if m.Priority == "" {
		m.Priority = FacetPriorityNormal
	}
	m.EventID = uuid.NewString()
	m.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	m.Type = sseFacetMutate

	body, err := json.Marshal(m)
	if err != nil {
		return SSEEvent{}, fmt.Errorf("facet mutation: encoding %q: %w", m.FacetID, err)
	}
	return SSEEvent{Type: sseFacetMutate, Data: string(body), Bulk: m.Priority == FacetPriorityLow}, nil
}

// publishFacet builds a mutation and hands it to one publish path. A refused
// mutation is a bug in the caller, so it is logged with the Facet it names.
func publishFacet(m FacetMutation, publish func(SSEEvent)) {
	event, err := newFacetEvent(m)
	if err != nil {
		log.Printf("[fa-live] %v", err)
		return
	}
	publish(event)
}

// PublishFacetToUser delivers a mutation to every connection one person holds.
func PublishFacetToUser(pial string, m FacetMutation) {
	publishFacet(m, func(e SSEEvent) { PublishToUser(pial, e) })
}

// PublishFacetToAccount delivers a mutation to one persona's connections.
func PublishFacetToAccount(account string, m FacetMutation) {
	publishFacet(m, func(e SSEEvent) { PublishToAccount(account, e) })
}

// PublishFacetToPostWatchers delivers a mutation to everyone watching a work.
func PublishFacetToPostWatchers(workID string, m FacetMutation) {
	publishFacet(m, func(e SSEEvent) { PublishToPostWatchers(workID, e) })
}

// PublishFacetToAll delivers a mutation to every connection.
func PublishFacetToAll(m FacetMutation) {
	publishFacet(m, PublishToAllSessions)
}

// ── Delivery across replicas ─────────────────────────────────────────────────

// liveRelay is nil when no broker is configured. Delivery then reaches this
// replica's connections only, which is correct for a single-replica deployment.
var liveRelay atomic.Pointer[sitra.LiveBus]

// StartLiveRelay connects this replica to the others through Sitra Achra.
func StartLiveRelay(ctx context.Context, brokers string) (func(), error) {
	bus, stop, err := sitra.StartLiveBus(ctx, brokers, uuid.NewString(), applyRemoteLive, func() {
		// Events may have been lost while consuming failed, so nothing before this
		// point can be vouched for and open connections must re-read.
		liveEvents.raiseFloor(nextLiveID())
		resyncLocalSessions()
	})
	if err != nil {
		return nil, err
	}
	liveRelay.Store(bus)
	return stop, nil
}

func applyRemoteLive(env sitra.LiveEnvelope) {
	if env.Kind == liveKindWatch {
		applyWatch(env.Key, env.Type, env.Data, env.Action == liveWatchAdd)
		return
	}
	deliverLive(env.Kind, env.Key, SSEEvent{Type: env.Type, Data: env.Data, JSON: env.JSON, ID: env.ID, Bulk: env.Bulk})
}

func relayLive(kind, key string, event SSEEvent) {
	if bus := liveRelay.Load(); bus != nil {
		bus.Publish(sitra.LiveEnvelope{
			Kind: kind, Key: key, ID: event.ID, Type: event.Type,
			Data: event.Data, JSON: event.JSON, Bulk: event.Bulk,
		})
	}
}

// relayWatch tells other replicas what a person is watching, because the watch
// request and that person's stream can land on different replicas.
func relayWatch(pial, watchType, id string, add bool) {
	bus := liveRelay.Load()
	if bus == nil {
		return
	}
	action := liveWatchRemove
	if add {
		action = liveWatchAdd
	}
	bus.Publish(sitra.LiveEnvelope{Kind: liveKindWatch, Key: pial, Type: watchType, Data: id, Action: action})
}

// ── Web stream resume ────────────────────────────────────────────────────────

// streamResumePoint reads where a web client asks to resume from. The runtime
// opens a new EventSource on reconnect, which sends no Last-Event-ID header, so it
// names its position in the query instead.
func streamResumePoint(r *http.Request) int64 {
	if id := lastEventID(r); id > 0 {
		return id
	}
	n, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("after")), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// resumeWebStream writes what a reconnecting client missed, or tells it to re-read,
// then gives the connection its cursor. It returns the IDs it replayed, because the
// session registered first and may also have those events queued.
func resumeWebStream(w io.Writer, r *http.Request, account, pial string) map[int64]bool {
	replayed := map[int64]bool{}
	if after := streamResumePoint(r); after > 0 {
		frames, ok := ReplayAfter(account, pial, after)
		if ok {
			for _, f := range frames {
				replayed[f.ID] = true
				if f.Event.Data != "" {
					writeSSEFrameID(w, f.ID, f.Event.Type, f.Event.Data)
				}
			}
		} else {
			writeSSEFrame(w, sseResync, "")
		}
	}
	writeSSEFrameID(w, nextLiveID(), sseCursor, "")
	return replayed
}

// ── Live Facets ──────────────────────────────────────────────────────────────

// liveWatchProfilePrefix marks a profile in the watch context, which otherwise
// holds work IDs.
const liveWatchProfilePrefix = "profile:"

// toastsFacetID is the Shell's toast container.
const toastsFacetID = "facet:f33d3r:shell:global:toasts"

// pollSurfaces are the shapes a poll can be on screen in. Each has its own Facets.
var pollSurfaces = []string{"pcd", "pcf"}

// renderFacet renders one partial to a string. A failure is logged with the Facet's
// name and costs viewers this update; their next read still shows the truth.
func (h *Handler) renderFacet(name string, data map[string]interface{}) string {
	if h.partial == nil {
		return ""
	}
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("[fa-live] rendering %s: %v", name, err)
		return ""
	}
	return buf.String()
}

// publishWatchedFacet renders a Facet and sends it to everyone watching a key.
func (h *Handler) publishWatchedFacet(watchKey, facetID, partial string, data map[string]interface{}) {
	if fragment := h.renderFacet(partial, data); fragment != "" {
		PublishFacetToPostWatchers(watchKey, FacetMutation{
			FacetID: facetID, Fragment: fragment, Priority: FacetPriorityLow,
		})
	}
}

// publishNewReply adds a new reply to the thread of everyone watching the work it
// answers. The card is rendered for no particular viewer, as new-post fan-out does.
// The wrapper carries the Facet ID the event model requires; the runtime skips the
// row if the reply's own card is already on screen, which it is for its author.
func (h *Handler) publishNewReply(parentWorkID, replyWorkID string) {
	card, err := h.renderWorkCardByID(replyWorkID, nil)
	if err != nil {
		log.Printf("[fa-live] reply %s: %v", replyWorkID, err)
		return
	}
	facetID := "facet:f33d3r:work:" + replyWorkID + ":reply_row"
	PublishFacetToPostWatchers(parentWorkID, FacetMutation{
		FacetID:       facetID,
		Fragment:      `<div data-facet-id="` + facetID + `">` + string(card) + `</div>`,
		Mode:          FacetAppend,
		ParentFacetID: "facet:f33d3r:work:" + parentWorkID + ":replies",
	})
}

// publishFollowCounts sends fresh follower and following counts to everyone
// watching these profiles.
func (h *Handler) publishFollowCounts(userIDs ...string) {
	if h.db == nil {
		return
	}
	for _, id := range userIDs {
		var followers, following int
		if err := h.db.QueryRow(
			`SELECT follower_count, following_count FROM user_profiles WHERE user_id = $1::uuid`, id,
		).Scan(&followers, &following); err != nil {
			log.Printf("[fa-live] follow counts %s: %v", id, err)
			continue
		}
		for stat, count := range map[string]int{"followers": followers, "following": following} {
			h.publishWatchedFacet(liveWatchProfilePrefix+id, "facet:f33d3r:profile:"+id+":"+stat,
				"profile_stat_count", map[string]interface{}{"UserID": id, "Stat": stat, "Count": count})
		}
	}
}

// publishProfileFacets sends an edited name, bio and avatar to everyone watching the
// profile, and the nav row to the owner's own devices.
func (h *Handler) publishProfileFacets(userID string) {
	if h.db == nil {
		return
	}
	u, err := dbpkg.GetUserByID(h.db, userID)
	if err != nil || u == nil {
		log.Printf("[fa-live] profile %s: %v", userID, err)
		return
	}
	key := liveWatchProfilePrefix + userID
	h.publishWatchedFacet(key, "facet:f33d3r:profile:"+userID+":display_name", "profile_display_name",
		map[string]interface{}{"UserID": userID, "DisplayName": u.DisplayName})
	h.publishWatchedFacet(key, "facet:f33d3r:profile:"+userID+":bio", "profile_bio",
		map[string]interface{}{"UserID": userID, "Bio": u.Bio})
	identity := map[string]interface{}{
		"UserID": userID, "Handle": u.Handle, "AvatarURL": u.AvatarURL, "DisplayName": u.DisplayName,
	}
	h.publishWatchedFacet(key, "facet:f33d3r:profile:"+userID+":avatar", "profile_avatar_slot", identity)
	if u.PIALID == "" {
		return
	}
	for partial, sub := range map[string]string{"nav_account_avatar": "nav_avatar", "nav_account_name": "nav_name"} {
		if fragment := h.renderFacet(partial, identity); fragment != "" {
			PublishFacetToUser(u.PIALID, FacetMutation{
				FacetID: "facet:f33d3r:account:" + userID + ":" + sub, Fragment: fragment,
			})
		}
	}
}

// publishPollTally sends fresh poll numbers to everyone watching the work. Only the
// numbers go out: which option a viewer chose stays on their own row. Someone who
// has not voted has no bars on screen, so only the total changes for them.
func (h *Handler) publishPollTally(workID, voterID string) {
	if h.db == nil {
		return
	}
	poll, err := dbpkg.GetPollResults(h.db, workID, voterID)
	if err != nil {
		log.Printf("[fa-live] poll %s: %v", workID, err)
		return
	}
	for _, surface := range pollSurfaces {
		for i, r := range poll.Results {
			data := map[string]interface{}{"PostID": workID, "Surface": surface, "Index": i, "Pct": r.Pct}
			h.publishWatchedFacet(workID, fmt.Sprintf("facet:f33d3r:work:%s:poll_fill_%s_%d", workID, surface, i), "poll_fill", data)
			h.publishWatchedFacet(workID, fmt.Sprintf("facet:f33d3r:work:%s:poll_pct_%s_%d", workID, surface, i), "poll_pct", data)
		}
		h.publishWatchedFacet(workID, "facet:f33d3r:work:"+workID+":poll_total_"+surface, "poll_total",
			map[string]interface{}{"PostID": workID, "Surface": surface, "Poll": poll})
	}
}

// publishToast shows a short message to one person. contentHTML must already be escaped.
func publishToast(pial, name, contentHTML string) {
	facetID := "facet:f33d3r:toast:" + html.EscapeString(pial) + ":" + html.EscapeString(name)
	PublishFacetToUser(pial, FacetMutation{
		FacetID: facetID,
		Fragment: `<div class="toast toast-show toast--success" role="status" data-expires-ms="6000" data-facet-id="` +
			facetID + `">` + contentHTML + `</div>`,
		Mode:          FacetPrepend,
		ParentFacetID: toastsFacetID,
	})
}

// ── Notification updates ─────────────────────────────────────────────────────

// StartNotificationUpdates renders Herald's unread announcements for this replica's
// connections. Every replica consumes the topic, so this delivers locally and does
// not relay.
func (h *Handler) StartNotificationUpdates(ctx context.Context, brokers string) (func(), error) {
	return sitra.StartNotificationUpdates(ctx, brokers, h.applyNotificationUpdate, func() {
		liveEvents.raiseFloor(nextLiveID())
		resyncLocalSessions()
	})
}

func (h *Handler) applyNotificationUpdate(u sitra.NotificationUpdate) {
	if u.AtMillis > 0 {
		defer func() {
			storedToBadge.Observe(time.Since(time.UnixMilli(u.AtMillis)).Seconds())
		}()
	}
	if h.db != nil {
		if err := dbpkg.SetUnreadCount(h.db, u.Account, u.Unread); err != nil {
			log.Printf("[notify] recording unread for %s: %v", u.Account, err)
		}
	}
	badge := h.renderFacet("notif_badge", map[string]interface{}{"PIALID": u.Pial, "UnreadCount": u.Unread})
	deliverLive(liveKindAccount, u.Account, SSEEvent{
		ID: nextLiveID(), Type: "notify", Data: badge, JSON: fmt.Sprintf(`{"unread":%d}`, u.Unread),
	})
	deliverLive(liveKindAccount, u.Account, SSEEvent{ID: nextLiveID(), Type: "notif_new", Data: "1"})
}

var storedToBadge = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "notification_store_to_badge_seconds",
	Help:    "Time from Herald storing a notification to its badge reaching the stream.",
	Buckets: []float64{.01, .05, .1, .25, .5, 1, 2, 5, 10},
})

func init() {
	observ.MustRegister(
		storedToBadge,
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "sse_dropped_total",
			Help: "Live events dropped because a connection's buffer was full.",
		}, func() float64 { return float64(sseDropCount.Load()) }),
	)
}
