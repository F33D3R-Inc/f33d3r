package handler

// download.go — GET /api/post/{id}/download
//
// The one route that hands out a downloadable copy of a work's media. The media
// gate never serves a download role or watermarked.mp4; only this route does,
// so the per-work switch and the forensic layer cannot be walked around.
//
// In Facet Architecture order:
//
//	triggering event  the download action on the work card (work_download_action
//	                  Facet), a plain link to this route
//	handler           downloadPost: the viewer is signed in; the work's wall
//	                  admits them (decideMediaAccess — the same decision the
//	                  media gate makes); the author allows downloads of this
//	                  work (works.downloads_allowed); the work carries media its
//	                  own author uploaded (db.WorkDownloadableAsset)
//	custody           once Caeor has the copy ready, the content_downloaded entry
//	                  in the PIAL ledger is written before a byte leaves, and a
//	                  failure to write it refuses the download
//	stream            Caeor's /download of that asset: always watermarked, and for
//	                  a subscriber-only work with the per-subscriber forensic
//	                  layer naming the downloader
//
// A work that carries only somebody else's media (a credited reference) has
// nothing of its own to hand out.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/f33d3r/feed-engine/internal/caeor"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// downloadWorkQuery reads the wall facts of one work, then its download switch
// and its author's handle.
const downloadWorkQuery = `
SELECT ` + mediaOwnerColumns + `,
       w.downloads_allowed,
       u.handle
FROM works w
JOIN users u ON u.id = w.author_id
LEFT JOIN user_profiles up ON up.user_id = w.author_id
WHERE w.id = $1::uuid`

