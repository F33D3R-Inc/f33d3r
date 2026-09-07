package handler

// download.go — GET /api/post/{id}/download
//
// Serves the watermarked MP4 for a post as a browser download.
//
// Access rules:
//   - Authenticated users only.
//   - For NSFW posts: viewer must have IsAgeVerified = true.
//   - For subscriber-only posts: viewer must be the post author OR an active subscriber.
//
// For subscriber-only content, a second forensic watermark layer is burned into the
// download on-the-fly by ffmpeg, identifying the downloader. This is done in a temp
// file, served to the client, then deleted. It ensures that if a paid-content download
// leaks, the exact subscriber who downloaded it can be identified.
//
// For free content the pre-burned watermarked.mp4 is streamed directly (no extra pass).

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

func (h *Handler) downloadPost(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	postID := r.PathValue("id")
	if postID == "" {
		http.Error(w, "post id required", http.StatusBadRequest)
		return
	}

	if h.db == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	// Resolve the work — we need VideoWatermarkedURL, IsNSFW, SubscriberOnly, AuthorID.
	work, err := dbpkg.GetWorkByID(h.db, postID, user.ID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Gate: NSFW content requires age verification.
	if work.IsNSFW && !user.IsAgeVerified {
		http.Error(w, "age verification required", http.StatusForbidden)
		return
	}

	// Gate: subscriber-only content requires authorship or active subscription.
	if work.SubscriberOnly && work.AuthorID != user.ID {
		if !dbpkg.IsSubscribed(h.db, user.ID, work.AuthorID) {
			http.Error(w, "subscription required", http.StatusForbidden)
			return
		}
	}

	// Resolve the watermarked MP4 path on disk.
	// video_watermarked_url is stored as /static/media/posts/{asset_id}/watermarked.mp4
	wmURL := work.VideoWatermarkedURL
	if wmURL == "" {
		http.Error(w, "download not available for this post", http.StatusNotFound)
		return
	}

	// Strip the URL prefix to get the filesystem path under web/.
	// /static/media/... → web/static/media/...
	relPath := strings.TrimPrefix(wmURL, "/")
	diskPath := filepath.Join("web", relPath)

	// Sanitize: ensure we stay inside the media directory.
	cleanPath := filepath.Clean(diskPath)
	if !strings.HasPrefix(cleanPath, filepath.Join("web", "static", "media")) {
		http.Error(w, "invalid media path", http.StatusBadRequest)
		return
	}

	// Safe download filename: {handle}_{post_id}.mp4
	safeHandle := sanitizeFilename(work.AuthorHandle)
	if safeHandle == "" {
		safeHandle = "video"
	}
	downloadFilename := fmt.Sprintf("%s_%s.mp4", safeHandle, postID)

	// For subscriber-only posts, add a forensic watermark identifying the downloader.
	if work.SubscriberOnly {
		// Log to PIAL ledger so chain-of-custody is complete: signed proof of who
		// downloaded this file and when. If this copy leaks, the ledger entry is evidence.
		dbpkg.LogPIALEvent(h.db, user.PIALID, user.ID, "content_downloaded", map[string]interface{}{
			"work_id":         postID,
			"creator_handle":  work.AuthorHandle,
			"subscriber_only": true,
		}, "download_gate")
		serveWithForensicWatermark(w, r, cleanPath, downloadFilename, work.AuthorHandle, user.Handle)
		return
	}

	// Free / unrestricted content — stream the pre-burned watermarked.mp4 directly.
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, downloadFilename))
	http.ServeFile(w, r, cleanPath)
}

// serveWithForensicWatermark burns a secondary, smaller watermark into a temp copy of
// the watermarked MP4, identifying the downloader. Serves the temp file, then deletes it.
//
// This is the forensic layer: if subscriber-only content leaks, the exact downloader
// handle is embedded. The primary creator watermark is already present in the source file.
//
// Secondary watermark spec:
//   - Text:     "Licensed to @{downloader_handle}"
//   - Position: bottom-right corner, static (10px margins)
//   - Font size: 1.5% of height (smaller than primary)
//   - Opacity:   0.4 (less prominent than primary)
//   - No Lissajous path — static position is intentional for forensic clarity
func serveWithForensicWatermark(w http.ResponseWriter, r *http.Request, sourcePath, downloadFilename, creatorHandle, downloaderHandle string) {
	const fontfile = "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"

	safeDownloader := sanitize_for_drawtext_go(downloaderHandle)
	if safeDownloader == "" {
		safeDownloader = "subscriber"
	}

	// Build the forensic drawtext filter.
	// Bottom-right: x = w - tw - 10, y = h - th - 10
	forensicFilter := fmt.Sprintf(
		"drawtext=fontfile=%s:text='Licensed to @%s':fontcolor=white@0.4:fontsize='h*0.015':shadowx=1:shadowy=1:shadowcolor=black@0.6:x='w-tw-10':y='h-th-10'",
		fontfile,
		safeDownloader,
	)

	// Write the temp file to /tmp — same machine, fast.
	tmpFile, err := os.CreateTemp("", "f33d3r-forensic-*.mp4")
	if err != nil {
		log.Printf("[download] forensic temp file error: %v", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath) // always clean up

	cmd := exec.CommandContext(r.Context(),
		"ffmpeg",
		"-y", "-i", sourcePath,
		"-vf", forensicFilter,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-profile:v", "main",
		"-level", "4.1",
		"-pix_fmt", "yuv420p",
		"-c:a", "copy", // audio unchanged — only video gets re-encoded
		"-movflags", "+faststart",
		"-f", "mp4",
		tmpPath,
	)

	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[download] forensic ffmpeg error for post creator=%s downloader=%s: %v\n%s",
			creatorHandle, downloaderHandle, err, string(out))
		http.Error(w, "server error generating download", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, downloadFilename))
	http.ServeFile(w, r, tmpPath)
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

// sanitize_for_drawtext_go mirrors the Rust function in the transcoding service.
// Allows alphanumerics, underscore, dot, and hyphen — nothing that can escape
// ffmpeg's single-quoted drawtext expression.
func sanitize_for_drawtext_go(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '-' {
			b.WriteRune(c)
		}
	}
	return b.String()
}
