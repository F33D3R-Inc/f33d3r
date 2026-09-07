package handler

// vision_compose.go — the Vision compose path.
//
// The Vision lane is ephemeral: a Vision is not a Work, mints no naming-plane
// node and expires on its own. This file is the lane's only writer.
//
// Every artboard choice an author makes is a preset KEY. The browser names a
// key, the server decides what that key looks like and renders the finished
// Fragment. No Vision surface is ever styled by the browser.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/scanclient"
	"github.com/f33d3r/feed-engine/internal/sitra"
)

// ── closed value sets ────────────────────────────────────────────────────────

// visionComposeContentTypes mirrors the CHECK constraint in migration 0008.
// The `kind='vision'` defect came from trusting a value the browser sent; this
// set is tested before anything is written, and an unknown value is refused.
var visionComposeContentTypes = map[string]bool{
	"text": true, "image": true, "video": true, "share": true,
}

// visionBodyMax is the artboard's hard rune ceiling.
const visionBodyMax = 280

// visionMediaClass is the Caeor media class for Vision objects. It is deliberately
// not "post": a Vision object is expirable and its purge is owned by the
// vision_media row written alongside it.
const visionMediaClass = "vision"

// visionUploadDir is the local fallback location when Caeor is unreachable.
var visionUploadDir = filepath.Join("web", "static", "uploads", "visions")

// visionMediaByteCeiling caps a single Vision image.
const visionMediaByteCeiling = 20 << 20

// errVisionMediaBanned is returned when a known-banned hash is offered.
var errVisionMediaBanned = errors.New("vision: media matches a banned hash")

// ── artboard presets ─────────────────────────────────────────────────────────

// visionPreset is one server-owned artboard choice. Key is what the row stores;
// Label is what the picker reads. The look lives in styles.css, keyed by Key —
// never in a style attribute and never in a value the browser supplies.
type visionPreset struct {
	Key   string
	Label string
}

var (
	visionBackgrounds = []visionPreset{
		{"void", "Void"},
		{"ember", "Ember"},
		{"tide", "Tide"},
		{"bloom", "Bloom"},
		{"pulp", "Pulp"},
		{"signal", "Signal"},
	}
	visionTypefaces = []visionPreset{
		{"grotesk", "Grotesk"},
		{"serif", "Serif"},
		{"mono", "Mono"},
		{"display", "Display"},
	}
	visionTypeScalePresets = []visionPreset{
		{"auto", "Auto"},
		{"xl", "Huge"},
		{"l", "Large"},
		{"m", "Medium"},
		{"s", "Small"},
	}
	visionAlignPresets = []visionPreset{
		{"left", "Left"},
		{"center", "Centre"},
		{"right", "Right"},
	}
)

const (
	visionDefaultBackground = "void"
	visionDefaultTypeface   = "grotesk"
	visionDefaultTypeScale  = "auto"
	visionDefaultAlign      = "center"
)

// visionPresetSets indexes the tables above for O(1) validation.
var (
	visionBackgroundSet = visionPresetSet(visionBackgrounds)
	visionTypefaceSet   = visionPresetSet(visionTypefaces)
	visionTypeScaleSet  = visionPresetSet(visionTypeScalePresets)
	visionAlignSet      = visionPresetSet(visionAlignPresets)
)

func visionPresetSet(presets []visionPreset) map[string]bool {
	out := make(map[string]bool, len(presets))
	for _, p := range presets {
		out[p.Key] = true
	}
	return out
}

// visionPresetOrDefault resolves a stored key for rendering. An unknown key
// renders as the default rather than as nothing — a row written before a preset
// was retired must still produce a complete Fragment.
func visionPresetOrDefault(key string, set map[string]bool, fallback string) string {
	key = strings.TrimSpace(key)
	if key == "" || !set[key] {
		return fallback
	}
	return key
}

