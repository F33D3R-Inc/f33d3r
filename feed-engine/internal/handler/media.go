package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// CaeorVideoResult is what a successful video upload returns to the client.
type CaeorVideoResult struct {
	Kind       string  `json:"kind"`             // "video"
	MasterURL  string  `json:"master_url"`       // HLS master.m3u8
	PosterURL  string  `json:"poster_url"`       // poster.jpg
	Duration   float32 `json:"duration_secs"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
}

// caeorVideoUpload forwards a video file to Caeor's transcoder endpoint and
// blocks until the HLS ladder + poster are produced (or timeout / failure).
// Sprint 0 / S0.6 — pragmatic synchronous version. Will become async with
// Kafka in Sprint 1 once the foundation is settled.
func (h *Handler) caeorVideoUpload(fileBytes []byte, filename string) (*CaeorVideoResult, error) {
	if h.caeorURL == "" {
		return nil, fmt.Errorf("caeor URL not configured")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(fileBytes); err != nil {
		return nil, err
	}
	mw.Close()

	req, err := http.NewRequest(http.MethodPost, h.caeorURL+"/v1/video/upload", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("caeor video upload returned %d", resp.StatusCode)
	}
	var queued struct {
		JobID   string `json:"job_id"`
		AssetID string `json:"asset_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&queued); err != nil {
		return nil, err
	}

	// Poll the job. Bound the wait — long videos still finish, but we don't
	// hold the request open forever. Five-minute hard cap; tune later.
	deadline := time.Now().Add(5 * time.Minute)
	backoff := 1 * time.Second
	statusURL := h.caeorURL + "/v1/video/job/" + queued.JobID
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("caeor video transcode timed out (job %s)", queued.JobID)
		}
		time.Sleep(backoff)
		if backoff < 5*time.Second {
			backoff += 500 * time.Millisecond
		}
		jr, err := h.httpClient.Get(statusURL)
		if err != nil {
			continue
		}
		var job struct {
			Status string `json:"status"`
			Error  string `json:"error"`
			Output *struct {
				MasterURL       string  `json:"master_url"`
				PosterURL       string  `json:"poster_url"`
				DurationSeconds float64 `json:"duration_seconds"`
				SourceWidth     int     `json:"source_width"`
				SourceHeight    int     `json:"source_height"`
			} `json:"output"`
		}
		dec := json.NewDecoder(jr.Body)
		_ = dec.Decode(&job)
		jr.Body.Close()
		switch job.Status {
		case "ready":
			if job.Output == nil {
				return nil, fmt.Errorf("caeor returned ready but no output")
			}
			return &CaeorVideoResult{
				Kind:      "video",
				MasterURL: job.Output.MasterURL,
				PosterURL: job.Output.PosterURL,
				Duration:  float32(job.Output.DurationSeconds),
				Width:     job.Output.SourceWidth,
				Height:    job.Output.SourceHeight,
			}, nil
		case "failed":
			return nil, fmt.Errorf("caeor transcode failed: %s", job.Error)
		}
	}
}

