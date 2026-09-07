package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/model"
)

// ── /api/v1 — the JSON read surface for native clients ───────────────────────
//
// The web app is server-rendered Facets over HTMX; it has no JSON API and
// needs none. The native clients (mobile/f33d3r_iOS, and the Android app's
// JSON lane) draw their own screens, so they read the same state as JSON
// DTOs. This surface is reads only: every mutation still goes through the one
// D-070 lane, POST /events, exactly as the web does, and the signing-key
// registration stays at /api/pial/signing-key/register. Authentication is the
// same user_sessions row the web uses, presented as a bearer token
// (GetSessionToken accepts both), so a session issued here is a session
// everywhere and a logout anywhere revokes it everywhere.
//
// Shape — path, status codes, error envelope, JSON keys — is the contract in
// F33D3RKit/Sources/F33D3RKit (Endpoint.swift, Models/), checked from this
// side by api_v1_contract_test.go against the same golden fixtures the Swift
// tests decode.

const (
	apiV1DefaultPage = 20
	apiV1MaxPage     = 50
)

// registerAPIV1 mounts the surface on mux. Reads share the read limiter with
// the pages they mirror; login and signup are metered per IP and per target
// account the way the web forms are.
func (h *Handler) registerAPIV1(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/auth/login", h.rlAuth.Limit(h.rlAccount.LimitKeyed(apiV1AccountKey, h.apiV1Login)))
	mux.HandleFunc("POST /api/v1/auth/signup", h.rlAuth.Limit(h.apiV1Signup))
	mux.HandleFunc("POST /api/v1/auth/logout", h.rlWrite.Limit(h.requireAPIUser(h.apiV1Logout)))
	mux.HandleFunc("GET /api/v1/me", h.rlRead.Limit(h.requireAPIUser(h.apiV1Me)))

	mux.HandleFunc("GET /api/v1/feed", h.rlRead.Limit(h.requireAPIUser(h.apiV1Feed)))
	mux.HandleFunc("GET /api/v1/works/{id}", h.rlRead.Limit(h.requireAPIUser(h.apiV1Work)))
	mux.HandleFunc("GET /api/v1/works/{id}/replies", h.rlRead.Limit(h.requireAPIUser(h.apiV1Replies)))
	mux.HandleFunc("GET /api/v1/works/{id}/quotes", h.rlRead.Limit(h.requireAPIUser(h.apiV1WorkQuotes)))
	mux.HandleFunc("GET /api/v1/users/{handle}", h.rlRead.Limit(h.requireAPIUser(h.apiV1Profile)))
	mux.HandleFunc("GET /api/v1/users/{handle}/works", h.rlRead.Limit(h.requireAPIUser(h.apiV1ProfileWorks)))
	mux.HandleFunc("GET /api/v1/notifications", h.rlRead.Limit(h.requireAPIUser(h.apiV1Notifications)))
	mux.HandleFunc("GET /api/v1/wallet", h.rlRead.Limit(h.requireAPIUser(h.apiV1Wallet)))
	mux.HandleFunc("GET /api/v1/search", h.rlRead.Limit(h.requireAPIUser(h.apiV1Search)))
	mux.HandleFunc("POST /api/v1/media", h.rlWrite.Limit(h.requireAPIUser(h.apiV1UploadMedia)))
	// The rest of the upload lane: video, answered with a job and polled on the
	// route under it; voice; and the GIF picker's search. Twins of the web's
	// cookie-session upload routes — the cores are shared, see api_v1_media.go.
	mux.HandleFunc("POST /api/v1/media/video", h.rlWrite.Limit(h.requireAPIUser(h.apiV1UploadVideo)))
	mux.HandleFunc("GET /api/v1/media/video/{id}", h.rlRead.Limit(h.requireAPIUser(h.apiV1VideoJob)))
	mux.HandleFunc("POST /api/v1/media/voice", h.rlWrite.Limit(h.requireAPIUser(h.apiV1UploadVoice)))
	mux.HandleFunc("GET /api/v1/gif/search", h.rlRead.Limit(h.requireAPIUser(h.apiV1GifSearch)))

	mux.HandleFunc("GET /api/v1/users/{handle}/followers", h.rlRead.Limit(h.requireAPIUser(h.apiV1Followers)))
	mux.HandleFunc("GET /api/v1/users/{handle}/following", h.rlRead.Limit(h.requireAPIUser(h.apiV1Following)))
	mux.HandleFunc("GET /api/v1/tags/trending", h.rlRead.Limit(h.requireAPIUser(h.apiV1TrendingTags)))
	mux.HandleFunc("GET /api/v1/sessions", h.rlRead.Limit(h.requireAPIUser(h.apiV1Sessions)))
	// The settings screens: who this account has silenced, what Herald is
	// allowed to send it, the 2FA enrolment step, and its own data export.
	// The writes behind these stay on POST /events.
	mux.HandleFunc("GET /api/v1/blocks", h.rlRead.Limit(h.requireAPIUser(h.apiV1Blocks)))
	mux.HandleFunc("GET /api/v1/notifications/preferences", h.rlRead.Limit(h.requireAPIUser(h.apiV1NotificationPrefs)))
	mux.HandleFunc("GET /api/v1/2fa/setup", h.rlWrite.Limit(h.requireAPIUser(h.apiV12FASetup)))
	mux.HandleFunc("GET /api/v1/account/export", h.rlRead.Limit(h.requireAPIUser(h.apiV1AccountExport)))
	mux.HandleFunc("GET /api/v1/visions", h.rlRead.Limit(h.requireAPIUser(h.apiV1Visions)))
	mux.HandleFunc("GET /api/v1/visions/{id}/viewers", h.rlRead.Limit(h.requireAPIUser(h.apiV1VisionViewers)))
	mux.HandleFunc("GET /api/v1/live", h.rlRead.Limit(h.requireAPIUser(h.apiV1Live)))
	mux.HandleFunc("GET /api/v1/sports", h.rlRead.Limit(h.requireAPIUser(h.apiV1Sports)))
	// One stock quote, for the card the app draws under a work carrying a
	// $TICKER. The web reads the same snapshot as a Facet fragment.
	mux.HandleFunc("GET /api/v1/cashtag/search", h.rlRead.Limit(h.requireAPIUser(h.apiV1CashtagSearch)))
	mux.HandleFunc("GET /api/v1/cashtag/{ticker}", h.rlRead.Limit(h.requireAPIUser(h.apiV1Cashtag)))
	mux.HandleFunc("GET /api/v1/live/{id}", h.rlRead.Limit(h.requireAPIUser(h.apiV1LiveRoom)))
	// Frequencies, read as JSON: the lanes and one room. Writes stay on
	// POST /events (frequency.*), which answers this same room DTO to a
	// native caller.
	mux.HandleFunc("GET /api/v1/frequencies", h.rlRead.Limit(h.requireAPIUser(h.apiV1Frequencies)))
	mux.HandleFunc("GET /api/v1/frequencies/{id}", h.rlRead.Limit(h.requireAPIUser(h.apiV1FrequencyRoom)))

	// Gnosis, read as JSON. The writes stay on POST /events (message_send,
	// message_read) and the sealed write stays on /api/gnosis/send-sealed,
	// which is where the ciphertext already goes.
	mux.HandleFunc("GET /api/v1/messages", h.rlRead.Limit(h.requireAPIUser(h.apiV1Messages)))
	mux.HandleFunc("GET /api/v1/messages/{id}", h.rlRead.Limit(h.requireAPIUser(h.apiV1MessageThread)))

	// F33D3R Numbers, read as JSON. Both routes are addressed by the caller's
	// own identity and answer only about it: the Numbers this account holds,
	// and the people asking to reach it. There is no route here that takes a
	// Number and says anything about who holds it, and there must never be
	// one — the whole design of the Number rests on that. The writes stay on
	// POST /events (number_mint, number_revoke, number_policy, contact_policy,
	// contact_decide, message_number).
	mux.HandleFunc("GET /api/v1/numbers", h.rlRead.Limit(h.requireAPIUser(h.apiV1Numbers)))
	mux.HandleFunc("GET /api/v1/contact/requests", h.rlRead.Limit(h.requireAPIUser(h.apiV1ContactRequests)))

	// The two streams: the account's own events and one live room. Read
	// limited like the web's /events/stream; nothing between them and the
	// client buffers, so frames reach the phone as they are written.
	mux.HandleFunc("GET /api/v1/events", h.rlRead.Limit(h.requireAPIUser(h.apiV1UserEvents)))
	mux.HandleFunc("GET /api/v1/live/{id}/events", h.rlRead.Limit(h.requireAPIUser(h.apiV1LiveEvents)))
	mux.HandleFunc("GET /api/v1/frequencies/{id}/events", h.rlRead.Limit(h.requireAPIUser(h.apiV1FrequencyEvents)))
}

