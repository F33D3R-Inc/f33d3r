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
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/sitra"
)

// VideoResult is what a successful video upload returns to the client.
type VideoResult struct {
	Kind      string  `json:"kind"`          // "video"
	MasterURL string  `json:"master_url"`    // HLS master.m3u8
	PosterURL string  `json:"poster_url"`    // poster.jpg
	Duration  float32 `json:"duration_secs"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
}

// transcodingVideoUpload forwards a video file to the transcoding brain and
// blocks until the HLS ladder + poster are produced (or timeout / failure).
// Uses videoClient (2-hour timeout) so large file uploads don't time out mid-transfer.
func (h *Handler) transcodingVideoUpload(fileBytes []byte, filename string) (*VideoResult, error) {
	if h.transcodingURL == "" {
		return nil, fmt.Errorf("TRANSCODING_URL not configured")
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

	req, err := http.NewRequest(http.MethodPost, h.transcodingURL+"/v1/video/upload", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.videoClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("transcoding upload returned %d", resp.StatusCode)
	}

	var queued struct {
		JobID   string `json:"job_id"`
		AssetID string `json:"asset_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&queued); err != nil {
		return nil, err
	}

	// Poll until ready. 30-minute cap handles 4K sources on CPU-only nodes.
	deadline := time.Now().Add(30 * time.Minute)
	backoff := 1 * time.Second
	statusURL := h.transcodingURL + "/v1/video/job/" + queued.JobID
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("transcoding timed out (job %s)", queued.JobID)
		}
		time.Sleep(backoff)
		if backoff < 8*time.Second {
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
		_ = json.NewDecoder(jr.Body).Decode(&job)
		jr.Body.Close()

		switch job.Status {
		case "ready":
			if job.Output == nil {
				return nil, fmt.Errorf("transcoding returned ready but no output")
			}
			return &VideoResult{
				Kind:      "video",
				MasterURL: job.Output.MasterURL,
				PosterURL: job.Output.PosterURL,
				Duration:  float32(job.Output.DurationSeconds),
				Width:     job.Output.SourceWidth,
				Height:    job.Output.SourceHeight,
			}, nil
		case "failed":
			return nil, fmt.Errorf("transcoding failed: %s", job.Error)
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
	// Memory ceiling for multipart parsing. Videos stream to disk beyond this.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "could not parse upload", http.StatusBadRequest)
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
	// Validate magic bytes to prevent extension spoofing (e.g. .html renamed to .jpg).
	{
		header := make([]byte, 12)
		if n, _ := file.Read(header); n >= 4 {
			ok := false
			if imageExt[ext] {
				// JPEG: FF D8 FF
				if header[0] == 0xFF && header[1] == 0xD8 && header[2] == 0xFF { ok = true }
				// PNG:  89 50 4E 47
				if header[0] == 0x89 && header[1] == 0x50 && header[2] == 0x4E && header[3] == 0x47 { ok = true }
				// GIF:  47 49 46 38 (GIF8)
				if string(header[:4]) == "GIF8" { ok = true }
				// WebP: 52 49 46 46 (RIFF) at 0-3, WEBP at 8-11
				if n >= 12 && string(header[:4]) == "RIFF" && string(header[8:12]) == "WEBP" { ok = true }
			} else if videoExt[ext] {
				// MP4/MOV: ftyp at bytes 4-7
				if n >= 8 && string(header[4:8]) == "ftyp" { ok = true }
				// WebM/MKV: 0x1A 0x45 0xDF 0xA3
				if header[0] == 0x1A && header[1] == 0x45 && header[2] == 0xDF && header[3] == 0xA3 { ok = true }
			}
			if !ok {
				http.Error(w, "file content does not match declared type", http.StatusBadRequest)
				return
			}
		}
		// Seek back so subsequent reads start from the beginning.
		file.Seek(0, 0)
	}

	w.Header().Set("Content-Type", "application/json")

	// ── Video path: non-blocking — save to disk, queue transcoding, return job_id ──
	// The client polls /upload/tus/status/{upload_id} until status == "ready".
	if videoExt[ext] {
		if err := os.MkdirAll(tusTempDir, 0755); err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		uploadID := uuid.New().String()
		tmpPath := filepath.Join(tusTempDir, uploadID)
		tmpFile, ferr := os.Create(tmpPath)
		if ferr != nil {
			log.Printf("[upload] create temp: %v", ferr)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		videoHasher := sha256.New()
		written, cerr := io.Copy(tmpFile, io.TeeReader(io.LimitReader(file, 2<<30), videoHasher))
		tmpFile.Close()
		if cerr != nil {
			os.Remove(tmpPath)
			http.Error(w, "write error", http.StatusInternalServerError)
			return
		}

		// SHA-256 computed during copy — used for CSAM check and dedup below.
		videoSHA256 := hex.EncodeToString(videoHasher.Sum(nil))

		// Uploader identity — needed for CSAM log, dedup check, and raw hash store.
		uploader := h.userFromRequest(nil, r)
		var uploaderPIAL, uploaderHandle string
		if uploader != nil {
			uploaderPIAL = uploader.PIALID
			uploaderHandle = uploader.Handle
		}

		if h.db != nil {
			// CSAM banned-hash check — synchronous, before the transcoding brain or Caeor
			// receives the file. If matched: delete temp file, nothing is stored, 400 returned.
			if banned, banCat, _ := dbpkg.IsBannedHash(h.db, "sha256", videoSHA256); banned {
				os.Remove(tmpPath)
				h.db.Exec(`INSERT INTO csam_scan_log (media_url, pial_id, content_type, result, score) VALUES ($1,$2,'video','flagged',1.0)`,
					fh.Filename, uploaderPIAL)
				log.Printf("[csam-BLOCK] banned sha256 %s (%s) — video upload blocked, pial=%s", videoSHA256[:16], banCat, uploaderPIAL)
				http.Error(w, "This content cannot be uploaded", http.StatusBadRequest)
				return
			}

			// Duplicate check — synchronous, same architecture as image dedup.
			// SHA-256 of the raw file is the primary key. Single indexed DB lookup.
			// If found: delete temp file, return duplicate info — no transcoding ever starts.
			origHandle, origPostID, found, _ := dbpkg.CheckVideoRawHash(h.db, videoSHA256)
			if found {
				os.Remove(tmpPath)
				isSelf := origHandle == uploaderHandle
				status := "duplicate"
				if isSelf {
					status = "self_duplicate"
				}
				var masterURL, posterURL string
				var duration float32
				var width, height int
				if origPostID != "" {
					h.db.QueryRow(`
						SELECT COALESCE(video_master_url,''), COALESCE(video_poster_url,''),
						       COALESCE(video_duration_secs,0)::REAL, COALESCE(video_width,0), COALESCE(video_height,0)
						FROM works WHERE id = $1`, origPostID).
						Scan(&masterURL, &posterURL, &duration, &width, &height)
				}
				log.Printf("[dedup] video duplicate blocked: uploader=@%s original=@%s post=%s", uploaderHandle, origHandle, origPostID)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status":           status,
					"original_handle":  origHandle,
					"original_post_id": origPostID,
					"master_url":       masterURL,
					"poster_url":       posterURL,
					"duration_secs":    duration,
					"width":            width,
					"height":           height,
				})
				return
			}

			// New file — store its raw SHA-256 so future uploads of the same file
			// are caught at this exact point before transcoding is ever queued.
			_ = dbpkg.StoreVideoRawHash(h.db, videoSHA256, uploaderPIAL, uploaderHandle)
		}

		upload := &tusUpload{
			ID:             uploadID,
			Length:         written,
			Offset:         written,
			Filename:       fh.Filename,
			Created:        time.Now(),
			UploaderPIAL:   uploaderPIAL,
			UploaderHandle: uploaderHandle,
			path:           tmpPath,
		}
		globalTusStore.mu.Lock()
		globalTusStore.uploads[uploadID] = upload
		globalTusStore.mu.Unlock()

		go h.tusTriggerTranscode(upload)

		json.NewEncoder(w).Encode(map[string]string{
			"kind":      "video_queued",
			"upload_id": uploadID,
		})
		return
	}

	// ── Image path ─────────────────────────────────────────────────────────────
	fileBytes, err := io.ReadAll(io.LimitReader(file, 20<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	// Compute SHA-256 once for both the CSAM check and the raw-hash dedup store.
	imgHashBytes := sha256.Sum256(fileBytes)
	imgSHA256 := hex.EncodeToString(imgHashBytes[:])

	// CSAM banned-hash check — synchronous, before Caeor receives the file.
	// If matched: nothing is stored, 400 returned immediately.
	if h.db != nil {
		if banned, banCat, _ := dbpkg.IsBannedHash(h.db, "sha256", imgSHA256); banned {
			u := h.userFromRequest(nil, r)
			pial := ""
			if u != nil {
				pial = u.PIALID
			}
			h.db.Exec(`INSERT INTO csam_scan_log (media_url, pial_id, content_type, result, score) VALUES ($1,$2,'image','flagged',1.0)`,
				fh.Filename, pial)
			log.Printf("[csam-BLOCK] banned sha256 %s (%s) — image upload blocked, pial=%s", imgSHA256[:16], banCat, pial)
			http.Error(w, "This content cannot be uploaded", http.StatusBadRequest)
			return
		}
	}

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

	// ── Image dedup check ──────────────────────────────────────────────────────
	// Ask content-scan whether this image already exists on the platform.
	// If it does, discard the newly uploaded copy and return the canonical URL
	// so the compose box warns the user and swaps in the existing media.
	imgUploader := h.userFromRequest(nil, r)
	uploaderPIAL, uploaderHandle := "", ""
	if imgUploader != nil {
		uploaderPIAL = imgUploader.PIALID
		uploaderHandle = imgUploader.Handle
	}
	// Image dedup via Abraxas Shield Phase 3.1 (media hash events). Fail-open for now.

	// Register canonical ownership so lineage attribution works when a different
	// user uploads the same image later. ON CONFLICT DO NOTHING preserves the
	// first uploader's ownership even if called multiple times for the same URL.
	if h.db != nil && imgUploader != nil {
		if err := dbpkg.CreateCanonicalImage(h.db, imgUploader.ID, imgUploader.PIALID, imgUploader.Handle, mediaURL); err != nil {
			log.Printf("[canonical] image register error for %s: %v", mediaURL, err)
		}
		// Seed raw-hash table so future browser pre-checks hit before byte 1 is sent.
		_ = dbpkg.StoreImageRawHash(h.db, imgSHA256, uploaderPIAL, uploaderHandle, mediaURL)
	}

	// Publish media.uploaded to Sitra Achra so Abraxas Shield can begin
	// content scoring as soon as the asset is confirmed stored.
	// postID is empty at compose-time (post not yet created) — skip in that case.
	postID := r.FormValue("post_id")
	if postID != "" {
		go h.publishMediaUploadedEvent(postID, mediaURL, uploaderPIAL)
	}

	json.NewEncoder(w).Encode(map[string]string{"kind": "image", "url": mediaURL})
}

// uploadVoicePost accepts a raw audio blob (webm/ogg/opus) for voice posts.
// Saves locally and returns {kind:"voice", url:"...", duration_secs:N}.
func (h *Handler) uploadVoicePost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	if err := r.ParseMultipartForm(50 << 20); err != nil {
		http.Error(w, "could not parse upload", http.StatusBadRequest)
		return
	}
	file, fh, err := r.FormFile("audio")
	if err != nil {
		http.Error(w, "no file provided", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Resolve uploader once — reused for CSAM log and Sitra event.
	u := h.userFromRequest(nil, r)

	ext := strings.ToLower(filepath.Ext(fh.Filename))
	audioExt := map[string]bool{".webm": true, ".ogg": true, ".opus": true, ".mp3": true, ".m4a": true, ".aac": true}
	if !audioExt[ext] {
		ext = ".webm" // MediaRecorder blobs often come without extension
	}

	data, err := io.ReadAll(io.LimitReader(file, 50<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	// Validate magic bytes — reject files that are not recognised audio formats.
	// This prevents executables or ZIPs renamed to .webm from being stored.
	if len(data) >= 4 {
		isAudio := false
		// WebM/MKV: 0x1A 0x45 0xDF 0xA3
		if data[0] == 0x1A && data[1] == 0x45 && data[2] == 0xDF && data[3] == 0xA3 { isAudio = true }
		// OGG: OggS
		if len(data) >= 4 && string(data[:4]) == "OggS" { isAudio = true }
		// MP3: ID3 or 0xFF 0xFB/0xF3/0xF2 sync word
		if string(data[:3]) == "ID3" { isAudio = true }
		if data[0] == 0xFF && (data[1] == 0xFB || data[1] == 0xF3 || data[1] == 0xF2) { isAudio = true }
		// AAC ADTS: 0xFF 0xF1 or 0xFF 0xF9
		if data[0] == 0xFF && (data[1] == 0xF1 || data[1] == 0xF9) { isAudio = true }
		// M4A/MP4: ftyp box at offset 4
		if len(data) >= 8 && string(data[4:8]) == "ftyp" { isAudio = true }
		if !isAudio {
			http.Error(w, "unsupported or invalid audio file", http.StatusBadRequest)
			return
		}
	}

	// CSAM hash check — synchronous, pre-storage. Mirrors the image/video pipeline.
	if h.db != nil {
		voiceHasher := sha256.New()
		voiceHasher.Write(data)
		voiceSHA256 := hex.EncodeToString(voiceHasher.Sum(nil))
		if banned, banCat, _ := dbpkg.IsBannedHash(h.db, "sha256", voiceSHA256); banned {
			pial := ""
			if u != nil { pial = u.PIALID }
			h.db.Exec(`INSERT INTO csam_scan_log (media_url, pial_id, content_type, result, score) VALUES ($1,$2,'audio','flagged',1.0)`,
				fh.Filename, pial)
			log.Printf("[csam-BLOCK] banned sha256 voice %s (%s) — pial=%s", voiceSHA256[:16], banCat, pial)
			http.Error(w, "This content cannot be uploaded", http.StatusBadRequest)
			return
		}
	}

	dir := filepath.Join("web", "static", "uploads", "voice")
	if err := os.MkdirAll(dir, 0755); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	name := uuid.New().String() + ext
	if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"kind": "voice",
		"url":  "/static/uploads/voice/" + name,
	})

	// Publish to Sitra Achra for Abraxas Shield async scoring.
	if u != nil && u.PIALID != "" {
		go h.publishMediaUploadedEvent("", "/static/uploads/voice/"+name, u.PIALID)
	}
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

// checkVideoHashHandler is the pre-upload hash gate for video files.
// The client computes SHA-256 locally and posts only the hex here.
// If the hash matches a known file the upload is skipped entirely.
// POST /upload/check-video-hash  body: sha256=<hex>
func (h *Handler) checkVideoHashHandler(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if h.db == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"unknown"}`))
		return
	}

	sha256hex := r.FormValue("sha256")
	if sha256hex == "" {
		http.Error(w, "sha256 required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	origHandle, postID, found, err := dbpkg.CheckVideoRawHash(h.db, sha256hex)
	if err != nil {
		log.Printf("[check-video-hash] DB error: %v", err)
		// Fail-open: let the upload proceed rather than blocking on a DB error.
		w.Write([]byte(`{"status":"unknown"}`))
		return
	}
	if !found {
		w.Write([]byte(`{"status":"unknown"}`))
		return
	}

	isSelf := origHandle == user.Handle
	status := "duplicate"
	if isSelf {
		status = "self_duplicate"
	}

	// Fetch canonical video URLs from the works table so the compose box can
	// reference the original. video_raw_hashes.post_id stores a work UUID.
	var masterURL, watermarkedURL, posterURL string
	var duration float32
	var width, height int
	if postID != "" {
		h.db.QueryRow(`
			SELECT COALESCE(video_master_url,''), COALESCE(video_watermarked_url,''),
			       COALESCE(video_poster_url,''),
			       COALESCE(video_duration_secs,0)::REAL, COALESCE(video_width,0), COALESCE(video_height,0)
			FROM works WHERE id = $1::uuid AND deleted_at IS NULL`, postID).
			Scan(&masterURL, &watermarkedURL, &posterURL, &duration, &width, &height)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":                status,
		"original_handle":       origHandle,
		"original_post_id":      postID,
		"master_url":            masterURL,
		"watermarked_mp4_url": watermarkedURL,
		"poster_url":            posterURL,
		"duration_secs":         duration,
		"width":                 width,
		"height":                height,
	})
}

// checkImageHashHandler is the pre-upload hash gate for image files.
// The client computes SHA-256 locally and posts only the hex here.
// If the hash matches a known file the canonical URL is returned immediately.
// POST /upload/check-image-hash  body: sha256=<hex>
func (h *Handler) checkImageHashHandler(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if h.db == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"unknown"}`))
		return
	}

	sha256hex := r.FormValue("sha256")
	if sha256hex == "" {
		http.Error(w, "sha256 required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// Banned-hash gate — same check as the upload path.
	if banned, banCat, _ := dbpkg.IsBannedHash(h.db, "sha256", sha256hex); banned {
		log.Printf("[check-image-hash] banned sha256 %s (%s) — blocked pre-upload, pial=%s", sha256hex[:16], banCat, user.PIALID)
		http.Error(w, "This content cannot be uploaded", http.StatusForbidden)
		return
	}

	mediaURL, origHandle, found, err := dbpkg.CheckImageRawHash(h.db, sha256hex)
	if err != nil {
		log.Printf("[check-image-hash] DB error: %v", err)
		w.Write([]byte(`{"status":"unknown"}`))
		return
	}
	if !found {
		w.Write([]byte(`{"status":"unknown"}`))
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "duplicate",
		"url":             mediaURL,
		"original_handle": origHandle,
	})
}

// checkVideoDedupHandler is the pre-upload gate for video dedup.
// The client hashes the full file locally before starting the TUS upload and
// calls this endpoint. If the raw SHA-256 is already in video_raw_hashes the
// upload is blocked before a single byte is transferred — matching the image
// dedup architecture exactly.
// POST /upload/check-dedup  body: {"sha256":"<hex>","type":"video"}
func (h *Handler) checkVideoDedupHandler(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if h.db == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"clean"}`))
		return
	}

	var req struct {
		SHA256 string `json:"sha256"`
		Type   string `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SHA256 == "" {
		http.Error(w, "sha256 required", http.StatusBadRequest)
		return
	}

	originalHandle, postID, found, err := dbpkg.CheckVideoRawHash(h.db, req.SHA256)
	if err != nil {
		log.Printf("[check-dedup] DB error: %v", err)
		// Fail-open: let the upload proceed rather than blocking on a DB error.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"clean"}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if !found {
		w.Write([]byte(`{"status":"clean"}`))
		return
	}

	isSelf := originalHandle == user.Handle
	status := "duplicate"
	if isSelf {
		status = "self_duplicate"
	}

	// Fetch canonical video URLs so the compose box can reference the original.
	var masterURL, posterURL string
	var duration float32
	var width, height int
	if postID != "" {
		h.db.QueryRow(`
			SELECT COALESCE(video_master_url,''), COALESCE(video_poster_url,''),
			       COALESCE(video_duration_secs,0)::REAL, COALESCE(video_width,0), COALESCE(video_height,0)
			FROM works WHERE id = $1`, postID).
			Scan(&masterURL, &posterURL, &duration, &width, &height)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":           status,
		"original_handle":  originalHandle,
		"original_post_id": postID,
		"master_url":       masterURL,
		"poster_url":       posterURL,
		"duration_secs":    duration,
		"width":            width,
		"height":           height,
	})
}

