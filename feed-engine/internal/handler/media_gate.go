package handler

// D-005: Server-side NSFW media URL gating.
//
// Architecture:
//   - signedMediaURLFunc is a template function registered in loadTemplates().
//     Templates call {{signedMediaURL $url $pialID $isNSFW}} — when isNSFW is
//     true, a 5-minute HMAC token is appended as ?tok=<token>.  Non-NSFW URLs
//     and empty URLs are returned unchanged.
//
//   - nsfwMediaGate is an http.Handler middleware that wraps the /media/ proxy.
//     For every request it checks whether the requested path belongs to an NSFW
//     post.  If yes, it requires and validates the ?tok= query param.  If the
//     token is missing, expired, or invalid it responds 403 and stops the
//     request before it reaches MinIO.  Non-NSFW paths are forwarded unchanged.

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/mediatoken"
)

// signedMediaURLFunc returns rawURL with a signed token appended when isNSFW
// is true.  The token encodes the media path + viewer PIAL + 5-minute expiry.
// When isNSFW is false or rawURL is empty, the URL is returned unchanged so
// non-NSFW media requires zero overhead.
func signedMediaURLFunc(rawURL, pialID string, isNSFW bool) string {
	if !isNSFW || rawURL == "" {
		return rawURL
	}
	// External URLs (e.g. Tenor GIFs) can't be gated — skip signing.
	if strings.HasPrefix(rawURL, "https://") || strings.HasPrefix(rawURL, "http://") {
		return rawURL
	}
	// Strip the leading "/media" prefix when deriving the media path for the
	// token, because the MinIO proxy strips it before forwarding.
	mediaPath := rawURL
	if strings.HasPrefix(mediaPath, "/media") {
		mediaPath = mediaPath[len("/media"):]
	}
	tok := mediatoken.Generate(mediaPath, pialID)
	if strings.Contains(rawURL, "?") {
		return rawURL + "&tok=" + tok
	}
	return rawURL + "?tok=" + tok
}

// nsfwMediaGate wraps the /media/ reverse proxy handler.
// For NSFW paths it enforces a valid signed token; clean paths pass through.
func (h *Handler) nsfwMediaGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The full request path still has the /media prefix here (StripPrefix
		// fires inside next).  Build the MinIO object key by stripping /media.
		reqPath := r.URL.Path
		objectPath := reqPath
		if strings.HasPrefix(objectPath, "/media") {
			objectPath = objectPath[len("/media"):]
		}

		// Quick path: if we have no DB, skip the check (dev / test).
		if h.db == nil {
			next.ServeHTTP(w, r)
			return
		}

		isNSFW, err := isNSFWPath(h.db, objectPath)
		if err != nil {
			// DB error — log and let the request through.  We never silently
			// block a user due to a transient DB hiccup; the CSS blur is still
			// client-side fallback.
			log.Printf("[media-gate] DB lookup error for %s: %v", objectPath, err)
			next.ServeHTTP(w, r)
			return
		}

		if !isNSFW {
			// Non-NSFW content: serve without restriction.
			next.ServeHTTP(w, r)
			return
		}

		// NSFW path — require a valid token.
		tok := r.URL.Query().Get("tok")
		if tok == "" {
			http.Error(w, "403 Forbidden — signed token required for adult content", http.StatusForbidden)
			return
		}

		tokenPath, tokenPIAL, err := mediatoken.Validate(tok)
		if errors.Is(err, mediatoken.ErrExpired) {
			http.Error(w, "403 Forbidden — media token expired", http.StatusForbidden)
			return
		}
		if err != nil {
			http.Error(w, "403 Forbidden — invalid media token", http.StatusForbidden)
			return
		}

		// Confirm the token was issued for this exact object path.
		// Strip any leading slash from both sides for a clean comparison.
		if strings.TrimLeft(tokenPath, "/") != strings.TrimLeft(objectPath, "/") {
			http.Error(w, "403 Forbidden — token path mismatch", http.StatusForbidden)
			return
		}

		// Verify the token was issued for the requesting viewer's PIAL.
		// This prevents sharing NSFW media URLs across accounts or with
		// unauthenticated users — a shared URL is only valid for the PIAL
		// it was signed for.
		if tokenPIAL != "" {
			sessionToken := GetSessionToken(r)
			var sessionPIAL string
			if sessionToken != "" && h.db != nil {
				// Resolve session → PIAL via token_hash (same pattern as GetSessionIdentity).
				hash := dbpkg.HashToken(sessionToken)
				h.db.QueryRow(
					`SELECT COALESCE(pial_id::TEXT, '') FROM user_sessions WHERE token_hash = $1 AND expires_at > NOW() LIMIT 1`,
					hash,
				).Scan(&sessionPIAL)
			}
			if sessionPIAL == "" || sessionPIAL != tokenPIAL {
				http.Error(w, "403 Forbidden — PIAL mismatch", http.StatusForbidden)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// isNSFWPath queries the DB to determine whether the requested media path
// belongs to an NSFW post.  It checks both the image media_urls array and the
// video_master_url column.
//
// Returns (false, nil) when no matching post is found — this is the safe
// default for assets that may not yet be associated with any post (e.g.
// thumbnails, HLS segments derived from a master URL).
func isNSFWPath(db *sql.DB, objectPath string) (bool, error) {
	// Reconstruct the /media-prefixed URL form that is stored in the database.
	// Caeor stores URLs as "/media/..." in posts.media_urls.
	mediaURL := "/media" + objectPath

	// For HLS video, every segment (.ts) and sub-manifest (.m3u8) shares the
	// same directory prefix as master.m3u8.  We match on prefix so a single
	// query covers all segments of a video.
	//
	// Strategy:
	//   1. Exact match in media_urls (images / avatars / raw files).
	//   2. Prefix match on video_master_url directory — covers master.m3u8,
	//      variant playlists, and .ts segments that live in the same directory.
	// The video directory prefix is everything before the last '/' in the
	// master URL — e.g. "/media/media-derived/posts/UUID" for
	// "/media/media-derived/posts/UUID/master.m3u8".
	// All HLS segments and playlists live under that same directory, so a
	// LIKE prefix match correctly gates every segment in the ladder.
	const q = `
		SELECT COALESCE(bool_or(is_nsfw), FALSE)
		FROM (
		    SELECT is_nsfw FROM posts
		    WHERE deleted_at IS NULL
		      AND (
		            $1 = ANY(media_urls)
		         OR (video_master_url != ''
		             AND $2 LIKE (REGEXP_REPLACE(video_master_url, '/[^/]*$', '') || '/%'))
		      )
		    UNION ALL
		    SELECT is_nsfw FROM works
		    WHERE deleted_at IS NULL
		      AND (
		            $1 = ANY(media_urls)
		         OR (video_master_url IS NOT NULL
		             AND video_master_url != ''
		             AND $2 LIKE (REGEXP_REPLACE(video_master_url, '/[^/]*$', '') || '/%'))
		      )
		) combined
	`
	var isNSFW bool
	err := db.QueryRow(q, mediaURL, mediaURL).Scan(&isNSFW)
	if errors.Is(err, sql.ErrNoRows) {
		// No matching post found — treat as non-NSFW (safe default).
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return isNSFW, nil
}