// visionPresetOrError resolves a key on the WRITE path, where an unknown value is
// a rejection and not something to paper over.
func visionPresetOrError(field, key string, set map[string]bool, fallback string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return fallback, nil
	}
	if !set[key] {
		return "", fmt.Errorf("unknown %s %q", field, key)
	}
	return key, nil
}

// resolveVisionTypeScale turns the stored scale into the rung actually rendered.
// "auto" is resolved here, on the server: the browser never measures text.
func resolveVisionTypeScale(body, scale string) string {
	scale = visionPresetOrDefault(scale, visionTypeScaleSet, visionDefaultTypeScale)
	if scale != "auto" {
		return scale
	}
	switch n := len([]rune(strings.TrimSpace(body))); {
	case n <= 24:
		return "xl"
	case n <= 70:
		return "l"
	case n <= 150:
		return "m"
	default:
		return "s"
	}
}

// ── views ────────────────────────────────────────────────────────────────────

// visionArtboardView is the finished text artboard: every field is a resolved
// preset key, and Scale is the concrete rung, never "auto".
type visionArtboardView struct {
	EntityID   string
	Body       string
	Background string
	Typeface   string
	Scale      string
	Align      string
	IsEmpty    bool
}

// buildVisionArtboard resolves raw preset keys into a renderable artboard.
func buildVisionArtboard(entityID, body, background, typeface, scale, align string) visionArtboardView {
	body = strings.TrimSpace(body)
	if len([]rune(body)) > visionBodyMax {
		body = string([]rune(body)[:visionBodyMax])
	}
	if entityID == "" {
		entityID = "new"
	}
	return visionArtboardView{
		EntityID:   entityID,
		Body:       body,
		Background: visionPresetOrDefault(background, visionBackgroundSet, visionDefaultBackground),
		Typeface:   visionPresetOrDefault(typeface, visionTypefaceSet, visionDefaultTypeface),
		Scale:      resolveVisionTypeScale(body, scale),
		Align:      visionPresetOrDefault(align, visionAlignSet, visionDefaultAlign),
		IsEmpty:    body == "",
	}
}

// visionComposerView is everything the compose Facets read.
type visionComposerView struct {
	Mode        string // text | media
	Handle      string
	CanNSFW     bool
	IsNSFW      bool
	Error       string
	Body        string
	Background  string
	Typeface    string
	TypeScale   string
	Align       string
	Backgrounds []visionPreset
	Typefaces   []visionPreset
	TypeScales  []visionPreset
	Aligns      []visionPreset
	Artboard    visionArtboardView
}

// visionCreatedView is the confirmation the author is handed after the write.
type visionCreatedView struct {
	VisionID     string
	Handle      string
	ContentType string
	ExpiryLabel string
	Artboard    visionArtboardView
	HasArtboard bool
	PreviewURL  string
	IsVideo     bool
}

// visionExpiryLabel says how long the Vision has left, in words, decided here.
func visionExpiryLabel(expiresAt time.Time) string {
	d := time.Until(expiresAt)
	switch {
	case d <= 0:
		return "expired"
	case d < time.Minute:
		return "gone in under a minute"
	case d < time.Hour:
		return fmt.Sprintf("gone in %dm", int(d.Minutes()))
	case d < 48*time.Hour:
		// Rounded, not truncated: a Vision posted a microsecond ago has 24h left,
		// and reading "gone in 23h" on the confirmation is simply wrong.
		return fmt.Sprintf("gone in %dh", int(d.Round(time.Hour).Hours()))
	default:
		return fmt.Sprintf("gone in %dd", int(d.Round(time.Hour).Hours()/24))
	}
}

