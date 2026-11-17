package sports

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// images.go — club logos and player headshots, served from this platform.
//
// The upstream's documents point at its own image CDN. A browser that followed
// those addresses would tell that CDN who is reading which game, on every page
// view, and would draw nothing at all the day the CDN refused it. So no address
// of the upstream's ever reaches a page: the model rewrites every image to this
// platform's own route (ImageURL), and ImageStore answers that route from a
// disk cache it fills on first request.
//
// What the route will fetch is closed, not open. It is not a general proxy:
//   - only the upstream's image hosts (imageHosts) are ever rewritten, and the
//     route itself only ever asks a.espncdn.com, so a crafted path cannot make
//     this server request anything else;
//   - only paths under /i/ that end in a raster image extension, with no "..";
//   - only the widths in imageWidths, which the upstream's resizer renders, so
//     the cache cannot be filled with one entry per arbitrary number;
//   - the answer must be 200, at most maxImageBytes, and sniff as PNG, JPEG,
//     WebP or GIF. SVG is refused: it can carry script, and this route serves
//     from the platform's own origin.

const (
	// ImageRoute is the path prefix the route is mounted at.
	ImageRoute = "/img/sports/"
	// LogoWidth and HeadshotWidth are the widths the page asks for. The largest
	// either is drawn at is under 64 CSS pixels, so 128 covers a 2x display.
	LogoWidth     = 128
	HeadshotWidth = 128

	imageOrigin   = "https://a.espncdn.com"
	maxImageBytes = 2 << 20
	// imageFresh is how long a cached image is served without asking again.
	// Logos change between seasons and headshots a few times a career.
	imageFresh = 7 * 24 * time.Hour
	// imageMissingFor is how long an upstream 404 is remembered, so a missing
	// headshot costs one request an hour rather than one per page view.
	imageMissingFor = time.Hour
	// imageFetchTimeout bounds one upstream image read.
	imageFetchTimeout = 10 * time.Second
	// imageParallel bounds concurrent upstream image reads.
	imageParallel = 6
	// maxMissingImages bounds the remembered-404 set.
	maxMissingImages = 10000
)

var (
	imageHosts = map[string]bool{
		"a.espncdn.com": true, "a1.espncdn.com": true, "a2.espncdn.com": true,
		"a3.espncdn.com": true, "a4.espncdn.com": true,
	}
	imageWidths  = map[int]bool{64: true, 128: true}
	imagePathRe  = regexp.MustCompile(`^i/[A-Za-z0-9_\-/]+\.(?:png|jpe?g|webp|gif)$`)
	imageAllowed = map[string]bool{
		"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true,
	}
)

// ImageURL rewrites an upstream image address to this platform's route, or
// returns "" when the address is not one this platform serves. "" is a valid
// answer everywhere an image is drawn: the surface draws initials instead.
func ImageURL(raw string, width int) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !imageWidths[width] {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	if !imageHosts[strings.ToLower(u.Hostname())] {
		return ""
	}
	p := u.Path
	if p == "/combiner/i" {
		// The upstream's resizer: the image itself is the img parameter.
		p = u.Query().Get("img")
	}
	p = strings.TrimPrefix(p, "/")
	if !validImagePath(p) {
		return ""
	}
	return ImageRoute + strconv.Itoa(width) + "/" + p
}

func validImagePath(p string) bool {
	return imagePathRe.MatchString(p) && !strings.Contains(p, "..") && !strings.Contains(p, "//")
}

// ImageStore answers the image route from a disk cache.
type ImageStore struct {
	dir    string
	client *http.Client
	sem    chan struct{}

	mu       sync.Mutex
	inflight map[string]*imageCall
	missing  map[string]time.Time
}

type imageCall struct {
	done chan struct{}
	err  error
}

// errImageMissing is an upstream that has no such image.
var errImageMissing = errors.New("image not found upstream")

