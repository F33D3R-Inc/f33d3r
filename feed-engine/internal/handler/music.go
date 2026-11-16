package handler

// music.go — the /music surface. A track is a work of kind 'track'.
//
// In Facet Architecture order:
//
//	upload            POST /upload/track stores the audio (Caeor kind audio) and
//	                  the optional cover (kind image) through the gated lanes and
//	                  answers their URLs; the artist's key then signs the track as
//	                  a work and POST /events creates it through createWork — the
//	                  same write path, outbox, scan and fan-out as every work
//	list              GET /music/tracks renders music_tracks: one music_track_row
//	                  Facet per track the viewer's wall admits,
//	                  facet:f33d3r:track:<work_id>:card
//	like              POST /api/track/like — signed in; work_reactions 'like';
//	                  the like Facet (facet:f33d3r:track:<work_id>:like) goes back
//	                  to the liker and its count to every watcher of the track
//	play              POST /api/track/play — signed in; the player reports what
//	                  happened (started | ended | left at a position) and the
//	                  server decides what that was: a play, a completion or a
//	                  skip. A play is counted (work_play_totals) and the plays
//	                  Facet (facet:f33d3r:track:<work_id>:plays) goes to every
//	                  watcher; every listening event is forwarded to Zior
//	delete            POST /api/track/delete — SoftDeleteWork, which withdraws the
//	                  artist's own media from Caeor
//
// The browser player keeps only playback of the URL the server rendered.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/f33d3r/feed-engine/internal/caeor"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/zior"
)

// trackUploadMax is the ceiling of one track's audio.
const trackUploadMax = 120 << 20

// trackPageSize is how many tracks one page of the list carries.
const trackPageSize = 20

// ── Zior — the audio brain ────────────────────────────────────────────────────

var (
	ziorOnce   sync.Once
	ziorShared *zior.Client
)

// zior returns the process-wide Zior client, built once from the config. A
// track is a work, so Zior hears it through the work's zior_voice obligation,
// keyed by the work id.
func (h *Handler) zior() *zior.Client {
	ziorOnce.Do(func() {
		url := ""
		if h.cfg != nil {
			url = h.cfg.ZiorURL
		}
		ziorShared = zior.NewClient(url)
	})
	return ziorShared
}

// ── Facet IDs ────────────────────────────────────────────────────────────────

func trackFacetID(workID, sub string) string {
	return "facet:f33d3r:track:" + workID + ":" + sub
}

// ── Reading tracks ───────────────────────────────────────────────────────────

// trackOf projects a track work into the track Facets' view. The title is the
// body's first line, the description the rest; the genre is the first tag.
func trackOf(w *model.Work, plays int64, viewer *model.User) *model.Track {
	title, description, _ := strings.Cut(w.Body, "\n")
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Untitled"
	}
	t := &model.Track{
		ID:           w.ID,
		AuthorID:     w.AuthorID,
		AuthorHandle: w.AuthorHandle,
		AuthorName:   w.AuthorName,
		AvatarURL:    w.AvatarURL,
		IsVerified:   w.IsVerified,
		Title:        title,
		Description:  strings.TrimSpace(description),
		AudioURL:     w.VoiceURL,
		DurationSecs: int(w.VoiceDurationSecs + 0.5),
		Tags:         w.Tags,
		PlayCount:    plays,
		LikeCount:    w.LikeCount,
		CreatedAt:    w.CreatedAt,
		TimeAgo:      TimeAgo(w.CreatedAt),
		LikedByUser:  w.LikedByUser,
		Mine:         viewer != nil && viewer.ID != "" && viewer.ID == w.AuthorID,
	}
	if len(w.MediaURLs) > 0 {
		t.CoverURL = w.MediaURLs[0]
	}
	if len(w.Tags) > 0 {
		t.Genre = w.Tags[0]
	}
	return t
}

// loadTracks reads a page of tracks, applies the viewer's wall and enrichment
// (the same gate and enrichment every work surface uses), and projects each
// into the track view. next is the cursor of the following page, "" on the
// last page.
func (h *Handler) loadTracks(viewer *model.User, genre, authorID string, limit int, before string) (tracks []*model.Track, next string, err error) {
	works, err := dbpkg.GetTrackWorks(h.db, genre, authorID, limit+1, before)
	if err != nil {
		return nil, "", err
	}
	if len(works) > limit {
		works = works[:limit]
		next = dbpkg.TrackPageCursor(works[len(works)-1])
	}
	works = h.gateWorks(viewer, works)
	h.enrichWorks(works, viewer)
	ids := make([]string, 0, len(works))
	for _, w := range works {
		ids = append(ids, w.ID)
	}
	plays, err := dbpkg.TrackPlayTotals(h.db, ids)
	if err != nil {
		return nil, "", err
	}
	tracks = make([]*model.Track, 0, len(works))
	for _, w := range works {
		tracks = append(tracks, trackOf(w, plays[w.ID], viewer))
	}
	return tracks, next, nil
}

