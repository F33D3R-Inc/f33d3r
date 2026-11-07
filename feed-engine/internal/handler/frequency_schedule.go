package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/auralis"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/sitra"
)

const (
	frequencyScheduleHorizon = 14 * 24 * time.Hour
	frequencyNotifyTimeout   = 10 * time.Second
	frequencyLiveKind        = "frequency_live"
)

// frequencyScheduleProblem is what a host is told about a time they cannot use.
func frequencyScheduleProblem(at time.Time) string {
	now := time.Now()
	switch {
	case at.Before(now):
		return "Pick a time in the future."
	case at.After(now.Add(frequencyScheduleHorizon)):
		return "A Frequency can be scheduled up to 14 days ahead."
	}
	return ""
}

// frequencyMayReach says whether an action may be aimed at this person. It refuses when
// either has blocked the other, when they muted the actor, and when a room's content is
// not something they can see.
func (h *Handler) frequencyMayReach(ctx context.Context, actor *model.User, id, targetPIAL string) bool {
	if h.db == nil {
		return false
	}
	account, err := dbpkg.GetUserIDFromPIAL(h.db, targetPIAL)
	if err != nil {
		return false
	}
	if dbpkg.IsBlocked(h.db, account, actor.ID) || dbpkg.IsBlocked(h.db, actor.ID, account) {
		return false
	}
	muted, err := dbpkg.GetMutedUserIDs(h.db, account)
	if err != nil || muted[actor.ID] {
		return false
	}
	ans, err := h.auralis.Get(ctx, id, "")
	if err != nil {
		return false
	}
	if ans.Frequency.Frequency.AdultContent {
		person, err := dbpkg.GetUserByID(h.db, account)
		if err != nil || !frequencyAdultAllowed(person) {
			return false
		}
	}
	return true
}