// duplicateVideoInfo holds the result of a synchronous duplicate check.
// All fields match the required JSON response shape for the compose box.
type duplicateVideoInfo struct {
	// Legacy / internal fields kept for backward-compat with tusJobStatus.
	OriginalPIAL      string
	OriginalHandle    string
	OriginalPostID    string // work UUID (legacy column name)
	ExistingMasterURL string
	ExistingPosterURL string
	DurationSecs      float64
	Width             int
	Height            int
	// Extended fields — populated by CheckVideoDuplicateByWork.
	CreatorName string
	WorkID      string
}

// syncCheckDuplicate — video dedup was handled by the Python content-scan brain.
// Abraxas Shield (Phase 3) is a pure Kafka consumer with no HTTP endpoint.
// Video-level dedup will be added in Phase 3.1 via media hash events.
// Fail-open: always returns nil so uploads proceed normally.
func (h *Handler) syncCheckDuplicate(videoMasterURL, uploaderPIAL, uploaderHandle string) *duplicateVideoInfo {
	return nil
}

// checkVideoDuplicateByBytes checks whether the raw file bytes match a video
// already stored on F33D3R by comparing SHA-256 against video_raw_hashes.
// Returns nil when the file is original (not a duplicate).
// Fail-open: any DB error returns nil so transcoding proceeds.
func (h *Handler) checkVideoDuplicateByBytes(fileBytes []byte, filename, uploaderPIAL, uploaderHandle string) *duplicateVideoInfo {
	if h.db == nil || len(fileBytes) == 0 {
		return nil
	}
	hasher := sha256.New()
	hasher.Write(fileBytes)
	rawHash := hex.EncodeToString(hasher.Sum(nil))

	res, err := dbpkg.CheckVideoDuplicateByWork(h.db, rawHash)
	if err != nil {
		log.Printf("[dedup] CheckVideoDuplicateByWork error: %v", err)
		return nil // fail-open
	}
	if res == nil {
		return nil // not a duplicate
	}
	return &duplicateVideoInfo{
		OriginalPIAL:      res.CreatorPIAL,
		OriginalHandle:    res.CreatorHandle,
		OriginalPostID:    res.WorkID,
		ExistingMasterURL: res.MasterURL,
		ExistingPosterURL: res.PosterURL,
		DurationSecs:      res.DurationSecs,
		Width:             res.Width,
		Height:            res.Height,
		CreatorName:       res.CreatorName,
		WorkID:            res.WorkID,
	}
}

