package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

var rawURLRe = regexp.MustCompile(`https?://[^\s"'<>\x00-\x1f]+`)

// bareExternalURLRe matches bare domain URLs without a protocol prefix.
// Applied only when rawURLRe finds no external URL.
var bareExternalURLRe = regexp.MustCompile(`(?:^|[\s\n])([a-zA-Z0-9][a-zA-Z0-9.\-]*\.[a-zA-Z]{2,}(?:/[^\s"'<>]*)?)`)

// extractFirstExternalURL returns the first absolute http/https URL in body
// that doesn't point back to this platform. Also normalises bare domains to https://.
func extractFirstExternalURL(body string) string {
	for _, raw := range rawURLRe.FindAllString(body, -1) {
		raw = strings.TrimRight(raw, ".,;:!?)'\"")
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if strings.HasSuffix(host, "f33d3r.com") || host == "localhost" || host == "127.0.0.1" {
			continue
		}
		return raw
	}
	// Bare domain fallback — normalise to https:// so the link_previews cache key matches.
	if m := bareExternalURLRe.FindStringSubmatch(body); m != nil {
		candidate := strings.TrimRight(m[1], ".,;:!?)'\"")
		if strings.HasPrefix(candidate, ".") || !strings.Contains(candidate, ".") {
			return ""
		}
		normalised := "https://" + candidate
		u, err := url.Parse(normalised)
		if err != nil || u.Hostname() == "" {
			return ""
		}
		host := strings.ToLower(u.Hostname())
		if strings.HasSuffix(host, "f33d3r.com") || host == "localhost" || host == "127.0.0.1" {
			return ""
		}
		return normalised
	}
	return ""
}

var (
	metaTagRe     = regexp.MustCompile(`(?i)<meta[^>]+>`)
	metaPropRe    = regexp.MustCompile(`(?i)(?:property|name)\s*=\s*["']([^"']+)["']`)
	metaContentRe = regexp.MustCompile(`(?i)content\s*=\s*["']([^"']*)["']`)
	htmlTitleRe   = regexp.MustCompile(`(?i)<title[^>]*>([^<]+)`)
)

func extractMetaContent(html, property string) string {
	for _, tag := range metaTagRe.FindAllString(html, -1) {
		pm := metaPropRe.FindStringSubmatch(tag)
		if pm == nil || !strings.EqualFold(pm[1], property) {
			continue
		}
		cm := metaContentRe.FindStringSubmatch(tag)
		if cm != nil {
			v := strings.TrimSpace(cm[1])
			if v != "" {
				return v
			}
		}
	}
	return ""
}

// ssrfSafe returns an error if any resolved IP for host is in a blocked range.
// Prevents the unfurler from being used as a proxy to hit internal services.
func ssrfSafe(host string) error {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	addrs, err := net.LookupHost(host)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("dns resolve failed for %s", host)
	}
	for _, addr := range addrs {
		ip, err := netip.ParseAddr(addr)
		if err != nil {
			return fmt.Errorf("bad ip %s", addr)
		}
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("private address blocked")
		}
	}
	return nil
}

const (
	unfurlTimeout     = 8 * time.Second
	unfurlMaxBodySize = 512 * 1024
	unfurlMaxRedirects = 3
)

var unfurlHTTPClient = &http.Client{
	Timeout: unfurlTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= unfurlMaxRedirects {
			return fmt.Errorf("too many redirects")
		}
		return ssrfSafe(req.URL.Host)
	},
}

// youtubeHostRe matches youtube.com and youtu.be for oEmbed dispatch.
var youtubeHostRe = regexp.MustCompile(`(?i)^(?:(?:www\.|m\.)?youtube\.com|youtu\.be)$`)

type youtubeOEmbed struct {
	Title        string `json:"title"`
	AuthorName   string `json:"author_name"`
	ProviderName string `json:"provider_name"`
	ThumbnailURL string `json:"thumbnail_url"`
}

// fetchYouTubeOEmbed uses YouTube's public oEmbed API — no auth required, no scraping.
func fetchYouTubeOEmbed(rawURL string) (*model.LinkPreview, error) {
	oembedURL := "https://www.youtube.com/oembed?url=" + url.QueryEscape(rawURL) + "&format=json"
	ctx, cancel := context.WithTimeout(context.Background(), unfurlTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, oembedURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "F33D3R-Unfurl/1.0 (+https://f33d3r.com)")
	resp, err := unfurlHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("youtube oembed http %d", resp.StatusCode)
	}
	var oe youtubeOEmbed
	if err := json.NewDecoder(resp.Body).Decode(&oe); err != nil {
		return nil, err
	}
	if oe.Title == "" {
		return nil, fmt.Errorf("youtube oembed: empty title")
	}
	siteName := oe.ProviderName
	if siteName == "" {
		siteName = "YouTube"
	}
	return &model.LinkPreview{
		URL:         rawURL,
		Title:       truncate(oe.Title, 200),
		Description: truncate(oe.AuthorName, 400),
		ImageURL:    truncate(oe.ThumbnailURL, 1000),
		SiteName:    siteName,
	}, nil
}