// apiV1AccountKey names the account a JSON login targets, for the per-account
// limiter. The body is read here and restored so the handler reads it again.
func apiV1AccountKey(r *http.Request) string {
	body, err := peekBody(r, 1<<16)
	if err != nil {
		return ""
	}
	var req struct {
		Handle string `json:"handle"`
	}
	_ = json.Unmarshal(body, &req)
	return strings.TrimSpace(strings.ToLower(req.Handle))
}

// requireAPIUser resolves the bearer session to an account or answers 401 in
// the JSON envelope the client branches on. The anonymous DemoUser sentinel
// that page handlers render for is not an account and never passes.
func (h *Handler) requireAPIUser(next func(http.ResponseWriter, *http.Request, *model.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.db == nil {
			apiError(w, http.StatusServiceUnavailable, "db_unavailable", "F33D3R is having trouble right now. Try again in a moment.")
			return
		}
		tok := GetSessionToken(r)
		if tok == "" {
			apiError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
			return
		}
		u := h.userFromRequest(w, r)
		if viewerAccountID(u) == "" {
			apiError(w, http.StatusUnauthorized, "session_expired", "Your session has expired. Sign in again.")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r, u)
	}
}

// ── JSON helpers ──────────────────────────────────────────────────────────────

func apiJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("[api/v1] encode: %v", err)
	}
}

