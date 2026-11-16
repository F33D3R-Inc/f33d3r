package handler

// media.go — the upload lanes.
//
// Every lane ends the same way: the bytes are proved to be what they claim by
// their magic number and streamed to Caeor, the one media-asset authority, on
// record under the signed-in uploader. Caeor puts every upload to Abraxas, the
// integrity authority, before it may become ready: banned bytes are refused
// (errMediaBanned), bytes under review are stored and held (errUploadHeld).
// This brain judges no content itself. Caeor answers with the asset — or, for
// an exact-byte duplicate, with the ORIGINAL asset and its uploader, who keeps
// the credit. Nothing is ever written to a local media tree, and there is no
// fallback: an upload Caeor cannot take is an upload that failed.
//
// A lane hands back the watermarked object Caeor serves (Asset.ViewerURL).
// The clean source never leaves Caeor.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/f33d3r/feed-engine/internal/caeor"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/sitra"
)

// Upload ceilings, per lane.
const (
	imageUploadMax     = 20 << 20
	profileUploadMax   = 10 << 20
	voiceUploadMax     = 50 << 20
	videoUploadMax     = 2 << 30
	encryptedUploadMax = 1 << 30
)

// The admission errors a caller turns into a status. Each lane words its own
// answer; what happened is decided once, here.
var (
	// errMediaUnsupported: the bytes are not a type this route accepts.
	errMediaUnsupported = errors.New("media type not accepted")
	// errUploadStorage: the file could not be stored.
	errUploadStorage = errors.New("upload storage error")
	// errUploadWrite: the file had a place but could not be written there in full.
	errUploadWrite = errors.New("upload write error")
	// errUploadTooLarge: the file is over this lane's ceiling.
	errUploadTooLarge = errors.New("upload is larger than this lane accepts")
	// errUploadNoUploader: nobody signed in made the upload, so nobody could be
	// put on record as its uploader.
	errUploadNoUploader = errors.New("upload has no signed-in uploader")
	// errMediaWithdrawn: the bytes are an exact duplicate of an asset that has
	// been withdrawn from the platform. Deleting media never frees it for a new
	// upload.
	errMediaWithdrawn = errors.New("this media has been removed from the platform and cannot be uploaded again")
	// errMediaBanned: the integrity authority refused the bytes. Caeor has
	// withdrawn the asset and kept it as the record.
	errMediaBanned = errors.New("this content cannot be uploaded")
	// errUploadHeld: the bytes are stored, and held by the integrity authority
	// until a review decides them. Nothing of them is shown meanwhile.
	errUploadHeld = errors.New("this upload is being reviewed before it can be posted; try posting it again once the review is done")
)

// uploadHTTPError answers an admission failure on a cookie-session lane.
func uploadHTTPError(w http.ResponseWriter, lane string, err error) {
	switch {
	case errors.Is(err, errMediaBanned):
		http.Error(w, "This content cannot be uploaded", http.StatusUnprocessableEntity)
	case errors.Is(err, errUploadHeld):
		http.Error(w, errUploadHeld.Error(), http.StatusConflict)
	case errors.Is(err, errUploadNoUploader):
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case errors.Is(err, errMediaUnsupported):
		http.Error(w, "unsupported or invalid file", http.StatusBadRequest)
	case errors.Is(err, errUploadTooLarge):
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
	case errors.Is(err, errMediaWithdrawn):
		http.Error(w, errMediaWithdrawn.Error(), http.StatusConflict)
	case errors.Is(err, caeor.ErrRefused):
		log.Printf("[upload] %s refused by caeor: %v", lane, err)
		http.Error(w, "the file could not be processed", http.StatusUnprocessableEntity)
	default:
		log.Printf("[upload] %s: %v", lane, err)
		http.Error(w, "storage error", http.StatusInternalServerError)
	}
}

// uploaderOf is the person an upload is put on record under, or
// errUploadNoUploader.
func uploaderOf(u *model.User) (caeor.Uploader, error) {
	if u == nil || u.ID == "" || u.ID == "demo_user" || u.PIALID == "" || u.Handle == "" {
		return caeor.Uploader{}, errUploadNoUploader
	}
	return caeor.Uploader{PIAL: u.PIALID, Account: u.ID, Handle: u.Handle}, nil
}

