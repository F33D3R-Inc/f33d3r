package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"github.com/f33d3r/feed-engine/internal/scanclient"
	"github.com/f33d3r/feed-engine/internal/sitra"
)

// VideoResult is what a successful video upload returns to the client.
type VideoResult struct {
	Kind      string  `json:"kind"`       // "video"
	MasterURL string  `json:"master_url"` // HLS master.m3u8
	PosterURL string  `json:"poster_url"` // poster.jpg
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

	// An avatar is permanently hosted media and gets the same gate a work does.
	avatarSum := sha256.Sum256(fileBytes)
	if gerr := h.bannedContentGate(r.Context(), gateInput{
		Kind:     scanclient.KindImage,
		Filename: fh.Filename,
		Data:     fileBytes,
		SHA256:   hex.EncodeToString(avatarSum[:]),
		PIALID:   user.PIALID,
		Label:    "avatar-upload",
	}); gerr != nil {
		banGateHTTPError(w, gerr)
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
		// Deliberate: the upload itself succeeded and the URL is still returned.
		// Reported rather than swallowed so a profile that did not take the new
		// avatar is visible in logs.
		if err := dbpkg.SaveProfile(h.db, &model.ProfileSave{
			UserID:    user.ID,
			AvatarURL: url,
			ThemeID:   user.ThemeID,
		}); err != nil {
			log.Printf("[media] avatar uploaded but not saved to profile for %s: %v", user.Handle, err)
		}
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

	// A profile header is permanently hosted media and gets the same gate.
	headerSum := sha256.Sum256(fileBytes)
	if gerr := h.bannedContentGate(r.Context(), gateInput{
		Kind:     scanclient.KindImage,
		Filename: fh.Filename,
		Data:     fileBytes,
		SHA256:   hex.EncodeToString(headerSum[:]),
		PIALID:   user.PIALID,
		Label:    "header-upload",
	}); gerr != nil {
		banGateHTTPError(w, gerr)
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
		// Deliberate, same as the avatar above: reported, not fatal to the upload.
		if err := dbpkg.SaveProfile(h.db, &model.ProfileSave{
			UserID:    user.ID,
			HeaderURL: url,
			ThemeID:   user.ThemeID,
		}); err != nil {
			log.Printf("[media] header uploaded but not saved to profile for %s: %v", user.Handle, err)
		}
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
	if !imageExt[ext] && !videoExtensions[ext] {
		http.Error(w, "unsupported file type — use jpg, png, gif, webp, mp4, mov, webm, mkv, m4v", http.StatusBadRequest)
		return
	}
	// The declared type is what the sender says; the magic number is what the
	// file is. A .html renamed to .jpg ends here.
	head, err := readMagic(file)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	if imageExt[ext] && imageExtensionFor(head) == "" {
		http.Error(w, "file content does not match declared type", http.StatusBadRequest)
		return
	}
	if _, isVideo := sniffVideo(head); videoExtensions[ext] && !isVideo {
		http.Error(w, "file content does not match declared type", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// ── Video path: non-blocking — spool to disk, queue transcoding, return the
	// upload id. The client polls /upload/tus/status/{upload_id} until the job
	// is ready.
	if videoExtensions[ext] {
		adm, err := h.admitVideoUpload(r.Context(), h.userFromRequest(nil, r), fh.Filename, file)
		if err != nil {
			if isGateRefusal(err) {
				banGateHTTPError(w, err)
				return
			}
			log.Printf("[upload] video %q: %v", fh.Filename, err)
			if errors.Is(err, errUploadWrite) {
				http.Error(w, "write error", http.StatusInternalServerError)
				return
			}
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		if adm.Duplicate != nil {
			writeVideoDuplicate(w, adm.Duplicate)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]string{
			"kind":      "video_queued",
			"upload_id": adm.Upload.ID,
		}); err != nil {
			log.Printf("[upload] encode video_queued: %v", err)
		}
		return
	}

	// ── Image path ─────────────────────────────────────────────────────────────
	fileBytes, err := io.ReadAll(io.LimitReader(file, 20<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	mediaURL, gateErr, err := h.admitImageUpload(r, fh.Filename, ext, fileBytes)
	if gateErr != nil {
		banGateHTTPError(w, gateErr)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	// Publish media.uploaded to Sitra Achra so Abraxas Shield can begin
	// content scoring as soon as the asset is confirmed stored.
	// postID is empty at compose-time (post not yet created) — skip in that case.
	if postID := r.FormValue("post_id"); postID != "" {
		uploaderPIAL := ""
		if u := h.userFromRequest(nil, r); u != nil {
			uploaderPIAL = u.PIALID
		}
		go h.publishMediaUploadedEvent(postID, mediaURL, uploaderPIAL)
	}

	json.NewEncoder(w).Encode(map[string]string{"kind": "image", "url": mediaURL})
}

// ── What a file is, by its bytes ─────────────────────────────────────────────
//
// One sniff per medium, shared by the web compose box and the JSON lane. A
// declared type is what the sender says; the magic number is what the file
// is, and it is the bytes that pick the stored extension — a recorder that
// names its output "voice.mp4" or "IMG_0412" gets the extension its bytes
// earn, and a player reading that extension gets a file it can open.

// videoExtensions are the containers the transcoding brain accepts, by the
// name the sender gave the file. Two families: ISO BMFF (.mp4 .mov .m4v) and
// Matroska/EBML (.webm .mkv).
var videoExtensions = map[string]bool{".mp4": true, ".mov": true, ".webm": true, ".m4v": true, ".mkv": true}

// The admission errors a caller turns into a status. Each lane words its own
// answer; what happened is decided once, here.
var (
	// errMediaUnsupported: the bytes are not a type this route accepts.
	errMediaUnsupported = errors.New("media type not accepted")
	// errUploadStorage: the file could not be given a place on disk.
	errUploadStorage = errors.New("upload storage error")
	// errUploadWrite: the file had a place but could not be written there in full.
	errUploadWrite = errors.New("upload write error")
)

// isGateRefusal reports whether err came out of the banned-content gate — a
// registry match or an unanswerable lookup — as opposed to this server
// failing at something else.
func isGateRefusal(err error) bool {
	return errors.Is(err, errMediaBanned) || errors.Is(err, errBanGateUnavailable)
}

// readMagic returns the first twelve bytes of an upload — enough for every
// signature checked here — and rewinds, so the caller reads the file from the
// start. A file shorter than that comes back short, and every sniff refuses it.
func readMagic(file io.ReadSeeker) ([]byte, error) {
	head := make([]byte, 12)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return head[:n], nil
}

// isISOBMFF: an ftyp box at offset 4. MP4, MOV, M4V, M4A and 3GP all share
// this framing; the brand inside the box tells them apart, and no route here
// needs to, because the transcoder and the audio players read the box
// themselves.
func isISOBMFF(head []byte) bool {
	return len(head) >= 8 && string(head[4:8]) == "ftyp"
}

// isEBML: the Matroska/WebM header, 1A 45 DF A3.
func isEBML(head []byte) bool {
	return len(head) >= 4 && head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3
}

// imageExtensionFor names the extension for an image by its bytes; "" for
// anything that is not an accepted image.
func imageExtensionFor(data []byte) string {
	if len(data) < 12 {
		return ""
	}
	switch {
	case data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return ".jpg"
	case data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47:
		return ".png"
	case string(data[:4]) == "GIF8":
		return ".gif"
	case string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return ".webp"
	}
	return ""
}

// sniffVideo names the canonical extension for a video container by its
// bytes — ".mp4" for ISO BMFF, ".webm" for EBML — and false for anything else.
func sniffVideo(head []byte) (string, bool) {
	switch {
	case isISOBMFF(head):
		return ".mp4", true
	case isEBML(head):
		return ".webm", true
	}
	return "", false
}

// videoFilenameFor is the name a video is handed to the transcoder under. The
// transcoder accepts a file by its extension, so the name must carry one it
// knows: the declared extension is kept when it belongs to the container the
// bytes prove, and replaced by that container's canonical extension when it
// does not — a phone that uploads "IMG_0412" or "clip.tmp" still reaches the
// transcoder as a file it accepts. false when the bytes are not video at all.
func videoFilenameFor(declared string, head []byte) (string, bool) {
	canonical, ok := sniffVideo(head)
	if !ok {
		return "", false
	}
	family := map[string]string{".mp4": ".mp4", ".mov": ".mp4", ".m4v": ".mp4", ".webm": ".webm", ".mkv": ".webm"}
	declaredExt := filepath.Ext(declared)
	if family[strings.ToLower(declaredExt)] == canonical {
		return declared, true
	}
	base := strings.TrimSuffix(declared, declaredExt)
	if base == "" {
		base = "upload"
	}
	return base + canonical, true
}

// audioExtensionFor names the extension for an audio file by its bytes; ""
// for anything that is not an accepted audio format. What the recorders send:
// MediaRecorder gives WebM/Opus or Ogg/Opus; Safari's MediaRecorder gives AAC
// in an ISO BMFF box (audio/mp4); iOS AVAudioRecorder gives AAC in an M4A box.
// The last two are the same ftyp framing and store as .m4a. MP3 and raw ADTS
// AAC are accepted for files people already have.
func audioExtensionFor(data []byte) string {
	if len(data) < 4 {
		return ""
	}
	switch {
	case isEBML(data):
		return ".webm"
	case string(data[:4]) == "OggS":
		return ".ogg"
	case string(data[:3]) == "ID3":
		return ".mp3"
	case data[0] == 0xFF && (data[1] == 0xFB || data[1] == 0xF3 || data[1] == 0xF2):
		return ".mp3"
	case data[0] == 0xFF && (data[1] == 0xF1 || data[1] == 0xF9):
		return ".aac"
	case isISOBMFF(data):
		return ".m4a"
	}
	return ""
}

// ── Video admission ──────────────────────────────────────────────────────────

// videoAdmission is what admitting one video came to. Exactly one field is
// set: Upload when the file is new and now in the transcoding pipeline,
// Duplicate when the same bytes are already on F33D3R and nothing was queued.
type videoAdmission struct {
	Upload    *tusUpload
	Duplicate *videoRawDuplicate
}

// admitVideoUpload is the one admission path for a video, whichever route
// received it: the web compose box (uploadPostMedia) and the native clients'
// POST /api/v1/media/video both end here. The bytes have already been proved
// a video container; filename carries an extension the transcoder accepts.
//
// In order: the file is spooled to the tus temp directory (2 GiB cap) with its
// SHA-256 taken on the way past, so a multi-gigabyte video is never held in
// memory; the banned-content gate reads it from disk; the raw hash is looked
// up against every video already admitted, and a match ends here with the
// original's canonical media and no transcoding job; a new hash is recorded
// so the next upload of the same bytes is caught at this same point; then the
// upload is registered and handed to the transcoder in the background.
//
// A gate refusal comes back as the gate's own error (isGateRefusal); disk
// failures are errUploadStorage or errUploadWrite. On every error the spooled
// file is removed — nothing is kept for a video that was not admitted.
func (h *Handler) admitVideoUpload(ctx context.Context, uploader *model.User, filename string, src io.Reader) (*videoAdmission, error) {
	if err := os.MkdirAll(tusTempDir, 0755); err != nil {
		return nil, fmt.Errorf("%w: %v", errUploadStorage, err)
	}
	uploadID := uuid.New().String()
	tmpPath := filepath.Join(tusTempDir, uploadID)
	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUploadStorage, err)
	}
	hasher := sha256.New()
	written, err := io.Copy(tmpFile, io.TeeReader(io.LimitReader(src, 2<<30), hasher))
	if cerr := tmpFile.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		removeSpooled(tmpPath)
		return nil, fmt.Errorf("%w: %v", errUploadWrite, err)
	}
	videoSHA256 := hex.EncodeToString(hasher.Sum(nil))

	var uploaderPIAL, uploaderHandle string
	if uploader != nil {
		uploaderPIAL = uploader.PIALID
		uploaderHandle = uploader.Handle
	}

	// Synchronous, before the transcoding brain or Caeor receives the file.
	// Nothing is stored on a match, and nothing is admitted on an unknown
	// verdict.
	if gerr := h.bannedContentGate(ctx, gateInput{
		Kind:     scanclient.KindVideo,
		Filename: filename,
		Path:     tmpPath,
		SHA256:   videoSHA256,
		PIALID:   uploaderPIAL,
		Label:    "video-upload",
	}); gerr != nil {
		removeSpooled(tmpPath)
		return nil, gerr
	}

	if h.db != nil {
		// Duplicate check, same architecture as image dedup: the raw SHA-256 is
		// the key, one indexed lookup. Dedup is a convenience, not a safety
		// gate — a lookup failure lets the upload continue — but the failure is
		// recorded rather than dropped.
		dup, dupErr := h.lookupVideoRawDuplicate(videoSHA256, uploaderHandle)
		if dupErr != nil {
			log.Printf("[dedup] video raw-hash lookup for pial=%s: %v", uploaderPIAL, dupErr)
		}
		if dup != nil {
			removeSpooled(tmpPath)
			log.Printf("[dedup] video duplicate blocked: uploader=@%s original=@%s work=%s", uploaderHandle, dup.OriginalHandle, dup.WorkID)
			return &videoAdmission{Duplicate: dup}, nil
		}
		if serr := dbpkg.StoreVideoRawHash(h.db, videoSHA256, uploaderPIAL, uploaderHandle); serr != nil {
			log.Printf("[dedup] video raw-hash store failed for pial=%s: %v", uploaderPIAL, serr)
		}
	}

	upload := &tusUpload{
		ID:             uploadID,
		Length:         written,
		Offset:         written,
		Filename:       filename,
		Created:        time.Now(),
		UploaderPIAL:   uploaderPIAL,
		UploaderHandle: uploaderHandle,
		path:           tmpPath,
	}
	globalTusStore.mu.Lock()
	globalTusStore.uploads[uploadID] = upload
	globalTusStore.mu.Unlock()

	go h.tusTriggerTranscode(upload)
	return &videoAdmission{Upload: upload}, nil
}

// removeSpooled deletes a spooled upload that was not admitted. Already gone
// is fine; anything else is said, because a file that should not exist is
// still on disk.
func removeSpooled(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("[upload] spooled file %s not removed: %v", path, err)
	}
}

// ── Video dedup ──────────────────────────────────────────────────────────────

// videoRawDuplicate is a video whose raw bytes are already on F33D3R: who
// uploaded it first, the work it became, and that work's canonical media, so
// a second poster references the original instead of storing a copy. The
// media fields stay empty while the hash is recorded but not yet linked to a
// work, or the work is gone.
type videoRawDuplicate struct {
	OriginalHandle string
	WorkID         string
	IsSelf         bool // the first uploader is the one asking
	MasterURL      string
	WatermarkedURL string
	PosterURL      string
	DurationSecs   float32
	Width, Height  int
}

// Status is the word the web's dedup answers have always carried: the person
// re-uploading their own video is told something different from the person
// re-uploading somebody else's.
func (d *videoRawDuplicate) Status() string {
	if d.IsSelf {
		return "self_duplicate"
	}
	return "duplicate"
}

// lookupVideoRawDuplicate is the one raw-hash lookup behind every video dedup
// answer: the upload itself and both pre-upload probes. nil, nil when the
// bytes are new. A duplicate whose canonical media could not be read is still
// a duplicate — it comes back with the error beside it, so the caller can
// refuse the copy and say why the URLs are missing.
func (h *Handler) lookupVideoRawDuplicate(sha256hex, viewerHandle string) (*videoRawDuplicate, error) {
	origHandle, workID, found, err := dbpkg.CheckVideoRawHash(h.db, sha256hex)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	d := &videoRawDuplicate{OriginalHandle: origHandle, WorkID: workID, IsSelf: origHandle == viewerHandle}
	if workID == "" {
		return d, nil
	}
	// video_raw_hashes.post_id holds a work id (legacy column name). A deleted
	// work has no canonical media to point at.
	err = h.db.QueryRow(`
		SELECT COALESCE(video_master_url,''), COALESCE(video_watermarked_url,''),
		       COALESCE(video_poster_url,''),
		       COALESCE(video_duration_secs,0)::REAL, COALESCE(video_width,0), COALESCE(video_height,0)
		FROM works WHERE id = $1 AND deleted_at IS NULL`, workID).
		Scan(&d.MasterURL, &d.WatermarkedURL, &d.PosterURL, &d.DurationSecs, &d.Width, &d.Height)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return d, fmt.Errorf("canonical media for work %s: %w", workID, err)
	}
	return d, nil
}

// videoDuplicateBody is the dedup answer the web compose box reads, from the
// upload and from both pre-upload probes alike.
type videoDuplicateBody struct {
	Status         string  `json:"status"`
	OriginalHandle string  `json:"original_handle"`
	OriginalPostID string  `json:"original_post_id"`
	MasterURL      string  `json:"master_url"`
	WatermarkedURL string  `json:"watermarked_mp4_url"`
	PosterURL      string  `json:"poster_url"`
	DurationSecs   float32 `json:"duration_secs"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
}

func writeVideoDuplicate(w http.ResponseWriter, d *videoRawDuplicate) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(videoDuplicateBody{
		Status:         d.Status(),
		OriginalHandle: d.OriginalHandle,
		OriginalPostID: d.WorkID,
		MasterURL:      d.MasterURL,
		WatermarkedURL: d.WatermarkedURL,
		PosterURL:      d.PosterURL,
		DurationSecs:   d.DurationSecs,
		Width:          d.Width,
		Height:         d.Height,
	}); err != nil {
		log.Printf("[dedup] encode duplicate answer: %v", err)
	}
}

// admitImageUpload is the one admission path for a still image, whichever
// route received it: the web compose box (uploadPostMedia) and the native
// clients' POST /api/v1/media both end here. The bytes have already been
// checked against their magic number; ext names the type they proved to be.
//
// In order: the banned-content gate (a refusal comes back as gateErr and
// nothing is stored), storage in Caeor with the local-disk fallback, then the
// forensic records — canonical ownership for lineage attribution and the raw
// SHA-256 behind duplicate detection and leak attribution. A storage failure
// is err; the forensic writes are logged, never fatal, and never silent.
func (h *Handler) admitImageUpload(r *http.Request, filename, ext string, fileBytes []byte) (mediaURL string, gateErr error, err error) {
	hash := sha256.Sum256(fileBytes)
	imgSHA256 := hex.EncodeToString(hash[:])

	uploader := h.userFromRequest(nil, r)
	uploaderPIAL, uploaderHandle := "", ""
	if uploader != nil {
		uploaderPIAL = uploader.PIALID
		uploaderHandle = uploader.Handle
	}

	// Synchronous, before Caeor receives the file. Nothing is stored on a
	// match, and nothing is admitted on an unknown verdict from the
	// exact-bytes layer.
	if gerr := h.bannedContentGate(r.Context(), gateInput{
		Kind:     scanclient.KindImage,
		Filename: filename,
		Data:     fileBytes,
		SHA256:   imgSHA256,
		PIALID:   uploaderPIAL,
		Label:    "image-upload",
	}); gerr != nil {
		return "", gerr, nil
	}

	mediaURL, err = h.caeorUpload(fileBytes, filename, "post")
	if err != nil {
		log.Printf("[caeor] unavailable for post media, falling back: %v", err)
		dir := filepath.Join("web", "static", "uploads", "posts")
		os.MkdirAll(dir, 0755)
		name := uuid.New().String() + ext
		if werr := os.WriteFile(filepath.Join(dir, name), fileBytes, 0644); werr != nil {
			return "", nil, werr
		}
		mediaURL = "/static/uploads/posts/" + name
	}

	// Canonical ownership so lineage attribution works when a different user
	// uploads the same image later. ON CONFLICT DO NOTHING preserves the first
	// uploader's ownership even if called multiple times for the same URL.
	if h.db != nil && uploader != nil {
		if cerr := dbpkg.CreateCanonicalImage(h.db, uploader.ID, uploader.PIALID, mediaURL); cerr != nil {
			log.Printf("[canonical] image register error for %s: %v", mediaURL, cerr)
		}
		// The forensic hash behind duplicate detection and leak attribution.
		// Not fatal to the upload, but never lost quietly.
		if herr := dbpkg.StoreImageRawHash(h.db, imgSHA256, uploaderPIAL, uploaderHandle, mediaURL); herr != nil {
			log.Printf("[media] raw image hash NOT stored for %s (%s): %v", uploaderHandle, mediaURL, herr)
		}
	}
	return mediaURL, nil, nil
}

// uploadVoicePost accepts a recorded audio blob for a voice post and answers
// {kind:"voice", url:"..."}. The duration is the recorder's to report — this
// server does not probe audio — and the compose box sends it with the post.
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

	data, err := io.ReadAll(io.LimitReader(file, 50<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	mediaURL, err := h.admitVoiceUpload(r.Context(), h.userFromRequest(nil, r), fh.Filename, data)
	if err != nil {
		switch {
		case errors.Is(err, errMediaUnsupported):
			http.Error(w, "unsupported or invalid audio file", http.StatusBadRequest)
		case isGateRefusal(err):
			banGateHTTPError(w, err)
		default:
			log.Printf("[upload] voice %q: %v", fh.Filename, err)
			http.Error(w, "storage error", http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{
		"kind": "voice",
		"url":  mediaURL,
	}); err != nil {
		log.Printf("[upload] encode voice answer: %v", err)
	}
}

// admitVoiceUpload is the one admission path for voice audio, whichever route
// received it: the web recorder (uploadVoicePost) and the native clients'
// POST /api/v1/media/voice both end here.
//
// The bytes decide the type and the stored extension (audioExtensionFor);
// anything that is not a recognised audio container is errMediaUnsupported,
// so an executable or a ZIP named voice.webm is never stored. Then the
// banned-content gate — synchronous, pre-storage; the audio fingerprint layer
// applies — then local storage, then the media.uploaded event to Sitra Achra
// so Abraxas Shield can begin scoring. A disk failure is errUploadStorage.
func (h *Handler) admitVoiceUpload(ctx context.Context, uploader *model.User, filename string, data []byte) (string, error) {
	ext := audioExtensionFor(data)
	if ext == "" {
		return "", errMediaUnsupported
	}
	uploaderPIAL := ""
	if uploader != nil {
		uploaderPIAL = uploader.PIALID
	}

	sum := sha256.Sum256(data)
	if gerr := h.bannedContentGate(ctx, gateInput{
		Kind:     scanclient.KindAudio,
		Filename: filename,
		Data:     data,
		SHA256:   hex.EncodeToString(sum[:]),
		PIALID:   uploaderPIAL,
		Label:    "voice-upload",
	}); gerr != nil {
		return "", gerr
	}

	dir := filepath.Join("web", "static", "uploads", "voice")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("%w: %v", errUploadStorage, err)
	}
	name := uuid.New().String() + ext
	if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
		return "", fmt.Errorf("%w: %v", errUploadStorage, err)
	}
	mediaURL := "/static/uploads/voice/" + name

	if uploaderPIAL != "" {
		go h.publishMediaUploadedEvent("", mediaURL, uploaderPIAL)
	}
	return mediaURL, nil
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

	dup, err := h.lookupVideoRawDuplicate(sha256hex, user.Handle)
	if err != nil {
		// Fail-open: the upload proceeds rather than blocking on a DB error,
		// and the error is recorded. A duplicate found before the error is
		// still reported, with whatever of its media could be read.
		log.Printf("[check-video-hash] %v", err)
	}
	if dup == nil {
		w.Write([]byte(`{"status":"unknown"}`))
		return
	}
	writeVideoDuplicate(w, dup)
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

	// Banned-hash gate — the exact-bytes layer of the upload gate, on a digest
	// the client computed. The perceptual layer cannot run on a digest, so it
	// runs when the bytes themselves arrive at the upload handler. A lookup
	// failure refuses the probe rather than reporting an unknown file.
	if gerr := h.bannedContentGate(r.Context(), gateInput{
		Kind:     scanclient.KindImage,
		Filename: "(pre-upload probe)",
		SHA256:   sha256hex,
		PIALID:   user.PIALID,
		Label:    "check-image-hash",
	}); gerr != nil {
		if errors.Is(gerr, errMediaBanned) {
			http.Error(w, "This content cannot be uploaded", http.StatusForbidden)
			return
		}
		banGateHTTPError(w, gerr)
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

	w.Header().Set("Content-Type", "application/json")

	dup, err := h.lookupVideoRawDuplicate(req.SHA256, user.Handle)
	if err != nil {
		// Fail-open: the upload proceeds rather than blocking on a DB error,
		// and the error is recorded. A duplicate found before the error is
		// still reported, with whatever of its media could be read.
		log.Printf("[check-dedup] %v", err)
	}
	if dup == nil {
		w.Write([]byte(`{"status":"clean"}`))
		return
	}
	writeVideoDuplicate(w, dup)
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