func (h *Handler) downloadPost(w http.ResponseWriter, r *http.Request) {
	// Nothing about a download is ever cached: the answer is per viewer, and a
	// forensic copy is per downloader.
	w.Header().Set("Cache-Control", "private, no-store")

	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" || user.ID == "demo_user" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	postID := r.PathValue("id")
	if _, err := uuid.Parse(postID); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if h.db == nil || !h.caeor.Configured() {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	var downloadsAllowed bool
	var authorHandle string
	work, err := scanMediaOwner(h.db.QueryRow(downloadWorkQuery, postID), &downloadsAllowed, &authorHandle)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("[download] reading work %s: %v", postID, err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	access, err := h.authorWall(work.AuthorID, user)
	if err != nil {
		log.Printf("[download] author wall for work %s: %v", postID, err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	switch decideMediaAccess(work, user, access) {
	case 0:
	case http.StatusForbidden:
		if work.SubscriberOnly && !access.IsSubscribed {
			http.Error(w, "subscription required", http.StatusForbidden)
			return
		}
		http.Error(w, "this content is not available to your account", http.StatusForbidden)
		return
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !downloadsAllowed && !access.IsOwner {
		http.Error(w, "the creator has turned off downloads for this post", http.StatusForbidden)
		return
	}

	asset, err := dbpkg.WorkDownloadableAsset(h.db, postID)
	if err != nil {
		log.Printf("[download] resolving the media of work %s: %v", postID, err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if asset == nil {
		http.Error(w, "download not available for this post", http.StatusNotFound)
		return
	}

	// Paid content carries the per-subscriber forensic layer. The author's own
	// download needs no layer naming them.
	forensic := work.SubscriberOnly && !access.IsOwner
	if forensic && asset.Slot == dbpkg.SlotVoice {
		// Caeor has no forensic layer for audio yet, and a paid copy is never
		// handed out without one.
		http.Error(w, "downloads of subscriber-only audio are not available yet", http.StatusForbidden)
		return
	}

	resp, err := h.caeor.Download(r.Context(), asset.AssetID, user.Handle, forensic)
	switch {
	case errors.Is(err, caeor.ErrNotFound), errors.Is(err, caeor.ErrWithdrawn), errors.Is(err, caeor.ErrNoDownload):
		http.Error(w, "download not available for this post", http.StatusNotFound)
		return
	case errors.Is(err, caeor.ErrNotReady):
		http.Error(w, "the download is still being prepared; try again shortly", http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("[download] work %s asset %s: %v", postID, asset.AssetID, err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	// Chain of custody: Caeor has the copy ready, and the record of who receives
	// it, and whether it names them, exists before a byte leaves. If the copy
	// leaks, the ledger entry and the burned-in name are the evidence.
	if err := dbpkg.RecordContentDownload(h.db, user.PIALID, user.ID, map[string]interface{}{
		"work_id":         postID,
		"creator_handle":  authorHandle,
		"asset_id":        asset.AssetID,
		"slot":            asset.Slot,
		"subscriber_only": work.SubscriberOnly,
		"forensic":        forensic,
	}); err != nil {
		log.Printf("[download] custody record for work %s by %s: %v", postID, user.ID, err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	if n := resp.Header.Get("Content-Length"); n != "" {
		w.Header().Set("Content-Length", n)
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": downloadFilename(authorHandle, postID, contentType),
	}))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, resp.Body); err != nil {
		// The headers are gone; the client sees a truncated file. Said, not swallowed.
		log.Printf("[download] streaming work %s asset %s to %s: %v", postID, asset.AssetID, user.ID, err)
	}
}

// downloadFilename is {handle}_{work_id}{ext}, with the extension the copy's
// media type names.
func downloadFilename(handle, workID, contentType string) string {
	safe := sanitizeFilename(handle)
	if safe == "" {
		safe = "f33d3r"
	}
	ext := ""
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		switch mediaType {
		case "video/mp4":
			ext = ".mp4"
		case "audio/mpeg":
			ext = ".mp3"
		case "audio/mp4":
			ext = ".m4a"
		case "image/webp":
			ext = ".webp"
		case "image/jpeg":
			ext = ".jpg"
		case "image/png":
			ext = ".png"
		default:
			if exts, err := mime.ExtensionsByType(mediaType); err == nil && len(exts) > 0 {
				ext = exts[0]
			}
		}
	}
	return fmt.Sprintf("%s_%s%s", safe, workID, ext)
}

// sanitizeFilename returns a safe filename component from a handle.
// Allows alphanumerics and underscore only.
func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// workSetDownloadsEvent is the author switching downloads of one of their works
// on or off (event work_set_downloads {work_id, allowed}). The answer is the
// work's download action Facet as the author now sees it, and the same Facet,
// as anybody else sees it, goes to everyone watching the work.
func (h *Handler) workSetDownloadsEvent(w http.ResponseWriter, r *http.Request, raw map[string]json.RawMessage) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" || user.ID == "demo_user" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(eventField(r, raw, "work_id"))
	if err != nil {
		http.Error(w, "work_id is not a work", http.StatusBadRequest)
		return
	}
	var allowed bool
	switch eventField(r, raw, "allowed") {
	case "true":
		allowed = true
	case "false":
		allowed = false
	default:
		http.Error(w, "allowed must be true or false", http.StatusBadRequest)
		return
	}
	if h.db == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := dbpkg.SetWorkDownloadsAllowed(h.db, id.String(), user.ID, allowed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		log.Printf("[download] %v", err)
		http.Error(w, "could not save that", http.StatusInternalServerError)
		return
	}
	work, err := dbpkg.GetWorkByID(h.db, id.String(), user.ID)
	if err != nil {
		log.Printf("[download] reading work %s after its download switch: %v", id, err)
		http.Error(w, "could not read the work", http.StatusInternalServerError)
		return
	}
	facetID := "facet:f33d3r:work:" + work.ID + ":download"
	if fragment := h.renderFacet("work_download_action", map[string]interface{}{"W": work, "Mine": false}); fragment != "" {
		PublishFacetToPostWatchers(work.ID, FacetMutation{FacetID: facetID, Fragment: fragment, Priority: FacetPriorityLow})
	}
	h.renderPartial(w, "work_download_action", map[string]interface{}{"W": work, "Mine": true})
}
