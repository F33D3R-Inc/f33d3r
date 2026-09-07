package handler

// Media read-path authorization.
//
// Architecture:
//   - mediaGate is the http.Handler middleware in front of every byte of user
//     media, both the /static/media/ tree and the legacy /media/ MinIO proxy.
//     For an object owned by a work it re-derives the SAME decision the work
//     detail page makes (workWall) before a single byte is served: the work must
//     be live (not deleted, not removed, not past its ephemeral expiry), the
//     author's account privacy must admit the viewer, no block may stand between
//     them, paid content requires a subscription, and adult content requires an
//     authenticated viewer the adult rules admit. The decision is taken on the
//     read path itself — never on a cleanup worker, which may lag.
//
//   - signedMediaURLFunc is a template function registered in loadTemplates().
//     Templates call {{signedMediaURL $url $pialID $isNSFW}}; the token it appends
//     is now a secondary credential, not the authority. A token that is present
//     must still verify (path + viewer PIAL).
//
// Objects no work owns (avatars, headers, covers) carry no work-level rules and
// are served without a database lookup.

import (
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/f33d3r/feed-engine/internal/mediatoken"
	"github.com/f33d3r/feed-engine/internal/model"
)

// mediaRoot is the on-disk home of the /static/media/ tree.
var mediaRoot = filepath.Join("web", "static", "media")

// ownerCache memoizes path → owning-work facts. Ownership is viewer-independent,
// so one lookup serves every viewer of that object.
var ownerCache = &ttlCache{max: 100000}

const (
	// Ownership facts are re-read at least this often, so a delete or takedown
	// closes the media within one TTL. An ephemeral expiry is exact: the entry is
	// never cached past expires_at.
	mediaOwnerTTL = 60 * time.Second
	// Negative (no owning work) memo — shorter, because an object can gain an
	// owner the moment its work row is written.
	mediaNoOwnerTTL = 15 * time.Second
)

// signedMediaURLFunc returns rawURL with a signed token appended when isNSFW
// is true. The token encodes the media path + viewer PIAL + 5-minute expiry.
// When isNSFW is false or rawURL is empty, the URL is returned unchanged.
func signedMediaURLFunc(rawURL, pialID string, isNSFW bool) string {
	if !isNSFW || rawURL == "" {
		return rawURL
	}
	// External URLs (e.g. Tenor GIFs) can't be gated — skip signing.
	if strings.HasPrefix(rawURL, "https://") || strings.HasPrefix(rawURL, "http://") {
		return rawURL
	}
	tok := mediatoken.Generate(canonicalMediaPath(rawURL), pialID)
	if strings.Contains(rawURL, "?") {
		return rawURL + "&tok=" + tok
	}
	return rawURL + "?tok=" + tok
}

// canonicalMediaPath reduces a URL or request path to the exact form stored in
// works.media_urls / works.video_master_url, so signing and gating agree on one
// key. Traversal segments are resolved away before the value is ever compared.
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

// mediaAssetStem returns the variant-independent prefix of an image object.
// Caeor writes {uuid}-thumb.webp, {uuid}-feed.webp and {uuid}-full.webp for one
// upload but stores only the primary URL on the work, so matching on the stem is
// what stops the full-resolution variant from escaping the wall. Returns "" when
// the trailing segment is too short to be an asset id — a short stem would match
// unrelated objects.
func mediaAssetStem(canonical string) string {
	dir, file := path.Split(canonical)
	file = strings.TrimSuffix(file, path.Ext(file))
	for _, v := range []string{"-thumb", "-feed", "-full"} {
		file = strings.TrimSuffix(file, v)
	}
	if len(file) < 8 {
		return ""
	}
	return dir + file
}

// mediaNeedsWorkLookup reports whether a media path can belong to a work.
// Work media lives under a posts/ segment; avatars, headers and covers never do,
// so the most-requested objects on the site cost no database work at all.
func mediaNeedsWorkLookup(canonical string) bool {
	return strings.Contains(canonical, "/posts/")
}

// mediaOwner is the set of facts a work asserts about one media object.
type mediaOwner struct {
	AuthorID             string
	Deleted              bool
	Removed              bool // moderation takedown (works.is_blocked)
	Expired              bool // ephemeral lifetime elapsed
	ExpiresAt            time.Time
	IsNSFW               bool
	IsGore               bool
	SubscriberOnly       bool
	AuthorIsPrivate      bool
	AuthorIsAdultCreator bool
}

// mediaOwnerSelect is the fact set every ownership lookup returns.
const mediaOwnerSelect = `
SELECT COALESCE(w.author_id::text, ''),
       (w.deleted_at IS NOT NULL),
       COALESCE(w.is_blocked, FALSE),
       (w.expires_at IS NOT NULL AND w.expires_at <= NOW()),
       w.expires_at,
       COALESCE(w.is_nsfw, FALSE),
       COALESCE(w.is_gore, FALSE),
       COALESCE(w.subscriber_only, FALSE),
       COALESCE(up.is_private, FALSE),
       COALESCE(up.is_adult_creator, FALSE)
FROM works w
LEFT JOIN user_profiles up ON up.user_id = w.author_id
`

