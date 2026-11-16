package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"f33d3r.com/ios/devserver/internal/store"
)

// The upload lane — the stand-in for Caeor.
//
// Every file is an asset, addressed by its content hash, and served at
// `/static/media/a/<asset_id>/<rendition>` — the form Caeor serves. The same
// bytes uploaded twice are one asset, and it stays its first uploader's: a
// second uploader is handed the original, which a work may cite as a credited
// reference. An asset the integrity authority has held answers 409
// held_for_review, and a banned one 422 content_refused (store.MediaAsset;
// a tester sets the status by hand). The bytes decide what a file is
// (sniff.go). Every route answers the shape in
// feed-engine/internal/handler/api_v1_media.go.

// assetPathPrefix is where Caeor serves assets, and where this server does.
const assetPathPrefix = "/static/media/a/"

// uploadMedia — POST /api/v1/media (multipart, field `file`) → 201 {"url","bytes"}
//
// Images only. A video goes through uploadVideo (video.go) and a recording
// through uploadVoice, because each of those has more to say than a path.
func (s *Server) uploadMedia(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The upload could not be read.")
		return
	}
	file, _, err := formFileAny(r, "file", "media")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "No file was attached.")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, 20<<20+1))
	if err != nil {
		serverError(w, err)
		return
	}
	if len(data) > 20<<20 {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "The file is larger than this upload accepts.")
		return
	}
	ext := imageExtensionFor(data)
	if ext == "" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media", "Only JPEG, PNG, GIF and WebP images are accepted.")
		return
	}
	url, _, ok := s.admitBytes(w, r, u, "image", "image"+ext, data)
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"url":   url,
		"bytes": len(data),
	})
}

// uploadVoice — POST /api/v1/media/voice (multipart, field `audio` or `file`)
// → 201 VoiceUploadDTO.
//
// There is no duration in the answer on purpose: the server does not probe
// audio, so the recorder's own measurement is what the work carries as
// voice_duration_secs. The same rule as feed-engine's admitVoiceUpload.
func (s *Server) uploadVoice(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The upload could not be read.")
		return
	}
	file, _, err := formFileAny(r, "audio", "file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "No file was attached.")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, 50<<20+1))
	if err != nil {
		serverError(w, err)
		return
	}
	if len(data) > 50<<20 {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "The file is larger than this upload accepts.")
		return
	}
	ext := audioExtensionFor(data)
	if ext == "" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media", "Only WebM/Opus, Ogg, MP3, AAC and M4A audio is accepted.")
		return
	}
	url, _, ok := s.admitBytes(w, r, u, "voice", "audio"+ext, data)
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, VoiceUploadDTO{URL: url})
}

// VoiceUploadDTO is the answer to POST /api/v1/media/voice. Mirrors
// feed-engine's VoiceUploadDTO and the Swift `VoiceUpload`.
type VoiceUploadDTO struct {
	URL string `json:"url"`
}

// assetIDOf is the asset id of a file: its content hash.
func assetIDOf(sum []byte) string { return hex.EncodeToString(sum[:16]) }

// admitBytes stores data as an asset and records its uploader, then answers
// the integrity authority's refusals itself. ok is false when an answer has
// already been written.
func (s *Server) admitBytes(w http.ResponseWriter, r *http.Request, u *store.User, kind, rendition string, data []byte) (url string, asset *store.MediaAsset, ok bool) {
	sum := sha256.Sum256(data)
	assetID := assetIDOf(sum[:])
	path, err := s.assetFile(assetID, rendition)
	if err != nil {
		serverError(w, err)
		return "", nil, false
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			serverError(w, err)
			return "", nil, false
		}
	} else if err != nil {
		serverError(w, err)
		return "", nil, false
	}
	asset, ok = s.admitAsset(w, r, u, assetID, kind)
	return assetPathPrefix + assetID + "/" + rendition, asset, ok
}

// admitAsset records an asset under its uploader and refuses a held or banned
// one in api_v1_media.go's words. ok is false when an answer has been written.
func (s *Server) admitAsset(w http.ResponseWriter, r *http.Request, u *store.User, assetID, kind string) (*store.MediaAsset, bool) {
	asset, err := s.store.AdmitMediaAsset(r.Context(), assetID, u.ID, kind)
	if err != nil {
		serverError(w, err)
		return nil, false
	}
	switch asset.Status {
	case store.AssetHeld:
		writeError(w, http.StatusConflict, "held_for_review",
			"this upload is being reviewed before it can be posted; try posting it again once the review is done")
		return nil, false
	case store.AssetBanned:
		writeError(w, http.StatusUnprocessableEntity, "content_refused", "This content cannot be uploaded.")
		return nil, false
	}
	return asset, true
}

// assetFile is the file of one rendition of an asset, its directory made.
func (s *Server) assetFile(assetID, rendition string) (string, error) {
	dir := filepath.Join(s.mediaDir, "a", assetID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, rendition), nil
}

// assetIDFromPath reads the asset id out of a path this server issued, or ""
// when the path is not one.
func assetIDFromPath(path string) string {
	rest, ok := strings.CutPrefix(path, assetPathPrefix)
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(rest, "/")
	if !ok || len(id) != 32 {
		return ""
	}
	return id
}

// formFileAny returns the first of the named multipart fields that carries a
// file. The web's forms and the native client name the field differently for
// the same upload; both are served.
func formFileAny(r *http.Request, names ...string) (multipart.File, *multipart.FileHeader, error) {
	var (
		f   multipart.File
		fh  *multipart.FileHeader
		err error
	)
	for _, name := range names {
		if f, fh, err = r.FormFile(name); err == nil {
			return f, fh, nil
		}
	}
	return nil, nil, err
}

// mediaDirFor derives the media directory from the data directory.
func mediaDirFor(dataDir string) string {
	return filepath.Join(dataDir, "media")
}
