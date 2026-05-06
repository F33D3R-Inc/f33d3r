package handler

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"

	"github.com/f33d3r/feed-engine/internal/aethyr"
	"github.com/f33d3r/feed-engine/internal/config"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/observ"
	"github.com/f33d3r/feed-engine/internal/middleware"
	"github.com/f33d3r/feed-engine/internal/model"
)

var (
	mdParser  = goldmark.New(
		goldmark.WithExtensions(extension.GFM, extension.Strikethrough),
		goldmark.WithRendererOptions(html.WithHardWraps(), html.WithXHTML()),
	)
	mentionRe    = regexp.MustCompile(`(?m)@([A-Za-z0-9_]{1,50})`)
	hashtagRe    = regexp.MustCompile(`(?m)#([A-Za-z0-9_]{1,100})`)
	mentionOutRe = regexp.MustCompile(`<a href="(/u/[A-Za-z0-9_]+)">(@[A-Za-z0-9_]+)</a>`)
)

func renderMarkdown(src string) template.HTML {
	src = mentionRe.ReplaceAllString(src, "[@$1](/u/$1)")
	src = hashtagRe.ReplaceAllString(src, "[#$1](/search?q=%23$1)")
	var buf bytes.Buffer
	if err := mdParser.Convert([]byte(src), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	out := mentionOutRe.ReplaceAllString(buf.String(), `<a href="$1" class="mention">$2</a>`)
	return template.HTML(out)
}

// Handler holds all shared dependencies.
// sessionEntry caches a resolved user for 30 s to avoid 2 DB queries per request.
type sessionEntry struct {
	user *model.User
	exp  time.Time
}

type Handler struct {
	cfg        *config.Config
	aethyr     *aethyr.Client
	db         *sql.DB
	funcMap    template.FuncMap
	pages      map[string]*template.Template
	partial    *template.Template
	tracksHTML *template.Template
	// Rate limiters — separate buckets for reads, writes, and auth endpoints.
	rlRead  *middleware.RateLimiter // 300/min per IP (pages, feeds)
	rlWrite *middleware.RateLimiter // 60/min per IP (posts, likes, follows)
	rlAuth  *middleware.RateLimiter // 10/min per IP (login, onboard)
	// In-memory presence: handle → last heartbeat time (SSE connection active = online)
	onlineUsers sync.Map
	// sessionCache: raw-token → sessionEntry (30 s TTL, evicted on logout)
	sessionCache sync.Map
	// Caeor media brain client
	caeorURL   string
	httpClient *http.Client
	// Thessalon commerce brain URL (proxied)
	thessalonURL string
}

func New(cfg *config.Config, aethyrClient *aethyr.Client, database *sql.DB) *Handler {
	h := &Handler{
		cfg:        cfg,
		aethyr:     aethyrClient,
		db:         database,
		rlRead:     middleware.NewRateLimiter(300),
		rlWrite:    middleware.NewRateLimiter(60),
		rlAuth:     middleware.NewRateLimiter(40),
		caeorURL:     cfg.CaeorURL,
		thessalonURL: cfg.ThessalonURL,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
	}
	// Legacy upload dirs kept so existing /static/uploads/* URLs keep working
	os.MkdirAll(filepath.Join("web", "static", "uploads", "avatars"), 0755)
	os.MkdirAll(filepath.Join("web", "static", "uploads", "headers"), 0755)
	os.MkdirAll(filepath.Join("web", "static", "uploads", "posts"), 0755)
	// New media dirs — Caeor writes here via shared volume
	os.MkdirAll(filepath.Join("web", "static", "media", "avatars"), 0755)
	os.MkdirAll(filepath.Join("web", "static", "media", "headers"), 0755)
	os.MkdirAll(filepath.Join("web", "static", "media", "posts"), 0755)
	h.loadTemplates()
	return h
}

func (h *Handler) loadTemplates() {
	h.funcMap = template.FuncMap{
		"timeAgo":      TimeAgo,
		"avatarColors": AvatarColors,
		"themeAccent":  ThemeAccent,
		"themeSurface": ThemeSurface,
		"add":          func(a, b int) int { return a + b },
		"pct":          func(f float64) int { return int(f * 100) },
		"safeHTML":     func(s string) template.HTML { return template.HTML(s) },
		"firstChar": func(s string) string {
			r := []rune(s)
			if len(r) == 0 {
				return "?"
			}
			return string(r[0:1])
		},
		"hasPrefix": strings.HasPrefix,
		// dict — build a map[string]interface{} from key/value pairs for
		// passing to shared template partials (Sprint 0 / S0.10).
		"dict": func(values ...interface{}) (map[string]interface{}, error) {
			if len(values)%2 != 0 {
				return nil, fmt.Errorf("dict requires an even number of arguments")
			}
			m := make(map[string]interface{}, len(values)/2)
			for i := 0; i < len(values); i += 2 {
				k, ok := values[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict keys must be strings, got %T", values[i])
				}
				m[k] = values[i+1]
			}
			return m, nil
		},
		"socialLinks": func(raw string) map[string]string {
			m := map[string]string{}
			if raw == "" { return m }
			_ = json.Unmarshal([]byte(raw), &m)
			// strip empty values
			for k, v := range m {
				if v == "" { delete(m, k) }
			}
			return m
		},
		"div": func(a, b int) int {
			if b == 0 { return 0 }
			return a / b
		},
		"mkRange": func(start, end int) []int {
			if end <= start { return nil }
			r := make([]int, end-start)
			for i := range r { r[i] = start + i }
			return r
		},
		"waveBarH": func(i int) int {
			// Deterministic pseudo-random heights for waveform visual
			heights := []int{8,14,22,18,10,28,16,12,24,20,8,30,14,18,26,10,22,16,12,28,20,8,24,18,14,30,10,22,16,28,12,20,18,8,26,14,24,10,30,16,12,22,18,28,14,20,10,24}
			return heights[i % len(heights)]
		},
		"fmtDuration": func(secs int) string {
			if secs == 0 { return "--:--" }
			m := secs / 60
			s := secs % 60
			return fmt.Sprintf("%d:%02d", m, s)
		},
		"realmName": func(r int) string {
			names := []string{"", "Wanderer", "Initiate", "Seeker", "Adept", "Guardian"}
			if r < 1 || r > 5 { return "Wanderer" }
			return names[r]
		},
		"realmNextXP": func(r int) int64 {
			thresholds := []int64{0, 500, 2000, 7500, 20000}
			if r >= 5 { return 20000 }
			return thresholds[r]
		},
		"realmPct": func(xp int64, r int) int {
			thresholds := []int64{0, 500, 2000, 7500, 20000}
			if r >= 5 { return 100 }
			cur := thresholds[r-1]
			next := thresholds[r]
			if next <= cur { return 100 }
			pct := int(float64(xp-cur) / float64(next-cur) * 100)
			if pct < 0 { return 0 }
			if pct > 100 { return 100 }
			return pct
		},
		"markdownHTML": renderMarkdown,
		"bioHTML": func(bio string) template.HTML {
			// Make URLs clickable in profile bios. Escapes everything else.
			urlRe := regexp.MustCompile(`https?://[^\s<>"']+`)
			safe := template.HTMLEscapeString(bio)
			linked := urlRe.ReplaceAllStringFunc(safe, func(u string) string {
				return `<a href="` + u + `" target="_blank" rel="noopener noreferrer nofollow" style="color:var(--accent);text-decoration:underline;text-underline-offset:2px">` + u + `</a>`
			})
			return template.HTML(linked)
		},
	}

	pageFiles := []string{
		"index.html", "explore.html", "search.html", "music.html", "notifications.html",
		"profile.html", "settings.html", "post.html", "bookmarks.html",
		"onboard.html", "onboard_role.html", "onboard_age.html", "onboard_content.html",
		"onboard_verify.html", "onboard_setup.html", "login.html", "wallet.html", "admin.html", "messages.html",
		"shop.html", "kyc.html", "achievements.html", "legal_2257.html",
		"privacy.html", "terms.html", "dmca.html",
		"forgot_password.html", "reset_password.html",
	}
	h.pages = make(map[string]*template.Template, len(pageFiles))
	for _, page := range pageFiles {
		extraFiles := []string{
			filepath.Join("web", "templates", "base.html"),
			filepath.Join("web", "templates", "feed_items.html"),
			filepath.Join("web", "templates", "_state.html"),
			filepath.Join("web", "templates", page),
		}
		if page == "music.html" {
			extraFiles = append(extraFiles, filepath.Join("web", "templates", "track_items.html"))
		}
		tmpl, err := template.New("base.html").Funcs(h.funcMap).ParseFiles(extraFiles...)
		if err != nil {
			log.Fatalf("[templates] %s: %v", page, err)
		}
		h.pages[page] = tmpl
	}

	partial, err := template.New("").Funcs(h.funcMap).ParseFiles(
		filepath.Join("web", "templates", "feed_items.html"),
		filepath.Join("web", "templates", "thread_chain.html"),
		filepath.Join("web", "templates", "_state.html"),
	)
	if err != nil {
		log.Fatalf("[templates] feed_items.html: %v", err)
	}
	h.partial = partial

	tracksHTML, err := template.New("").Funcs(h.funcMap).ParseFiles(
		filepath.Join("web", "templates", "track_items.html"),
	)
	if err != nil {
		log.Fatalf("[templates] track_items.html: %v", err)
	}
	h.tracksHTML = tracksHTML
	log.Printf("[templates] loaded %d page sets + partials", len(h.pages))
}

// Routes returns the complete HTTP mux.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	staticFS := http.FileServer(http.Dir("web/static"))
	mux.Handle("/static/", http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Uploads are content-addressed (UUID filenames) — cache aggressively.
		// JS/CSS use ?v= cache busters — also safe to cache long.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		staticFS.ServeHTTP(w, r)
	})))

	// /media/* → MinIO reverse proxy.
	// Caeor generates URLs like /media/media-derived/posts/xxx/master.m3u8 (MINIO_PUBLIC_BASE=/media).
	// Proxying through feed-engine keeps media on the same origin as the app — eliminates CORS for
	// hls.js and mixed-content errors when the app is served over HTTPS via Caddy.
	{
		minioEndpoint := os.Getenv("MINIO_ENDPOINT")
		if minioEndpoint == "" {
			minioEndpoint = "http://minio:9000"
		}
		if minioTarget, err := url.Parse(minioEndpoint); err == nil {
			mediaProxy := httputil.NewSingleHostReverseProxy(minioTarget)
			mux.Handle("/media/", http.StripPrefix("/media", mediaProxy))
		}
	}

	// Auth — rate limited; no auth cookie required
	mux.HandleFunc("/onboard", h.rlAuth.Limit(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost { h.handleOnboard(w, r); return }
		h.onboardPage(w, r)
	}))
	// Onboard multi-step flow
	mux.HandleFunc("/onboard/role",    h.rlAuth.Limit(h.requireHandle(h.onboardRolePage)))
	mux.HandleFunc("/onboard/age",     h.rlAuth.Limit(h.requireHandle(h.onboardAgePage)))
	mux.HandleFunc("/onboard/content", h.rlAuth.Limit(h.requireHandle(h.onboardContentPage)))
	mux.HandleFunc("/onboard/verify",  h.rlAuth.Limit(h.requireHandle(h.onboardVerifyPage)))
	mux.HandleFunc("/onboard/setup",   h.rlRead.Limit(h.requireHandle(h.onboardSetupPage)))
	mux.HandleFunc("/login", h.rlAuth.Limit(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost { h.handleLogin(w, r); return }
		h.loginPage(w, r)
	}))
	mux.HandleFunc("/logout", h.handleLogout)
	mux.HandleFunc("/forgot-password", h.rlAuth.Limit(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost { h.forgotPasswordSubmit(w, r); return }
		h.forgotPasswordPage(w, r)
	}))
	mux.HandleFunc("/reset-password", h.rlAuth.Limit(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost { h.resetPasswordSubmit(w, r); return }
		h.resetPasswordPage(w, r)
	}))

	// Pages (require auth, read rate limit)
	mux.HandleFunc("/", h.rlRead.Limit(h.requireHandle(h.indexPage)))
	mux.HandleFunc("/explore", h.rlRead.Limit(h.requireHandle(h.explorePage)))
	mux.HandleFunc("/music", h.rlRead.Limit(h.requireHandle(h.musicPage)))
	mux.HandleFunc("/notifications", h.rlRead.Limit(h.requireHandle(h.notificationsPage)))
	mux.HandleFunc("/bookmarks", h.rlRead.Limit(h.requireHandle(h.bookmarksPage)))
	mux.HandleFunc("/profile", h.rlRead.Limit(h.requireHandle(h.profilePage)))
	mux.HandleFunc("/settings", h.rlRead.Limit(h.requireHandle(h.settingsPage)))
	mux.HandleFunc("/post/{id}", h.rlRead.Limit(h.requireHandle(h.postDetailPage)))
	mux.HandleFunc("/u/{handle}", h.rlRead.Limit(h.requireHandle(h.userProfilePage)))
	mux.HandleFunc("/search", h.rlRead.Limit(h.requireHandle(h.searchPage)))
	mux.HandleFunc("/wallet", h.rlRead.Limit(h.requireHandle(h.walletPage)))
	mux.HandleFunc("/admin", h.rlRead.Limit(h.requireHandle(h.adminPage)))
	mux.HandleFunc("/messages", h.rlRead.Limit(h.requireHandle(h.messagesPage)))
	mux.HandleFunc("/messages/", h.rlRead.Limit(h.requireHandle(h.messagesPage)))
	mux.HandleFunc("/shop/{handle}",    h.rlRead.Limit(h.requireHandle(h.shopPage)))
	mux.HandleFunc("/achievements",     h.rlRead.Limit(h.requireHandle(h.achievementsPage)))
	mux.HandleFunc("/kyc",              h.rlRead.Limit(h.requireHandle(h.kycPage)))
	mux.HandleFunc("/kyc/submit",       h.rlWrite.Limit(h.requireHandle(h.kycSubmit)))
	mux.HandleFunc("/kyc/adult/enable", h.rlWrite.Limit(h.requireHandle(h.kycAdultEnable)))
	mux.HandleFunc("/legal/2257",       h.rlRead.Limit(h.legal2257Page))

	// ── Legal / Compliance (public — no auth required) ───────────────────────
	mux.HandleFunc("/privacy",     h.rlRead.Limit(h.privacyPage))
	mux.HandleFunc("/terms",       h.rlRead.Limit(h.termsPage))
	mux.HandleFunc("/dmca",        h.rlRead.Limit(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost { h.dmcaSubmit(w, r); return }
		h.dmcaPage(w, r)
	}))
	mux.HandleFunc("/dmca/submit", h.rlWrite.Limit(h.dmcaSubmit))
	mux.HandleFunc("/admin/dmca",  h.rlRead.Limit(h.requireHandle(h.dmcaAdminQueue)))
	// ── GDPR / CCPA ──────────────────────────────────────────────────────────
	mux.HandleFunc("/api/data/delete", h.rlWrite.Limit(h.requireHandle(h.requestDataDeletion)))
	mux.HandleFunc("/api/data/export", h.rlRead.Limit(h.requireHandle(h.requestDataExport)))

	// Vovin WebSocket push — real-time message delivery (no write rate limit; read-only subscription)
	mux.HandleFunc("/vovin/v1/push/", h.requireHandle(h.vovinWSProxy))
	// Vovin proxy — forward authenticated requests to aethyr-msg
	mux.HandleFunc("/vovin/", h.rlWrite.Limit(h.requireHandle(h.vovinProxy)))
	// Ain Soph proxy — ETHRA/AET wallet + governance (injects PIAL identity header)
	mux.HandleFunc("/ainsoph/", h.rlRead.Limit(h.requireHandle(h.ainSophProxy)))
	// Verity proxy — KYC/compliance decisions (injects PIAL identity header)
	mux.HandleFunc("/verity/", h.rlRead.Limit(h.requireHandle(h.verityProxy)))
	// Aethyr Ledger proxy — AET settlement fabric (write rate limited)
	mux.HandleFunc("/ledger/", h.rlWrite.Limit(h.requireHandle(h.ledgerProxy)))
	// Thessalon proxy — subscriptions, PPV, tips (write rate limited)
	mux.HandleFunc("/thessalon/", h.rlWrite.Limit(h.requireHandle(h.thessalonProxy)))

	// HTMX partials (write rate limit)
	mux.HandleFunc("/feed", h.rlRead.Limit(h.feedPartial))
	mux.HandleFunc("/feed/item/like", h.rlWrite.Limit(h.likeAction))
	mux.HandleFunc("/feed/item/save", h.rlWrite.Limit(h.saveAction))
	mux.HandleFunc("/feed/item/repost", h.rlWrite.Limit(h.repostAction))
	mux.HandleFunc("/feed/item/dislike", h.rlWrite.Limit(h.dislikeAction))
	mux.HandleFunc("/profile/save", h.rlWrite.Limit(h.saveProfile))
	mux.HandleFunc("/music/tracks", h.rlRead.Limit(h.tracksPartial))

	// User lists
	mux.HandleFunc("/api/user/", h.rlRead.Limit(h.userListAPI))

	// JSON API (write rate limit on mutations)
	mux.HandleFunc("/api/post", h.rlWrite.Limit(h.createPost))
	mux.HandleFunc("/api/post/delete", h.rlWrite.Limit(h.deletePost))
	mux.HandleFunc("/api/post/edit",   h.rlWrite.Limit(h.editPost))
	mux.HandleFunc("/api/post/thread", h.rlWrite.Limit(h.createThread))
	mux.HandleFunc("/api/thread/{id}", h.rlRead.Limit(h.requireHandle(h.threadChainAPI)))
	mux.HandleFunc("/api/poll", h.rlWrite.Limit(h.requireHandle(h.createPoll)))
	mux.HandleFunc("/api/poll/vote", h.rlWrite.Limit(h.requireHandle(h.castPollVote)))
	// Creator
	mux.HandleFunc("/api/creator/enable", h.rlWrite.Limit(h.requireHandle(h.enableCreatorAPI)))
	mux.HandleFunc("/api/subscribe", h.rlWrite.Limit(h.requireHandle(h.subscribeToCreator)))
	mux.HandleFunc("/api/unsubscribe", h.rlWrite.Limit(h.requireHandle(h.unsubscribeFromCreator)))
	mux.HandleFunc("/api/creator/plans", h.rlRead.Limit(h.requireHandle(h.getCreatorPlans)))
	mux.HandleFunc("/api/track/like", h.rlWrite.Limit(h.trackLikeAction))
	mux.HandleFunc("/api/track/play", h.rlWrite.Limit(h.trackPlayAction))
	mux.HandleFunc("/api/track/delete", h.rlWrite.Limit(h.requireHandle(h.deleteTrackAPI)))
	mux.HandleFunc("/api/reply", h.rlWrite.Limit(h.replyAPI))
	mux.HandleFunc("/api/follow", h.rlWrite.Limit(h.followAPI))
	mux.HandleFunc("/api/bookmark", h.rlWrite.Limit(h.bookmarkAPI))
	mux.HandleFunc("/api/feedback", h.rlWrite.Limit(h.feedbackAPI))
	mux.HandleFunc("/api/notifications/read-all", h.rlWrite.Limit(h.requireHandle(h.markNotificationsRead)))
	mux.HandleFunc("/api/notifications/count",    h.rlRead.Limit(h.requireHandle(h.notificationsCount)))
	// SSE — real-time push for notifications + wallet balance (no polling, no JSON)
	mux.HandleFunc("/api/events", h.requireHandle(h.sseEvents))
	mux.HandleFunc("/api/online/", h.rlRead.Limit(h.userOnlineStatus))
	mux.HandleFunc("/api/report", h.rlWrite.Limit(h.requireHandle(h.reportContent)))
	mux.HandleFunc("/api/admin/report/resolve",  h.rlWrite.Limit(h.requireHandle(h.resolveReport)))
	mux.HandleFunc("/api/admin/set-role",        h.rlWrite.Limit(h.requireHandle(h.adminSetRole)))
	mux.HandleFunc("/api/admin/inject-aet",      h.rlWrite.Limit(h.requireHandle(h.adminInjectAET)))
	mux.HandleFunc("/api/admin/reset-password",  h.rlWrite.Limit(h.requireHandle(h.adminResetPassword)))
	mux.HandleFunc("/api/admin/set-email",       h.rlWrite.Limit(h.requireHandle(h.adminSetEmail)))
	mux.HandleFunc("/api/users/search", h.rlRead.Limit(h.mentionSearchAPI))
	mux.HandleFunc("/api/health", h.healthAPI)

	// Prometheus metrics — scraped by infra/observability/prometheus.yml.
	// No auth: brain metrics are not sensitive and the endpoint is bound
	// to the internal network. See Sprint 0 / S0.2.
	mux.Handle("/metrics", observ.MetricsHandler())

	// File uploads (write rate limit)
	mux.HandleFunc("/upload/avatar", h.rlWrite.Limit(h.uploadAvatar))
	mux.HandleFunc("/upload/header", h.rlWrite.Limit(h.uploadHeader))
	mux.HandleFunc("/upload/track", h.rlWrite.Limit(h.uploadTrack))
	mux.HandleFunc("/upload/post-media", h.rlWrite.Limit(h.requireHandle(h.uploadPostMedia)))

	// Apply CSRF check globally then return
	return middleware.CSRF(mux)
}