// storeBytes streams an in-memory upload to Caeor.
func (h *Handler) storeBytes(ctx context.Context, kind caeor.Kind, up caeor.Uploader, filename string, data []byte) (*caeor.Asset, error) {
	asset, err := h.caeor.Upload(ctx, kind, up, filename, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, storeError(err)
	}
	return checkStoredAsset(asset)
}

// storeFile streams a spooled upload to Caeor without holding it in memory.
func (h *Handler) storeFile(ctx context.Context, kind caeor.Kind, up caeor.Uploader, filename, path string) (*caeor.Asset, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: opening spooled upload: %v", errUploadStorage, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: reading spooled upload: %v", errUploadStorage, err)
	}
	asset, err := h.caeor.Upload(ctx, kind, up, filename, f, st.Size())
	if err != nil {
		return nil, storeError(err)
	}
	return checkStoredAsset(asset)
}

// storeError names what a failed hand-over to Caeor was: the integrity
// authority's refusal, or a storage failure.
func storeError(err error) error {
	if errors.Is(err, caeor.ErrBanned) {
		return errMediaBanned
	}
	return fmt.Errorf("%w: %w", errUploadStorage, err)
}

// checkStoredAsset refuses an answer that resolved to a withdrawn original, or
// to an asset the integrity authority is still holding.
func checkStoredAsset(a *caeor.Asset) (*caeor.Asset, error) {
	if a.Withdrawn {
		return nil, errMediaWithdrawn
	}
	if a.Held() {
		return nil, errUploadHeld
	}
	if a.Status == caeor.StatusFailed {
		return nil, fmt.Errorf("%w: caeor could not process asset %s: %s", errUploadStorage, a.AssetID, a.Failure)
	}
	return a, nil
}

// spoolUpload copies an upload to a private temp file in the tus spool, so a
// large file is never held in memory. A file over limit is errUploadTooLarge.
// On every error nothing is left behind. Caeor hashes the bytes it receives.
func spoolUpload(src io.Reader, limit int64) (path string, size int64, err error) {
	if err := os.MkdirAll(tusTempDir, 0o700); err != nil {
		return "", 0, fmt.Errorf("%w: %v", errUploadStorage, err)
	}
	f, err := os.CreateTemp(tusTempDir, "spool-*")
	if err != nil {
		return "", 0, fmt.Errorf("%w: %v", errUploadStorage, err)
	}
	path = f.Name()
	size, err = io.Copy(f, io.LimitReader(src, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		removeSpooled(path)
		return "", 0, fmt.Errorf("%w: %v", errUploadWrite, err)
	}
	if size > limit {
		removeSpooled(path)
		return "", 0, errUploadTooLarge
	}
	return path, size, nil
}

// removeSpooled deletes a spooled upload. Already gone is fine; anything else
// is said, because a file that should not exist is still on disk.
func removeSpooled(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("[upload] spooled file %s not removed: %v", path, err)
	}
}

// ── Profile pictures ─────────────────────────────────────────────────────────

// admitProfileImage is the one admission path for an avatar or a header:
// magic number, then Caeor, which puts it to the integrity authority like any
// upload (permanently hosted media gets the same gate a work does). Returns
// the URL to store on the profile.
func (h *Handler) admitProfileImage(ctx context.Context, user *model.User, kind caeor.Kind, filename string, data []byte) (string, error) {
	up, err := uploaderOf(user)
	if err != nil {
		return "", err
	}
	if imageExtensionFor(data) == "" {
		return "", errMediaUnsupported
	}
	asset, err := h.storeBytes(ctx, kind, up, "image"+imageExtensionFor(data), data)
	if err != nil {
		return "", err
	}
	url := asset.ViewerURL()
	if url == "" {
		return "", fmt.Errorf("%w: caeor gave %s asset %s no viewer URL", errUploadStorage, kind, asset.AssetID)
	}
	return url, nil
}

// readProfileUpload reads one profile picture field, bounded.
func readProfileUpload(r *http.Request, field string) ([]byte, string, error) {
	file, fh, err := r.FormFile(field)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, profileUploadMax+1))
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", errUploadWrite, err)
	}
	if len(data) > profileUploadMax {
		return nil, "", errUploadTooLarge
	}
	return data, fh.Filename, nil
}

func (h *Handler) uploadAvatar(w http.ResponseWriter, r *http.Request) {
	h.uploadProfileImage(w, r, caeor.KindAvatar, "avatar")
}

func (h *Handler) uploadHeader(w http.ResponseWriter, r *http.Request) {
	h.uploadProfileImage(w, r, caeor.KindHeader, "header")
}

