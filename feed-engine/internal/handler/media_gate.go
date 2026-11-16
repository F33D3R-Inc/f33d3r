package handler

// Media read-path authorization.
//
// Architecture:
//   - mediaGate is the http.Handler middleware in front of every byte a person
//     uploaded: the /static/media/ tree Caeor writes (served layout
//     /static/media/a/<asset_id>/...) and the legacy /static/uploads/ tree. It
//     fails closed: no Caeor, no database, no bytes.
//
//   - Every requested path is resolved through Caeor, the one media-asset
//     authority (GET /v1/assets/resolve?path=), and the answer is memoized per
//     path for mediaGovernanceTTL. Caeor says which asset the object belongs
//     to, who uploaded it, its kind, whether it is withdrawn, which object of
//     the asset it is (role) and whether that object is watermarked.
//
//   - Refused whatever any wall says, with 404: an unknown object, and any
//       object Caeor does not call servable — a withdrawn or unfinished asset,
//       a private source, the download file (handed out only by the download
//       route, which applies the per-work download switch and the forensic
//       layer), and any unwatermarked content object (legacy media is served
//       only once Caeor has re-marked it in place).
//
//   - Work media (image, video, voice, audio, clip) is governed by its
//     UPLOADER's works: the works that reference the asset (work_assets) AND
//     are authored by the asset's uploader_pial. A work by anybody else that
//     names the same asset is a credited reference and unlocks nothing, so a
//     public work cannot open a creator's subscriber-only or adult media. The
//     uploader's live Visions that carry the asset govern it the same way.
//
//   - For each governing work the gate re-derives the SAME decision the work
//     detail page makes (workWall): live (not deleted, not removed by an admin
//     or by the scanner, not past its ephemeral expiry, not scheduled for
//     later), the author's privacy admits the viewer, no block, a subscription
//     for paid content, and the adult rules for adult content. Any one admitting
//     work admits the object.
//
//   - An asset no work of its uploader references is unpublished: only the
//     uploader reads it (the compose preview).
//
//   - Avatars, headers and encrypted ciphertext have no work wall. Ciphertext is
//     opened by its content key, delivered on payment, never by the read path.
//
//   - signedMediaURLFunc is a template function registered in loadTemplates().
//     The token it appends is a secondary credential, not the authority. A token
//     that is present must still verify (path + viewer PIAL).

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/caeor"
	"github.com/f33d3r/feed-engine/internal/mediatoken"
	"github.com/f33d3r/feed-engine/internal/model"
)

// mediaRoot is the on-disk home of the /static/media/ tree: the volume Caeor
// writes its served objects into.
var mediaRoot = filepath.Join("web", "static", "media")

// uploadsRoot is the on-disk home of the legacy /static/uploads/ tree. Nothing
// is written there any more; its objects are served only as Caeor resolves them
// (Caeor's import registered every legacy path).
var uploadsRoot = filepath.Join("web", "static", "uploads")

// governanceCache memoizes path → governance facts. They are viewer-independent,
// so one lookup serves every viewer of that object.
var governanceCache = &ttlCache{max: 100000}

const (
	// Governance is re-read at least this often, so a delete, a withdrawal or a
	// takedown closes the media within one TTL. An ephemeral expiry or a
	// scheduled release is exact: the entry is never cached past either.
	mediaGovernanceTTL = 60 * time.Second
	// An object Caeor does not know. The memo is short only so the map does not
	// fill with invented paths.
	mediaUnknownTTL = 15 * time.Second
	// Unpublished media is never memoized: it gains its first work the moment the
	// uploader publishes, and a memo would hold it shut for followers.

	// mediaResolveTimeout bounds one Caeor resolve on the read path.
	mediaResolveTimeout = 3 * time.Second
	// mediaOwnerLimit bounds how many governing works and Visions are read for
	// one asset; any one admitting is enough.
	mediaOwnerLimit = 64
)

