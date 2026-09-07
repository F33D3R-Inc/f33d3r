package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

const tusVersion = "1.0.0"
const tusTempDir = "/tmp/tus-uploads"

type tusUpload struct {
	ID             string
	Length         int64
	Offset         int64
	Filename       string
	Created        time.Time
	JobID          string // filled after transcoding starts
	UploaderPIAL   string // set at create time for duplicate detection
	UploaderHandle string
	RawSHA256      string              // hex SHA-256 of the assembled raw file; set before transcoding
	path           string              // temp file path
	DupInfo        *duplicateVideoInfo // non-nil when pre-transcoding duplicate detected; no job queued
	SitraPublished bool                // true after media.uploaded Sitra event has fired
}

type tusStore struct {
	mu      sync.RWMutex
	uploads map[string]*tusUpload
}

var globalTusStore = &tusStore{uploads: make(map[string]*tusUpload)}

// tusCreate handles POST /upload/tus/ — creates a new resumable upload session.
func (h *Handler) tusCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	uploadLengthStr := r.Header.Get("Upload-Length")
	if uploadLengthStr == "" {
		http.Error(w, "Upload-Length required", http.StatusBadRequest)
		return
	}
	uploadLength, err := strconv.ParseInt(uploadLengthStr, 10, 64)
	if err != nil || uploadLength < 0 {
		http.Error(w, "invalid Upload-Length", http.StatusBadRequest)
		return
	}

	// Parse Upload-Metadata header: key value,key value (base64 values)
	filename := "upload"
	meta := r.Header.Get("Upload-Metadata")
	if meta != "" {
		for _, pair := range strings.Split(meta, ",") {
			pair = strings.TrimSpace(pair)
			parts := strings.SplitN(pair, " ", 2)
			if len(parts) == 2 && strings.TrimSpace(parts[0]) == "filename" {
				decoded := decodeBase64Safe(strings.TrimSpace(parts[1]))
				if decoded != "" {
					filename = filepath.Base(decoded)
				}
			}
		}
	}

	user := h.userFromRequest(w, r)

	id := uuid.New().String()
	path := filepath.Join(tusTempDir, id)

	// Create the temp file
	f, err := os.Create(path)
	if err != nil {
		log.Printf("[tus] create file error: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	f.Close()

	upload := &tusUpload{
		ID:             id,
		Length:         uploadLength,
		Offset:         0,
		Filename:       filename,
		Created:        time.Now(),
		UploaderPIAL:   user.PIALID,
		UploaderHandle: user.Handle,
		path:           path,
	}

	globalTusStore.mu.Lock()
	globalTusStore.uploads[id] = upload
	globalTusStore.mu.Unlock()

	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Location", "/upload/tus/"+id)
	w.Header().Set("Upload-Offset", "0")
	w.WriteHeader(http.StatusCreated)
}

// tusHead handles HEAD /upload/tus/{id} — returns current upload offset.
func (h *Handler) tusHead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	globalTusStore.mu.RLock()
	upload, ok := globalTusStore.uploads[id]
	globalTusStore.mu.RUnlock()

	if !ok {
		http.Error(w, "upload not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Upload-Offset", strconv.FormatInt(upload.Offset, 10))
	w.Header().Set("Upload-Length", strconv.FormatInt(upload.Length, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// tusPatch handles PATCH /upload/tus/{id} — appends a chunk to the upload.
func (h *Handler) tusPatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if ct := r.Header.Get("Content-Type"); ct != "application/offset+octet-stream" {
		http.Error(w, "Content-Type must be application/offset+octet-stream", http.StatusUnsupportedMediaType)
		return
	}

	offsetStr := r.Header.Get("Upload-Offset")
	if offsetStr == "" {
		http.Error(w, "Upload-Offset required", http.StatusBadRequest)
		return
	}
	clientOffset, err := strconv.ParseInt(offsetStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid Upload-Offset", http.StatusBadRequest)
		return
	}

	globalTusStore.mu.Lock()
	upload, ok := globalTusStore.uploads[id]
	if !ok {
		globalTusStore.mu.Unlock()
		http.Error(w, "upload not found", http.StatusNotFound)
		return
	}

	if clientOffset != upload.Offset {
		currentOffset := upload.Offset
		globalTusStore.mu.Unlock()
		w.Header().Set("Tus-Resumable", tusVersion)
		w.Header().Set("Upload-Offset", strconv.FormatInt(currentOffset, 10))
		http.Error(w, "offset conflict", http.StatusConflict)
		return
	}
	globalTusStore.mu.Unlock()

	// Open file in append mode
	f, err := os.OpenFile(upload.path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("[tus] open file error for %s: %v", id, err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	written, err := io.Copy(f, r.Body)
	f.Close()
	if err != nil {
		log.Printf("[tus] write error for %s: %v", id, err)
		http.Error(w, "write error", http.StatusInternalServerError)
		return
	}

	globalTusStore.mu.Lock()
	upload.Offset += written
	newOffset := upload.Offset
	complete := upload.Length > 0 && upload.Offset >= upload.Length
	globalTusStore.mu.Unlock()

	if complete {
		go h.tusTriggerTranscode(upload)
	}

	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Upload-Offset", strconv.FormatInt(newOffset, 10))
	w.WriteHeader(http.StatusNoContent)
}

// ── Job state ────────────────────────────────────────────────────────────────

// Video job states, as this server reports them. The transcoder's own words
// (queued|transcoding|ready|failed) are mapped onto these, and two the
// transcoder never sees are added: an upload not yet handed to it, and a
// duplicate that never will be.
const (
	videoJobQueued     = "queued"     // admitted here, not yet accepted by the transcoder
	videoJobProcessing = "processing" // the transcoder has it, or could not be asked just now
	videoJobReady      = "ready"      // Output is set
	videoJobFailed     = "failed"     // Error says why
	videoJobDuplicate  = "duplicate"  // Duplicate is set; nothing was transcoded
)

// videoJobOutput is what a finished transcode produced. The URLs are the
// relative paths the rest of the media surface serves.
type videoJobOutput struct {
	MasterURL      string
	WatermarkedURL string
	PosterURL      string
	DurationSecs   float64
	Width, Height  int
}

// videoJobState is one admitted upload as the server sees it right now. Both
// status routes — the web's /upload/tus/status/{id} and the JSON lane's
// /api/v1/media/video/{id} — read this and project it; neither asks the
// transcoder on its own.
type videoJobState struct {
	UploadID     string
	UploaderPIAL string
	Status       string
	Output       *videoJobOutput     // videoJobReady
	Error        string              // videoJobFailed
	Duplicate    *duplicateVideoInfo // videoJobDuplicate
	// Raw is the transcoder's answer as it arrived, for the web compose box,
	// which reads the transcoder's own keys. nil when there is no answer to
	// relay: the job is not yet with the transcoder, or it could not be asked.
	Raw []byte
}

// videoJobState reads the state of upload id; false when no such upload was
// admitted here. Asking the transcoder is part of reading: the first time it
// reports the job ready, the media.uploaded event goes to Sitra Achra, once
// per upload, whichever lane happened to be polling.
func (h *Handler) videoJobState(id string) (*videoJobState, bool) {
	globalTusStore.mu.RLock()
	upload, ok := globalTusStore.uploads[id]
	var st *videoJobState
	var jobID string
	if ok {
		st = &videoJobState{
			UploadID:     upload.ID,
			UploaderPIAL: upload.UploaderPIAL,
			Status:       videoJobQueued,
			Duplicate:    upload.DupInfo,
		}
		jobID = upload.JobID
	}
	globalTusStore.mu.RUnlock()
	if !ok {
		return nil, false
	}

	// A pre-transcoding duplicate never had a job to ask about.
	if st.Duplicate != nil {
		st.Status = videoJobDuplicate
		return st, true
	}
	// Not yet handed to the transcoder, or no transcoder to hand it to.
	if jobID == "" || h.transcodingURL == "" {
		return st, true
	}

	resp, err := h.httpClient.Get(h.transcodingURL + "/v1/video/job/" + jobID)
	if err != nil {
		log.Printf("[tus] job status proxy error: %v", err)
		st.Status = videoJobProcessing
		return st, true
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[tus] job status read error: %v", err)
		st.Status = videoJobProcessing
		return st, true
	}
	st.Raw = body
	h.applyTranscoderAnswer(st, upload, resp.StatusCode, body)
	return st, true
}

// applyTranscoderAnswer reads the transcoder's reply into st. Its jobs live in
// its memory, so a 404 means a restart lost this one — failed, and the client
// uploads again; any other refusal, and an answer that cannot be read, is
// treated as transient and the client keeps polling.
func (h *Handler) applyTranscoderAnswer(st *videoJobState, upload *tusUpload, httpStatus int, body []byte) {
	var job struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Output *struct {
			MasterURL      string  `json:"master_url"`
			WatermarkedURL string  `json:"watermarked_mp4_url"`
			PosterURL      string  `json:"poster_url"`
			DurationSecs   float64 `json:"duration_seconds"`
			Width          int     `json:"source_width"`
			Height         int     `json:"source_height"`
		} `json:"output"`
	}
	parseErr := json.Unmarshal(body, &job)
	switch {
	case httpStatus == http.StatusNotFound:
		st.Status = videoJobFailed
		st.Error = "the transcoder no longer has this job; upload again"
	case httpStatus >= 400 || parseErr != nil:
		if parseErr != nil {
			log.Printf("[tus] transcoder answer for %s not readable: %v", st.UploadID, parseErr)
		}
		st.Status = videoJobProcessing
	case job.Status == "ready" && job.Output != nil && job.Output.MasterURL != "":
		st.Status = videoJobReady
		st.Output = &videoJobOutput{
			MasterURL:      job.Output.MasterURL,
			WatermarkedURL: job.Output.WatermarkedURL,
			PosterURL:      job.Output.PosterURL,
			DurationSecs:   job.Output.DurationSecs,
			Width:          job.Output.Width,
			Height:         job.Output.Height,
		}
		h.publishTranscodedOnce(upload, job.Output.MasterURL)
	case job.Status == "failed":
		st.Status = videoJobFailed
		st.Error = job.Error
		if st.Error == "" {
			st.Error = "transcoding failed"
		}
	case job.Status == "queued":
		st.Status = videoJobQueued
	default:
		// "transcoding", or "ready" before the output is written.
		st.Status = videoJobProcessing
	}
}

// publishTranscodedOnce fires media.uploaded to Sitra Achra the first time an
// upload is seen ready, and never again for it, so Abraxas Shield scores the
// asset once however many pollers see it finish.
func (h *Handler) publishTranscodedOnce(upload *tusUpload, masterURL string) {
	globalTusStore.mu.Lock()
	first := !upload.SitraPublished
	upload.SitraPublished = true
	uploaderPIAL := upload.UploaderPIAL
	globalTusStore.mu.Unlock()
	if first {
		go h.publishMediaUploadedEvent("", masterURL, uploaderPIAL)
	}
}

// tusJobStatus handles GET /upload/tus/status/{id} — the web projection of
// videoJobState. The compose box reads the transcoder's own keys, so its
// answer is relayed as it arrived; before there is one, the state word alone.
func (h *Handler) tusJobStatus(w http.ResponseWriter, r *http.Request) {
	st, ok := h.videoJobState(r.PathValue("id"))
	if !ok {
		http.Error(w, "upload not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// Pre-transcoding duplicate: no job was queued. The canonical shape the
	// compose box expects, naming the original's creator by handle and name.
	// The creator's PIAL is not in it: a PIAL is never sent to a browser.
	if d := st.Duplicate; d != nil {
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "duplicate",
			"original": map[string]interface{}{
				"master_url":     d.ExistingMasterURL,
				"poster_url":     d.ExistingPosterURL,
				"duration_secs":  d.DurationSecs,
				"width":          d.Width,
				"height":         d.Height,
				"work_id":        d.WorkID,
				"creator_handle": d.OriginalHandle,
				"creator_name":   d.CreatorName,
			},
		}); err != nil {
			log.Printf("[tus] encode duplicate status: %v", err)
		}
		return
	}

	if st.Raw != nil {
		w.WriteHeader(http.StatusOK)
		w.Write(st.Raw)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":%q}`, st.Status)
}

// tusTriggerTranscode reads the assembled temp file, runs a pre-transcoding
// duplicate check, and only queues transcoding if the content is original.
func (h *Handler) tusTriggerTranscode(upload *tusUpload) {
	fileBytes, err := os.ReadFile(upload.path)
	if err != nil {
		log.Printf("[tus] read assembled file error for %s: %v", upload.ID, err)
		return
	}

	// Compute raw SHA-256 once — used for both dedup check and seeding the store.
	rawHasher := sha256.New()
	rawHasher.Write(fileBytes)
	rawSHA256 := hex.EncodeToString(rawHasher.Sum(nil))

	// Gate 1: duplicate check.
	// Check whether this exact raw file already exists on F33D3R.
	// If it does: erase the temp file, record the dupInfo so the polling endpoint
	// returns the canonical duplicate response, and bail — no chit minted, no
	// transcoding job queued.
	if dupInfo := h.checkVideoDuplicateByBytes(fileBytes, upload.Filename, upload.UploaderPIAL, upload.UploaderHandle); dupInfo != nil {
		log.Printf("[tus] duplicate detected for upload %s (work %s) — aborting transcoding", upload.ID, dupInfo.WorkID)

		// Erase the temp file immediately — do not store it.
		if removeErr := os.Remove(upload.path); removeErr != nil && !os.IsNotExist(removeErr) {
			log.Printf("[tus] cleanup temp file error for duplicate %s: %v", upload.ID, removeErr)
		}

		// Record dupInfo so GET /upload/tus/status/{id} returns the duplicate response.
		globalTusStore.mu.Lock()
		upload.DupInfo = dupInfo
		globalTusStore.mu.Unlock()
		return
	}

	// Gate 1 passed — file is original.
	// Seed video_raw_hashes so future uploads of the same file are blocked
	// before a single byte is transferred (client-side pre-check via checkVideoDedupHandler).
	// ON CONFLICT DO NOTHING: first uploader is authoritative.
	if h.db != nil && upload.UploaderPIAL != "" {
		if storeErr := dbpkg.StoreVideoRawHash(h.db, rawSHA256, upload.UploaderPIAL, upload.UploaderHandle); storeErr != nil {
			log.Printf("[tus] StoreVideoRawHash error for %s: %v", upload.ID, storeErr)
			// Non-fatal: transcoding proceeds; dedup store may miss this entry.
		}
	}

	// Store the hash on the upload so work_event can link it after InsertWork.
	globalTusStore.mu.Lock()
	upload.RawSHA256 = rawSHA256
	globalTusStore.mu.Unlock()

	jobID, err := h.transcodingVideoQueue(fileBytes, upload.Filename, upload.UploaderHandle)
	if err != nil {
		log.Printf("[tus] transcodingVideoQueue error for %s: %v", upload.ID, err)
		return
	}

	globalTusStore.mu.Lock()
	upload.JobID = jobID
	globalTusStore.mu.Unlock()

	log.Printf("[tus] upload %s queued as transcoding job %s", upload.ID, jobID)

	// Clean up temp file after successful queue.
	if err := os.Remove(upload.path); err != nil {
		log.Printf("[tus] cleanup temp file error: %v", err)
	}
}

// decodeBase64Safe decodes a base64 string, trying standard then URL encoding.
func decodeBase64Safe(s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.URLEncoding.DecodeString(s)
		if err != nil {
			return ""
		}
	}
	return string(b)
}

// transcodingVideoQueue POSTs a video to the transcoding brain and returns the job_id immediately.
// Unlike transcodingVideoUpload, this does NOT poll — caller gets the job_id to poll separately.
// creatorHandle is burned into the video watermark by the transcoding service.
func (h *Handler) transcodingVideoQueue(fileBytes []byte, filename, creatorHandle string) (string, error) {
	if h.transcodingURL == "" {
		return "", fmt.Errorf("TRANSCODING_URL not configured")
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(fileBytes); err != nil {
		return "", err
	}
	if creatorHandle != "" {
		if err := mw.WriteField("creator_handle", creatorHandle); err != nil {
			return "", err
		}
	}
	mw.Close()

	req, err := http.NewRequest(http.MethodPost, h.transcodingURL+"/v1/video/upload", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := h.videoClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("transcoding upload returned %d", resp.StatusCode)
	}

	var queued struct {
		JobID   string `json:"job_id"`
		AssetID string `json:"asset_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&queued); err != nil {
		return "", err
	}
	if queued.JobID == "" {
		return "", fmt.Errorf("transcoding returned empty job_id")
	}
	return queued.JobID, nil
}