func (h *Handler) musicPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if h.db == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	myTracks, _, err := h.loadTracks(user, "", user.ID, 50, "")
	if err != nil {
		log.Printf("[music] tracks of %s: %v", user.ID, err)
		http.Error(w, "music is unavailable right now", http.StatusInternalServerError)
		return
	}
	stats, err := dbpkg.GetArtistStats(h.db, user.ID)
	if err != nil {
		log.Printf("[music] %v", err)
		http.Error(w, "music is unavailable right now", http.StatusInternalServerError)
		return
	}
	h.render(w, r, "music.html", h.withRail(map[string]interface{}{
		"User":        user,
		"Surface":     "music",
		"SessionID":   uuid.New().String(),
		"Title":       "Zior · Music",
		"ShowScores":  h.cfg.ShowScores,
		"Clusters":    []model.MusicCluster{},
		"Themes":      ThemesWithActive(user.ThemeID),
		"MyTracks":    myTracks,
		"ArtistStats": stats,
		"Genre":       r.URL.Query().Get("genre"),
	}, user, "music"))
}

// tracksPartial is GET /music/tracks — the music_tracks Facet.
func (h *Handler) tracksPartial(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if h.db == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	genre := strings.TrimSpace(r.URL.Query().Get("genre"))
	tracks, next, err := h.loadTracks(user, genre, "", trackPageSize, r.URL.Query().Get("after"))
	if err != nil {
		log.Printf("[music] track list: %v", err)
		http.Error(w, "tracks are unavailable right now", http.StatusInternalServerError)
		return
	}
	nextURL := ""
	if next != "" {
		q := url.Values{}
		if genre != "" {
			q.Set("genre", genre)
		}
		q.Set("after", next)
		nextURL = "/music/tracks?" + q.Encode()
	}
	h.renderPartial(w, "music_tracks", map[string]interface{}{
		"Tracks":            tracks,
		"CurrentUserHandle": user.Handle,
		"Genre":             genre,
		"After":             next,
		"NextURL":           nextURL,
	})
}

// ── Upload ───────────────────────────────────────────────────────────────────