// signedMediaURLFunc returns rawURL with a signed token appended when isNSFW
// is true. The token encodes the media path + the viewer's account id (or none)
// + 5-minute expiry. It travels in a URL, so it never carries a PIAL.
// When isNSFW is false or rawURL is empty, the URL is returned unchanged.
func signedMediaURLFunc(rawURL, accountID string, isNSFW bool) string {
	if !isNSFW || rawURL == "" {
		return rawURL
	}
	// External URLs (e.g. GIF picker results) can't be gated — skip signing.
	if strings.HasPrefix(rawURL, "https://") || strings.HasPrefix(rawURL, "http://") {
		return rawURL
	}
	tok := mediatoken.Generate(canonicalMediaPath(rawURL), accountID)
	if strings.Contains(rawURL, "?") {
		return rawURL + "&tok=" + tok
	}
	return rawURL + "?tok=" + tok
}

// canonicalMediaPath reduces a URL or request path to the exact form stored in
// works and resolved by Caeor, so signing and gating agree on one key.
// Traversal segments are resolved away before the value is ever compared.
func canonicalMediaPath(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	if raw == "" {
		return ""
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	return path.Clean(raw)
}

// mediaOwner is the set of facts one governing work (or Vision) asserts about
// the media it carries.
type mediaOwner struct {
	AuthorID             string
	Deleted              bool
	Removed              bool // moderation takedown: is_blocked, or scan_state 'blocked'
	Expired              bool // ephemeral lifetime elapsed
	ExpiresAt            time.Time
	Scheduled            bool // scheduled for a release that has not come yet
	ScheduledAt          time.Time
	Pending              bool // its verdict has not arrived: its author's (and moderators') alone
	IsNSFW               bool
	IsGore               bool
	SubscriberOnly       bool
	AuthorIsPrivate      bool
	AuthorIsAdultCreator bool
}

// mediaOwnerColumns is the fact set every work lookup returns, in scanMediaOwner
// order. Removed mirrors db.workNotBlockedSQL. Requires works w and
// user_profiles up.
const mediaOwnerColumns = `
       COALESCE(w.author_id::text, ''),
       (w.deleted_at IS NOT NULL),
       (COALESCE(w.is_blocked, FALSE) OR COALESCE(w.scan_state, '') = 'blocked'),
       (w.expires_at IS NOT NULL AND w.expires_at <= NOW()),
       w.expires_at,
       (w.scheduled_at IS NOT NULL AND w.scheduled_at > NOW()),
       w.scheduled_at,
       (w.scan_state = 'pending'),
       COALESCE(w.is_nsfw, FALSE),
       COALESCE(w.is_gore, FALSE),
       COALESCE(w.subscriber_only, FALSE),
       COALESCE(up.is_private, FALSE),
       COALESCE(up.is_adult_creator, FALSE)`

// mediaOwnerSelect selects mediaOwnerColumns; callers append WHERE.
const mediaOwnerSelect = `
SELECT ` + mediaOwnerColumns + `
FROM works w
LEFT JOIN user_profiles up ON up.user_id = w.author_id
`

// uploaderWorksQuery returns the uploader's works that reference the asset.
// $1 asset id, $2 the asset's uploader PIAL. A work by anybody else that names
// the asset is not a governing work, whatever it says.
const uploaderWorksQuery = mediaOwnerSelect + `
WHERE w.author_pial = $2::uuid
  AND w.id IN (SELECT wa.work_id FROM work_assets wa
                WHERE wa.asset_id = $1::uuid AND wa.uploader_pial = $2::uuid)
ORDER BY w.created_at DESC
LIMIT 64`

// uploaderVisionsQuery returns the uploader's Visions that carry the asset, as
// the same fact set. A Vision answers for the account that posted it
// (author_account), never for another persona on the same identity. A Vision
// addressed to followers is walled like a private author; its expiry is its
// lifetime; a Vision waiting on its verdict is its author's alone, as the
// Vision itself is. A Vision has no schedule, no subscription and no gore flag.
// $1 asset id, $2 the asset's uploader PIAL.
const uploaderVisionsQuery = `
SELECT v.author_account::text,
       (v.deleted_at IS NOT NULL),
       (v.is_blocked OR v.scan_state = 'blocked'),
       (v.expires_at <= NOW()),
       v.expires_at,
       FALSE,
       NULL::timestamptz,
       (v.scan_state = 'pending'),
       v.is_nsfw,
       FALSE,
       FALSE,
       (COALESCE(up.is_private, FALSE) OR v.audience = 'followers'),
       COALESCE(up.is_adult_creator, FALSE)
  FROM visions v
  JOIN vision_media vm ON vm.author_pial = v.author_pial AND vm.seq = v.seq
  LEFT JOIN user_profiles up ON up.user_id = v.author_account
 WHERE vm.caeor_asset_id = $1::text
   AND v.author_pial = $2::uuid
   AND vm.purged_at IS NULL
 ORDER BY v.created_at DESC
 LIMIT 64`

// mediaGovernance is what decides one object, for every viewer.
type mediaGovernance struct {
	// Object is Caeor's answer for the path; nil when Caeor knows no servable
	// object there.
	Object *caeor.Resolved
	// Owners are the uploader's works and Visions that carry the asset. Empty
	// means the asset is unpublished.
	Owners []mediaOwner
}

// servable reports whether the object may be served at all, before any wall.
// Caeor answers it: an object is servable only when its asset is ready and on
// the platform and the object is a watermarked viewer-facing copy — never a
// private source, never the download file, never legacy media still awaiting
// its re-mark.
func (g *mediaGovernance) servable() bool {
	o := g.Object
	return o != nil && o.Servable && !o.Withdrawn && o.Status == caeor.StatusReady
}

// resolveMediaGovernance returns the governance of the object at canonical.
// Errors are returned, never swallowed: the caller denies rather than guessing.
func (h *Handler) resolveMediaGovernance(ctx context.Context, canonical string) (*mediaGovernance, error) {
	if v, ok := governanceCache.get(canonical); ok {
		return v.(*mediaGovernance), nil
	}
	rctx, cancel := context.WithTimeout(ctx, mediaResolveTimeout)
	defer cancel()
	obj, err := h.caeor.Resolve(rctx, canonical)
	if errors.Is(err, caeor.ErrNotFound) {
		gov := &mediaGovernance{}
		governanceCache.put(canonical, gov, time.Now().Add(mediaUnknownTTL))
		return gov, nil
	}
	if err != nil {
		return nil, err
	}
	gov := &mediaGovernance{Object: obj}
	if !gov.servable() || !obj.Kind.IsWorkMedia() {
		// Nothing about the asset's works changes this answer within one TTL.
		governanceCache.put(canonical, gov, time.Now().Add(mediaGovernanceTTL))
		return gov, nil
	}
	if obj.UploaderPIAL == "" {
		return nil, errors.New("caeor resolved " + canonical + " to an asset with no uploader")
	}

	works, err := h.queryMediaOwners(uploaderWorksQuery, obj.AssetID, obj.UploaderPIAL)
	if err != nil {
		return nil, err
	}
	visions, err := h.queryMediaOwners(uploaderVisionsQuery, obj.AssetID, obj.UploaderPIAL)
	if err != nil {
		return nil, err
	}
	gov.Owners = append(works, visions...)
	if len(gov.Owners) == 0 {
		return gov, nil
	}

	exp := time.Now().Add(mediaGovernanceTTL)
	// Never memo past an ephemeral expiry or a scheduled release — the wall must
	// move on the second.
	for _, o := range gov.Owners {
		if !o.ExpiresAt.IsZero() && o.ExpiresAt.After(time.Now()) && o.ExpiresAt.Before(exp) {
			exp = o.ExpiresAt
		}
		if o.Scheduled && o.ScheduledAt.Before(exp) {
			exp = o.ScheduledAt
		}
	}
	governanceCache.put(canonical, gov, exp)
	return gov, nil
}

// queryMediaOwners runs one owner-facts query and scans its rows. A row whose
// author account is gone governs nothing and is dropped.
func (h *Handler) queryMediaOwners(query string, args ...any) ([]mediaOwner, error) {
	rows, err := h.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	owners := []mediaOwner{}
	for rows.Next() {
		o, err := scanMediaOwner(rows)
		if err != nil {
			return nil, err
		}
		if o.AuthorID == "" {
			continue
		}
		owners = append(owners, o)
	}
	return owners, rows.Err()
}

// scanMediaOwner scans one row that starts with mediaOwnerColumns; extra
// receives any columns the query selects after them.
func scanMediaOwner(row interface{ Scan(...any) error }, extra ...any) (mediaOwner, error) {
	var o mediaOwner
	var expiresAt, scheduledAt *time.Time
	dest := append([]any{&o.AuthorID, &o.Deleted, &o.Removed, &o.Expired, &expiresAt,
		&o.Scheduled, &scheduledAt, &o.Pending,
		&o.IsNSFW, &o.IsGore, &o.SubscriberOnly, &o.AuthorIsPrivate, &o.AuthorIsAdultCreator}, extra...)
	if err := row.Scan(dest...); err != nil {
		return mediaOwner{}, err
	}
	if expiresAt != nil {
		o.ExpiresAt = *expiresAt
	}
	if scheduledAt != nil {
		o.ScheduledAt = *scheduledAt
	}
	return o, nil
}

// decideMediaAccess is the pure media-wall decision for one governing work.
// Returns 0 to allow, or the HTTP status to answer with. 404 is used wherever a
// denial would otherwise disclose that the object exists; 403 is used for the
// paid and adult gates, which the viewer can act on.
//
// The rules mirror workWall (works_feed.go) exactly, plus the liveness and paid
// facts the page path gets from its query.
func decideMediaAccess(o mediaOwner, viewer *model.User, a authorAccess) int {
	if o.Deleted || o.Removed || o.Expired {
		return http.StatusNotFound
	}
	if a.IsOwner {
		return 0
	}
	if o.Pending {
		// Waiting on its verdict: only a moderator, who may have to judge it,
		// sees it besides its author.
		if viewer != nil && viewer.IsAdmin() {
			return 0
		}
		return http.StatusNotFound
	}
	if o.Scheduled {
		return http.StatusNotFound
	}
	if a.IsBlocked {
		return http.StatusNotFound
	}
	if privateWalled(o.AuthorIsPrivate, a) {
		return http.StatusNotFound
	}
	if o.SubscriberOnly && !a.IsSubscribed {
		return http.StatusForbidden
	}
	if o.IsNSFW || o.IsGore || o.AuthorIsAdultCreator {
		anon := viewer == nil || viewer.ID == "" || viewer.ID == "demo_user"
		if anon || excludeNSFW(viewer) {
			return http.StatusForbidden
		}
		// The card asks an unverified adult to verify before showing adult media, and the
		// file must say the same, or the gate only hides a link.
		if (o.IsNSFW || o.AuthorIsAdultCreator) && !(viewer.IsAgeVerified || viewer.IsVerified) {
			return http.StatusForbidden
		}
	}
	return 0
}

// decideGovernedAccess is the decision for one work-media object across
// everything that governs it. walls holds the viewer's standing with each
// governing author. Returns 0 to allow or the status to deny with.
func decideGovernedAccess(gov *mediaGovernance, viewer *model.User, walls map[string]authorAccess) int {
	if !gov.servable() {
		return http.StatusNotFound
	}
	if len(gov.Owners) == 0 {
		// Unpublished: the uploader's own preview, nobody else's.
		if viewer != nil && viewer.PIALID != "" && viewer.PIALID == gov.Object.UploaderPIAL {
			return 0
		}
		return http.StatusNotFound
	}
	denial := http.StatusNotFound
	for _, o := range gov.Owners {
		s := decideMediaAccess(o, viewer, walls[o.AuthorID])
		if s == 0 {
			return 0
		}
		if s == http.StatusForbidden {
			denial = s
		}
	}
	return denial
}

// authorizeMedia answers whether this request may read this object.
// Returns 0 to allow, an HTTP status to deny with, and whether the object is
// walled work media (which decides how the response may be cached).
func (h *Handler) authorizeMedia(r *http.Request, canonical string) (status int, governed bool, err error) {
	gov, err := h.resolveMediaGovernance(r.Context(), canonical)
	if err != nil {
		return 0, true, err
	}
	if !gov.servable() {
		return http.StatusNotFound, true, nil
	}
	if !gov.Object.Kind.IsWorkMedia() {
		// Profile furniture and ciphertext: no work wall.
		return 0, false, nil
	}

	viewer, err := h.requestViewer(r)
	if err != nil {
		return 0, true, err
	}

	// A token, when present, must verify — it can only narrow access, never widen it.
	if tok := r.URL.Query().Get("tok"); tok != "" {
		tokenPath, tokenAccount, terr := mediatoken.Validate(tok)
		if terr != nil || canonicalMediaPath(tokenPath) != canonical {
			return http.StatusForbidden, true, nil
		}
		if tokenAccount != "" && (viewer == nil || viewer.ID != tokenAccount) {
			return http.StatusForbidden, true, nil
		}
	}

	// The governing works all belong to the uploader's identity, but a PIAL may
	// author under more than one account, so the wall is asked per account.
	walls := make(map[string]authorAccess, 1)
	for _, o := range gov.Owners {
		if _, done := walls[o.AuthorID]; done {
			continue
		}
		access, err := h.authorWall(o.AuthorID, viewer)
		if err != nil {
			return 0, true, err
		}
		walls[o.AuthorID] = access
	}
	return decideGovernedAccess(gov, viewer, walls), true, nil
}

// errMediaGateUnavailable is why the gate refuses everything when it has no
// database or no Caeor: an authorization check that cannot run denies.
var errMediaGateUnavailable = errors.New("media gate has no database or no Caeor")

// mediaGate wraps a media-serving handler with the read-path wall.
func (h *Handler) mediaGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		canonical := canonicalMediaPath(r.URL.Path)
		var status int
		var governed bool
		var err error
		if h.db == nil || !h.caeor.Configured() {
			err = errMediaGateUnavailable
		} else {
			status, governed, err = h.authorizeMedia(r, canonical)
		}
		if err != nil {
			// Serving the bytes on a failed check is how a captured URL outlives
			// its content.
			log.Printf("[media-gate] authorization failed for %s: %v", canonical, err)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "503 Service Unavailable — media authorization unavailable", http.StatusServiceUnavailable)
			return
		}
		if status != 0 {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, http.StatusText(status), status)
			return
		}
		if governed {
			// Walled bytes are never shared or long-lived in any cache: the wall can
			// close at any moment, and the answer differs per viewer.
			w.Header().Set("Cache-Control", "private, max-age=60, must-revalidate")
			w.Header().Set("Vary", "Cookie, Authorization")
		}
		next.ServeHTTP(w, r)
	})
}