// frequencyRemindButton renders this person's own reminder control. It is fetched
// by each viewer, because the shared controls of a scheduled room name nobody.
func (h *Handler) frequencyRemindButton(w http.ResponseWriter, id string, reminded bool) {
	frag, err := h.renderFrequencyFragment("frequency_remind_button", map[string]interface{}{"ID": id, "Reminded": reminded})
	if err != nil {
		log.Printf("[frequency] %v", err)
		http.Error(w, "fragment unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// facetFrequencyRemind — GET /facets/frequency/{id}/remind
func (h *Handler) facetFrequencyRemind(w http.ResponseWriter, r *http.Request) {
	if !h.frequencyReady(w, r) {
		return
	}
	id := r.PathValue("id")
	viewer := h.userFromRequest(w, r)
	pial := ""
	if a := liveActor(viewer); a != nil {
		pial = a.PIALID
	}
	ans, err := h.auralis.Get(r.Context(), id, pial)
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	h.frequencyRemindButton(w, id, ans.Frequency.Viewer != nil && ans.Frequency.Viewer.Reminded)
}

// frequencyRemind turns a person's reminder on or off and answers with the button.
func (h *Handler) frequencyRemind(w http.ResponseWriter, r *http.Request, user *model.User, id string, raw map[string]json.RawMessage) {
	on := eventBoolField(r, raw, true, "on")
	if on {
		ans, err := h.auralis.Get(r.Context(), id, user.PIALID)
		if err != nil {
			h.frequencyRefuse(w, r, id, err)
			return
		}
		if code, msg, refused := h.frequencyGate(user, &ans.Frequency); refused {
			h.frequencyProblem(w, r, http.StatusForbidden, code, msg)
			return
		}
	}
	ans, err := h.auralis.Remind(r.Context(), user.PIALID, id, on)
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	h.frequencyDirty(id)
	h.frequencyRemindButton(w, id, ans.Frequency.Viewer != nil && ans.Frequency.Viewer.Reminded)
}

// facetFrequenciesByHost — GET /facets/frequency-host/{handle}
// What a person is hosting or about to, for their profile. Empty when nothing is.
func (h *Handler) facetFrequenciesByHost(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if h.auralis == nil || !h.auralis.Configured() || h.db == nil {
		return
	}
	host, err := dbpkg.GetUserByHandle(h.db, strings.TrimPrefix(r.PathValue("handle"), "@"))
	if err != nil || host == nil || host.PIALID == "" {
		return
	}
	viewer := h.userFromRequest(w, r)
	ans, err := h.auralis.ListByHost(r.Context(), host.PIALID, viewerPIALOf(viewer), 6)
	if err != nil {
		log.Printf("[frequency] host lane %s: %v", host.Handle, err)
		return
	}
	for _, card := range h.buildFrequencyCards(ans.Items, viewer) {
		frag, err := h.renderFrequencyFragment("frequency_card", card)
		if err != nil {
			log.Printf("[frequency] %v", err)
			continue
		}
		_, _ = w.Write([]byte(frag))
	}
}

func viewerPIALOf(viewer *model.User) string {
	if a := liveActor(viewer); a != nil {
		return a.PIALID
	}
	return ""
}

// frequencySearchCards finds rooms by title, and rooms hosted by an exact handle.
func (h *Handler) frequencySearchCards(ctx context.Context, query string, viewer *model.User) []FrequencyCardData {
	if h.auralis == nil || !h.auralis.Configured() || query == "" {
		return nil
	}
	pial := viewerPIALOf(viewer)
	var items []auralis.ListItem
	if h.db != nil {
		if host, err := dbpkg.GetUserByHandle(h.db, strings.TrimPrefix(query, "@")); err == nil && host != nil && host.PIALID != "" {
			if byHost, err := h.auralis.ListByHost(ctx, host.PIALID, pial, 6); err == nil {
				items = append(items, byHost.Items...)
			}
		}
	}
	found, err := h.auralis.Search(ctx, query, pial, 12)
	if err != nil {
		log.Printf("[frequency] search: %v", err)
	} else {
		seen := map[string]bool{}
		for i := range items {
			seen[items[i].Frequency.ID] = true
		}
		for _, item := range found.Items {
			if !seen[item.Frequency.ID] {
				items = append(items, item)
			}
		}
	}
	return h.buildFrequencyCards(items, viewer)
}

// frequencyFollowedFirst puts rooms from hosts the viewer follows at the front,
// keeping the order Auralis gave within each group.
func (h *Handler) frequencyFollowedFirst(items []auralis.ListItem, viewer *model.User) {
	a := liveActor(viewer)
	if a == nil || h.db == nil || len(items) < 2 {
		return
	}
	hosts := make([]string, 0, len(items))
	for i := range items {
		hosts = append(hosts, items[i].Frequency.HostPialID)
	}
	followed, err := dbpkg.FollowedAmongPIALs(h.db, a.ID, hosts)
	if err != nil {
		log.Printf("[frequency] followed hosts: %v", err)
		return
	}
	sort.SliceStable(items, func(i, j int) bool {
		return followed[items[i].Frequency.HostPialID] && !followed[items[j].Frequency.HostPialID]
	})
}

// FrequencyEvent is called for every committed room event. Most only change what
// is on screen; a few also owe someone a notification.
func (h *Handler) FrequencyEvent(ev sitra.FrequencyEvent) {
	h.FrequencyChanged(ev.FrequencyID)
	ctx, cancel := context.WithTimeout(context.Background(), frequencyNotifyTimeout)
	defer cancel()
	var err error
	switch ev.EventType {
	case "frequency.started":
		err = h.frequencyAnnounceStart(ctx, ev.FrequencyID)
	case "frequency.reminders_due":
		err = h.frequencyTellReminded(ctx, ev.FrequencyID, "frequency_reminder", ev.EventID, "scheduled", " starts soon")
	case "frequency.cancelled":
		err = h.frequencyTellReminded(ctx, ev.FrequencyID, "frequency_cancelled", ev.EventID, "cancelled", " was cancelled")
	case "frequency.scheduled":
		err = h.frequencyTellReminded(ctx, ev.FrequencyID, "frequency_rescheduled", ev.EventID, "scheduled", " has a new time")
	}
	if err != nil {
		log.Printf("[frequency] notify %s %s: %v", ev.EventType, ev.FrequencyID, err)
	}
}

// frequencyTellReminded tells everyone who asked for a reminder, if the room is
// still in the state the news is about.
func (h *Handler) frequencyTellReminded(ctx context.Context, id, kind, eventID, state, what string) error {
	ans, err := h.auralis.Get(ctx, id, "")
	if err != nil {
		return err
	}
	s := &ans.Frequency.Frequency
	if s.State != state {
		return nil
	}
	// A host changing the time repeatedly tells each person once an hour at most.
	key := kind + "|" + eventID
	if kind == "frequency_rescheduled" {
		key = kind + "|" + id + "|" + time.Now().UTC().Format("2006010215")
	}
	return h.writeFrequencyNotices(ctx, s, kind, key, "“"+s.Title+"”"+what)
}

// frequencyAnnounceStart tells reminder holders, who asked, and then queues the
// host's followers, who are limited per person. Both use one event id per person,
// so someone who is both is told once.
func (h *Handler) frequencyAnnounceStart(ctx context.Context, id string) error {
	ans, err := h.auralis.Get(ctx, id, "")
	if err != nil {
		return err
	}
	s := &ans.Frequency.Frequency
	if !s.IsLive() {
		return nil
	}
	title := "“" + s.Title + "” is live"
	if err := h.writeFrequencyNotices(ctx, s, frequencyLiveKind, "", title); err != nil {
		return err
	}
	// An invite-only room reaches people through its invitations.
	if s.Visibility == "private" || h.db == nil {
		return nil
	}
	hostID, err := dbpkg.GetUserIDFromPIAL(h.db, s.HostPialID)
	if err != nil {
		return err
	}
	host, err := dbpkg.GetUserByID(h.db, hostID)
	if err != nil || host == nil {
		return err
	}
	return dbpkg.CreateFanoutJob(h.db, dbpkg.FanoutJob{
		ActorID: hostID, ActorHandle: host.Handle, Kind: frequencyLiveKind,
		TargetID: id, TargetType: "frequency", Preview: s.Title,
		Audience: s.Visibility, Adult: s.AdultContent,
	})
}

// writeFrequencyNotices writes one notice per reminder holder, dropping anyone the
// host and they have blocked between them or who may no longer see the room. An
// empty key derives the same event id the follower fan-out uses.
func (h *Handler) writeFrequencyNotices(ctx context.Context, s *auralis.Summary, kind, key, body string) error {
	if h.db == nil {
		return nil
	}
	pials, err := h.auralis.Reminders(ctx, s.ID)
	if err != nil {
		return err
	}
	hostID, err := dbpkg.GetUserIDFromPIAL(h.db, s.HostPialID)
	if err != nil {
		return err
	}
	host, err := dbpkg.GetUserByID(h.db, hostID)
	if err != nil || host == nil {
		return err
	}
	for _, pial := range pials {
		account, err := dbpkg.GetUserIDFromPIAL(h.db, pial)
		if err != nil {
			continue
		}
		person, err := dbpkg.GetUserByID(h.db, account)
		if err != nil || person == nil {
			continue
		}
		if dbpkg.IsBlocked(h.db, hostID, account) || dbpkg.IsBlocked(h.db, account, hostID) || (s.AdultContent && !frequencyAdultAllowed(person)) {
			continue
		}
		eventID := dbpkg.FanoutEventID(kind, s.ID, account)
		if key != "" {
			eventID = dbpkg.FanoutEventID(kind, key, account)
		}
		if _, err := dbpkg.WriteNotice(h.db, dbpkg.Notice{
			EventID: eventID, Recipient: account, Kind: kind, Actor: hostID,
			TargetID: s.ID, TargetType: "frequency",
			Payload:   map[string]interface{}{"preview": s.Title},
			PushTitle: "@" + host.Handle, PushBody: body, PushURL: "/frequencies/" + s.ID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// frequencyReplayDir is where the replay volume is mounted.
const frequencyReplayDir = "/app/replays"

// frequencyReplay — GET /frequencies/{id}/replay
// Streams a replay to anyone who may see the room, with seeking.
func (h *Handler) frequencyReplay(w http.ResponseWriter, r *http.Request) {
	if !h.frequencyReady(w, r) {
		return
	}
	id := r.PathValue("id")
	viewer := h.userFromRequest(w, r)
	ans, err := h.auralis.Get(r.Context(), id, viewerPIALOf(viewer))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, _, refused := h.frequencyGate(viewer, &ans.Frequency); refused {
		http.NotFound(w, r)
		return
	}
	name, err := h.auralis.ReplayFile(r.Context(), viewerPIALOf(viewer), id, h.frequencyAudienceFacts(viewer, ans.Frequency.Frequency.HostPialID))
	if err != nil {
		if auralis.IsOutage(err) {
			log.Printf("[frequency] replay %s: %v", id, err)
		}
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(frequencyReplayDir, filepath.Base(name)))
	if err != nil {
		log.Printf("[frequency] replay %s: %v", id, err)
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/mp4")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// facetFrequencyReplay — GET /facets/frequency/{id}/replay
// The player, or why not. The ended stage is shared by everyone, so each viewer fetches
// this for themselves, and a player is only shown to someone who may listen.
func (h *Handler) facetFrequencyReplay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if h.auralis == nil || !h.auralis.Configured() {
		return
	}
	id := r.PathValue("id")
	viewer := h.userFromRequest(w, r)
	ans, err := h.auralis.Get(r.Context(), id, viewerPIALOf(viewer))
	if err != nil {
		return
	}
	if _, _, refused := h.frequencyGate(viewer, &ans.Frequency); refused {
		return
	}
	name := "frequency_replay_player"
	_, err = h.auralis.ReplayFile(r.Context(), viewerPIALOf(viewer), id, h.frequencyAudienceFacts(viewer, ans.Frequency.Frequency.HostPialID))
	switch {
	case err == nil:
	case auralis.Code(err) == "followers_only":
		name = "frequency_replay_followers"
	case auralis.Code(err) == "subscribers_only":
		name = "frequency_replay_subscribers"
	default:
		return
	}
	frag, err := h.renderFrequencyFragment(name, map[string]interface{}{"ID": id})
	if err != nil {
		log.Printf("[frequency] %v", err)
		return
	}
	_, _ = w.Write([]byte(frag))
}

// facetFrequencyReplayControls — GET /facets/frequency/{id}/replay_controls
// The player is shared by everyone, so the host's Delete is fetched by each viewer.
func (h *Handler) facetFrequencyReplayControls(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if h.auralis == nil || !h.auralis.Configured() {
		return
	}
	id := r.PathValue("id")
	pial := viewerPIALOf(h.userFromRequest(w, r))
	if pial == "" {
		return
	}
	ans, err := h.auralis.Get(r.Context(), id, pial)
	if err != nil || ans.Frequency.Frequency.HostPialID != pial {
		return
	}
	h.renderFrequencyReplayControls(w, id, ans.Frequency.Frequency.ClipsEnabled)
}