// mediaOwnerFastQuery matches by equality only, so it can use an index.
// $1 is the object path, $2 the candidate derivative URLs, $3 the candidate HLS
// master URLs. Measured on 100k works: 1.2 ms with the indexes below, against
// 712 ms for the scanning form. Those indexes do not exist yet — see the note on
// mediaOwnerScanQuery.
const mediaOwnerFastQuery = mediaOwnerSelect + `
WHERE w.media_urls && $2::text[]
   OR w.video_master_url = ANY($3::text[])
   OR COALESCE(w.video_watermarked_url, '') = $1::text
   OR COALESCE(w.video_poster_url, '') = $1::text
LIMIT 16`

// mediaOwnerScanQuery is the general fallback, run only when the fast form finds
// nothing. It matches video assets by directory rather than by derived name, so
// ownership still resolves if the transcoder ever changes its file layout: the
// wall degrades in speed, never in correctness.
//
// REQUIRED FOLLOW-UP (owner of internal/db/migrations, not applied here):
//
//	CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_works_media_urls_gin
//	    ON works USING GIN (media_urls);
//	CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_works_video_master_url
//	    ON works (video_master_url) WHERE video_master_url IS NOT NULL AND video_master_url <> '';
//
// Until they exist every lookup that misses the memo is a sequential scan of
// works (~5 us per row; ~700 ms at 100k works).
const mediaOwnerScanQuery = mediaOwnerSelect + `
WHERE EXISTS (SELECT 1 FROM unnest(w.media_urls) mu
               WHERE mu = $1::text OR ($2::text <> $1::text AND starts_with(mu, $2::text)))
   OR (COALESCE(w.video_master_url, '') <> ''
       AND starts_with($1::text, regexp_replace(w.video_master_url, '/[^/]*$', '') || '/'))
   OR (COALESCE(w.video_master_url, '') <> ''
       AND starts_with($1::text, regexp_replace(w.video_master_url, '/clean/[^/]*$', '') || '/'))
   OR COALESCE(w.video_watermarked_url, '') = $1::text
   OR COALESCE(w.video_poster_url, '') = $1::text
LIMIT 16`

// mediaCandidates derives, from one object path, the stored URLs a work could
// hold for it: the path itself, every image derivative of the same asset, and
// the HLS master playlist of every ancestor directory (an object of a stream
// always lives under the directory tree its master sits in).
func mediaCandidates(canonical string) (derivatives, masters []string) {
	derivatives = []string{canonical}
	if stem := mediaAssetStem(canonical); stem != "" {
		ext := path.Ext(canonical)
		for _, v := range []string{"", "-thumb", "-feed", "-full"} {
			if c := stem + v + ext; c != canonical {
				derivatives = append(derivatives, c)
			}
		}
	}
	dir := path.Dir(canonical)
	for i := 0; i < 5 && dir != "/" && dir != "." && dir != ""; i++ {
		masters = append(masters, dir+"/master.m3u8")
		dir = path.Dir(dir)
	}
	return derivatives, masters
}

// resolveMediaOwners returns every work that owns the object at canonical.
// An empty slice means no work claims it. Errors are returned, never swallowed:
// the caller denies rather than guessing.
func (h *Handler) resolveMediaOwners(canonical string) ([]mediaOwner, error) {
	if v, ok := ownerCache.get(canonical); ok {
		return v.([]mediaOwner), nil
	}
	derivatives, masters := mediaCandidates(canonical)
	owners, err := h.queryMediaOwners(mediaOwnerFastQuery, canonical, pq.Array(derivatives), pq.Array(masters))
	if err != nil {
		return nil, err
	}
	if len(owners) == 0 {
		stem := mediaAssetStem(canonical)
		if stem == "" {
			stem = canonical
		}
		if owners, err = h.queryMediaOwners(mediaOwnerScanQuery, canonical, stem); err != nil {
			return nil, err
		}
	}

	exp := time.Now().Add(mediaOwnerTTL)
	if len(owners) == 0 {
		exp = time.Now().Add(mediaNoOwnerTTL)
	}
	// Never memo past an ephemeral expiry — the wall must close on the second.
	for _, o := range owners {
		if !o.ExpiresAt.IsZero() && o.ExpiresAt.Before(exp) {
			exp = o.ExpiresAt
		}
	}
	ownerCache.put(canonical, owners, exp)
	return owners, nil
}