// uploadProfileImage is POST /upload/avatar and POST /upload/header.
func (h *Handler) uploadProfileImage(w http.ResponseWriter, r *http.Request, kind caeor.Kind, field string) {
	if err := r.ParseMultipartForm(profileUploadMax); err != nil {
		http.Error(w, "file too large", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	data, filename, err := readProfileUpload(r, field)
	if err != nil {
		if errors.Is(err, http.ErrMissingFile) {
			http.Error(w, "no file", http.StatusBadRequest)
			return
		}
		uploadHTTPError(w, field, err)
		return
	}
	url, err := h.admitProfileImage(r.Context(), user, kind, filename, data)
	if err != nil {
		uploadHTTPError(w, field, err)
		return
	}
	if h.db == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	save := &model.ProfileSave{UserID: user.ID, ThemeID: user.ThemeID}
	if kind == caeor.KindAvatar {
		save.AvatarURL = url
	} else {
		save.HeaderURL = url
	}
	if err := dbpkg.SaveProfile(h.db, save); err != nil {
		log.Printf("[media] %s stored for @%s but not saved to the profile: %v", field, user.Handle, err)
		http.Error(w, "the picture could not be saved to your profile", http.StatusInternalServerError)
		return
	}
	go h.publishProfileFacets(user.ID)
	h.evictCachedAccount(user.ID, "")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"url": url}); err != nil {
		log.Printf("[media] encode %s answer: %v", field, err)
	}
}

// ── Work media: the compose box ──────────────────────────────────────────────