// imageDuplicateInfo holds the result of a synchronous image dedup check.
type imageDuplicateInfo struct {
	OriginalHandle string
	OriginalPIAL   string
	OriginalPostID string
	ImageURL       string // canonical image URL from the original upload
}

// checkImageDuplicate — image dedup via content-scan HTTP is removed.
// Abraxas Shield Phase 3.1 will add image hash events. Fail-open for now.
func (h *Handler) checkImageDuplicate(imageBytes []byte, uploadedURL, uploaderPIAL, uploaderHandle string) *imageDuplicateInfo {
	return nil
}

// cleanupDuplicateImage deletes a newly uploaded image from Caeor when it
// is confirmed as a duplicate. Best-effort — errors only logged.
func (h *Handler) cleanupDuplicateImage(imageURL string) {
	h.caeorDelete(imageURL)
}

// cleanupDuplicateTranscoded deletes orphaned HLS transcoding output from Caeor
// when a duplicate video is detected before the post is created.
func (h *Handler) cleanupDuplicateTranscoded(masterURL string) {
	if masterURL == "" {
		return
	}
	h.caeorDelete(masterURL)
}

// caeorDelete calls Caeor's DELETE /v1/media endpoint to remove a media asset.
// Best-effort: errors are logged but never fatal to the caller.
func (h *Handler) caeorDelete(mediaURL string) {
	if h.cfg.CaeorURL == "" || mediaURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"url": mediaURL})
	req, err := http.NewRequest(http.MethodDelete, h.cfg.CaeorURL+"/v1/media", bytes.NewReader(body))
	if err != nil {
		log.Printf("[caeor] delete request build error for %s: %v", mediaURL, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.httpClient.Do(req)
	if err != nil {
		log.Printf("[caeor] delete error for %s: %v", mediaURL, err)
		return
	}
	resp.Body.Close()
	log.Printf("[caeor] deleted orphaned asset: %s (status %d)", mediaURL, resp.StatusCode)
}


// uploadEncryptedMedia stores a client-encrypted blob (marketplace paid content).
// The bytes are already AES-256-GCM encrypted by the browser, so the server stays
// zero-knowledge; it only persists opaque
// ciphertext and hands back a URL. No server-side processing. Up to 1 GiB.
// Returns {"url": "/static/uploads/encrypted/<uuid>.enc"}
// POST /api/media/upload-encrypted
func (h *Handler) uploadEncryptedMedia(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	// Keep only 32 MB in memory; anything larger spills to a temp file, so a 600 MB–1 GiB encrypted
	// blob streams through without buffering the whole payload in RAM.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"file_too_large"}`, http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"no_file"}`, http.StatusBadRequest)
		return
	}
	defer file.Close()

	dir := filepath.Join("web", "static", "uploads", "encrypted")
	if err := os.MkdirAll(dir, 0755); err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"storage_error"}`, http.StatusInternalServerError)
		return
	}
	name := uuid.New().String() + ".enc"
	dst, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"storage_error"}`, http.StatusInternalServerError)
		return
	}
	defer dst.Close()
	if _, err := io.Copy(dst, io.LimitReader(file, 1<<30)); err != nil { // 1 GiB ceiling (clears the 600 MB floor)
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"write_error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"url": "/static/uploads/encrypted/" + name,
	})
}

// publishMediaUploadedEvent fires a content.events message to Sitra Achra
// after a media asset is confirmed stored in Caeor.
func (h *Handler) publishMediaUploadedEvent(postID, mediaURL, pialID string) {
	if h.sitra == nil {
		return
	}
	payload, err := json.Marshal(map[string]string{
		"event":     "media.uploaded",
		"post_id":   postID,
		"media_url": mediaURL,
		"pial_id":   pialID,
	})
	if err != nil {
		return
	}
	h.sitra.Publish(context.Background(), sitra.TopicContent, []byte(pialID), payload)
}
