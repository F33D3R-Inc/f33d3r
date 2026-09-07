package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/scanclient"
	"github.com/f33d3r/feed-engine/internal/zior"
)

// ── Zior — the audio brain ────────────────────────────────────────────────────
//
// Every audio publish seam hands the file to Zior on a detached goroutine and
// the publish answers without waiting. The text mapper's vector, written by
// the insert, stands until Zior's replaces it; if Zior never answers, it
// stands for good. Zior's key is always this brain's own id for the audio —
// the track id for a music track, the work id for a voice work — so its
// /signal/{id} and /similar/{id} are addressable from here later.

var (
	ziorOnce   sync.Once
	ziorShared *zior.Client
)

// zior returns the process-wide Zior client, built once from the config.
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

// ziorAnalyzeTrack sends a freshly stored music track to Zior and, when it
// answers, stores its vector, mood, descriptor and contexts on the track's
// row (migration 0021). Runs detached from the upload with its own deadline;
// a file Zior cannot analyse is logged and the track keeps no vector, which
// is the truthful state.
func (h *Handler) ziorAnalyzeTrack(trackID, creatorID, filename string, audio []byte) {
	client := h.zior()
	if !client.Enabled() || h.db == nil || trackID == "" || len(audio) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), zior.UploadTimeout)
	defer cancel()
	a, err := client.Analyze(ctx, trackID, creatorID, filename, bytes.NewReader(audio))
	if err != nil {
		log.Printf("[zior] track %s: %v", trackID, err)
		return
	}
	if err := dbpkg.SetTrackZiorSignal(h.db, trackID, a.Vector.Slice(), a.Mood, a.Descriptor, a.ContextTags); err != nil {
		log.Printf("[zior] track %s: storing signal: %v", trackID, err)
	}
}

func (h *Handler) musicPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	sessionID := uuid.New().String()
	genre := r.URL.Query().Get("genre")

	var myTracks []*model.Track
	var artistStats dbpkg.ArtistStats
	if h.db != nil {
		myTracks, _ = dbpkg.GetTracksByAuthor(h.db, user.ID, 50)
		for _, t := range myTracks {
			t.TimeAgo = TimeAgo(t.CreatedAt)
		}
		artistStats = dbpkg.GetArtistStats(h.db, user.ID)
	}

	h.render(w, r, "music.html", h.withRail(map[string]interface{}{
		"User":        user,
		"Surface":     "music",
		"SessionID":   sessionID,
		"Title":       "Zior · Music",
		"ShowScores":  h.cfg.ShowScores,
		"Clusters":    h.fetchClusters(r.Context()),
		"Themes":      ThemesWithActive(user.ThemeID),
		"MyTracks":    myTracks,
		"ArtistStats": artistStats,
		"Genre":       genre,
	}, user, "music"))
}

func (h *Handler) tracksPartial(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	genre := r.URL.Query().Get("genre")
	after := r.URL.Query().Get("after")

	const pageSize = 20
	var tracks []*model.Track
	// One more than the page says whether a next page exists; the cursor the
	// sentinel carries is the last shown track's id (GetRecentTracks pages by
	// the created_at of that id).
	var nextAfter string
	if h.db != nil {
		tracks, _ = dbpkg.GetRecentTracks(h.db, genre, pageSize+1, after)
		if len(tracks) > pageSize {
			tracks = tracks[:pageSize]
			nextAfter = tracks[len(tracks)-1].ID
		}
		for _, t := range tracks {
			t.TimeAgo = TimeAgo(t.CreatedAt)
			t.LikedByUser, _ = dbpkg.IsTrackLiked(h.db, user.ID, t.ID)
		}
	}

	nextURL := ""
	if nextAfter != "" {
		q := url.Values{}
		if genre != "" {
			q.Set("genre", genre)
		}
		if filter := r.URL.Query().Get("filter"); filter != "" {
			q.Set("filter", filter)
		}
		q.Set("after", nextAfter)
		nextURL = "/music/tracks?" + q.Encode()
	}

	h.renderPartial(w, "music_tracks", map[string]interface{}{
		"Tracks":            tracks,
		"CurrentUserHandle": user.Handle,
		"Genre":             genre,
		"After":             nextAfter,
		"NextURL":           nextURL,
	})
}