// uploadTrack is POST /upload/track: the track's audio and optional cover go
// through the gated lanes into Caeor. The answer is their URLs; the track
// becomes a work when the artist signs it and POST /events creates it.
func (h *Handler) uploadTrack(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "the upload could not be read", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	if _, err := uploaderOf(user); err != nil {
		http.Error(w, "sign in to upload tracks", http.StatusUnauthorized)
		return
	}
	if h.db == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	can, err := dbpkg.CheckCapability(h.db, user.PIALID, model.CapMusicUpload)
	if err != nil {
		log.Printf("[track] capability of %s: %v", user.PIALID, err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if !can {
		http.Error(w, "Music uploads are restricted on this account", http.StatusForbidden)
		return
	}

	audioFile, audioHeader, err := r.FormFile("audio")
	if err != nil {
		http.Error(w, "audio file required", http.StatusBadRequest)
		return
	}
	defer audioFile.Close()
	audio, err := io.ReadAll(io.LimitReader(audioFile, trackUploadMax+1))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	if len(audio) > trackUploadMax {
		uploadHTTPError(w, "track", errUploadTooLarge)
		return
	}

	// The cover is read and checked before the audio is stored, so a refused
	// cover never leaves an orphaned track asset behind.
	var cover []byte
	coverName := ""
	if coverFile, coverHeader, err := r.FormFile("cover"); err == nil {
		defer coverFile.Close()
		cover, err = io.ReadAll(io.LimitReader(coverFile, imageUploadMax+1))
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		if len(cover) > imageUploadMax {
			uploadHTTPError(w, "track cover", errUploadTooLarge)
			return
		}
		coverName = coverHeader.Filename
	} else if !errors.Is(err, http.ErrMissingFile) {
		http.Error(w, "the cover could not be read", http.StatusBadRequest)
		return
	}

	audioURL, err := h.admitAudio(r.Context(), user, caeor.KindAudio, audioHeader.Filename, audio, "track-upload")
	if err != nil {
		uploadHTTPError(w, "track", err)
		return
	}
	coverURL := ""
	if len(cover) > 0 {
		adm, err := h.admitImageUpload(r.Context(), user, coverName, cover)
		if err != nil {
			uploadHTTPError(w, "track cover", err)
			return
		}
		coverURL = adm.URL
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"audio_url": audioURL, "cover_url": coverURL}); err != nil {
		log.Printf("[track] encode upload answer: %v", err)
	}
}

// ── Likes ────────────────────────────────────────────────────────────────────

// trackLikeAction is POST /api/track/like {track_id}: toggles the viewer's like
// of a track. The answer is the like Facet as this viewer sees it; the count
// goes to everyone watching the track.
func (h *Handler) trackLikeAction(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" || user.ID == "demo_user" {
		http.Error(w, "sign in to like tracks", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the request could not be read", http.StatusBadRequest)
		return
	}
	track, ok := h.visibleTrack(w, user, r.FormValue("track_id"))
	if !ok {
		return
	}
	liked, err := dbpkg.WorkReactedBy(h.db, track.ID, user.ID, "like")
	if err != nil {
		log.Printf("[track] %v", err)
		http.Error(w, "could not save that", http.StatusInternalServerError)
		return
	}
	if _, err := dbpkg.SetWorkReaction(h.db, track.ID, user.ID, user.Handle, "like", !liked); err != nil {
		log.Printf("[track] like of %s by %s: %v", track.ID, user.ID, err)
		http.Error(w, "could not save that", http.StatusInternalServerError)
		return
	}
	go h.learnFromReaction(user, track.ID, "like", !liked)
	h.publishWorkEngagement(track.ID)

	count, err := dbpkg.WorkLikeCount(h.db, track.ID)
	if err != nil {
		log.Printf("[track] %v", err)
		http.Error(w, "could not read the likes", http.StatusInternalServerError)
		return
	}
	// Everyone watching sees the count; the viewer's own liked state is theirs
	// alone, so watchers receive the Facet as a viewer who has not liked it.
	h.publishWatchedFacet(track.ID, trackFacetID(track.ID, "like"), "music_track_like", map[string]interface{}{
		"ID": track.ID, "LikeCount": count, "Liked": false,
	})
	h.renderPartial(w, "music_track_like", map[string]interface{}{
		"ID": track.ID, "LikeCount": count, "Liked": !liked,
	})
}

// ── Plays ────────────────────────────────────────────────────────────────────

// trackListen is what the player reports: what happened, not what it means.
type trackListen string

const (
	listenStarted trackListen = "started" // playback of the track began
	listenEnded   trackListen = "ended"   // the track played to its end
	listenLeft    trackListen = "left"    // the listener left it at position_secs
)

// listenMeaning is the server's reading of one report: a play, a completion, a
// skip, or nothing worth recording. A track left after it began and before its
// last second is a skip; left at (or past) the end it was already completed.
func listenMeaning(event trackListen, position, duration float64) (zior.PlayEventType, bool) {
	switch event {
	case listenStarted:
		return zior.EventPlay, true
	case listenEnded:
		return zior.EventComplete, true
	case listenLeft:
		if position > 0.5 && (duration <= 0 || position < duration-1) {
			return zior.EventSkip, true
		}
	}
	return "", false
}

// trackPlayAction is POST /api/track/play: one listening report from the
// player. Form fields: track_id, event (started | ended | left),
// position_secs, duration_secs, session_id. A play is counted and its Fragment
// pushed to every watcher; every meaningful event is forwarded to Zior. 204.
func (h *Handler) trackPlayAction(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" || user.ID == "demo_user" {
		http.Error(w, "sign in to listen", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the request could not be read", http.StatusBadRequest)
		return
	}
	event := trackListen(strings.TrimSpace(r.FormValue("event")))
	switch event {
	case listenStarted, listenEnded, listenLeft:
	default:
		http.Error(w, "event must be started, ended or left", http.StatusBadRequest)
		return
	}
	position, err := optionalSeconds(r.FormValue("position_secs"))
	if err != nil {
		http.Error(w, "position_secs: "+err.Error(), http.StatusBadRequest)
		return
	}
	duration, err := optionalSeconds(r.FormValue("duration_secs"))
	if err != nil {
		http.Error(w, "duration_secs: "+err.Error(), http.StatusBadRequest)
		return
	}
	track, ok := h.visibleTrack(w, user, r.FormValue("track_id"))
	if !ok {
		return
	}
	meaning, ok := listenMeaning(event, position, duration)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if meaning == zior.EventPlay {
		plays, err := dbpkg.RecordTrackPlay(h.db, track.ID)
		if err != nil {
			log.Printf("[track] play of %s: %v", track.ID, err)
			http.Error(w, "could not count that play", http.StatusInternalServerError)
			return
		}
		h.publishWatchedFacet(track.ID, trackFacetID(track.ID, "plays"), "music_track_plays", map[string]interface{}{
			"ID": track.ID, "PlayCount": plays,
		})
	}
	now := time.Now().UTC()
	h.zior().SendEvents([]zior.PlayEvent{{
		TrackID:      track.ID,
		UserID:       user.ID,
		EventType:    meaning,
		PositionSecs: position,
		DurationSecs: duration,
		SessionID:    truncate(strings.TrimSpace(r.FormValue("session_id")), 128),
		Timestamp:    &now,
	}})
	w.WriteHeader(http.StatusNoContent)
}

// optionalSeconds parses an optional non-negative number of seconds.
func optionalSeconds(raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f < 0 || f > workMaxDurationSecs {
		return 0, fmt.Errorf("must be a number of seconds")
	}
	return f, nil
}

// visibleTrack loads a live track the viewer's wall admits, answering the
// request itself when there is none.
func (h *Handler) visibleTrack(w http.ResponseWriter, viewer *model.User, rawID string) (*model.Work, bool) {
	id, err := uuid.Parse(strings.TrimSpace(rawID))
	if err != nil {
		http.Error(w, "track_id is not a track", http.StatusBadRequest)
		return nil, false
	}
	if h.db == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	track, err := dbpkg.GetTrackWork(h.db, id.String())
	if err != nil {
		log.Printf("[track] %v", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	if track == nil || len(h.gateWorks(viewer, []*model.Work{track})) == 0 {
		http.Error(w, "track not found", http.StatusNotFound)
		return nil, false
	}
	return track, true
}

// ── Delete ───────────────────────────────────────────────────────────────────

// deleteTrackAPI is POST /api/track/delete {track_id}: the artist deleting one
// of their tracks. It is a work deletion: the artist's own media of the track
// is withdrawn from Caeor, never freed for re-upload credit.
func (h *Handler) deleteTrackAPI(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the request could not be read", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(r.FormValue("track_id")))
	if err != nil {
		http.Error(w, "track_id is not a track", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	if h.db == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	track, err := dbpkg.GetTrackWork(h.db, id.String())
	if err != nil {
		log.Printf("[track] %v", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if track == nil || track.AuthorID != user.ID {
		http.Error(w, "not found or forbidden", http.StatusForbidden)
		return
	}
	if err := dbpkg.SoftDeleteWork(h.db, track.ID, user.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found or forbidden", http.StatusForbidden)
			return
		}
		log.Printf("[track] deleting %s: %v", track.ID, err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	// The card leaves every screen it is on: an empty Fragment in its place.
	cardID := trackFacetID(track.ID, "card")
	tombstone := `<div data-facet-id="` + cardID + `" hidden></div>`
	PublishFacetToPostWatchers(track.ID, FacetMutation{FacetID: cardID, Fragment: tombstone})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := io.WriteString(w, tombstone); err != nil {
		log.Printf("[track] answering delete of %s: %v", track.ID, err)
	}
}

// ── Creator dashboard ────────────────────────────────────────────────────────

// artistTracks is one artist's tracks and totals for the creator dashboard.
func (h *Handler) artistTracks(ctx context.Context, user *model.User, limit int) ([]*model.Track, dbpkg.ArtistStats, error) {
	if err := ctx.Err(); err != nil {
		return nil, dbpkg.ArtistStats{}, err
	}
	tracks, _, err := h.loadTracks(user, "", user.ID, limit, "")
	if err != nil {
		return nil, dbpkg.ArtistStats{}, err
	}
	stats, err := dbpkg.GetArtistStats(h.db, user.ID)
	if err != nil {
		return nil, dbpkg.ArtistStats{}, err
	}
	return tracks, stats, nil
}
