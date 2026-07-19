package handler

import (
	"encoding/json"
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
		dbpkg.EnrichWorksWithReactions(h.db, w2, user.ID)
		dbpkg.EnrichWorksWithQuotes(h.db, w2)
		dbpkg.EnrichWorksWithLinkPreviews(h.db, w2)
		for _, work := range w2 {
			work.TimeAgo = TimeAgo(work.CreatedAt)
		}
		works = filterAdultForViewer(user, w2)
	}
	isFollowing := h.db != nil && user.ID != "" && dbpkg.IsFollowingTopic(h.db, user.ID, strings.ToLower(tag))
	rail := h.railData(user, "search")
	h.render(w, "tag.html", map[string]interface{}{
		"User":              user,
		"Tag":               tag,
		"Works":             works,
		"Title":             "#" + tag + " · F33D3R",
		"SessionID":         uuid.New().String(),
		"ShowScores":        h.cfg.ShowScores,
		"CurrentUserHandle": user.Handle,
		"IsFollowingTopic":  isFollowing,
		"Themes":            ThemesWithActive(user.ThemeID),
		"TrendingTags":      rail["TrendingTags"],
		"SuggestedUsers":    rail["SuggestedUsers"],
		"RailContext":       rail["RailContext"],
	})
}

// ── GIF search proxy (KLIPY) ──────────────────────────────────────────────────

// gifSearch proxies GET /api/gif/search?q=... to the KLIPY GIF API
// (https://klipy.com — a distinct provider from Giphy/Tenor; the app key is
// embedded in the URL path). Empty q returns trending GIFs. If the KLIPY key is
// not set, returns empty results. The KLIPY response is normalised to
// {results:[{id,title,url,thumb_url}]} so the front-end GIF picker is unchanged.
func (h *Handler) gifSearch(w http.ResponseWriter, r *http.Request) {
	if h.cfg.KlipyAPIKey == "" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[],"error":"gif_search_not_configured"}`))
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
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

	resp, err := http.Get(apiURL)
	if err != nil {
		http.Error(w, "gif search failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		http.Error(w, "gif search read error", http.StatusBadGateway)
		return
	}

	// Parse KLIPY response. Each item carries a `file` map of size tiers
	// (hd|md|sm|xs), and each tier a map of formats (gif|webp|mp4|…) with a url.
	var raw struct {
		Data struct {
			Data []struct {
				ID    interface{}                          `json:"id"`
				Title string                               `json:"title"`
				File  map[string]map[string]struct{ URL string `json:"url"` } `json:"file"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[]}`))
		return
	}

	// gifInTier returns the gif URL for the first available tier in order.
	gifInTier := func(file map[string]map[string]struct{ URL string `json:"url"` }, tiers ...string) string {
		for _, t := range tiers {
			if formats, ok := file[t]; ok {
				if g, ok := formats["gif"]; ok && g.URL != "" {
					return g.URL
				}
			}
		}
		return ""
	}

	type gifResult struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		URL      string `json:"url"`
		ThumbURL string `json:"thumb_url"`
	}
	out := make([]gifResult, 0, len(raw.Data.Data))
	for _, d := range raw.Data.Data {
		full := gifInTier(d.File, "md", "hd", "sm", "xs")
		thumb := gifInTier(d.File, "sm", "xs", "md", "hd")
		if full == "" {
			continue
		}
		if thumb == "" {
			thumb = full
		}
		out = append(out, gifResult{
			ID:       klipyID(d.ID),
			Title:    d.Title,
			URL:      full,
			ThumbURL: thumb,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	data, _ := json.Marshal(map[string]interface{}{"results": out})
	fmt.Fprint(w, string(data))
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
	if len(text) > 25000 {
		text = text[:25000]
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