// NewImageStore opens the cache directory, creating it.
func NewImageStore(dir string) (*ImageStore, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("sports images: cache dir %s: %w", dir, err)
	}
	return &ImageStore{
		dir:      dir,
		client:   &http.Client{Timeout: imageFetchTimeout},
		sem:      make(chan struct{}, imageParallel),
		inflight: map[string]*imageCall{},
		missing:  map[string]time.Time{},
	}, nil
}

// ServeHTTP answers GET <ImageRoute>{width}/{path...}. The caller mounts it
// with those two path values.
func (s *ImageStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	width, err := strconv.Atoi(r.PathValue("width"))
	path := r.PathValue("path")
	if err != nil || !imageWidths[width] || !validImagePath(path) {
		http.NotFound(w, r)
		return
	}
	key := imageKey(width, path)
	file := filepath.Join(s.dir, key[:2], key)

	info, statErr := os.Stat(file)
	if statErr != nil || time.Since(info.ModTime()) > imageFresh {
		if ferr := s.fill(r.Context(), width, path, key, file); ferr != nil {
			if statErr != nil {
				// Nothing cached to fall back on. The page draws initials.
				w.Header().Set("Cache-Control", "public, max-age=3600")
				if !errors.Is(ferr, errImageMissing) {
					log.Printf("[sports] image %d/%s: %v", width, path, ferr)
				}
				http.NotFound(w, r)
				return
			}
			// A stale copy is better than none; it is served and refreshed on a
			// later request.
		}
	}

	f, err := os.Open(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	ctype := http.DetectContentType(head[:n])
	if !imageAllowed[ctype] {
		// The cache was written only after this same check, so this is a file
		// that changed underneath us. Refuse it rather than serve it.
		http.NotFound(w, r)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "image unavailable", http.StatusInternalServerError)
		return
	}
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "image unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=604800, stale-while-revalidate=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", `"`+key[:16]+strconv.FormatInt(st.ModTime().Unix(), 36)+`"`)
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func imageKey(width int, path string) string {
	sum := sha256.Sum256([]byte(strconv.Itoa(width) + "|" + path))
	return hex.EncodeToString(sum[:])
}

// fill reads one image from the upstream into the cache. Concurrent requests
// for the same image wait for one read.
func (s *ImageStore) fill(ctx context.Context, width int, path, key, file string) error {
	s.mu.Lock()
	if until, ok := s.missing[key]; ok {
		if time.Now().Before(until) {
			s.mu.Unlock()
			return errImageMissing
		}
		delete(s.missing, key)
	}
	if call, ok := s.inflight[key]; ok {
		s.mu.Unlock()
		select {
		case <-call.done:
			return call.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	call := &imageCall{done: make(chan struct{})}
	s.inflight[key] = call
	s.mu.Unlock()

	// The read is detached from the request that started it: a viewer who
	// navigates away must not cancel the read the next viewer is waiting on.
	call.err = s.download(context.Background(), width, path, file)

	s.mu.Lock()
	delete(s.inflight, key)
	if errors.Is(call.err, errImageMissing) {
		if len(s.missing) >= maxMissingImages {
			now := time.Now()
			for k, until := range s.missing {
				if now.After(until) {
					delete(s.missing, k)
				}
			}
			if len(s.missing) >= maxMissingImages {
				// Every entry is still current: someone is asking for a great
				// many images that do not exist. Forgetting them costs at most
				// one upstream request each; growing without bound costs memory.
				s.missing = map[string]time.Time{}
			}
		}
		s.missing[key] = time.Now().Add(imageMissingFor)
	}
	s.mu.Unlock()
	close(call.done)
	return call.err
}

func (s *ImageStore) download(ctx context.Context, width int, path, file string) error {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()

	q := url.Values{"img": {"/" + path}, "w": {strconv.Itoa(width)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageOrigin+"/combiner/i?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden:
		return errImageMissing
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("upstream %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxImageBytes {
		return fmt.Errorf("image larger than %d bytes", maxImageBytes)
	}
	if ct := http.DetectContentType(body); !imageAllowed[ct] {
		return fmt.Errorf("upstream answered %s, not an image this route serves", ct)
	}

	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".img-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
