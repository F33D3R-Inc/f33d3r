package handler

import (
	"errors"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/f33d3r/feed-engine/internal/model"
)

// ── /api/v1/media — the upload lane for native clients ───────────────────────
//
// The web compose box uploads over cookie-session routes (/upload/post-media,
// /upload/voice, /upload/tus/status/{id}, /api/gif/search) that answer in the
// keys its JavaScript reads. The native clients present a bearer and read
// DTOs, so each of those routes has a twin here. The twins own no logic: the
// admission, the dedup, the transcoder hand-off, the job state and the GIF
// fetch are one core each (media.go, tus.go, tag.go), and both lanes call it,
// so a file the web would refuse is refused here for the same reason.
//
// The DTOs live here rather than in api_v1_dto.go because no Swift model
// reads them yet; api_v1_contract_test.go takes them up when one does.

// VideoJobDTO is the state of one video upload, as POST /api/v1/media/video
// answers on admission and GET /api/v1/media/video/{id} answers on every
// poll — one shape, so the client keeps one decoder.
//
// Status is one of:
//
//	queued      admitted; the transcoder has not yet taken the job. upload_id is set.
//	processing  the transcoder has it (or could not be asked this instant). Keep polling.
//	ready       master_url, poster_url, watermarked_url, duration_secs, width and height are set.
//	failed      error says why; upload again.
//	duplicate   the same bytes are already on F33D3R. duplicate_of_work_id and
//	            duplicate_of_handle name the original, and the media fields carry
//	            its canonical renditions when the work is known — reference those
//	            rather than uploading again. Nothing was transcoded.
//
// Media URLs are the relative paths the rest of /api/v1 uses for a work's
// video; the client resolves them against the server it is talking to.
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

// VoiceUploadDTO is the answer to POST /api/v1/media/voice. There is no
// duration_secs: this server does not probe audio, so it has no duration to
// give and would only be repeating what the recorder already knows. The
// client sends the duration with the post (voice_duration_secs), as the web
// does.
type VoiceUploadDTO struct {
	URL string `json:"url"`
}

// GifSearchDTO is the answer to GET /api/v1/gif/search. The provider's shape
// stays on the server; this is the contract the app owns.
type GifSearchDTO struct {
	Results []GifResultDTO `json:"results"`
}

// GifResultDTO is one GIF the picker can offer.
type GifResultDTO struct {
	ID         string `json:"id"`
	URL        string `json:"url"`         // the GIF to attach
	PreviewURL string `json:"preview_url"` // a smaller rendition for the picker grid
	Width      int    `json:"width"`       // of url; 0 when the provider did not say
	Height     int    `json:"height"`
	Title      string `json:"title"`
}

// apiV1UploadVideo — POST /api/v1/media/video (multipart, field `file` or
// `media`) → 202 VideoJobDTO{status: queued}, or 200 VideoJobDTO{status:
// duplicate} when the bytes are already on F33D3R and nothing was queued.
//
// The bytes decide what the file is; the declared name only supplies an
// extension when it agrees with them (videoFilenameFor).
func (h *Handler) apiV1UploadVideo(w http.ResponseWriter, r *http.Request, user *model.User) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		apiError(w, http.StatusBadRequest, "bad_request", "The upload could not be read.")
		return
	}
	file, fh, err := formFileAny(r, "file", "media")
	if err != nil {
		apiError(w, http.StatusBadRequest, "bad_request", "No file was attached.")
		return
	}
	defer file.Close()

	head, err := readMagic(file)
	if err != nil {
		apiServerError(w, err)
		return
	}
	filename, ok := videoFilenameFor(fh.Filename, head)
	if !ok {
		apiError(w, http.StatusUnsupportedMediaType, "unsupported_media", "Only MP4, MOV, M4V, WebM and MKV video is accepted.")
		return
	}

	adm, err := h.admitVideoUpload(r.Context(), user, filename, file)
	if err != nil {
		if isGateRefusal(err) {
			apiGateError(w, err)
			return
		}
		apiServerError(w, err)
		return
	}
	if adm.Duplicate != nil {
		apiJSON(w, http.StatusOK, videoJobDTOForDuplicate(adm.Duplicate))
		return
	}
	apiJSON(w, http.StatusAccepted, VideoJobDTO{UploadID: adm.Upload.ID, Status: videoJobQueued})
}

// apiV1VideoJob — GET /api/v1/media/video/{id} → 200 VideoJobDTO.
//
// An upload is its uploader's. Anyone else asking is told there is no such
// job, the same answer as for an id that never existed, so the route confirms
// nothing about other people's uploads.
func (h *Handler) apiV1VideoJob(w http.ResponseWriter, r *http.Request, user *model.User) {
	st, ok := h.videoJobState(r.PathValue("id"))
	if !ok || (st.UploaderPIAL != "" && st.UploaderPIAL != user.PIALID) {
		apiError(w, http.StatusNotFound, "video_job_not_found", "No such upload.")
		return
	}
	apiJSON(w, http.StatusOK, videoJobDTO(st))
}