// buildVisionComposer assembles the composer for one author.
func buildVisionComposer(mode string, user *model.User, r *http.Request) visionComposerView {
	if mode != "media" {
		mode = "text"
	}
	body := ""
	background, typeface, typeScale, align := visionDefaultBackground, visionDefaultTypeface, visionDefaultTypeScale, visionDefaultAlign
	isNSFW := false
	if r != nil {
		body = strings.TrimSpace(r.FormValue("body"))
		background = visionPresetOrDefault(r.FormValue("artboard_background"), visionBackgroundSet, visionDefaultBackground)
		typeface = visionPresetOrDefault(r.FormValue("artboard_typeface"), visionTypefaceSet, visionDefaultTypeface)
		typeScale = visionPresetOrDefault(r.FormValue("artboard_type_scale"), visionTypeScaleSet, visionDefaultTypeScale)
		align = visionPresetOrDefault(r.FormValue("artboard_align"), visionAlignSet, visionDefaultAlign)
		isNSFW = r.FormValue("nsfw") == "1"
	}
	v := visionComposerView{
		Mode:        mode,
		CanNSFW:     visionCanPostNSFW(user),
		IsNSFW:      isNSFW,
		Body:        body,
		Background:  background,
		Typeface:    typeface,
		TypeScale:   typeScale,
		Align:       align,
		Backgrounds: visionBackgrounds,
		Typefaces:   visionTypefaces,
		TypeScales:  visionTypeScalePresets,
		Aligns:      visionAlignPresets,
		Artboard:    buildVisionArtboard("new", body, background, typeface, typeScale, align),
	}
	if user != nil {
		v.Handle = user.Handle
	}
	if !v.CanNSFW {
		v.IsNSFW = false
	}
	return v
}

// ── actors ───────────────────────────────────────────────────────────────────

// visionActor returns the account allowed to write a Vision, or nil. A Vision row
// carries author_pial, so an account without a PIAL cannot author one.
func visionActor(u *model.User) *model.User {
	if u == nil || u.ID == "" || u.ID == "demo_user" || u.PIALID == "" {
		return nil
	}
	return u
}

// visionCanPostNSFW mirrors the live lane's clearance test.
func visionCanPostNSFW(u *model.User) bool {
	if u == nil {
		return false
	}
	return u.IsAdultCreator || (u.IsAdult && u.ContentSetting == "adult_enabled")
}

// ── media ────────────────────────────────────────────────────────────────────

// visionImageExts are the container types a Vision image may arrive as.
var visionImageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
}

// visionSniffImage rejects an extension the leading bytes do not back up, so a
// renamed file cannot enter the lane behind an image extension.
func visionSniffImage(b []byte, filename string) (string, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if !visionImageExts[ext] {
		return "", fmt.Errorf("unsupported file type %q — use jpg, png, gif or webp", ext)
	}
	if len(b) < 4 {
		return "", errors.New("file is too short to be an image")
	}
	switch {
	case b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF: // JPEG
		return ext, nil
	case b[0] == 0x89 && b[1] == 0x50 && b[2] == 0x4E && b[3] == 0x47: // PNG
		return ext, nil
	case string(b[:4]) == "GIF8": // GIF
		return ext, nil
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP": // WebP
		return ext, nil
	}
	return "", errors.New("file content does not match its declared type")
}

// storeVisionImage runs the banned-hash gate, then stores the object under the
// expirable Vision media class. The returned URL is also the object key, which is
// what the vision_media purge sweep later deletes.
func (h *Handler) storeVisionImage(ctx context.Context, fileBytes []byte, filename, pialID string) (string, error) {
	ext, err := visionSniffImage(fileBytes, filename)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(fileBytes)
	digest := hex.EncodeToString(sum[:])

	// Ephemeral content is not exempt: the same gate the permanent lanes use,
	// exact bytes plus the perceptual layer.
	if gerr := h.bannedContentGate(ctx, gateInput{
		Kind:     scanclient.KindImage,
		Filename: filename,
		Data:     fileBytes,
		SHA256:   digest,
		PIALID:   pialID,
		Label:    "vision-image",
	}); gerr != nil {
		if errors.Is(gerr, errMediaBanned) {
			return "", errVisionMediaBanned
		}
		return "", gerr
	}

	mediaURL, err := h.caeorUpload(fileBytes, filename, visionMediaClass)
	if err != nil {
		log.Printf("[vision] caeor unavailable for vision media, storing locally: %v", err)
		if mkErr := os.MkdirAll(visionUploadDir, 0o755); mkErr != nil {
			return "", fmt.Errorf("vision: media directory: %w", mkErr)
		}
		name := uuid.New().String() + ext
		if wErr := os.WriteFile(filepath.Join(visionUploadDir, name), fileBytes, 0o644); wErr != nil {
			return "", fmt.Errorf("vision: store media: %w", wErr)
		}
		mediaURL = "/static/uploads/visions/" + name
	}
	if mediaURL == "" {
		return "", errors.New("vision: media store returned no URL")
	}
	return mediaURL, nil
}