// caeorUpload forwards a multipart file to Caeor for processing and returns
// the primary derivative URL. Falls back to saveUpload if Caeor is unavailable.
func (h *Handler) caeorUpload(fileBytes []byte, filename, mediaType string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(fileBytes); err != nil {
		return "", err
	}
	mw.Close()

	req, err := http.NewRequest(http.MethodPost,
		h.caeorURL+"/v1/media/upload?type="+mediaType, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("caeor returned %d", resp.StatusCode)
	}
	var result struct {
		Primary string `json:"primary"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Primary, nil
}

func (h *Handler) uploadAvatar(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "file too large", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	file, fh, err := r.FormFile("avatar")
	if err != nil {
		http.Error(w, "no file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	fileBytes, err := io.ReadAll(io.LimitReader(file, 10<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	url, err := h.caeorUpload(fileBytes, fh.Filename, "avatar")
	if err != nil {
		// Caeor unavailable — fall back to direct storage
		log.Printf("[caeor] unavailable, falling back: %v", err)
		url, err = h.saveUpload(bytes.NewReader(fileBytes), fh.Filename, "avatars")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if h.db != nil {
		_ = dbpkg.SaveProfile(h.db, &model.ProfileSave{
			UserID:    user.ID,
			AvatarURL: url,
			ThemeID:   user.ThemeID,
		})
	}
	// Bust cache so next page load reflects the new avatar.
	if tok := GetSessionToken(r); tok != "" {
		h.sessionCache.Delete(tok)
	}
	if handle := HandleFromCookie(r); handle != "" {
		h.sessionCache.Delete("__handle__" + handle)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": url})
}

func (h *Handler) uploadHeader(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "file too large", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	file, fh, err := r.FormFile("header")
	if err != nil {
		http.Error(w, "no file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	fileBytes, err := io.ReadAll(io.LimitReader(file, 10<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	url, err := h.caeorUpload(fileBytes, fh.Filename, "header")
	if err != nil {
		log.Printf("[caeor] unavailable, falling back: %v", err)
		url, err = h.saveUpload(bytes.NewReader(fileBytes), fh.Filename, "headers")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if h.db != nil {
		_ = dbpkg.SaveProfile(h.db, &model.ProfileSave{
			UserID:    user.ID,
			HeaderURL: url,
			ThemeID:   user.ThemeID,
		})
	}
	if tok := GetSessionToken(r); tok != "" {
		h.sessionCache.Delete(tok)
	}
	if handle := HandleFromCookie(r); handle != "" {
		h.sessionCache.Delete("__handle__" + handle)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": url})
}

func (h *Handler) uploadPostMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	const maxMedia = 10 << 20 // 10 MB
	if err := r.ParseMultipartForm(maxMedia); err != nil {
		http.Error(w, "file too large (10 MB max)", http.StatusBadRequest)
		return
	}
	file, fh, err := r.FormFile("media")
	if err != nil {
		http.Error(w, "no file provided", http.StatusBadRequest)
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(fh.Filename))
	imageExt := map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true}
	videoExt := map[string]bool{".mp4": true, ".mov": true, ".webm": true, ".m4v": true, ".mkv": true}
	if !imageExt[ext] && !videoExt[ext] {
		http.Error(w, "unsupported file type — use jpg, png, gif, webp, mp4, mov, webm, mkv, m4v", http.StatusBadRequest)
		return
	}

	// Video uploads can be very large (4K source). Caeor handles up to 10 GB
	// internally; we still cap reads here to defend against overrun.
	readCap := maxMedia
	if videoExt[ext] {
		readCap = 10 << 30 // 10 GB matches Caeor's MAX_UPLOAD_BYTES
	}
	fileBytes, err := io.ReadAll(io.LimitReader(file, int64(readCap)))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// ── Video path: HLS transcode via Caeor ──────────────────────────────
	if videoExt[ext] {
		result, verr := h.caeorVideoUpload(fileBytes, fh.Filename)
		if verr != nil {
			log.Printf("[caeor] video transcode failed: %v", verr)
			http.Error(w, "video transcode failed: "+verr.Error(), http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(result)
		return
	}

	// ── Image path: existing flow ────────────────────────────────────────
	mediaURL, err := h.caeorUpload(fileBytes, fh.Filename, "post")
	if err != nil {
		log.Printf("[caeor] unavailable for post media, falling back: %v", err)
		dir := filepath.Join("web", "static", "uploads", "posts")
		os.MkdirAll(dir, 0755)
		name := uuid.New().String() + ext
		if werr := os.WriteFile(filepath.Join(dir, name), fileBytes, 0644); werr != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		mediaURL = "/static/uploads/posts/" + name
	}
	json.NewEncoder(w).Encode(map[string]string{"kind": "image", "url": mediaURL})
}

func (h *Handler) saveUpload(file io.Reader, filename, folder string) (string, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	allowed := map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true}
	if !allowed[ext] {
		return "", fmt.Errorf("unsupported file type %q — use jpg, png, gif or webp", ext)
	}
	dir := filepath.Join("web", "static", "uploads", folder)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	name := uuid.New().String() + ext
	dst, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, io.LimitReader(file, 10<<20)); err != nil {
		return "", err
	}
	return "/static/uploads/" + folder + "/" + name, nil
}
