package handler

// react_video.go — React With Video studio entry.
//
// GET /react-video/{id} renders the capture studio for a target work. The
// original is embedded server-side (reusing the quote template — "paste like
// quotes, don't build a work"); the client boots the camera substrate
// (CaptureKit.*) into the returned overlay and records a reaction. The reaction
// posts back through the normal video + quote path as kind='react_video'.

import (
	"encoding/json"
	"log"
	"net/http"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
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

// uploadReactVideo admits a React-With-Video reaction recording through the
// one video lane (admitVideoUpload): magic bytes, the banned-content gate, then
// Caeor, which transcodes it to the watermarked HLS ladder like every video —
// every viewer-facing copy is watermarked, a reaction included. The studio
// polls /upload/tus/status/{upload_id} and signs the work once it is ready.
// POST /upload/react-video → {kind:"video_queued", upload_id}
func (h *Handler) uploadReactVideo(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "could not parse upload", http.StatusBadRequest)
		return
	}
	file, fh, err := r.FormFile("video")
	if err != nil {
		http.Error(w, "no file provided", http.StatusBadRequest)
		return
	}
	defer file.Close()

	head, err := readMagic(file)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	// MediaRecorder blobs often arrive without an extension; the bytes name it.
	filename, ok := videoFilenameFor(fh.Filename, head)
	if !ok {
		http.Error(w, "unsupported or invalid video file", http.StatusBadRequest)
		return
	}
	upload, err := h.admitVideoUpload(r.Context(), h.userFromRequest(nil, r), filename, file)
	if err != nil {
		uploadHTTPError(w, "react-video", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"kind": "video_queued", "upload_id": upload.ID}); err != nil {
		log.Printf("[react-video] encode answer: %v", err)
	}
}
