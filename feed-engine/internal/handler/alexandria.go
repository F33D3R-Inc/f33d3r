package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// alexandriaProxy proxies /library/* → Alexandria brain (:8097).
// Injects X-Pial-Identity so Alexandria can verify ownership on mutations.
func (h *Handler) alexandriaProxy(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, `{"error":"pial_not_ready"}`, http.StatusServiceUnavailable)
		return
	}

	upstreamURL, err := url.Parse(h.alexandriaURL)
	if err != nil {
		http.Error(w, `{"error":"alexandria_unavailable"}`, http.StatusBadGateway)
		return
	}

	pathSuffix := strings.TrimPrefix(r.URL.Path, "/library")
	if pathSuffix == "" {
		pathSuffix = "/"
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstreamURL.Scheme
			req.URL.Host = upstreamURL.Host
			req.URL.Path = pathSuffix
			if r.URL.RawQuery != "" {
				req.URL.RawQuery = r.URL.RawQuery
			}
			req.Host = upstreamURL.Host
			req.Header.Set("X-Pial-Identity", user.PIALID)
			req.Header.Set("X-Pial-Handle", user.Handle)
			req.Header.Del("Cookie")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[alexandria proxy] %v", err)
			http.Error(w, `{"error":"alexandria_unavailable"}`, http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// alexandriaTitlePayload is the JSON body sent to Alexandria POST /v1/titles.
type alexandriaTitlePayload struct {
	AuthorPIALID string   `json:"author_pial_id"`
	SectionSlug  string   `json:"section_slug,omitempty"`
	Body         string   `json:"body"`
	MediaArea    string   `json:"media_area"`
	Visibility   string   `json:"visibility"`
	IsNSFW       bool     `json:"is_nsfw,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	LegacyWorkID string   `json:"legacy_work_id,omitempty"`
}

// alexandriaCreateTitle dual-writes a new work into the Alexandria catalog.
// Runs in a goroutine — failures are logged but never block the caller.
func (h *Handler) alexandriaCreateTitle(p alexandriaTitlePayload) {
	if h.alexandriaURL == "" {
		return
	}

	body, err := json.Marshal(p)
	if err != nil {
		log.Printf("[alexandria] marshal: %v", err)
		return
	}

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, h.alexandriaURL+"/v1/titles", bytes.NewReader(body))
	if err != nil {
		log.Printf("[alexandria] build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pial-Identity", p.AuthorPIALID)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[alexandria] create_title: %v", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusCreated {
		log.Printf("[alexandria] create_title returned %d", resp.StatusCode)
	}
}
