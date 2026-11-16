package handler

// tus.go — resumable uploads and the life of a video upload.
//
// The browser-facing tus protocol stays here because the edge is the single
// public entry: a client never talks to Caeor. Chunks are appended to a private
// spool file; when the last one lands, the assembled file is hashed in a stream,
// judged by the banned-content gate, and streamed to Caeor (handOffVideo). The
// file is never read into memory.
//
// Every whole-file video lane (the compose box, React With Video, the native
// clients, a Frequency clip) joins the same path through handOffVideo, so the
// status routes — /upload/tus/status/{id}, /api/v1/media/video/{id}, the clip
// Facet and the bulk-upload status — read one state (videoJobState).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/f33d3r/feed-engine/internal/caeor"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

const tusVersion = "1.0.0"
const tusTempDir = "/tmp/tus-uploads"

const (
	// videoHandOffTimeout bounds hashing, gating and streaming one video to Caeor.
	videoHandOffTimeout = 2 * time.Hour
	// videoStateTimeout bounds one status read from Caeor.
	videoStateTimeout = 5 * time.Second
)

// tusUpload is one upload this server is assembling or has handed to Caeor.
type tusUpload struct {
	ID              string
	Length          int64
	Offset          int64
	Filename        string
	Created         time.Time
	UploaderPIAL    string
	UploaderAccount string
	UploaderHandle  string
	path            string // spool file
	// complete is set once the whole file is here and its hand-off has begun.
	complete bool
	// AssetID is Caeor's asset for the upload, set once Caeor accepted it. For
	// an exact-byte duplicate it is the ORIGINAL asset.
	AssetID string
	// DuplicateOfOther is set when Caeor resolved the bytes to an asset somebody
	// else uploaded: the upload becomes a credited reference to the original.
	DuplicateOfOther bool
	// Failure says why the upload never reached Caeor; set, it is final.
	Failure        string
	SitraPublished bool
}

type tusStore struct {
	mu      sync.RWMutex
	uploads map[string]*tusUpload
}

var globalTusStore = &tusStore{uploads: make(map[string]*tusUpload)}

func (s *tusStore) put(u *tusUpload) {
	s.mu.Lock()
	s.uploads[u.ID] = u
	s.mu.Unlock()
}

func (s *tusStore) get(id string) (*tusUpload, bool) {
	s.mu.RLock()
	u, ok := s.uploads[id]
	s.mu.RUnlock()
	return u, ok
}

// newTusUpload records one upload by up, assembled at path.
func newTusUpload(up caeor.Uploader, filename, path string, length int64) *tusUpload {
	return &tusUpload{
		ID:              uuid.New().String(),
		Length:          length,
		Offset:          length,
		Filename:        filename,
		Created:         time.Now(),
		UploaderPIAL:    up.PIAL,
		UploaderAccount: up.Account,
		UploaderHandle:  up.Handle,
		path:            path,
	}
}

// ownedTusUpload returns the upload when the requester is its uploader. Anyone
// else is told there is no such upload, so the routes confirm nothing about
// other people's uploads.
func (h *Handler) ownedTusUpload(w http.ResponseWriter, r *http.Request) (*tusUpload, bool) {
	upload, ok := globalTusStore.get(r.PathValue("id"))
	user := h.userFromRequest(w, r)
	if !ok || user == nil || user.PIALID == "" || upload.UploaderPIAL != user.PIALID {
		http.Error(w, "upload not found", http.StatusNotFound)
		return nil, false
	}
	return upload, true
}