// apiErrorBody is the envelope: a stable code the client branches on and a
// message it may show.
type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func apiError(w http.ResponseWriter, status int, code, message string) {
	apiJSON(w, status, map[string]apiErrorBody{"error": {Code: code, Message: message}})
}

func apiServerError(w http.ResponseWriter, err error) {
	log.Printf("[api/v1] %v", err)
	apiError(w, http.StatusInternalServerError, "server_error", "F33D3R is having trouble right now. Try again in a moment.")
}

func apiReadJSON(r *http.Request, v interface{}) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 4<<20)
	return json.NewDecoder(r.Body).Decode(v)
}

// apiLimit reads ?limit=, capped.
func apiLimit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return apiV1DefaultPage
	}
	if n > apiV1MaxPage {
		return apiV1MaxPage
	}
	return n
}

// apiCursor parses a ?cursor= as the RFC3339 instant the previous page ended
// on. Empty means "from now". ok is false for a cursor that is not a time.
func apiCursor(raw string) (t time.Time, ok bool) {
	if raw == "" {
		return time.Now(), true
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// apiNextCursor is the cursor for the page after `works`, or "" when this page
// was short and there is nothing below it. On a ranked surface the caller
// passes the ranker's window cursor, which sits below everything considered.
func apiNextCursor(works []*model.Work, limit int, rankCursor time.Time) string {
	if len(works) == 0 || len(works) < limit {
		return ""
	}
	t := feedTime(works[len(works)-1])
	if !rankCursor.IsZero() {
		t = rankCursor
	}
	return t.Format(time.RFC3339Nano)
}

// peekBody reads up to max bytes of the body and puts them back, so a
// limiter key can be taken from a JSON body the handler still has to decode.
func peekBody(r *http.Request, max int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, max))
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
	return body, nil
}