// requireHandle redirects to /login if no handle cookie is set.
func (h *Handler) requireHandle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if HandleFromCookie(r) == "" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// ── Session-aware user resolution ────────────────────────────────────────────

func (h *Handler) userFromRequest(w http.ResponseWriter, r *http.Request) *model.User {
	tok := GetSessionToken(r)

	// ── Prefer session token (new auth) ─────────────────────────────────────
	if h.db != nil && tok != "" && !strings.HasPrefix(tok, "__handle__") {
		// Fast path: check in-memory cache first (30 s TTL)
		if v, ok := h.sessionCache.Load(tok); ok {
			if e := v.(sessionEntry); time.Now().Before(e.exp) {
				return e.user
			}
			h.sessionCache.Delete(tok)
		}

		// Slow path: DB lookup (2 queries)
		if uid, _ := dbpkg.GetUserIDFromSession(h.db, tok); uid != "" {
			if u, err := dbpkg.GetUserByID(h.db, uid); err == nil && u != nil {
				if u.PIALID == "" {
					if pid := dbpkg.BootstrapPIAL(h.db, u.ID, u.Handle); pid != "" {
						u.PIALID = pid
					}
				}
				h.sessionCache.Store(tok, sessionEntry{user: u, exp: time.Now().Add(30 * time.Second)})
				return u
			}
		}
	}

	// ── Legacy handle cookie — READ ONLY, never auto-create ──────────────────
	handle := HandleFromCookie(r)
	// Reject invalid or suspiciously long handles (hashes, etc.)
	if handle == "" || len(handle) > 30 || !handleRe.MatchString(handle) {
		return DemoUser()
	}

	// Legacy path also uses cache (keyed on handle token)
	legacyKey := "__handle__" + handle
	if v, ok := h.sessionCache.Load(legacyKey); ok {
		if e := v.(sessionEntry); time.Now().Before(e.exp) {
			return e.user
		}
		h.sessionCache.Delete(legacyKey)
	}

	if h.db != nil {
		if u, err := dbpkg.GetUserByHandle(h.db, handle); err == nil && u != nil {
			if u.PIALID == "" {
				if pid := dbpkg.BootstrapPIAL(h.db, u.ID, u.Handle); pid != "" {
					u.PIALID = pid
				} else {
					log.Printf("[pial] auto-bootstrap failed for account %s", u.ID)
				}
			}
			u.InterestVector    = randomVector(8)
			u.RecentContentIDs  = []string{}
			u.CreatorAffinities = map[string]float32{}
			u.IsColdStart       = u.PostCount == 0
			u.SafetyEpsilon     = SafetyEpsilon(u.ContentSetting)
			if u.ThemeID == ""        { u.ThemeID = "void" }
			if u.Tier == ""           { u.Tier = "free" }
			if u.ContentSetting == "" { u.ContentSetting = "default" }
			h.sessionCache.Store(legacyKey, sessionEntry{user: u, exp: time.Now().Add(30 * time.Second)})
			return u
		}
	}

	u := DemoUser()
	u.Handle = handle
	return u
}

func (h *Handler) render(w http.ResponseWriter, page string, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl, ok := h.pages[page]
	if !ok {
		http.Error(w, "unknown page: "+page, http.StatusInternalServerError)
		return
	}
	if err := tmpl.ExecuteTemplate(w, "base.html", data); err != nil {
		log.Printf("[render] %s: %v", page, err)
	}
}

// ── Legal ─────────────────────────────────────────────────────────────────────

func (h *Handler) legal2257Page(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	h.render(w, "legal_2257.html", map[string]interface{}{
		"User":       user,
		"Title":      "18 U.S.C. § 2257 Compliance · F33D3R",
		"SessionID":  uuid.New().String(),
		"ShowScores": h.cfg.ShowScores,
		"Themes":     ThemesWithActive(user.ThemeID),
	})
}