// visionLocalAssetURL keeps a Vision pointed at this platform's own storage. A
// caller-supplied absolute URL would let a Vision render a remote asset nothing
// here can scan, watermark or purge.
func visionLocalAssetURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("media_url is required")
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "", errors.New("media_url must be a path served by this platform")
	}
	if strings.Contains(raw, "..") {
		return "", errors.New("media_url is not a valid asset path")
	}
	return raw, nil
}

// visionSharedWorkID accepts either a bare work id or a pasted f33d3r work link
// and returns the work UUID. Anything else is refused rather than handed to the
// driver as if it were an identifier.
func visionSharedWorkID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("pick a work to share")
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.TrimRight(raw, "/")
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		raw = raw[i+1:]
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return "", errors.New("that is not a f33d3r work link")
	}
	return id.String(), nil
}

// visionDimension parses an optional numeric form field. A malformed value is an
// error, never a silent zero.
func visionDimension(field, raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a whole number", field)
	}
	return n, nil
}

// visionDurationSecs parses the optional video duration.
func visionDurationSecs(raw string) (float32, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(raw, 32)
	if err != nil || f < 0 {
		return 0, errors.New("duration_secs must be a positive number")
	}
	return float32(f), nil
}

// ── Fragment rendering ───────────────────────────────────────────────────────

// renderVisionFragment renders one Vision Facet. A failure is returned, never
// swallowed: a half-built Fragment must not reach a surface.
func (h *Handler) renderVisionFragment(name string, data interface{}) (string, error) {
	if h.partial == nil {
		return "", errors.New("vision: partial template set unavailable")
	}
	var buf strings.Builder
	if err := h.partial.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("vision: render %s: %w", name, err)
	}
	if buf.Len() == 0 {
		return "", fmt.Errorf("vision: %s produced an empty fragment", name)
	}
	return buf.String(), nil
}

// renderVisionComposerError answers a rejected compose with the composer the
// author was using, carrying the reason. The browser is handed a Fragment, not
// an error string it would have to render itself.
func (h *Handler) renderVisionComposerError(w http.ResponseWriter, r *http.Request, user *model.User, mode, msg string, status int) {
	v := buildVisionComposer(mode, user, r)
	v.Error = msg
	name := "vision_text_composer"
	if v.Mode == "media" {
		name = "vision_media_composer"
	}
	frag, err := h.renderVisionFragment(name, v)
	if err != nil {
		log.Printf("[vision] %v", err)
		http.Error(w, msg, status)
		return
	}
	// A Fragment-swapping caller only applies a 2xx response, so a rejection it
	// must read is carried by the rendered Fragment and the status describes the
	// transport. Every other caller — the camera's own fetch — gets the honest
	// code and the same rendered reason.
	if r.Header.Get("HX-Request") == "true" {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(frag))
}

// ── compose surfaces ─────────────────────────────────────────────────────────

