package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

var safeTagRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,100}$`)

func (h *Handler) tagPage(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("name")
	if !safeTagRe.MatchString(tag) {
		http.NotFound(w, r)
		return
	}
	user := h.userFromRequest(w, r)
	var works interface{}
	if h.db != nil {
		w2, sErr := dbpkg.SearchWorks(h.db, tag, 40, user.ID)
		if sErr != nil {
			log.Printf("[tag] search works %q: %v", tag, sErr)
		}
		// One enrichment sequence, defined once (enrichWorks) — including the
		// ballot, so a poll found by tag draws the same as a poll in the feed.
		h.enrichWorks(w2, user)
		works = filterAdultForViewer(user, w2)
	}
	isFollowing := h.db != nil && user.ID != "" && dbpkg.IsFollowingTopic(h.db, user.ID, strings.ToLower(tag))
	h.render(w, r, "tag.html", h.withRail(map[string]interface{}{
		"User":              user,
		"Tag":               tag,
		"Works":             works,
		"Title":             "#" + tag + " · F33D3R",
		"SessionID":         uuid.New().String(),
		"ShowScores":        h.cfg.ShowScores,
		"CurrentUserHandle": user.Handle,
		"IsFollowingTopic":  isFollowing,
		"Themes":            ThemesWithActive(user.ThemeID),
	}, user, "search"))
}

// ── GIF search (KLIPY) ───────────────────────────────────────────────────────
//
// KLIPY (https://klipy.com) is the provider — distinct from Giphy/Tenor; the
// app key is a path segment of its URL. One fetch-and-parse, gifLookup, feeds
// both the web picker (gifSearch) and the JSON lane (apiV1GifSearch); each
// projects the results in the keys its client reads.

// gifResult is one GIF as this server understands it, whichever client asked.
type gifResult struct {
	ID         string
	Title      string
	URL        string // the GIF to attach, mid tier
	PreviewURL string // a smaller rendition for the picker grid
	Width      int    // of URL's rendition; 0 when the provider did not say
	Height     int
}

// errGifUnconfigured: no KLIPY key, so there is no provider to ask.
var errGifUnconfigured = errors.New("gif search: KLIPHY_API is not set")

// errGifMalformed: the provider answered with something other than its
// documented shape.
var errGifMalformed = errors.New("gif search: provider answer not readable")

// gifUpstreamError: the provider could not be reached, or its answer could
// not be read to the end.
type gifUpstreamError struct {
	op  string // "request" | "read"
	err error
}

func (e *gifUpstreamError) Error() string { return "gif search " + e.op + ": " + e.err.Error() }
func (e *gifUpstreamError) Unwrap() error { return e.err }

// gifLookup asks KLIPY for GIFs matching q — trending when q is empty — and
// normalises the answer. KLIPY returns each item with a `file` map of size
// tiers (hd|md|sm|xs), each a map of formats (gif|webp|mp4|…) carrying a url
// and its dimensions. Items with no gif rendition at all are dropped.
func (h *Handler) gifLookup(ctx context.Context, q string) ([]gifResult, error) {
	if h.cfg.KlipyAPIKey == "" {
		return nil, errGifUnconfigured
	}

	params := url.Values{}
	params.Set("per_page", "24")
	params.Set("rating", "g")
	// KLIPY: app key is a path segment; the action (trending|search) is the verb.
	base := "https://api.klipy.com/api/v1/" + url.PathEscape(h.cfg.KlipyAPIKey) + "/gifs/"
	var apiURL string
	if q == "" {
		apiURL = base + "trending?" + params.Encode()
	} else {
		params.Set("q", q)
		apiURL = base + "search?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, &gifUpstreamError{op: "request", err: err}
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, &gifUpstreamError{op: "request", err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil, &gifUpstreamError{op: "read", err: err}
	}

	type rendition struct {
		URL    string `json:"url"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	var raw struct {
		Data struct {
			Data []struct {
				ID    interface{}                     `json:"id"`
				Title string                          `json:"title"`
				File  map[string]map[string]rendition `json:"file"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", errGifMalformed, err)
	}

	// gifInTier returns the gif rendition of the first tier present, in order.
	gifInTier := func(file map[string]map[string]rendition, tiers ...string) (rendition, bool) {
		for _, t := range tiers {
			if g, ok := file[t]["gif"]; ok && g.URL != "" {
				return g, true
			}
		}
		return rendition{}, false
	}

	out := make([]gifResult, 0, len(raw.Data.Data))
	for _, d := range raw.Data.Data {
		full, ok := gifInTier(d.File, "md", "hd", "sm", "xs")
		if !ok {
			continue
		}
		preview, ok := gifInTier(d.File, "sm", "xs", "md", "hd")
		if !ok {
			preview = full
		}
		out = append(out, gifResult{
			ID:         klipyID(d.ID),
			Title:      d.Title,
			URL:        full.URL,
			PreviewURL: preview.URL,
			Width:      full.Width,
			Height:     full.Height,
		})
	}
	return out, nil
}

// gifSearch — GET /api/gif/search?q=... — the web picker's projection of
// gifLookup: {results:[{id,title,url,thumb_url}]}. With no key configured the
// picker is told so inside a 200, which it draws as an empty grid; an
// unreadable provider answer is an empty grid too, and is logged.
func (h *Handler) gifSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	results, err := h.gifLookup(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")))
	if err != nil {
		var upstream *gifUpstreamError
		switch {
		case errors.Is(err, errGifUnconfigured):
			w.Write([]byte(`{"results":[],"error":"gif_search_not_configured"}`))
		case errors.As(err, &upstream) && upstream.op == "read":
			http.Error(w, "gif search read error", http.StatusBadGateway)
		case errors.As(err, &upstream):
			http.Error(w, "gif search failed", http.StatusBadGateway)
		default:
			log.Printf("[gif] %v", err)
			w.Write([]byte(`{"results":[]}`))
		}
		return
	}

	type webGif struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		URL      string `json:"url"`
		ThumbURL string `json:"thumb_url"`
	}
	out := make([]webGif, 0, len(results))
	for _, g := range results {
		out = append(out, webGif{ID: g.ID, Title: g.Title, URL: g.URL, ThumbURL: g.PreviewURL})
	}
	if err := json.NewEncoder(w).Encode(map[string]interface{}{"results": out}); err != nil {
		log.Printf("[gif] encode: %v", err)
	}
}

// facetHashtagSearch returns rendered hashtag suggestion rows for the compose
// autocomplete (most-used tags matching the prefix). GET /facets/hashtag/search?q=...
func (h *Handler) facetHashtagSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	q = strings.TrimPrefix(q, "#")
	// Strip anything that is not a valid tag character so the "new tag" row we
	// echo back is always safe to inject.
	q = hashtagCharsRe.ReplaceAllString(q, "")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if q == "" || h.db == nil {
		return
	}
	tags, sErr := dbpkg.SearchTags(h.db, q, 8)
	if sErr != nil {
		log.Printf("[hashtag-search] %q: %v", q, sErr)
	}
	// Global search like X: always offer the tag the user is typing as the first
	// row — even when no post has used it yet — unless an existing tag is an
	// exact (case-insensitive) match, in which case the real row covers it.
	exact := false
	for _, t := range tags {
		if strings.EqualFold(t.Tag, q) {
			exact = true
			break
		}
	}
	if !exact {
		h.renderPartial(w, "hashtag_autocomplete_row", dbpkg.TrendingTag{Tag: q, Count: 0})
	}
	for _, t := range tags {
		h.renderPartial(w, "hashtag_autocomplete_row", t)
	}
}

// hashtagCharsRe matches characters NOT allowed in a hashtag (used to sanitise
// the autocomplete query before echoing it back as a selectable row).
var hashtagCharsRe = regexp.MustCompile(`[^A-Za-z0-9_]`)

// facetComposeHighlight returns the server-rendered highlight backdrop for the compose
// box: the raw text with @/#/$ tokens wrapped as styled links (same renderer the posted
// work uses). The browser swaps this fragment behind the transparent-text textarea.
// FA: server owns the render; the client only swaps the fragment and syncs scroll.
// POST /facets/compose_highlight  (form: content)
func (h *Handler) facetComposeHighlight(w http.ResponseWriter, r *http.Request) {
	text := r.FormValue("content")
	if len(text) > workBodyMaxRunes {
		text = text[:workBodyMaxRunes]
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, string(highlightComposeText(text)))
}

// klipyID normalises a KLIPY item id, which arrives as either a JSON string or a
// JSON number, into a plain decimal string (no scientific notation).
func klipyID(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}
