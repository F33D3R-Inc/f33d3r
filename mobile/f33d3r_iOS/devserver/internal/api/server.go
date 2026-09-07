// Package api is the HTTP surface: the `/api/v1` JSON reads, the `POST /events`
// write lane, and the two brain stand-ins the iOS client talks to through
// Nantar. Every handler's shape — path, status codes, error text, JSON keys —
// follows the plan in ~/.claude/plans and the Swift contract in F33D3RKit.
package api

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"f33d3r.com/ios/devserver/internal/store"
)

// Server holds the store and serves the routes.
type Server struct {
	store *store.Store
	// mediaDir is where uploaded files land and are served from under /media.
	mediaDir string
	// seedMediaDir holds the images the seeded works reference.
	seedMediaDir string
	// hub fans live-room events out to open SSE connections.
	hub *hub
	// videos is the process's table of video uploads — the transcoder stand-in.
	videos *videoJobs
	// gifKey is the KLIPY app key; empty means the local GIF library.
	gifKey string
}

// New builds a server.
func New(st *store.Store, mediaDir string) *Server {
	return &Server{store: st, mediaDir: mediaDir, seedMediaDir: "seedmedia", hub: newHub(), videos: newVideoJobs()}
}

// WithSeedMedia points the seed image route at dir.
func (s *Server) WithSeedMedia(dir string) *Server {
	s.seedMediaDir = dir
	return s
}

// Handler is the routed mux with logging.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// api/v1 — JSON, bearer auth.
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("POST /api/v1/auth/signup", s.signup)
	mux.HandleFunc("POST /api/v1/auth/logout", s.requireUser(s.logout))
	mux.HandleFunc("GET /api/v1/me", s.requireUser(s.me))

	mux.HandleFunc("GET /api/v1/feed", s.requireUser(s.feed))
	mux.HandleFunc("GET /api/v1/works/{id}", s.requireUser(s.work))
	mux.HandleFunc("GET /api/v1/works/{id}/replies", s.requireUser(s.replies))
	mux.HandleFunc("GET /api/v1/works/{id}/quotes", s.requireUser(s.quotes))
	mux.HandleFunc("GET /api/v1/users/{handle}", s.requireUser(s.profile))
	mux.HandleFunc("GET /api/v1/users/{handle}/works", s.requireUser(s.profileWorks))
	mux.HandleFunc("GET /api/v1/notifications", s.requireUser(s.notifications))
	mux.HandleFunc("GET /api/v1/wallet", s.requireUser(s.wallet))
	mux.HandleFunc("GET /api/v1/search", s.requireUser(s.search))
	mux.HandleFunc("GET /api/v1/users/{handle}/followers", s.requireUser(s.followers))
	mux.HandleFunc("GET /api/v1/users/{handle}/following", s.requireUser(s.following))
	mux.HandleFunc("GET /api/v1/tags/trending", s.requireUser(s.trendingTags))
	mux.HandleFunc("GET /api/v1/sessions", s.requireUser(s.sessions))
	mux.HandleFunc("GET /api/v1/events", s.requireUser(s.userEvents))
	mux.HandleFunc("GET /api/v1/visions", s.requireUser(s.visions))
	mux.HandleFunc("GET /api/v1/visions/{id}/viewers", s.requireUser(s.visionViewers))
	mux.HandleFunc("GET /api/v1/live", s.requireUser(s.live))
	mux.HandleFunc("GET /api/v1/live/{id}", s.requireUser(s.liveRoom))
	mux.HandleFunc("GET /api/v1/live/{id}/events", s.requireUser(s.liveEvents))
	mux.HandleFunc("GET /api/v1/frequencies", s.requireUser(s.frequencies))
	mux.HandleFunc("GET /api/v1/frequencies/{id}", s.requireUser(s.frequency))
	mux.HandleFunc("GET /api/v1/frequencies/{id}/events", s.requireUser(s.frequencyEvents))
	mux.HandleFunc("POST /api/v1/media", s.requireUser(s.uploadMedia))
	mux.HandleFunc("POST /api/v1/media/voice", s.requireUser(s.uploadVoice))
	mux.HandleFunc("POST /api/v1/media/video", s.requireUser(s.uploadVideo))
	mux.HandleFunc("GET /api/v1/media/video/{id}", s.requireUser(s.videoJob))
	mux.HandleFunc("GET /api/v1/gif/search", s.requireUser(s.gifSearch))

	// Origin-root lanes the web app uses too.
	mux.HandleFunc("POST /events", s.requireUser(s.events))
	mux.HandleFunc("POST /api/pial/signing-key/register", s.requireUser(s.registerSigningKey))

	// Served media: uploads from the data directory, and the checked-in seed
	// images the seeded works point at.
	mux.Handle("GET /media/seed/", http.StripPrefix("/media/seed/", http.FileServer(http.Dir(s.seedMediaDir))))
	mux.Handle("GET /media/", http.StripPrefix("/media/", http.FileServer(http.Dir(s.mediaDir))))

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "f33d3r-ios-devserver"})
	})

	return logRequests(mux)
}

// ── Auth middleware ────────────────────────────────────────────────────────────

type ctxKey int

const userKey ctxKey = 1

// requireUser resolves `Authorization: Bearer <token>` to an account, or
// answers 401 in the JSON error envelope the client branches on.
func (s *Server) requireUser(next func(http.ResponseWriter, *http.Request, *store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
			return
		}
		u, err := s.store.UserForToken(r.Context(), token)
		if err != nil {
			if err == store.ErrNotFound {
				writeError(w, http.StatusUnauthorized, "session_expired", "Your session has expired. Sign in again.")
				return
			}
			serverError(w, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, u)), u)
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// ── JSON helpers ──────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("api: encode: %v", err)
	}
}

// errorBody is `apiErrorBody`: a stable code and a showable message.
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]errorBody{"error": {Code: code, Message: message}})
}

func serverError(w http.ResponseWriter, err error) {
	log.Printf("api: %v", err)
	writeError(w, http.StatusInternalServerError, "server_error", "F33D3R is having trouble right now. Try again in a moment.")
}

// plainError is the write lane's `http.Error`: a bare line of text, which is
// what the Swift client's MalkuthError keeps verbatim.
func plainError(w http.ResponseWriter, status int, msg string) {
	http.Error(w, msg, status)
}

func readJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 4<<20)
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}

// limitParam reads ?limit=, capped the way Nantar caps it.
func limitParam(r *http.Request, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ── Logging ───────────────────────────────────────────────────────────────────

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// Flush lets the SSE routes stream through the logging wrapper.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer.
func (sw *statusWriter) Unwrap() http.ResponseWriter { return sw.ResponseWriter }

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s → %d (%s)", r.Method, r.URL.RequestURI(), sw.status, time.Since(start).Round(time.Microsecond))
	})
}