// facetVisionComposer — GET /facets/vision/composer?mode=text|media
// The compose Composite Facet, rendered whole on the server.
func (h *Handler) facetVisionComposer(w http.ResponseWriter, r *http.Request) {
	user := visionActor(h.userFromRequest(w, r))
	if user == nil {
		http.Error(w, "log in to post a Vision", http.StatusUnauthorized)
		return
	}
	mode := strings.TrimSpace(r.URL.Query().Get("mode"))
	v := buildVisionComposer(mode, user, nil)
	name := "vision_text_composer"
	if v.Mode == "media" {
		name = "vision_media_composer"
	}
	h.renderPartial(w, name, v)
}

// facetVisionArtboardHandler — POST /facets/vision/artboard
// A preset change is a server round trip: the artboard is re-rendered here and
// the browser swaps the returned Fragment in. The browser never styles it.
func (h *Handler) facetVisionArtboardHandler(w http.ResponseWriter, r *http.Request) {
	user := visionActor(h.userFromRequest(w, r))
	if user == nil {
		http.Error(w, "log in to post a Vision", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the artboard form", http.StatusBadRequest)
		return
	}
	art := buildVisionArtboard(
		"new",
		r.FormValue("body"),
		r.FormValue("artboard_background"),
		r.FormValue("artboard_typeface"),
		r.FormValue("artboard_type_scale"),
		r.FormValue("artboard_align"),
	)
	h.renderPartial(w, "vision_artboard", art)
}

// ── create ───────────────────────────────────────────────────────────────────

// visionCreate — POST /visions
// The single write into the ephemeral lane. Accepts a form or a multipart body:
// the camera posts its captured frame here, the composers post their form here.
//
// scan_state is left at the schema default 'pending'. The verdict belongs to the
// content scan, never to the insert.
func (h *Handler) visionCreate(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	user := visionActor(h.userFromRequest(w, r))
	if user == nil {
		http.Error(w, "log in to post a Vision", http.StatusUnauthorized)
		return
	}

	isMultipart := strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data")
	if isMultipart {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "could not read the upload", http.StatusBadRequest)
			return
		}
	} else if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}

	// Content type is checked against the closed set before anything else runs.
	contentType := strings.TrimSpace(r.FormValue("content_type"))
	if contentType == "" {
		contentType = "text"
	}
	if !visionComposeContentTypes[contentType] {
		log.Printf("[vision] rejected unknown content_type %q from PIAL %s", contentType, user.PIALID)
		http.Error(w, "unknown Vision content type", http.StatusBadRequest)
		return
	}

	composerMode := "text"
	if contentType != "text" {
		composerMode = "media"
	}

	body := strings.TrimSpace(r.FormValue("body"))
	if len([]rune(body)) > visionBodyMax {
		body = string([]rune(body)[:visionBodyMax])
	}

	background, err := visionPresetOrError("artboard_background", r.FormValue("artboard_background"), visionBackgroundSet, visionDefaultBackground)
	if err != nil {
		h.renderVisionComposerError(w, r, user, composerMode, err.Error(), http.StatusBadRequest)
		return
	}
	typeface, err := visionPresetOrError("artboard_typeface", r.FormValue("artboard_typeface"), visionTypefaceSet, visionDefaultTypeface)
	if err != nil {
		h.renderVisionComposerError(w, r, user, composerMode, err.Error(), http.StatusBadRequest)
		return
	}
	typeScale, err := visionPresetOrError("artboard_type_scale", r.FormValue("artboard_type_scale"), visionTypeScaleSet, visionDefaultTypeScale)
	if err != nil {
		h.renderVisionComposerError(w, r, user, composerMode, err.Error(), http.StatusBadRequest)
		return
	}
	align, err := visionPresetOrError("artboard_align", r.FormValue("artboard_align"), visionAlignSet, visionDefaultAlign)
	if err != nil {
		h.renderVisionComposerError(w, r, user, composerMode, err.Error(), http.StatusBadRequest)
		return
	}

	isNSFW := r.FormValue("nsfw") == "1"
	if isNSFW && !visionCanPostNSFW(user) {
		h.renderVisionComposerError(w, r, user, composerMode,
			"Your account is not cleared to post 18+ content.", http.StatusForbidden)
		return
	}

	in := dbpkg.VisionInput{
		AuthorPIAL:         user.PIALID,
		ContentType:        contentType,
		Body:               body,
		ArtboardBackground: background,
		ArtboardTypeface:   typeface,
		ArtboardTypeScale:  typeScale,
		ArtboardAlign:      align,
		IsNSFW:             isNSFW,
	}

	source := strings.TrimSpace(r.FormValue("source"))
	if source != "camera" {
		source = "composer"
	}
	metadata, err := json.Marshal(map[string]string{"source": source})
	if err != nil {
		log.Printf("[vision] metadata marshal: %v", err)
		http.Error(w, "the Vision could not be prepared", http.StatusInternalServerError)
		return
	}
	in.Metadata = metadata

	previewURL := ""
	switch contentType {
	case "text":
		if body == "" {
			h.renderVisionComposerError(w, r, user, "text",
				"Write something first — a text Vision needs words.", http.StatusBadRequest)
			return
		}

	case "image":
		mediaURL, mErr := h.visionImageFromRequest(r, user.PIALID)
		if mErr != nil {
			status := http.StatusBadRequest
			if errors.Is(mErr, errVisionMediaBanned) {
				log.Printf("[vision] blocked media offered by PIAL %s", user.PIALID)
				h.renderVisionComposerError(w, r, user, "media",
					"This content cannot be uploaded.", http.StatusBadRequest)
				return
			}
			h.renderVisionComposerError(w, r, user, "media", mErr.Error(), status)
			return
		}
		previewURL = mediaURL
		in.MediaURLs = []string{mediaURL}
		width, wErr := visionDimension("width", r.FormValue("width"))
		if wErr != nil {
			h.renderVisionComposerError(w, r, user, "media", wErr.Error(), http.StatusBadRequest)
			return
		}
		height, hErr := visionDimension("height", r.FormValue("height"))
		if hErr != nil {
			h.renderVisionComposerError(w, r, user, "media", hErr.Error(), http.StatusBadRequest)
			return
		}
		in.Media = []dbpkg.VisionMediaInput{{
			ObjectKey: mediaURL,
			AssetURL:  mediaURL,
			MediaKind: "image",
			Width:     width,
			Height:    height,
		}}

	case "video":
		// Video reaches the lane already transcoded by the existing media path;
		// the Vision records the master it plays and owns that object's purge.
		mediaURL, uErr := visionLocalAssetURL(r.FormValue("media_url"))
		if uErr != nil {
			h.renderVisionComposerError(w, r, user, "media", uErr.Error(), http.StatusBadRequest)
			return
		}
		width, wErr := visionDimension("width", r.FormValue("width"))
		if wErr != nil {
			h.renderVisionComposerError(w, r, user, "media", wErr.Error(), http.StatusBadRequest)
			return
		}
		height, hErr := visionDimension("height", r.FormValue("height"))
		if hErr != nil {
			h.renderVisionComposerError(w, r, user, "media", hErr.Error(), http.StatusBadRequest)
			return
		}
		duration, dErr := visionDurationSecs(r.FormValue("duration_secs"))
		if dErr != nil {
			h.renderVisionComposerError(w, r, user, "media", dErr.Error(), http.StatusBadRequest)
			return
		}
		previewURL = mediaURL
		in.MediaURLs = []string{mediaURL}
		in.Media = []dbpkg.VisionMediaInput{{
			ObjectKey:    mediaURL,
			AssetURL:     mediaURL,
			MediaKind:    "video",
			Width:        width,
			Height:       height,
			DurationSecs: duration,
		}}

	case "share":
		workID, idErr := visionSharedWorkID(r.FormValue("shared_work_id"))
		if idErr != nil {
			h.renderVisionComposerError(w, r, user, "media", idErr.Error(), http.StatusBadRequest)
			return
		}
		work, wErr := dbpkg.GetWorkByID(h.db, workID, user.ID)
		if wErr != nil {
			if errors.Is(wErr, sql.ErrNoRows) {
				h.renderVisionComposerError(w, r, user, "media",
					"That work is not available to share.", http.StatusBadRequest)
				return
			}
			log.Printf("[vision] load shared work %s: %v", workID, wErr)
			http.Error(w, "the shared work could not be read", http.StatusInternalServerError)
			return
		}
		in.SharedWorkID = work.ID
		// A shared 18+ work carries its gate into the Vision that wraps it.
		if work.IsNSFW {
			in.IsNSFW = true
		}
	}

	ref, err := dbpkg.InsertVision(h.db, in)
	if err != nil {
		log.Printf("[vision] insert for %s: %v", user.ID, err)
		h.renderVisionComposerError(w, r, user, composerMode,
			"The Vision could not be posted. Try again.", http.StatusInternalServerError)
		return
	}
	visionID := ref.String()

	// Milestones belong to the lane that owns the object.
	go dbpkg.TryAwardVisionAchievements(h.db, user.ID, user.PIALID)

	// Hand the new Vision to the content-safety fabric. The row opens 'pending'
	// and stays there until a scan answers for it.
	go h.publishVisionContentEvent(visionID, user.PIALID, in.Body, in.MediaURLs)

	created := visionCreatedView{
		VisionID:    visionID,
		Handle:      user.Handle,
		ContentType: contentType,
		ExpiryLabel: visionExpiryLabel(time.Now().Add(dbpkg.DefaultVisionTTL)),
		Artboard:    buildVisionArtboard(visionID, in.Body, background, typeface, typeScale, align),
		HasArtboard: contentType == "text",
		PreviewURL:  previewURL,
		IsVideo:     contentType == "video",
	}
	h.renderPartial(w, "vision_created", created)
}

