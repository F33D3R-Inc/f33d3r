package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"

	"f33d3r.com/ios/devserver/internal/store"
)

// The video job lane — the stand-in for Caeor's transcoder.
//
// The real platform takes a video in, hands it to a transcoder, and the
// client polls a job until it is ready, failed, or found to be a duplicate.
// The shape of that conversation is what the composer is written against, so
// this stand-in keeps the shape and only changes what happens in the middle:
// with no ffmpeg on the machine there is no transcode, so the file itself is
// the master rendition (AVPlayer plays an MP4 by URL as readily as an HLS
// playlist), the watermarked rendition is the same file, and the duration and
// picture size come from the MP4's own `moov` box (mp4probe.go) rather than
// from a decode. Jobs live in memory for the life of the process; a restart
// forgets them, and the composer's poll gets the 404 the contract names.

// Job statuses — the closed set in feed-engine's VideoJobDTO.
const (
	videoJobQueued     = "queued"
	videoJobProcessing = "processing"
	videoJobReady      = "ready"
	videoJobFailed     = "failed"
	videoJobDuplicate  = "duplicate"
)

// VideoJobDTO is the state of one upload. Mirrors feed-engine's VideoJobDTO
// and the Swift `VideoJob`: every field but status is omitempty, and which
// are present is what the status means.
type VideoJobDTO struct {
	UploadID          string   `json:"upload_id,omitempty"`
	Status            string   `json:"status"`
	MasterURL         string   `json:"master_url,omitempty"`
	PosterURL         string   `json:"poster_url,omitempty"`
	WatermarkedURL    string   `json:"watermarked_url,omitempty"`
	DurationSecs      *float64 `json:"duration_secs,omitempty"`
	Width             *int     `json:"width,omitempty"`
	Height            *int     `json:"height,omitempty"`
	Error             string   `json:"error,omitempty"`
	DuplicateOfWorkID string   `json:"duplicate_of_work_id,omitempty"`
	DuplicateOfHandle string   `json:"duplicate_of_handle,omitempty"`
}

// videoJob is one upload's state, owned by the account that made it.
type videoJob struct {
	id      string
	ownerID string
	dto     VideoJobDTO
}

// videoJobs is the process's table of uploads.
type videoJobs struct {
	mu   sync.Mutex
	byID map[string]*videoJob
}

func newVideoJobs() *videoJobs { return &videoJobs{byID: map[string]*videoJob{}} }

func (j *videoJobs) put(job *videoJob) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.byID[job.id] = job
}

// get returns the job when it exists and belongs to ownerID. Anyone else is
// told nothing, the same as for an id that never existed.
func (j *videoJobs) get(id, ownerID string) (VideoJobDTO, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.byID[id]
	if !ok || job.ownerID != ownerID {
		return VideoJobDTO{}, false
	}
	return job.dto, true
}

func (j *videoJobs) update(id string, fn func(*VideoJobDTO)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job, ok := j.byID[id]; ok {
		fn(&job.dto)
	}
}