// apiV1UploadVoice — POST /api/v1/media/voice (multipart, field `audio` or
// `file`) → 201 VoiceUploadDTO.
func (h *Handler) apiV1UploadVoice(w http.ResponseWriter, r *http.Request, user *model.User) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		apiError(w, http.StatusBadRequest, "bad_request", "The upload could not be read.")
		return
	}
	file, fh, err := formFileAny(r, "audio", "file")
	if err != nil {
		apiError(w, http.StatusBadRequest, "bad_request", "No file was attached.")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 50<<20))
	if err != nil {
		apiServerError(w, err)
		return
	}

	mediaURL, err := h.admitVoiceUpload(r.Context(), user, fh.Filename, data)
	if err != nil {
		switch {
		case errors.Is(err, errMediaUnsupported):
			apiError(w, http.StatusUnsupportedMediaType, "unsupported_media", "Only WebM/Opus, Ogg, MP3, AAC and M4A audio is accepted.")
		case isGateRefusal(err):
			apiGateError(w, err)
		default:
			apiServerError(w, err)
		}
		return
	}
	apiJSON(w, http.StatusCreated, VoiceUploadDTO{URL: mediaURL})
}

// apiV1GifSearch — GET /api/v1/gif/search?q= → 200 GifSearchDTO; trending
// when q is empty.
//
// A deployment with no provider says so in a code the client branches on,
// not as an empty grid: the web picker draws "no GIFs" for both, but a
// client deciding whether to offer the button at all cannot.
func (h *Handler) apiV1GifSearch(w http.ResponseWriter, r *http.Request, _ *model.User) {
	results, err := h.gifLookup(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")))
	if err != nil {
		if errors.Is(err, errGifUnconfigured) {
			apiError(w, http.StatusServiceUnavailable, "gif_search_unavailable", "GIF search is not available on this server.")
			return
		}
		log.Printf("[api/v1] gif search: %v", err)
		apiError(w, http.StatusBadGateway, "gif_search_failed", "GIF search did not answer. Try again in a moment.")
		return
	}
	out := GifSearchDTO{Results: make([]GifResultDTO, 0, len(results))}
	for _, g := range results {
		out.Results = append(out.Results, GifResultDTO{
			ID:         g.ID,
			URL:        g.URL,
			PreviewURL: g.PreviewURL,
			Width:      g.Width,
			Height:     g.Height,
			Title:      g.Title,
		})
	}
	apiJSON(w, http.StatusOK, out)
}

// ── Projections and helpers ──────────────────────────────────────────────────

// videoJobDTO projects the server's view of an upload into the client's.
func videoJobDTO(st *videoJobState) VideoJobDTO {
	dto := VideoJobDTO{UploadID: st.UploadID, Status: st.Status, Error: st.Error}
	if o := st.Output; o != nil {
		dto.setRenditions(o.MasterURL, o.PosterURL, o.WatermarkedURL, o.DurationSecs, o.Width, o.Height)
	}
	if d := st.Duplicate; d != nil {
		dto.DuplicateOfWorkID = d.WorkID
		dto.DuplicateOfHandle = d.OriginalHandle
		dto.setRenditions(d.ExistingMasterURL, d.ExistingPosterURL, "", d.DurationSecs, d.Width, d.Height)
	}
	return dto
}

// videoJobDTOForDuplicate is the admission-time duplicate: the raw hash was
// already known before any job existed, so there is no upload_id to poll.
func videoJobDTOForDuplicate(d *videoRawDuplicate) VideoJobDTO {
	dto := VideoJobDTO{
		Status:            videoJobDuplicate,
		DuplicateOfWorkID: d.WorkID,
		DuplicateOfHandle: d.OriginalHandle,
	}
	dto.setRenditions(d.MasterURL, d.PosterURL, d.WatermarkedURL, float64(d.DurationSecs), d.Width, d.Height)
	return dto
}

// setRenditions fills the media fields when there is a master to point at.
// Without one there are no renditions, and a width of zero would be noise
// rather than a measurement.
func (dto *VideoJobDTO) setRenditions(master, poster, watermarked string, durationSecs float64, width, height int) {
	if master == "" {
		return
	}
	dto.MasterURL = relativeMediaURL(master)
	dto.PosterURL = relativeMediaURL(poster)
	dto.WatermarkedURL = relativeMediaURL(watermarked)
	dto.DurationSecs = &durationSecs
	dto.Width = &width
	dto.Height = &height
}

// relativeMediaURL keeps a media URL in the relative form the rest of /api/v1
// uses. The transcoder and the works table hold relative paths; should either
// ever hand back an absolute one, the host it names is internal and not this
// contract's to leak.
func relativeMediaURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.RequestURI()
}

// formFileAny returns the first multipart file found under any of names. The
// web's forms name the field for the medium (`media`, `audio`); the JSON
// lane's generic name is `file`, and both are accepted so a client built
// against either set of forms works.
func formFileAny(r *http.Request, names ...string) (multipart.File, *multipart.FileHeader, error) {
	var err error
	for _, name := range names {
		var f multipart.File
		var fh *multipart.FileHeader
		if f, fh, err = r.FormFile(name); err == nil {
			return f, fh, nil
		}
	}
	return nil, nil, err
}

// apiGateError answers a banned-content gate refusal in the JSON envelope: a
// registry match is the sender's content being refused, an unanswerable gate
// is this server's, and the client is told which.
func apiGateError(w http.ResponseWriter, err error) {
	if errors.Is(err, errMediaBanned) {
		apiError(w, http.StatusBadRequest, "content_refused", "This content cannot be uploaded.")
		return
	}
	apiError(w, http.StatusServiceUnavailable, "scan_unavailable", "Upload refused — the content safety check could not run. Please try again.")
}