// visionImageFromRequest reads the posted frame and stores it under the Vision
// media class. The camera posts the file itself; the media composer may instead
// name an asset this platform already holds.
func (h *Handler) visionImageFromRequest(r *http.Request, pialID string) (string, error) {
	if r.MultipartForm != nil {
		file, fh, err := r.FormFile("media")
		if err == nil {
			defer file.Close()
			fileBytes, rErr := io.ReadAll(io.LimitReader(file, visionMediaByteCeiling))
			if rErr != nil {
				return "", errors.New("the image could not be read")
			}
			if len(fileBytes) == 0 {
				return "", errors.New("the image is empty")
			}
			return h.storeVisionImage(r.Context(), fileBytes, fh.Filename, pialID)
		}
		if !errors.Is(err, http.ErrMissingFile) {
			return "", fmt.Errorf("the upload could not be read: %w", err)
		}
	}
	// No file: the caller may instead name a photo this platform already holds.
	if strings.TrimSpace(r.FormValue("media_url")) == "" {
		return "", errors.New("choose a photo to post")
	}
	return visionLocalAssetURL(r.FormValue("media_url"))
}

// publishVisionContentEvent hands a new Vision to Sitra Achra for content safety.
// scan_state is announced as the state the row actually opens in: announcing a
// verdict here is what told scanners to skip the content on the Work lane.
func (h *Handler) publishVisionContentEvent(visionID, pialID, body string, mediaURLs []string) {
	if h.sitra == nil {
		return
	}
	payload, err := json.Marshal(map[string]interface{}{
		"event":      "vision.created",
		"vision_id":   visionID,
		"pial_id":    pialID,
		"body":       body,
		"media_urls": mediaURLs,
		"has_media":  len(mediaURLs) > 0,
		"scan_state": "pending",
		"created_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		log.Printf("[sitra] marshal vision content event: %v", err)
		return
	}
	h.sitra.Publish(context.Background(), sitra.TopicContent, []byte(pialID), payload)
}
