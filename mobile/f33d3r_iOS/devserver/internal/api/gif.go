package api

import (
	"f33d3r.com/ios/devserver/internal/store"

	"encoding/json"
	"errors"
	"fmt"
	"image/gif"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GIF search — GET /api/v1/gif/search?q= → 200 GifSearchDTO; trending when q
// is empty.
//
// Two sources, one contract. With a KLIPY key the server asks KLIPY, exactly
// as feed-engine's gifLookup does. Without one it answers from the GIFs in
// `<seedmedia>/gifs`, so the picker works on a machine with no provider and
// no network: the files are served under `/media/seed/gifs/` and their names
// are their titles, which is what a search matches. A server with neither says
// so in the code the client branches on, `503 gif_search_unavailable`, rather
// than as an empty grid.

// GifSearchDTO mirrors feed-engine's GifSearchDTO and the Swift `GifSearchPage`.
type GifSearchDTO struct {
	Results []GifResultDTO `json:"results"`
}

// GifResultDTO mirrors feed-engine's GifResultDTO and the Swift `GifResult`.
type GifResultDTO struct {
	ID         string `json:"id"`
	URL        string `json:"url"`
	PreviewURL string `json:"preview_url"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Title      string `json:"title"`
}

var errGifUnconfigured = errors.New("gif search: no provider and no local library")

// WithGifProvider sets the KLIPY app key. Empty keeps the local library.
func (s *Server) WithGifProvider(key string) *Server {
	s.gifKey = key
	return s
}

func (s *Server) gifSearch(w http.ResponseWriter, r *http.Request, _ *store.User) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var (
		results []GifResultDTO
		err     error
	)
	if s.gifKey != "" {
		results, err = s.klipyLookup(r, q)
	} else {
		results, err = s.localGifLookup(q)
	}
	if err != nil {
		if errors.Is(err, errGifUnconfigured) {
			writeError(w, http.StatusServiceUnavailable, "gif_search_unavailable", "GIF search is not available on this server.")
			return
		}
		log.Printf("api: gif search: %v", err)
		writeError(w, http.StatusBadGateway, "gif_search_failed", "GIF search did not answer. Try again in a moment.")
		return
	}
	if results == nil {
		results = []GifResultDTO{}
	}
	writeJSON(w, http.StatusOK, GifSearchDTO{Results: results})
}

// localGifLookup lists the library, filtered by every word of q appearing in
// a file's name. Trending — an empty q — is the whole library, by name.
func (s *Server) localGifLookup(q string) ([]GifResultDTO, error) {
	dir := filepath.Join(s.seedMediaDir, "gifs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errGifUnconfigured
		}
		return nil, err
	}
	words := strings.Fields(strings.ToLower(q))
	var out []GifResultDTO
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.EqualFold(filepath.Ext(name), ".gif") {
			continue
		}
		title := strings.ReplaceAll(strings.TrimSuffix(name, filepath.Ext(name)), "-", " ")
		if !matchesAll(strings.ToLower(title), words) {
			continue
		}
		width, height := gifSize(filepath.Join(dir, name))
		path := "/media/seed/gifs/" + url.PathEscape(name)
		out = append(out, GifResultDTO{
			ID: strings.TrimSuffix(name, filepath.Ext(name)), URL: path, PreviewURL: path,
			Width: width, Height: height, Title: title,
		})
	}
	if len(out) == 0 && len(words) == 0 {
		// An empty library is a server with nothing to offer, not a search
		// that found nothing.
		return nil, errGifUnconfigured
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, nil
}

func matchesAll(title string, words []string) bool {
	for _, w := range words {
		if !strings.Contains(title, w) {
			return false
		}
	}
	return true
}

// gifSize reads the logical screen size off the header. Zero when the file
// cannot be read, which the contract allows ("0 when the provider did not say").
func gifSize(path string) (int, int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	cfg, err := gif.DecodeConfig(f)
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// klipyLookup asks KLIPY and normalises its answer, the way feed-engine's
// gifLookup does: each item has a `file` map of size tiers, each a map of
// formats carrying a url and its dimensions; items with no gif rendition are
// dropped.
func (s *Server) klipyLookup(r *http.Request, q string) ([]GifResultDTO, error) {
	params := url.Values{}
	params.Set("per_page", "24")
	params.Set("rating", "g")
	base := "https://api.klipy.com/api/v1/" + url.PathEscape(s.gifKey) + "/gifs/"
	apiURL := base + "trending?" + params.Encode()
	if q != "" {
		params.Set("q", q)
		apiURL = base + "search?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	type rendition struct {
		URL    string `json:"url"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	var raw struct {
		Data struct {
			Data []struct {
				ID    any                             `json:"id"`
				Title string                          `json:"title"`
				File  map[string]map[string]rendition `json:"file"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("klipy: %w", err)
	}
	gifInTier := func(file map[string]map[string]rendition, tiers ...string) (rendition, bool) {
		for _, t := range tiers {
			if g, ok := file[t]["gif"]; ok && g.URL != "" {
				return g, true
			}
		}
		return rendition{}, false
	}
	out := make([]GifResultDTO, 0, len(raw.Data.Data))
	for _, d := range raw.Data.Data {
		full, ok := gifInTier(d.File, "md", "hd", "sm", "xs")
		if !ok {
			continue
		}
		preview, ok := gifInTier(d.File, "sm", "xs", "md", "hd")
		if !ok {
			preview = full
		}
		out = append(out, GifResultDTO{
			ID: fmt.Sprint(d.ID), Title: d.Title, URL: full.URL, PreviewURL: preview.URL,
			Width: full.Width, Height: full.Height,
		})
	}
	return out, nil
}