// queryMediaOwners runs one ownership query and scans its rows.
func (h *Handler) queryMediaOwners(query string, args ...any) ([]mediaOwner, error) {
	rows, err := h.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	owners := []mediaOwner{}
	for rows.Next() {
		var o mediaOwner
		var expiresAt *time.Time
		if err := rows.Scan(&o.AuthorID, &o.Deleted, &o.Removed, &o.Expired, &expiresAt,
			&o.IsNSFW, &o.IsGore, &o.SubscriberOnly, &o.AuthorIsPrivate, &o.AuthorIsAdultCreator); err != nil {
			return nil, err
		}
		if expiresAt != nil {
			o.ExpiresAt = *expiresAt
		}
		owners = append(owners, o)
	}
	return owners, rows.Err()
}

// decideMediaAccess is the pure media-wall decision for one owning work.
// Returns 0 to allow, or the HTTP status to answer with. 404 is used wherever a
// denial would otherwise disclose that the object exists; 403 is used for the
// adult gate, which the viewer can act on.
//
// The rules mirror workWall (works_feed.go) exactly, plus the liveness and paid
// facts the page path gets for free from its query and the read path did not.
func decideMediaAccess(o mediaOwner, viewer *model.User, a authorAccess) int {
	if o.Deleted || o.Removed || o.Expired {
		return http.StatusNotFound
	}
	if a.IsOwner {
		return 0
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
	}
	return 0
}

// authorizeMedia answers whether this request may read this object.
// Returns 0 to allow, an HTTP status to deny with, and whether any work owns the
// object (which decides how the response may be cached).
func (h *Handler) authorizeMedia(r *http.Request, canonical string) (status int, owned bool, err error) {
	if !mediaNeedsWorkLookup(canonical) {
		return 0, false, nil
	}
	owners, err := h.resolveMediaOwners(canonical)
	if err != nil {
		return 0, false, err
	}
	if len(owners) == 0 {
		return 0, false, nil
	}

	viewer, err := h.requestViewer(r)
	if err != nil {
		return 0, true, err
	}

	// A token, when present, must verify — it can only narrow access, never widen it.
	if tok := r.URL.Query().Get("tok"); tok != "" {
		tokenPath, tokenPIAL, terr := mediatoken.Validate(tok)
		if terr != nil || canonicalMediaPath(tokenPath) != canonical {
			return http.StatusForbidden, true, nil
		}
		if tokenPIAL != "" && (viewer == nil || viewer.PIALID != tokenPIAL) {
			return http.StatusForbidden, true, nil
		}
	}

	denial := http.StatusNotFound
	for _, o := range owners {
		access, aerr := h.authorWall(o.AuthorID, viewer)
		if aerr != nil {
			return 0, true, aerr
		}
		s := decideMediaAccess(o, viewer, access)
		if s == 0 {
			return 0, true, nil
		}
		if s == http.StatusForbidden {
			denial = s
		}
	}
	return denial, true, nil
}

// mediaGate wraps a media-serving handler with the read-path wall.
func (h *Handler) mediaGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Without a database there is nothing to authorize against (dev / tests).
		if h.db == nil {
			next.ServeHTTP(w, r)
			return
		}
		canonical := canonicalMediaPath(r.URL.Path)
		status, owned, err := h.authorizeMedia(r, canonical)
		if err != nil {
			// An authorization check that could not run denies. Serving the bytes
			// on a database error is how a captured URL outlives its content.
			log.Printf("[media-gate] authorization failed for %s: %v", canonical, err)
			http.Error(w, "503 Service Unavailable — media authorization unavailable", http.StatusServiceUnavailable)
			return
		}
		if status != 0 {
			http.Error(w, http.StatusText(status), status)
			return
		}
		if owned {
			// Walled bytes are never shared or long-lived in any cache: the wall can
			// close at any moment, and the answer differs per viewer.
			w.Header().Set("Cache-Control", "private, max-age=60, must-revalidate")
			w.Header().Set("Vary", "Cookie")
		}
		next.ServeHTTP(w, r)
	})
}

// staticMediaHandler serves the /static/media/ tree behind the wall. It is
// registered ahead of the general /static/ route so user media — the only static
// subtree with per-viewer rules — cannot be read around the gate.
func (h *Handler) staticMediaHandler() http.Handler {
	fs := http.StripPrefix("/static/media/", http.FileServer(http.Dir(mediaRoot)))
	// Cache policy is applied only after the wall admits the request, so a denial
	// is never the thing a browser caches.
	cached := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if w.Header().Get("Cache-Control") == "" {
			// Objects no work owns follow the same policy as the rest of /static/.
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
		rel := strings.TrimPrefix(canonicalMediaPath(r.URL.Path), "/static/media/")
		// Resolve existence before any authorization work, so a flood of invented
		// paths costs a stat and not a database lookup.
		full := filepath.Join(mediaRoot, filepath.FromSlash(rel))
		if !strings.HasPrefix(full, mediaRoot+string(filepath.Separator)) {
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