func (h *Handler) uploadPostMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	// Memory ceiling for multipart parsing. Larger parts stream to disk.
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

	user := h.userFromRequest(nil, r)

	// ── Video: spooled, gated, then handed to Caeor in the background. The
	// client polls /upload/tus/status/{upload_id} until the asset is ready.
	if videoExtensions[ext] {
		upload, err := h.admitVideoUpload(r.Context(), user, fh.Filename, file)
		if err != nil {
			uploadHTTPError(w, "video", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{
			"kind":      "video_queued",
			"upload_id": upload.ID,
		}); err != nil {
			log.Printf("[upload] encode video_queued: %v", err)
		}
		return
	}

	// ── Image ────────────────────────────────────────────────────────────────
	fileBytes, err := io.ReadAll(io.LimitReader(file, imageUploadMax+1))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	if len(fileBytes) > imageUploadMax {
		uploadHTTPError(w, "image", errUploadTooLarge)
		return
	}
	adm, err := h.admitImageUpload(r.Context(), user, fh.Filename, fileBytes)
	if err != nil {
		uploadHTTPError(w, "image", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(adm.answer()); err != nil {
		log.Printf("[upload] encode image answer: %v", err)
	}
}

// ── What a file is, by its bytes ─────────────────────────────────────────────
//
// One sniff per medium, shared by the web compose box and the JSON lane. A
// declared type is what the sender says; the magic number is what the file is.

// videoExtensions are the containers accepted as video, by the name the sender
// gave the file. Two families: ISO BMFF (.mp4 .mov .m4v) and Matroska/EBML
// (.webm .mkv).
var videoExtensions = map[string]bool{".mp4": true, ".mov": true, ".webm": true, ".m4v": true, ".mkv": true}

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
// this framing.
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

// videoFilenameFor is the name a video is recorded under: the declared
// extension is kept when it belongs to the container the bytes prove, and
// replaced by that container's canonical extension when it does not. false
// when the bytes are not video at all.
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
// in an ISO BMFF box; iOS AVAudioRecorder gives AAC in an M4A box. MP3, FLAC,
// WAV and raw ADTS AAC are accepted for files people already have.
func audioExtensionFor(data []byte) string {
	if len(data) < 4 {
		return ""
	}
	switch {
	case isEBML(data):
		return ".webm"
	case string(data[:4]) == "OggS":
		return ".ogg"
	case string(data[:4]) == "fLaC":
		return ".flac"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return ".wav"
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

// admitVideoUpload is the one admission path for a video sent whole (the web
// compose box, the React With Video studio and the native clients' POST
// /api/v1/media/video). The bytes have already been proved a video container.
//
// In order: the file is spooled (2 GiB cap), so a multi-gigabyte video is
// never held in memory; then the upload is recorded and streamed to Caeor in
// the background (handOffVideo), where the integrity authority judges it, and
// the caller answers with the upload id the client polls. A refusal reaches the
// client through that poll. On every error the spooled file is removed.
func (h *Handler) admitVideoUpload(ctx context.Context, uploader *model.User, filename string, src io.Reader) (*tusUpload, error) {
	up, err := uploaderOf(uploader)
	if err != nil {
		return nil, err
	}
	path, size, err := spoolUpload(src, videoUploadMax)
	if err != nil {
		return nil, err
	}
	upload := newTusUpload(up, filename, path, size)
	upload.complete = true
	globalTusStore.put(upload)
	go h.handOffVideo(upload, caeor.KindVideo)
	return upload, nil
}

// ── Image admission ──────────────────────────────────────────────────────────

// imageAdmission is what admitting one image came to.
type imageAdmission struct {
	URL   string
	Asset *caeor.Asset
	// OriginalHandle names the original uploader when the bytes were an exact
	// duplicate of somebody else's image; Self when they were the uploader's own.
	OriginalHandle string
	Duplicate      bool
	Self           bool
}

// answer is the compose box's image answer.
func (a *imageAdmission) answer() map[string]string {
	out := map[string]string{"kind": "image", "url": a.URL}
	switch {
	case a.Duplicate && a.Self:
		out["status"] = "self_duplicate"
	case a.Duplicate:
		out["status"] = "duplicate"
		out["original_handle"] = a.OriginalHandle
	}
	return out
}

// admitImageUpload is the one admission path for a still image, whichever
// route received it: the web compose box and the native clients' POST
// /api/v1/media. In order: magic number, then Caeor, which puts the bytes to
// the integrity authority before they can be shown. An exact-byte duplicate of somebody else's
// image comes back as their asset: the work that uses it is a credited
// reference, and the original uploader keeps the credit.
func (h *Handler) admitImageUpload(ctx context.Context, uploader *model.User, filename string, data []byte) (*imageAdmission, error) {
	up, err := uploaderOf(uploader)
	if err != nil {
		return nil, err
	}
	if imageExtensionFor(data) == "" {
		return nil, errMediaUnsupported
	}
	asset, err := h.storeBytes(ctx, caeor.KindImage, up, "image"+imageExtensionFor(data), data)
	if err != nil {
		return nil, err
	}
	adm := &imageAdmission{URL: asset.ViewerURL(), Asset: asset}
	if adm.URL == "" {
		return nil, fmt.Errorf("%w: caeor gave image asset %s no viewer URL", errUploadStorage, asset.AssetID)
	}
	if asset.DuplicateOf != nil || asset.CreditTo != nil {
		// An exact duplicate resolves to the original asset; an ownership
		// finding names the original owner on this new asset. Either way the
		// credit goes to the person the bytes came from.
		owner := asset.CreditPIAL()
		adm.Duplicate = true
		adm.Self = owner == up.PIAL
		if !adm.Self {
			handle, _, err := dbpkg.UploaderCredit(h.db, owner)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", errUploadStorage, err)
			}
			adm.OriginalHandle = handle
		}
	}
	go h.publishMediaUploadedEvent("", adm.URL, up.PIAL)
	return adm, nil
}

// ── Voice ────────────────────────────────────────────────────────────────────

// uploadVoicePost accepts a recorded audio blob for a voice post and answers
// {kind:"voice", url:"..."}. The duration is the recorder's to report, and the
// compose box sends it with the post.
func (h *Handler) uploadVoicePost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(voiceUploadMax); err != nil {
		http.Error(w, "could not parse upload", http.StatusBadRequest)
		return
	}
	file, fh, err := r.FormFile("audio")
	if err != nil {
		http.Error(w, "no file provided", http.StatusBadRequest)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, voiceUploadMax+1))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	if len(data) > voiceUploadMax {
		uploadHTTPError(w, "voice", errUploadTooLarge)
		return
	}
	mediaURL, err := h.admitVoiceUpload(r.Context(), h.userFromRequest(nil, r), fh.Filename, data)
	if err != nil {
		uploadHTTPError(w, "voice", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"kind": "voice", "url": mediaURL}); err != nil {
		log.Printf("[upload] encode voice answer: %v", err)
	}
}

// admitVoiceUpload is the one admission path for voice audio, whichever route
// received it: the web recorder and the native clients' POST /api/v1/media/voice.
// The bytes decide the type; then Caeor, which puts the audio to the
// integrity authority.
func (h *Handler) admitVoiceUpload(ctx context.Context, uploader *model.User, filename string, data []byte) (string, error) {
	return h.admitAudio(ctx, uploader, caeor.KindVoice, filename, data, "voice-upload")
}

// admitAudio stores one audio file of kind (a voice recording or a music
// track) and returns the audio object Caeor serves.
func (h *Handler) admitAudio(ctx context.Context, uploader *model.User, kind caeor.Kind, filename string, data []byte, label string) (string, error) {
	if audioExtensionFor(data) == "" {
		return "", errMediaUnsupported
	}
	up, err := uploaderOf(uploader)
	if err != nil {
		return "", err
	}
	asset, err := h.storeBytes(ctx, kind, up, audioFilename(filename, data), data)
	if err != nil {
		return "", err
	}
	url := asset.ViewerURL()
	if url == "" {
		return "", fmt.Errorf("%w: caeor gave %s asset %s no audio URL", errUploadStorage, kind, asset.AssetID)
	}
	go h.publishMediaUploadedEvent("", url, up.PIAL)
	return url, nil
}

// audioFilename is the name an audio file is handed to Caeor under: the bytes
// pick the extension, whatever the recorder called the file.
func audioFilename(declared string, data []byte) string {
	ext := audioExtensionFor(data)
	base := strings.TrimSuffix(filepath.Base(declared), filepath.Ext(declared))
	if base == "" || base == "." {
		base = "audio"
	}
	return base + ext
}

// ── Encrypted (marketplace paid content) ─────────────────────────────────────

// uploadEncryptedMedia stores a client-encrypted blob (marketplace paid
// content). The bytes are AES-256-GCM ciphertext made in the browser, so the
// server stays zero-knowledge: the blob is registered with Caeor as kind
// encrypted, exactly as it arrived, and the content key is delivered only on
// payment confirmation. Caeor puts the ciphertext to the integrity authority
// like every upload; only its exact bytes can be matched.
// Up to 1 GiB, streamed — never held in memory.
// POST /api/media/upload-encrypted → {"url": "..."}
func (h *Handler) uploadEncryptedMedia(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	user := h.userFromRequest(w, r)
	up, err := uploaderOf(user)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	// Keep only 32 MB in memory; anything larger spills to a temp file.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, `{"error":"file_too_large"}`, http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"no_file"}`, http.StatusBadRequest)
		return
	}
	defer file.Close()

	path, _, err := spoolUpload(file, encryptedUploadMax)
	if err != nil {
		if errors.Is(err, errUploadTooLarge) {
			http.Error(w, `{"error":"file_too_large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		log.Printf("[upload] encrypted spool for @%s: %v", up.Handle, err)
		http.Error(w, `{"error":"storage_error"}`, http.StatusInternalServerError)
		return
	}
	defer removeSpooled(path)

	asset, err := h.storeFile(r.Context(), caeor.KindEncrypted, up, "content.bin", path)
	if errors.Is(err, errMediaBanned) {
		http.Error(w, `{"error":"content_refused"}`, http.StatusUnprocessableEntity)
		return
	}
	if errors.Is(err, errUploadHeld) {
		http.Error(w, `{"error":"held_for_review"}`, http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("[upload] encrypted for @%s: %v", up.Handle, err)
		http.Error(w, `{"error":"storage_error"}`, http.StatusInternalServerError)
		return
	}
	url := asset.ViewerURL()
	if url == "" {
		log.Printf("[upload] encrypted asset %s for @%s has no URL", asset.AssetID, up.Handle)
		http.Error(w, `{"error":"storage_error"}`, http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]string{"url": url}); err != nil {
		log.Printf("[upload] encode encrypted answer: %v", err)
	}
}

// publishMediaUploadedEvent fires a content.events message to Sitra Achra
// after a media asset is confirmed stored in Caeor, so Abraxas Shield begins
// scoring it.
func (h *Handler) publishMediaUploadedEvent(postID, mediaURL, pialID string) {
	if h.sitra == nil || pialID == "" {
		return
	}
	payload, err := json.Marshal(map[string]string{
		"event":     "media.uploaded",
		"post_id":   postID,
		"media_url": mediaURL,
		"pial_id":   pialID,
	})
	if err != nil {
		log.Printf("[sitra] encode media.uploaded for %s: %v", mediaURL, err)
		return
	}
	h.sitra.Publish(context.Background(), sitra.TopicContent, []byte(pialID), payload)
}