// tusCreate handles POST /upload/tus/ — creates a new resumable upload session.
func (h *Handler) tusCreate(w http.ResponseWriter, r *http.Request) {
	up, err := uploaderOf(h.userFromRequest(w, r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	uploadLength, err := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
	if err != nil || uploadLength <= 0 {
		http.Error(w, "Upload-Length required", http.StatusBadRequest)
		return
	}
	if uploadLength > videoUploadMax {
		w.Header().Set("Tus-Max-Size", strconv.FormatInt(videoUploadMax, 10))
		http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Upload-Metadata: key value,key value (base64 values)
	filename := "upload"
	for _, pair := range strings.Split(r.Header.Get("Upload-Metadata"), ",") {
		parts := strings.SplitN(strings.TrimSpace(pair), " ", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[0]) == "filename" {
			if decoded := decodeBase64Safe(strings.TrimSpace(parts[1])); decoded != "" {
				filename = filepath.Base(decoded)
			}
		}
	}

	path, err := createSpool()
	if err != nil {
		log.Printf("[tus] create spool: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	upload := newTusUpload(up, filename, path, uploadLength)
	upload.Offset = 0
	globalTusStore.put(upload)

	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Location", "/upload/tus/"+upload.ID)
	w.Header().Set("Upload-Offset", "0")
	w.WriteHeader(http.StatusCreated)
}

// createSpool makes an empty private spool file for a resumable upload.
func createSpool() (string, error) {
	if err := os.MkdirAll(tusTempDir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(tusTempDir, "tus-*")
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		removeSpooled(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// tusHead handles HEAD /upload/tus/{id} — returns current upload offset.
func (h *Handler) tusHead(w http.ResponseWriter, r *http.Request) {
	upload, ok := h.ownedTusUpload(w, r)
	if !ok {
		return
	}
	globalTusStore.mu.RLock()
	offset, length := upload.Offset, upload.Length
	globalTusStore.mu.RUnlock()
	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Upload-Offset", strconv.FormatInt(offset, 10))
	if length > 0 {
		w.Header().Set("Upload-Length", strconv.FormatInt(length, 10))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// tusPatch handles PATCH /upload/tus/{id} — appends a chunk to the upload. A
// session opened without a length (a bulk slot) takes it from the first PATCH
// that states it. A chunk may never run past the declared length.
func (h *Handler) tusPatch(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "application/offset+octet-stream" {
		http.Error(w, "Content-Type must be application/offset+octet-stream", http.StatusUnsupportedMediaType)
		return
	}
	clientOffset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil {
		http.Error(w, "Upload-Offset required", http.StatusBadRequest)
		return
	}
	upload, ok := h.ownedTusUpload(w, r)
	if !ok {
		return
	}

	globalTusStore.mu.Lock()
	if upload.complete {
		globalTusStore.mu.Unlock()
		http.Error(w, "upload already complete", http.StatusConflict)
		return
	}
	if upload.Length == 0 {
		declared, err := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
		if err != nil || declared <= 0 || declared > videoUploadMax {
			globalTusStore.mu.Unlock()
			http.Error(w, "Upload-Length required", http.StatusBadRequest)
			return
		}
		upload.Length = declared
	}
	if clientOffset != upload.Offset {
		currentOffset := upload.Offset
		globalTusStore.mu.Unlock()
		w.Header().Set("Tus-Resumable", tusVersion)
		w.Header().Set("Upload-Offset", strconv.FormatInt(currentOffset, 10))
		http.Error(w, "offset conflict", http.StatusConflict)
		return
	}
	remaining := upload.Length - upload.Offset
	globalTusStore.mu.Unlock()

	f, err := os.OpenFile(upload.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("[tus] open spool for %s: %v", upload.ID, err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	written, err := io.Copy(f, io.LimitReader(r.Body, remaining))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		log.Printf("[tus] write for %s: %v", upload.ID, err)
		http.Error(w, "write error", http.StatusInternalServerError)
		return
	}

	globalTusStore.mu.Lock()
	upload.Offset += written
	newOffset := upload.Offset
	complete := upload.Offset >= upload.Length && !upload.complete
	if complete {
		upload.complete = true
	}
	globalTusStore.mu.Unlock()

	if complete {
		go h.handOffVideo(upload, caeor.KindVideo)
	}

	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Upload-Offset", strconv.FormatInt(newOffset, 10))
	w.WriteHeader(http.StatusNoContent)
}

// ── The hand-off to Caeor ────────────────────────────────────────────────────

// handOffVideo takes an assembled upload to Caeor as an asset of kind: the
// spooled file is hashed in a stream and judged by the banned-content gate
// (unless the lane already did), then streamed to Caeor. The spool file is
// removed whatever happens. A failure is recorded on the upload and its owner
// is told through the video job.
func (h *Handler) handOffVideo(upload *tusUpload, kind caeor.Kind) {
	ctx, cancel := context.WithTimeout(context.Background(), videoHandOffTimeout)
	defer cancel()
	defer removeSpooled(upload.path)

	var job *dbpkg.VideoJob
	if h.db != nil {
		j, err := dbpkg.OpenVideoJob(h.db, upload.ID, upload.UploaderHandle, upload.UploaderPIAL, upload.Filename)
		if err != nil {
			// The upload proceeds; only the "processing finished" notice is lost.
			log.Printf("[upload] recording video job %s: %v", upload.ID, err)
		}
		job = j
	}
	fail := func(reason string, cause error) {
		log.Printf("[upload] %s %s for @%s: %v", kind, upload.ID, upload.UploaderHandle, cause)
		globalTusStore.mu.Lock()
		upload.Failure = reason
		globalTusStore.mu.Unlock()
		if job != nil {
			h.finishVideoJob(*job, false)
		}
	}

	// The bytes decide what the file is and the extension it is handed over
	// under; a resumable upload's declared name is only a suggestion.
	filename, err := spooledVideoName(upload)
	if err != nil {
		fail("the uploaded file is not a video this server accepts", err)
		return
	}

	up := caeor.Uploader{PIAL: upload.UploaderPIAL, Account: upload.UploaderAccount, Handle: upload.UploaderHandle}
	asset, err := h.storeFile(ctx, kind, up, filename, upload.path)
	if err != nil {
		switch {
		case errors.Is(err, errMediaBanned):
			fail("This content cannot be uploaded.", err)
		case errors.Is(err, errUploadHeld):
			fail(errUploadHeld.Error(), err)
		case errors.Is(err, errMediaWithdrawn):
			fail(errMediaWithdrawn.Error(), err)
		case errors.Is(err, caeor.ErrRefused):
			fail("the video could not be processed", err)
		default:
			fail("the video could not be stored; upload again", err)
		}
		return
	}

	globalTusStore.mu.Lock()
	upload.AssetID = asset.AssetID
	upload.DuplicateOfOther = asset.DuplicateOf != nil && asset.UploaderPIAL != upload.UploaderPIAL
	globalTusStore.mu.Unlock()
	if job != nil {
		if err := dbpkg.SetVideoJobID(h.db, upload.ID, asset.AssetID); err != nil {
			log.Printf("[upload] recording asset %s for video job %s: %v", asset.AssetID, upload.ID, err)
		}
	}
	log.Printf("[upload] %s %s by @%s is Caeor asset %s", kind, upload.ID, upload.UploaderHandle, asset.AssetID)
}

// spooledVideoName sniffs a spooled upload and names it with the extension
// its container earns.
func spooledVideoName(upload *tusUpload) (string, error) {
	f, err := os.Open(upload.path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head, err := readMagic(f)
	if err != nil {
		return "", err
	}
	name, ok := videoFilenameFor(upload.Filename, head)
	if !ok {
		return "", errMediaUnsupported
	}
	return name, nil
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

// ── Job state ────────────────────────────────────────────────────────────────

// Video job states, as this server reports them.
const (
	videoJobQueued     = "queued"     // admitted here, not yet accepted by Caeor
	videoJobProcessing = "processing" // Caeor has it, or could not be asked just now
	videoJobReady      = "ready"      // Output is set
	videoJobFailed     = "failed"     // Error says why
	videoJobDuplicate  = "duplicate"  // the bytes are somebody else's original; Duplicate is set
)

// videoJobOutput is what a finished asset serves: the watermarked HLS master
// and its poster. The downloadable copy is never a URL; the download route
// hands it out by work.
type videoJobOutput struct {
	MasterURL     string
	PosterURL     string
	DurationSecs  float64
	Width, Height int
}

// videoDuplicate is an upload Caeor resolved to somebody else's original: the
// work that uses it is a credited reference, and the original uploader keeps
// the credit.
type videoDuplicate struct {
	CreatorHandle string
	CreatorName   string
}

// videoJobState is one admitted upload as the server sees it right now.
type videoJobState struct {
	UploadID     string
	UploaderPIAL string
	AssetID      string
	Status       string
	Output       *videoJobOutput // videoJobReady, videoJobDuplicate
	Error        string          // videoJobFailed
	Duplicate    *videoDuplicate // videoJobDuplicate
}

// videoJobState reads the state of upload id; false when no such upload was
// admitted here. The first time it is seen ready, the media.uploaded event goes
// to Sitra Achra, once per upload, whichever lane happened to be polling.
func (h *Handler) videoJobState(ctx context.Context, id string) (*videoJobState, bool) {
	upload, ok := globalTusStore.get(id)
	if !ok {
		return nil, false
	}
	globalTusStore.mu.RLock()
	st := &videoJobState{
		UploadID:     upload.ID,
		UploaderPIAL: upload.UploaderPIAL,
		AssetID:      upload.AssetID,
		Status:       videoJobQueued,
		Error:        upload.Failure,
	}
	duplicateOfOther := upload.DuplicateOfOther
	globalTusStore.mu.RUnlock()

	if st.Error != "" {
		st.Status = videoJobFailed
		return st, true
	}
	if st.AssetID == "" {
		return st, true
	}

	cctx, cancel := context.WithTimeout(ctx, videoStateTimeout)
	defer cancel()
	asset, err := h.caeor.Get(cctx, st.AssetID)
	switch {
	case errors.Is(err, caeor.ErrNotFound):
		st.Status = videoJobFailed
		st.Error = "the media service no longer has this upload; upload again"
		return st, true
	case err != nil:
		log.Printf("[upload] state of %s (asset %s): %v", st.UploadID, st.AssetID, err)
		st.Status = videoJobProcessing
		return st, true
	}
	switch {
	case asset.Withdrawn:
		st.Status = videoJobFailed
		st.Error = errMediaWithdrawn.Error()
	case asset.Status == caeor.StatusFailed:
		st.Status = videoJobFailed
		st.Error = asset.Failure
		if st.Error == "" {
			st.Error = "processing failed"
		}
	case asset.Held():
		// Stored and held by the integrity authority. The poll ends here: the
		// video is not postable until a review releases it, and the person is
		// told so rather than left watching a spinner.
		st.Status = videoJobFailed
		st.Error = errUploadHeld.Error()
	case asset.Status == caeor.StatusReady && asset.URLs.HLSMaster != "":
		width, height := asset.Dimensions()
		st.Output = &videoJobOutput{
			MasterURL:    asset.URLs.HLSMaster,
			PosterURL:    asset.URLs.Poster,
			DurationSecs: asset.Duration(),
			Width:        width,
			Height:       height,
		}
		st.Status = videoJobReady
		if duplicateOfOther {
			handle, name, err := dbpkg.UploaderCredit(h.db, asset.UploaderPIAL)
			if err != nil {
				log.Printf("[upload] credit of %s: %v", asset.UploaderPIAL, err)
				st.Status = videoJobProcessing
				st.Output = nil
				return st, true
			}
			st.Status = videoJobDuplicate
			st.Duplicate = &videoDuplicate{CreatorHandle: handle, CreatorName: name}
		} else {
			h.publishTranscodedOnce(upload, asset.URLs.HLSMaster)
		}
	default:
		st.Status = videoJobProcessing
	}
	return st, true
}

// publishTranscodedOnce fires media.uploaded to Sitra Achra the first time an
// upload is seen ready, and never again for it.
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
// videoJobState, in the keys the compose box reads.
func (h *Handler) tusJobStatus(w http.ResponseWriter, r *http.Request) {
	upload, ok := h.ownedTusUpload(w, r)
	if !ok {
		return
	}
	st, ok := h.videoJobState(r.Context(), upload.ID)
	if !ok {
		http.Error(w, "upload not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(webVideoStatus(st)); err != nil {
		log.Printf("[upload] encode status of %s: %v", upload.ID, err)
	}
}

// webVideoStatus is the compose box's shape of a video job.
func webVideoStatus(st *videoJobState) map[string]interface{} {
	out := map[string]interface{}{"status": st.Status}
	switch st.Status {
	case videoJobFailed:
		out["error"] = st.Error
	case videoJobReady:
		o := st.Output
		out["output"] = map[string]interface{}{
			"master_url":       o.MasterURL,
			"poster_url":       o.PosterURL,
			"duration_seconds": o.DurationSecs,
			"source_width":     o.Width,
			"source_height":    o.Height,
		}
	case videoJobDuplicate:
		o := st.Output
		out["original"] = map[string]interface{}{
			"master_url":     o.MasterURL,
			"poster_url":     o.PosterURL,
			"duration_secs":  o.DurationSecs,
			"width":          o.Width,
			"height":         o.Height,
			"creator_handle": st.Duplicate.CreatorHandle,
			"creator_name":   st.Duplicate.CreatorName,
		}
	}
	return out
}
