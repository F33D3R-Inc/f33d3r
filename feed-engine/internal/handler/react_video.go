package handler

// react_video.go — React With Video studio entry.
//
// GET /react-video/{id} renders the capture studio for a target work. The
// original is embedded server-side (reusing the quote template — "paste like
// quotes, don't build a work"); the client boots the camera substrate
// (CaptureKit.*) into the returned overlay and records a reaction. The reaction
// posts back through the normal video + quote path as kind='react_video'.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/google/uuid"
)

func (h *Handler) reactVideoStudio(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	workID := r.PathValue("id")
	if workID == "" {
		http.Error(w, "work id required", http.StatusBadRequest)
		return
	}

	work, err := dbpkg.GetWorkByID(h.db, workID, user.ID)
	if err != nil || work == nil {
		http.Error(w, "work not found", http.StatusNotFound)
		return
	}

	h.renderPartial(w, "react_video_studio", map[string]interface{}{
		"Work": work,
		"User": user,
	})
}

// uploadReactVideo stores a React-With-Video reaction clip directly (no HLS
// transcode) and returns its URL. Reaction clips are short WebM recordings, and
// green-screen reactions carry an alpha channel the HLS ladder would flatten —
// so they are served as raw WebM and played with a plain <video>. Mirrors
// uploadVoicePost: magic-byte validation + CSAM hash gate + local store.
// POST /upload/react-video → {url}
func (h *Handler) uploadReactVideo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(256 << 20); err != nil {
		http.Error(w, "could not parse upload", http.StatusBadRequest)
		return
	}
	file, fh, err := r.FormFile("video")
	if err != nil {
		http.Error(w, "no file provided", http.StatusBadRequest)
		return
	}
	defer file.Close()

	u := h.userFromRequest(nil, r)

	ext := strings.ToLower(filepath.Ext(fh.Filename))
	if ext != ".webm" && ext != ".mp4" {
		ext = ".webm" // MediaRecorder blobs often arrive without an extension
	}

	data, err := io.ReadAll(io.LimitReader(file, 256<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	// Validate magic bytes — accept only WebM/MKV or MP4 (ftyp box at offset 4).
	valid := false
	if len(data) >= 4 && data[0] == 0x1A && data[1] == 0x45 && data[2] == 0xDF && data[3] == 0xA3 {
		valid = true
	}
	if len(data) >= 8 && string(data[4:8]) == "ftyp" {
		valid = true
	}
	if !valid {
		http.Error(w, "unsupported or invalid video file", http.StatusBadRequest)
		return
	}

	// CSAM hash check — synchronous, pre-storage. Mirrors the media pipeline.
	if h.db != nil {
		hsh := sha256.Sum256(data)
		sum := hex.EncodeToString(hsh[:])
		if banned, banCat, _ := dbpkg.IsBannedHash(h.db, "sha256", sum); banned {
			pial := ""
			if u != nil {
				pial = u.PIALID
			}
			h.db.Exec(`INSERT INTO csam_scan_log (media_url, pial_id, content_type, result, score) VALUES ($1,$2,'video','flagged',1.0)`,
				fh.Filename, pial)
			log.Printf("[csam-BLOCK] banned sha256 react-video %s (%s) — pial=%s", sum[:16], banCat, pial)
			http.Error(w, "This content cannot be uploaded", http.StatusBadRequest)
			return
		}
	}

	dir := filepath.Join("web", "static", "uploads", "react")
	if err := os.MkdirAll(dir, 0755); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	name := uuid.New().String() + ext
	if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	url := "/static/uploads/react/" + name
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": url})

	if u != nil && u.PIALID != "" {
		go h.publishMediaUploadedEvent("", url, u.PIALID)
	}
}
