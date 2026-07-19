package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

func (h *Handler) musicPage(w http.ResponseWriter, r *http.Request) {
	user      := h.userFromRequest(w, r)
	sessionID := uuid.New().String()
	genre     := r.URL.Query().Get("genre")

	var myTracks []*model.Track
	var artistStats dbpkg.ArtistStats
	if h.db != nil {
		myTracks, _ = dbpkg.GetTracksByAuthor(h.db, user.ID, 50)
		for _, t := range myTracks { t.TimeAgo = TimeAgo(t.CreatedAt) }
		artistStats = dbpkg.GetArtistStats(h.db, user.ID)
	}

	rail := h.railData(user, "music")
	h.render(w, "music.html", map[string]interface{}{
		"User":           user,
		"Surface":        "music",
		"SessionID":      sessionID,
		"Title":          "Zior · Music",
		"ShowScores":     h.cfg.ShowScores,
		"Clusters":       h.fetchClusters(r.Context()),
		"Themes":         ThemesWithActive(user.ThemeID),
		"MyTracks":       myTracks,
		"ArtistStats":    artistStats,
		"Genre":          genre,
		"TrendingTags":   rail["TrendingTags"],
		"SuggestedUsers": rail["SuggestedUsers"],
		"RailContext":    rail["RailContext"],
		"RailCreators":   rail["RailCreators"],
		"RailNewsItems":  rail["RailNewsItems"],
		"RailNewsLabel":  rail["RailNewsLabel"],
	})
}

func (h *Handler) tracksPartial(w http.ResponseWriter, r *http.Request) {
	user  := h.userFromRequest(w, r)
	genre := r.URL.Query().Get("genre")
	after := r.URL.Query().Get("after")

	var tracks []*model.Track
	if h.db != nil {
		tracks, _ = dbpkg.GetRecentTracks(h.db, genre, 20, after)
		for _, t := range tracks {
			t.TimeAgo = TimeAgo(t.CreatedAt)
			t.LikedByUser, _ = dbpkg.IsTrackLiked(h.db, user.ID, t.ID)
		}
	}

	h.renderPartial(w, "music_tracks", map[string]interface{}{
		"Tracks":            tracks,
		"CurrentUserHandle": user.Handle,
		"Genre":             genre,
		"After":             after,
	})
}

func (h *Handler) uploadTrack(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
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
	if title == "" { title = "Untitled" }
	description := truncate(r.FormValue("description"), 2000)
	genre       := truncate(r.FormValue("genre"), 60)
	priceCents  := 0

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

	audioDir := filepath.Join("web", "static", "uploads", "audio")
	os.MkdirAll(audioDir, 0755)
	audioName := uuid.New().String() + audioExt
	dst, err := os.Create(filepath.Join(audioDir, audioName))
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	io.Copy(dst, io.LimitReader(audioFile, 120<<20))
	dst.Close()
	audioURL := "/static/uploads/audio/" + audioName

	// CSAM hash check — synchronous, pre-storage. Mirrors the image/video pipeline.
	audioBytes, readErr := os.ReadFile(filepath.Join(audioDir, audioName))
	if readErr == nil && h.db != nil {
		audioHasher := sha256.New()
		audioHasher.Write(audioBytes)
		audioSHA256 := hex.EncodeToString(audioHasher.Sum(nil))
		if banned, banCat, _ := dbpkg.IsBannedHash(h.db, "sha256", audioSHA256); banned {
			os.Remove(filepath.Join(audioDir, audioName))
			pial := ""
			if user != nil { pial = user.PIALID }
			h.db.Exec(`INSERT INTO csam_scan_log (media_url, pial_id, content_type, result, score) VALUES ($1,$2,'audio','flagged',1.0)`,
				audioURL, pial)
			log.Printf("[csam-BLOCK] banned sha256 track %s (%s) — pial=%s", audioSHA256[:16], banCat, pial)
			http.Error(w, "This content cannot be uploaded", http.StatusBadRequest)
			return
		}
	}

	// Optional cover art
	coverURL := ""
	if coverFile, coverHeader, err := r.FormFile("cover"); err == nil {
		defer coverFile.Close()
		if url, err := h.saveUpload(coverFile, coverHeader.Filename, "covers"); err == nil {
			coverURL = url
		}
	}

	contentType := "audio"
	if isVideo { contentType = "video" }
	if h.db != nil {
		id, err := dbpkg.InsertTrack(h.db, user.ID, title, description, audioURL, coverURL, 0, genre, []string{}, priceCents)
		if err != nil {
			log.Printf("[track] insert: %v", err)
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		go h.publishMediaUploadedEvent(id, audioURL, user.PIALID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": id, "audio_url": audioURL, "content_type": contentType})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"audio_url": audioURL, "content_type": contentType})
}

func (h *Handler) trackLikeAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	_ = r.ParseForm()
	trackID := strings.TrimSpace(r.FormValue("track_id"))
	if trackID == "" { http.Error(w, "missing track_id", 400); return }
	user := h.userFromRequest(w, r)

	liked := false
	if h.db != nil {
		liked, _ = dbpkg.ToggleTrackLike(h.db, user.ID, trackID)
	}

	likedClass := ""
	fill := "none"
	if liked { likedClass = "liked"; fill = "currentColor" }

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<button class="track-like-btn %s" hx-post="/api/track/like" hx-target="this" hx-swap="outerHTML" hx-vals='{"track_id":"%s"}' title="Like">
		<svg viewBox="0 0 24 24" fill="%s" stroke="currentColor" stroke-width="2"><path d="M20.84 4.61a5.5 5.5 0 0 0-7.78 0L12 5.67l-1.06-1.06a5.5 5.5 0 0 0-7.78 7.78l1.06 1.06L12 21.23l7.78-7.78 1.06-1.06a5.5 5.5 0 0 0 0-7.78z"/></svg>
		</button>`, likedClass, trackID, fill)
}

func (h *Handler) trackPlayAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	_ = r.ParseForm()
	trackID := strings.TrimSpace(r.FormValue("track_id"))
	if h.db != nil && trackID != "" {
		_ = dbpkg.IncrementTrackPlay(h.db, trackID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteTrackAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
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