// gatedFileTree serves the on-disk tree root at urlPrefix behind the wall. Both
// user-upload trees are registered through it ahead of the general /static/
// route, so no user-uploaded byte can be read around the gate.
func (h *Handler) gatedFileTree(urlPrefix, root string) http.Handler {
	fs := http.StripPrefix(urlPrefix, http.FileServer(http.Dir(root)))
	// Cache policy is applied only after the wall admits the request, so a denial
	// is never the thing a browser caches. The gate has already set a private
	// policy on walled work media; what is left (avatars, headers, ciphertext)
	// follows the rest of /static/.
	cached := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if w.Header().Get("Cache-Control") == "" {
			if strings.HasSuffix(r.URL.Path, ".m3u8") || strings.HasSuffix(r.URL.Path, ".ts") {
				w.Header().Set("Cache-Control", "public, max-age=3600")
			} else {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
		}
		fs.ServeHTTP(w, r)
	})
	gated := h.mediaGate(cached)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(canonicalMediaPath(r.URL.Path), urlPrefix)
		// Resolve existence before any authorization work, so a flood of invented
		// paths costs a stat and not a Caeor round trip.
		full := filepath.Join(root, filepath.FromSlash(rel))
		if !strings.HasPrefix(full, root+string(filepath.Separator)) {
			http.NotFound(w, r)
			return
		}
		if st, err := os.Stat(full); err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		gated.ServeHTTP(w, r)
	})
}

// staticMediaHandler serves the /static/media/ tree (Caeor's served objects)
// behind the wall.
func (h *Handler) staticMediaHandler() http.Handler {
	return h.gatedFileTree("/static/media/", mediaRoot)
}

// staticUploadsHandler serves the legacy /static/uploads/ tree behind the wall.
func (h *Handler) staticUploadsHandler() http.Handler {
	return h.gatedFileTree("/static/uploads/", uploadsRoot)
}

// localMediaFile maps a served /static/media/ URL to its file on the shared
// media volume, for a server-side reader of an asset's own bytes (Zior). false
// when the URL does not name a file inside that tree.
func localMediaFile(mediaURL string) (string, bool) {
	const prefix = "/static/media/"
	canonical := canonicalMediaPath(mediaURL)
	if !strings.HasPrefix(canonical, prefix) {
		return "", false
	}
	full := filepath.Join(mediaRoot, filepath.FromSlash(strings.TrimPrefix(canonical, prefix)))
	if !strings.HasPrefix(full, mediaRoot+string(filepath.Separator)) {
		return "", false
	}
	return full, true
}