func fetchOGMetadata(rawURL string) (*model.LinkPreview, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}

	// YouTube blocks server-side scrapers — use the public oEmbed API instead.
	if youtubeHostRe.MatchString(u.Hostname()) {
		return fetchYouTubeOEmbed(rawURL)
	}

	if err := ssrfSafe(u.Host); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), unfurlTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "F33D3R-Unfurl/1.0 (+https://f33d3r.com)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en")

	resp, err := unfurlHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "text/html") && !strings.Contains(ct, "xhtml") {
		return nil, fmt.Errorf("not html content-type: %s", ct)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, unfurlMaxBodySize))
	if err != nil {
		return nil, err
	}
	// Only parse up to </head> — OG tags live in the head
	html := string(raw)
	if i := strings.Index(strings.ToLower(html), "</head>"); i > 0 {
		html = html[:i]
	}

	title := extractMetaContent(html, "og:title")
	if title == "" {
		title = extractMetaContent(html, "twitter:title")
	}
	if title == "" {
		if m := htmlTitleRe.FindStringSubmatch(html); m != nil {
			title = strings.TrimSpace(m[1])
		}
	}

	description := extractMetaContent(html, "og:description")
	if description == "" {
		description = extractMetaContent(html, "twitter:description")
	}
	if description == "" {
		description = extractMetaContent(html, "description")
	}

	imageURL := extractMetaContent(html, "og:image")
	if imageURL == "" {
		imageURL = extractMetaContent(html, "twitter:image")
	}

	siteName := extractMetaContent(html, "og:site_name")
	if siteName == "" {
		siteName = u.Hostname()
	}

	if title == "" && description == "" && imageURL == "" {
		return nil, fmt.Errorf("no og metadata found")
	}

	// Resolve relative image URL against the page base
	if imageURL != "" {
		if imgU, err2 := url.Parse(imageURL); err2 == nil && !imgU.IsAbs() {
			imageURL = u.ResolveReference(imgU).String()
		}
	}

	return &model.LinkPreview{
		URL:         rawURL,
		Title:       truncate(title, 200),
		Description: truncate(description, 400),
		ImageURL:    truncate(imageURL, 1000),
		SiteName:    truncate(siteName, 100),
	}, nil
}

// triggerLinkPreview is called as a goroutine after a legacy post is created.
// It finds the first external URL in body, fetches OG metadata, and caches it.
func (h *Handler) triggerLinkPreview(postID, body string) {
	if h.db == nil {
		return
	}
	rawURL := extractFirstExternalURL(body)
	if rawURL == "" {
		return
	}

	// Use the cache if fresh
	if cached, err := dbpkg.GetCachedLinkPreview(h.db, rawURL); err == nil && cached != nil {
		_ = dbpkg.SetPostLinkPreview(h.db, postID, rawURL)
		return
	}

	preview, err := fetchOGMetadata(rawURL)
	if err != nil || preview == nil {
		return
	}
	if err := dbpkg.UpsertLinkPreview(h.db, preview); err != nil {
		return
	}
	_ = dbpkg.SetPostLinkPreview(h.db, postID, rawURL)
}

// triggerWorkLinkPreview is called as a goroutine after a work is created.
// It fetches OG metadata for the first external URL in body and stores it in the
// link_previews cache so EnrichWorksWithLinkPreviews can serve it at display time.
// YouTube URLs are now supported via the oEmbed API in fetchOGMetadata.
func (h *Handler) triggerWorkLinkPreview(body string) {
	if h.db == nil {
		return
	}
	rawURL := extractFirstExternalURL(body)
	if rawURL == "" {
		return
	}
	// Already cached — nothing to do.
	if cached, err := dbpkg.GetCachedLinkPreview(h.db, rawURL); err == nil && cached != nil {
		return
	}
	preview, err := fetchOGMetadata(rawURL)
	if err != nil || preview == nil {
		return
	}
	_ = dbpkg.UpsertLinkPreview(h.db, preview)
}