func (h *Handler) uploadTrack(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	if err := r.ParseMultipartForm(120 << 20); err != nil {
		http.Error(w, "file too large (max 120MB)", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	if user.ID == "" || user.ID == "demo_user" {
		http.Error(w, "sign in to upload tracks", http.StatusUnauthorized)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		title = "Untitled"
	}
	description := truncate(r.FormValue("description"), 2000)
	genre := truncate(r.FormValue("genre"), 60)
	priceCents := 0

	// Audio file
	audioFile, audioHeader, err := r.FormFile("audio")
	if err != nil {
		http.Error(w, "audio file required", http.StatusBadRequest)
		return
	}
	defer audioFile.Close()

	audioExt := strings.ToLower(filepath.Ext(audioHeader.Filename))
	allowedAudio := map[string]bool{
		".mp3": true, ".wav": true, ".flac": true, ".ogg": true,
		".aac": true, ".opus": true, ".m4a": true,
		".mp4": true, ".webm": true, ".mov": true,
	}
	if !allowedAudio[audioExt] {
		http.Error(w, "unsupported format — use MP3, WAV, FLAC, OGG, AAC, Opus, M4A, MP4, WebM, or MOV", http.StatusBadRequest)
		return
	}
	isVideo := audioExt == ".mp4" || audioExt == ".webm" || audioExt == ".mov"

	// The track is read into memory and gated before anything reaches disk, so
	// banned content is never stored — not even for the moment it takes to
	// delete it again.
	audioBytes, err := io.ReadAll(io.LimitReader(audioFile, 120<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	if len(audioBytes) == 0 {
		http.Error(w, "audio file is empty", http.StatusBadRequest)
		return
	}

	// Banned-content gate. A video container gets the frame-hash layer as well
	// as the audio fingerprint; a pure audio track gets the fingerprint.
	gateKind := scanclient.KindAudio
	if isVideo {
		gateKind = scanclient.KindVideo
	}
	trackSum := sha256.Sum256(audioBytes)
	trackPIAL := ""
	if user != nil {
		trackPIAL = user.PIALID
	}
	if gerr := h.bannedContentGate(r.Context(), gateInput{
		Kind:     gateKind,
		Filename: audioHeader.Filename,
		Data:     audioBytes,
		SHA256:   hex.EncodeToString(trackSum[:]),
		PIALID:   trackPIAL,
		Label:    "track-upload",
	}); gerr != nil {
		banGateHTTPError(w, gerr)
		return
	}

	audioDir := filepath.Join("web", "static", "uploads", "audio")
	if err := os.MkdirAll(audioDir, 0755); err != nil {
		log.Printf("[track] mkdir %s: %v", audioDir, err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	audioName := uuid.New().String() + audioExt
	if err := os.WriteFile(filepath.Join(audioDir, audioName), audioBytes, 0644); err != nil {
		log.Printf("[track] write %s: %v", audioName, err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	audioURL := "/static/uploads/audio/" + audioName

	// Optional cover art. Read first so the same banned-content gate runs on it
	// before it is written: cover art is permanently hosted media too.
	coverURL := ""
	if coverFile, coverHeader, err := r.FormFile("cover"); err == nil {
		defer coverFile.Close()
		coverBytes, cerr := io.ReadAll(io.LimitReader(coverFile, 20<<20))
		if cerr != nil {
			log.Printf("[track] cover read for pial=%s: %v", trackPIAL, cerr)
		} else if len(coverBytes) > 0 {
			coverSum := sha256.Sum256(coverBytes)
			if gerr := h.bannedContentGate(r.Context(), gateInput{
				Kind:     scanclient.KindImage,
				Filename: coverHeader.Filename,
				Data:     coverBytes,
				SHA256:   hex.EncodeToString(coverSum[:]),
				PIALID:   trackPIAL,
				Label:    "track-cover",
			}); gerr != nil {
				os.Remove(filepath.Join(audioDir, audioName))
				banGateHTTPError(w, gerr)
				return
			}
			if url, uerr := h.saveUpload(bytes.NewReader(coverBytes), coverHeader.Filename, "covers"); uerr != nil {
				log.Printf("[track] cover store for pial=%s: %v", trackPIAL, uerr)
			} else {
				coverURL = url
			}
		}
	}

	contentType := "audio"
	if isVideo {
		contentType = "video"
	}
	if h.db != nil {
		id, err := dbpkg.InsertTrack(h.db, user.ID, title, description, audioURL, coverURL, 0, genre, []string{}, priceCents)
		if err != nil {
			log.Printf("[track] insert: %v", err)
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		go h.publishMediaUploadedEvent(id, audioURL, user.PIALID)
		// Zior hears the track keyed by its own id; the upload does not wait.
		go h.ziorAnalyzeTrack(id, user.ID, audioHeader.Filename, audioBytes)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": id, "audio_url": audioURL, "content_type": contentType})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"audio_url": audioURL, "content_type": contentType})
}

func (h *Handler) trackLikeAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	_ = r.ParseForm()
	trackID := strings.TrimSpace(r.FormValue("track_id"))
	if trackID == "" {
		http.Error(w, "missing track_id", 400)
		return
	}
	user := h.userFromRequest(w, r)

	liked := false
	if h.db != nil {
		liked, _ = dbpkg.ToggleTrackLike(h.db, user.ID, trackID)
	}

	likedClass := ""
	fill := "none"
	if liked {
		likedClass = "liked"
		fill = "currentColor"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<button class="track-like-btn %s" hx-post="/api/track/like" hx-target="this" hx-swap="outerHTML" hx-vals='{"track_id":"%s"}' title="Like">
		<svg viewBox="0 0 24 24" fill="%s" stroke="currentColor" stroke-width="2"><path d="M20.84 4.61a5.5 5.5 0 0 0-7.78 0L12 5.67l-1.06-1.06a5.5 5.5 0 0 0-7.78 7.78l1.06 1.06L12 21.23l7.78-7.78 1.06-1.06a5.5 5.5 0 0 0 0-7.78z"/></svg>
		</button>`, likedClass, trackID, fill)
}

// trackPlayAction is the listener-interaction endpoint for the music player
// (POST /api/track/play, 204, no body). Form fields:
//
//	track_id      — the track (required)
//	event_type    — track_play | track_skip | track_complete | track_replay
//	                (also track_share, track_save; the bare kind is accepted
//	                too). Absent means track_play, which is what the player
//	                has always posted.
//	position_secs — playhead when the event happened
//	duration_secs — the track's length as the player knows it
//	session_id    — the page session, so Zior can tell one sitting from another
//
// A play still counts on the track's row; every event is forwarded to Zior,
// which folds it into the track's behavioural vector, retention and velocity.
// The forward is detached: the player never waits on Zior.
func (h *Handler) trackPlayAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	_ = r.ParseForm()
	trackID := strings.TrimSpace(r.FormValue("track_id"))
	if trackID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	eventName := r.FormValue("event_type")
	if eventName == "" {
		eventName = "track_play"
	}
	eventType, ok := zior.ParsePlayEventType(eventName)
	if !ok {
		http.Error(w, "unknown event_type: "+eventName, http.StatusBadRequest)
		return
	}
	if h.db != nil && eventType == zior.EventPlay {
		_ = dbpkg.IncrementTrackPlay(h.db, trackID)
	}

	user := h.userFromRequest(w, r)
	listener := ""
	if user != nil {
		listener = user.ID
	}
	position, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("position_secs")), 64)
	duration, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("duration_secs")), 64)
	now := time.Now().UTC()
	h.zior().SendEvents([]zior.PlayEvent{{
		TrackID:      trackID,
		UserID:       listener,
		EventType:    eventType,
		PositionSecs: position,
		DurationSecs: duration,
		SessionID:    truncate(strings.TrimSpace(r.FormValue("session_id")), 128),
		Timestamp:    &now,
	}})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteTrackAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	_ = r.ParseForm()
	trackID := strings.TrimSpace(r.FormValue("track_id"))
	if trackID == "" {
		http.Error(w, "missing track_id", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	if h.db != nil {
		deleted, err := dbpkg.DeleteTrack(h.db, trackID, user.ID)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		if !deleted {
			http.Error(w, "not found or forbidden", http.StatusForbidden)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"deleted": true})
}

func (h *Handler) fetchClusters(ctx context.Context) []model.MusicCluster {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = c
	return []model.MusicCluster{}
}
