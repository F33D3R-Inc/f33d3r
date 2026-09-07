package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"

	"f33d3r.com/ios/devserver/internal/store"
)

// The upload lane — the stand-in for Caeor.
//
// Files are content-addressed by SHA-256, so the same bytes uploaded twice are
// one file: the property MediaAsset has on the real platform. The bytes decide
// what a file is and what it is stored as (sniff.go); the declared type and
// name are hints at most. Every route answers the shape in
// feed-engine/internal/handler/api_v1_media.go, which is the shape the Swift
// client decodes.

// uploadMedia — POST /api/v1/media (multipart, field `file`) → 201 {"url":"/media/…"}
//
// Images only. A video goes through uploadVideo (video.go) and a recording
// through uploadVoice, because each of those has more to say than a path.
func (s *Server) uploadMedia(w http.ResponseWriter, r *http.Request, _ *store.User) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The upload could not be read.")
		return
	}
	file, header, err := formFileAny(r, "file", "media")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "No file was attached.")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, 32<<20))
	if err != nil {
		serverError(w, err)
		return
	}
	ext := imageExtensionFor(header.Header.Get("Content-Type"), header.Filename, data)
	if ext == "" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media", "Only JPEG, PNG, GIF, WebP and HEIC images are accepted.")
		return
	}
	url, err := s.storeBytes("uploads", ext, data)
	if err != nil {
		serverError(w, err)
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
func (s *Server) uploadVoice(w http.ResponseWriter, r *http.Request, _ *store.User) {
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

	data, err := io.ReadAll(io.LimitReader(file, 50<<20))
	if err != nil {
		serverError(w, err)
		return
	}
	ext := audioExtensionFor(data)
	if ext == "" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media", "Only WebM/Opus, Ogg, MP3, AAC and M4A audio is accepted.")
		return
	}
	url, err := s.storeBytes("voice", ext, data)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, VoiceUploadDTO{URL: url})
}

// VoiceUploadDTO is the answer to POST /api/v1/media/voice. Mirrors
// feed-engine's VoiceUploadDTO and the Swift `VoiceUpload`.
type VoiceUploadDTO struct {
	URL string `json:"url"`
}

// storeBytes writes data under mediaDir/folder as its content hash and
// returns the path the work references it by. Writing the same bytes twice
// is a no-op.
func (s *Server) storeBytes(folder, ext string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:16]) + ext
	dir := filepath.Join(s.mediaDir, folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", err
		}
	}
	return "/media/" + folder + "/" + name, nil
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