// uploadVideo — POST /api/v1/media/video (multipart, field `file` or `media`)
// → 202 VideoJobDTO{queued}, or 200 VideoJobDTO{duplicate} when a live work
// already carries these exact bytes and nothing was queued.
func (s *Server) uploadVideo(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The upload could not be read.")
		return
	}
	file, fh, err := formFileAny(r, "file", "media")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "No file was attached.")
		return
	}
	defer file.Close()

	head, err := readMagic(file)
	if err != nil {
		serverError(w, err)
		return
	}
	if _, ok := videoFilenameFor(fh.Filename, head); !ok {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media", "Only MP4, MOV, M4V, WebM and MKV video is accepted.")
		return
	}
	// The stored name is the bytes' own container, whatever the upload was
	// called: the same clip sent as clip.mov and copy.mp4 is one file, and
	// one file is what makes the duplicate check below mean anything.
	canonicalExt, _ := sniffVideo(head)

	url, path, err := s.storeVideo(file, canonicalExt)
	if err != nil {
		serverError(w, err)
		return
	}

	// The same bytes already on a live work: nothing to transcode, and the
	// author decides whether to post a reference to the original.
	if original, err := s.store.FindWorkByVideoMaster(r.Context(), url); err == nil {
		dto := VideoJobDTO{Status: videoJobDuplicate, DuplicateOfWorkID: original.ID}
		if original.Author != nil {
			dto.DuplicateOfHandle = original.Author.Handle
		}
		dto.setRenditions(original.VideoMasterURL, original.VideoPosterURL, original.VideoWatermarkedURL,
			original.VideoDurationSecs, original.VideoWidth, original.VideoHeight)
		writeJSON(w, http.StatusOK, dto)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		serverError(w, err)
		return
	}

	job := &videoJob{id: uuid.NewString(), ownerID: u.ID, dto: VideoJobDTO{Status: videoJobQueued}}
	job.dto.UploadID = job.id
	s.videos.put(job)
	go s.finishVideo(job.id, url, path)
	writeJSON(w, http.StatusAccepted, VideoJobDTO{UploadID: job.id, Status: videoJobQueued})
}

// videoJob — GET /api/v1/media/video/{id} → 200 VideoJobDTO, or 404
// video_job_not_found for an id this account did not upload.
func (s *Server) videoJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	dto, ok := s.videos.get(r.PathValue("id"), u.ID)
	if !ok {
		writeError(w, http.StatusNotFound, "video_job_not_found", "No such upload.")
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// finishVideo is the transcoder's part: read what the file says about itself
// and declare the job ready. A file the probe cannot read is still playable
// — the player measures it — so an unreadable moov is a ready job with no
// numbers, and only a file that cannot be opened at all is a failure.
func (s *Server) finishVideo(id, url, path string) {
	s.videos.update(id, func(d *VideoJobDTO) { d.Status = videoJobProcessing })

	f, err := os.Open(path)
	if err != nil {
		s.videos.update(id, func(d *VideoJobDTO) {
			d.Status = videoJobFailed
			d.Error = "The uploaded file could not be opened: " + err.Error()
		})
		return
	}
	defer f.Close()

	probe, err := probeMP4(f)
	if err != nil && !errors.Is(err, errNotMP4) {
		log.Printf("api: video probe %s: %v", filepath.Base(path), err)
	}
	s.videos.update(id, func(d *VideoJobDTO) {
		d.Status = videoJobReady
		d.setRenditions(url, "", url, probe.DurationSecs, probe.Width, probe.Height)
	})
}

// setRenditions fills the media fields, leaving unknown numbers absent
// rather than zero so the client does not draw a 0:00 badge.
func (d *VideoJobDTO) setRenditions(master, poster, watermarked string, durationSecs float64, width, height int) {
	d.MasterURL = master
	d.PosterURL = poster
	d.WatermarkedURL = watermarked
	if durationSecs > 0 {
		d.DurationSecs = &durationSecs
	}
	if width > 0 && height > 0 {
		d.Width = &width
		d.Height = &height
	}
}

// storeVideo streams the upload to disk under its content hash — a phone's
// video is not something to hold in memory — and returns the served path and
// the file path.
func (s *Server) storeVideo(file io.Reader, ext string) (url, path string, err error) {
	dir := filepath.Join(s.mediaDir, "video")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	tmp, err := os.CreateTemp(dir, "upload-*")
	if err != nil {
		return "", "", err
	}
	tmpPath := tmp.Name()
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(file, 2<<30))
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(tmpPath)
		return "", "", fmt.Errorf("store video: %v %v", copyErr, closeErr)
	}
	name := hex.EncodeToString(hasher.Sum(nil)[:16]) + ext
	path = filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		os.Remove(tmpPath)
	} else if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return "", "", err
	}
	return "/media/video/" + name, path, nil
}
