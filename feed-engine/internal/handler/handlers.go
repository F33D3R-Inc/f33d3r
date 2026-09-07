package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
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
	"github.com/lib/pq"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"

	"github.com/f33d3r/feed-engine/internal/aethyr"
	"github.com/f33d3r/feed-engine/internal/auralis"
	"github.com/f33d3r/feed-engine/internal/config"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/jung"
	"github.com/f33d3r/feed-engine/internal/manhattan"
	"github.com/f33d3r/feed-engine/internal/middleware"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/nexus"
	"github.com/f33d3r/feed-engine/internal/observ"
	"github.com/f33d3r/feed-engine/internal/realm"
	"github.com/f33d3r/feed-engine/internal/registry"
	"github.com/f33d3r/feed-engine/internal/rss"
	"github.com/f33d3r/feed-engine/internal/sitra"
	"github.com/f33d3r/feed-engine/internal/sports"
)

var (
	mdParser = goldmark.New(
		goldmark.WithExtensions(extension.GFM, extension.Strikethrough),
		goldmark.WithRendererOptions(html.WithHardWraps(), html.WithXHTML()),
	)
	mentionRe    = regexp.MustCompile(`(?m)@([A-Za-z0-9_]{1,50})`)
	hashtagRe    = regexp.MustCompile(`(?m)#([A-Za-z0-9_]{1,100})`)
	cashtagRe    = regexp.MustCompile(`(?m)\$([A-Z]{1,5})\b`)
	mentionOutRe = regexp.MustCompile(`<a href="(/[A-Za-z0-9_]+)">(@[A-Za-z0-9_]+)</a>`)
	// Matches anchor tags where the visible text IS a raw URL — goldmark GFM autolinks produce these.
	// We replace the link text with a shortened display version; the href stays untouched.
	urlAnchorRe = regexp.MustCompile(`<a href="(https?://[^"]+)">(https?://[^<]+)</a>`)
)

// shortenURL strips the protocol and truncates long paths for display.
// "https://aljazeera.com/news/2026/5/16/long-slug" → "aljazeera.com/news/2026/5/16..."
func shortenURL(raw string) string {
	// Strip scheme
	display := raw
	if strings.HasPrefix(display, "https://") {
		display = display[8:]
	} else if strings.HasPrefix(display, "http://") {
		display = display[7:]
	}
	// Strip trailing slash for clean display
	display = strings.TrimRight(display, "/")
	const maxLen = 30
	if len(display) > maxLen {
		display = display[:maxLen] + "…"
	}
	return display
}

func renderMarkdown(src string) template.HTML {
	src = mentionRe.ReplaceAllString(src, "[@$1](/$1)")
	src = hashtagRe.ReplaceAllString(src, "[#$1](/tag/$1)")
	src = cashtagRe.ReplaceAllString(src, "[$$1](/stocks/$1)")
	var buf bytes.Buffer
	if err := mdParser.Convert([]byte(src), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	// Style @mention links
	out := mentionOutRe.ReplaceAllString(buf.String(), `<a href="$1" class="mention">$2</a>`)
	// Style $TICKER cashtag links
	out = regexp.MustCompile(`<a href="(/stocks/[A-Z]{1,5})">\$([A-Z]{1,5})</a>`).ReplaceAllString(
		out, `<a href="$1" class="cashtag">$$$2</a>`)

	// Shorten raw URL display text — keep href intact, replace visible label
	out = urlAnchorRe.ReplaceAllStringFunc(out, func(match string) string {
		groups := urlAnchorRe.FindStringSubmatch(match)
		if len(groups) < 3 {
			return match
		}
		href := groups[1]
		return `<a href="` + href + `" class="post-link" target="_blank" rel="noopener noreferrer nofollow">` + template.HTMLEscapeString(shortenURL(href)) + `</a>`
	})
	return template.HTML(out)
}

// composeHighlightRe matches the same @mention / #hashtag / $CASHTAG tokens the post
// renderer linkifies. Kept in sync with mentionRe/hashtagRe/cashtagRe.
var composeHighlightRe = regexp.MustCompile(`@[A-Za-z0-9_]{1,50}|#[A-Za-z0-9_]{1,100}|\$[A-Z]{1,5}\b`)

// highlightComposeText is the server-rendered highlight backdrop for the compose box.
// It wraps @/#/$ tokens in the SAME styled links the post renderer uses (/{handle}, /tag/,
// /stocks/ with .mention/.hashtag/.cashtag classes) over HTML-escaped RAW text.
// Unlike renderMarkdown it runs NO markdown conversion, so every character, space and
// newline is preserved — the output aligns one-to-one with the textarea behind which
// it is drawn. FA: the server owns this render; the browser only swaps the fragment.
func highlightComposeText(text string) template.HTML {
	var b strings.Builder
	last := 0
	for _, loc := range composeHighlightRe.FindAllStringIndex(text, -1) {
		b.WriteString(template.HTMLEscapeString(text[last:loc[0]]))
		tok := text[loc[0]:loc[1]]
		body := tok[1:]
		var href, class string
		switch tok[0] {
		case '@':
			href, class = "/"+body, "mention"
		case '#':
			href, class = "/tag/"+body, "hashtag"
		case '$':
			href, class = "/stocks/"+body, "cashtag"
		}
		b.WriteString(`<a href="` + href + `" class="` + class + `">` + template.HTMLEscapeString(tok) + `</a>`)
		last = loc[1]
	}
	b.WriteString(template.HTMLEscapeString(text[last:]))
	return template.HTML(b.String())
}

// Handler holds all shared dependencies.
// sessionEntry caches a resolved user for 30 s to avoid 2 DB queries per request.
type sessionEntry struct {
	user *model.User
	exp  time.Time
}

type Handler struct {
	cfg      *config.Config
	aethyr   *aethyr.Client
	db       *sql.DB
	funcMap  template.FuncMap
	assetVer string // cache-busting ?v= stamp, hashed from web/static/{css,js} at boot
	pages    map[string]*template.Template
	// shellPages names the pages whose shell-class block renders non-empty: they
	// draw a different Shell (the auth pages) and can never be delivered into the
	// standing one, so a Playground request for them is answered with HX-Redirect.
	shellPages map[string]bool
	partial    *template.Template
	tracksHTML *template.Template
	atlas      *template.Template // Facet Atlas dev page (GET /_atlas)
	// Rate limiters — separate buckets for reads, writes, and auth endpoints.
	rlRead  *middleware.RateLimiter // 300/min per IP (pages, feeds)
	rlWrite *middleware.RateLimiter // 60/min per IP (posts, likes, follows)
	rlAuth  *middleware.RateLimiter // 10/min per IP (login, onboard)
	// rlAccount meters credential and code guesses per target account, on top
	// of rlAuth's per-IP budget: a distributed guess against one handle is
	// invisible to a per-IP limiter and lands squarely on this one.
	rlAccount *middleware.RateLimiter // 20/min per account (login, 2FA, reset codes)
	// Number resolution and contact initiation get their own buckets, separate
	// from rlRead/rlWrite: a Number's 60-bit keyspace is only safe if guessing it
	// is metered independently of ordinary reading and posting.
	rlNumberResolve *middleware.RateLimiter // 20/min per IP (Number lookups)
	rlContactInit   *middleware.RateLimiter // 10/min per IP (contact + key bundle)
	// In-memory presence: handle → last heartbeat time (SSE connection active = online)
	onlineUsers sync.Map
	// sessionCache: raw-token → sessionEntry (30 s TTL, evicted on logout)
	sessionCache sync.Map
	// Caeor media brain (images) + Transcoding brain (video HLS)
	caeorURL       string
	transcodingURL string
	httpClient     *http.Client
	// Separate client for large video uploads — no 30 s timeout
	videoClient *http.Client
	// Themis — unified commerce + Bitcoin marketplace brain
	themisURL string
	// Alexandria hypermedia library brain URL (content catalog + dual-write)
	alexandriaURL string
	// NEXUS multi-persona identity client (proxies to Verity)
	nexusClient *nexus.Client
	// RSS right-rail cache — polled every 5 min in background
	rssCache    *rss.Cache
	sportsCache *sports.Cache
	// Sitra Achra — Kafka/Redpanda event backbone (fire-and-forget)
	sitra *sitra.Producer
	// Manhattan — the naming plane. Resolves a public name to the node behind
	// it. Deliberately a client and not a database handle: this brain reaches
	// another brain's entities by name, never by joining into its tables.
	manhattan *manhattan.Client
	// registry — registry-brain, the handle authority. A handle is allocated
	// there and nowhere else; this brain asks, it does not decide.
	registry *registry.Client
	// auralis — the Frequencies brain. Owns every Frequency; this brain renders.
	auralis *auralis.Client
}

func New(cfg *config.Config, aethyrClient *aethyr.Client, database *sql.DB) *Handler {
	h := &Handler{
		cfg:       cfg,
		aethyr:    aethyrClient,
		db:        database,
		manhattan: manhattan.New(cfg.ManhattanURL),
		registry:  registry.New(cfg.RegistryBrainURL),
		auralis:   auralis.New(cfg.AuralisURL, cfg.InternalAPIKey, cfg.AuralisTimeout),
		rlRead:    middleware.NewRateLimiter(300),
		rlWrite:   middleware.NewRateLimiter(60),
		rlAuth:    middleware.NewRateLimiter(40),
		rlAccount: middleware.NewRateLimiter(20),

		rlNumberResolve: middleware.NewRateLimiter(20),
		rlContactInit:   middleware.NewRateLimiter(10),
		caeorURL:        cfg.CaeorURL,
		transcodingURL:  cfg.TranscodingURL,
		themisURL:       cfg.ThemisURL,
		alexandriaURL:   cfg.AlexandriaURL,
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		videoClient:     &http.Client{Timeout: 2 * time.Hour},
		nexusClient:     nexus.NewClient(cfg.VerityURL, cfg.InternalAPIKey),
		rssCache:        rss.NewCache(),
		sitra:           sitra.New(),
	}
	// The avatar ring: the author strip says who is hosting a Frequency right
	// now, and the mappers that build it read this cache rather than the brain.
	useFrequencyPresence(h.auralis)
	// Legacy upload dirs kept so existing /static/uploads/* URLs keep working
	os.MkdirAll(filepath.Join("web", "static", "uploads", "avatars"), 0755)
	os.MkdirAll(filepath.Join("web", "static", "uploads", "headers"), 0755)
	os.MkdirAll(filepath.Join("web", "static", "uploads", "posts"), 0755)
	os.MkdirAll(filepath.Join("web", "static", "uploads", "voice"), 0755)
	// New media dirs — Caeor writes here via shared volume
	os.MkdirAll(filepath.Join("web", "static", "media", "avatars"), 0755)
	os.MkdirAll(filepath.Join("web", "static", "media", "headers"), 0755)
	os.MkdirAll(filepath.Join("web", "static", "media", "posts"), 0755)
	// TUS resumable upload temp dir
	os.MkdirAll(tusTempDir, 0755)
	// Sports lane — ONE poller for the platform, one provider per league so each
	// league keeps its own cadence. The lane is always on: NewProviders never
	// returns an empty set (Big Balls when BBS_API is set, ESPN otherwise), and
	// a misconfiguration is logged while the keyless provider keeps the cards
	// up. Nothing in configuration can switch this off.
	sportsProviders, sportsErr := sports.NewProviders(cfg.SportsProvider, cfg.SportsAPIKey, cfg.SportsBaseURL)
	if sportsErr != nil {
		log.Printf("[sports] %v", sportsErr)
	}
	if len(sportsProviders) > 0 {
		log.Printf("[sports] lane on — provider %s, %d leagues", sportsProviders[0].Name(), len(sportsProviders))
	}
	h.sportsCache = sports.NewCache(sportsProviders...)

	h.loadTemplates()
	h.rssCache.Start(context.Background(), 5*time.Minute)
	// The mutation sink must be registered before the poller's first refresh, so
	// no state change can slip past the stream.
	h.sportsCache.OnMutation(h.sportsMutation)
	h.sportsCache.Start(context.Background())

	// Wire the security alerter — fires on every critical/high security event.
	// If SECURITY_ALERT_EMAIL is configured, an email is sent immediately.
	InitSecurityAlerter(func(eventType, severity, ip, details string) {
		if cfg.SecurityAlertEmail == "" {
			return
		}
		subject := "[F33D3R Security Alert] " + severity + ": " + eventType
		body := "<h2>Security Alert</h2>" +
			"<p><strong>Event:</strong> " + eventType + "</p>" +
			"<p><strong>Severity:</strong> " + severity + "</p>" +
			"<p><strong>IP:</strong> " + ip + "</p>" +
			"<p><strong>Details:</strong> <pre>" + details + "</pre></p>"
		h.sendEmail(cfg.SecurityAlertEmail, subject, body)
	})

	return h
}

// templateHandle reads a handle out of a template argument.
//
// Data reaches Facets as often through map[string]interface{} — a DB row, a
// dict built by a caller — as through a typed struct, so an argument arrives
// with static type interface{} and a strictly typed func(string) would fail the
// whole page render. Anything that is not a handle is an error the renderer
// reports rather than a ring quietly missing from one surface; a nil or empty
// value is not an error, because "no handle" is the realm floor.
func templateHandle(fn string, v interface{}) (string, error) {
	switch h := v.(type) {
	case nil:
		return "", nil
	case string:
		return h, nil
	case template.HTML:
		return string(h), nil
	default:
		return "", fmt.Errorf("%s: need a handle string, got %T", fn, v)
	}
}

// templateDict and templateAdd are the two shape helpers every Facet partial
// reaches for. They are named funcs rather than closures inside the funcMap so a
// test that parses a handful of partials can register the SAME implementations
// the server registers, instead of a lookalike that can drift from it.

// templateDict builds a map from key/value pairs, for passing an explicit
// dataset to a shared partial.
func templateDict(values ...interface{}) (map[string]interface{}, error) {
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
}

// templateAdd is integer addition, used for 1-based ranks in ranged partials.
func templateAdd(a, b int) int { return a + b }

// pageFiles is the set of full-page templates. A page absent from this list is a
// page h.render answers "unknown page: X.html" with — a hard 500 on a route that
// looks completely finished everywhere else.
//
// That has now happened three times: stocks.html (/stocks/{ticker}),
// works.html (/works) and deactivated.html (/deactivated) each shipped rendered
// but unregistered, the last two straight past a comment left here explaining
// the first. A list a person has to remember to update is not a mechanism, and
// no amount of care makes it one.
//
// So it is checked by machine instead: TestEveryRenderedPageIsRegistered walks
// every h.render call in this package and fails on a page that is missing here,
// and TestEveryRegisteredPageIsRendered fails on an entry nothing renders. That
// pair is the actual fix; these lines are only its data. Add a page here when
// you add a render of it, and the test will tell you by name if you forget.
var pageFiles = []string{
	"index.html", "explore.html", "search.html", "music.html", "creator_dashboard.html", "creator_verification_required.html", "creator_welcome.html", "creator_setup.html", "seller_dashboard_market.html", "buyer_library.html", "creator_bulk_upload.html", "marketplace.html", "notifications.html",
	"profile.html", "followers.html", "following.html", "settings.html", "bookmarks.html",
	"messages.html",
	"onboard.html", "login.html", "wallet.html", "admin.html",
	"shop.html", "kyc.html", "achievements.html", "legal_2257.html",
	"privacy.html", "terms.html", "dmca.html",
	"forgot_password.html", "backup_codes_setup.html",
	"settings/personas.html",
	"settings/add_account.html",
	"analytics.html",
	"tag.html",
	"lists.html",
	"list_detail.html",
	"org_panel.html",
	"articles.html", "article_editor.html", "article.html",
	"vision_camera.html",
	"visions.html",
	"works.html",
	"works_detail.html",
	"deactivated.html",
	"live.html",
	"stocks.html",
	"sports_hub.html",
	"sports_game.html",
	"frequency.html",
	"frequencies.html",
}

func (h *Handler) loadTemplates() {
	if h.assetVer == "" {
		h.assetVer = assetVersion("web/static")
		log.Printf("[assets] version %s", h.assetVer)
	}
	h.funcMap = template.FuncMap{
		"assetVer":           func() string { return h.assetVer },
		"celebrationClass":   h.celebrationClass,
		"currentCelebration": h.currentCelebrationName,
		"timeAgo":            TimeAgo,
		"derefTime": func(t *time.Time) time.Time {
			if t == nil {
				return time.Time{}
			}
			return *t
		},
		// isRealUser — nil-safe "logged in with a real account" test. Accepts
		// interface{} so a missing/nil/typed-nil .User can never panic the
		// template (Go's `and` is NOT short-circuit: `and .User (ne .User.ID …)`
		// dereferences .User even when it is nil). The anon viewer is the
		// non-nil demo_user sentinel, which this correctly treats as logged-out.
		"isRealUser": func(v interface{}) bool {
			u, ok := v.(*model.User)
			return ok && u != nil && u.ID != "" && u.ID != "demo_user"
		},
		// avatarColors / firstChar take an interface{} for the same reason
		// realmRing does: they are the funcs the ONE avatar Facet runs, they are
		// reached with data that arrives out of DB-row maps as often as out of
		// structs, and a type error inside the avatar Facet does not lose a
		// ring — it fails the whole page.
		"avatarColors": func(v interface{}) ([2]string, error) {
			seed, err := templateHandle("avatarColors", v)
			if err != nil {
				return [2]string{}, err
			}
			return AvatarColors(seed), nil
		},
		"themeAccent":  ThemeAccent,
		"themeSurface": ThemeSurface,
		"add":          templateAdd,
		"truncate":     truncate,
		"pct":          func(f float64) int { return int(f * 100) },
		"safeHTML":     func(s string) template.HTML { return template.HTML(s) },
		"firstChar": func(v interface{}) (string, error) {
			str, err := templateHandle("firstChar", v)
			if err != nil {
				return "", err
			}
			r := []rune(str)
			if len(r) == 0 {
				return "?", nil
			}
			return string(r[0:1]), nil
		},
		"hasPrefix":  strings.HasPrefix,
		"trimPrefix": strings.TrimPrefix,
		// dict — build a map[string]interface{} from key/value pairs for
		// passing to shared template partials (Sprint 0 / S0.10).
		"dict": templateDict,
		"socialLinks": func(raw string) map[string]string {
			m := map[string]string{}
			if raw == "" {
				return m
			}
			_ = json.Unmarshal([]byte(raw), &m)
			// strip empty values
			for k, v := range m {
				if v == "" {
					delete(m, k)
				}
			}
			return m
		},
		"div": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"mkRange": func(start, end int) []int {
			if end <= start {
				return nil
			}
			r := make([]int, end-start)
			for i := range r {
				r[i] = start + i
			}
			return r
		},
		"waveBarH": func(i int) int {
			// Deterministic pseudo-random heights for waveform visual
			heights := []int{8, 14, 22, 18, 10, 28, 16, 12, 24, 20, 8, 30, 14, 18, 26, 10, 22, 16, 12, 28, 20, 8, 24, 18, 14, 30, 10, 22, 16, 28, 12, 20, 18, 8, 26, 14, 24, 10, 30, 16, 12, 22, 18, 28, 14, 20, 10, 24}
			return heights[i%len(heights)]
		},
		"fmtDuration": func(secs float32) string {
			if secs == 0 {
				return ""
			}
			m := int(secs) / 60
			s := int(secs) % 60
			return fmt.Sprintf("%d:%02d", m, s)
		},
		// realmRing / realmOf — THE ring authority for the server renderer.
		//
		// The avatar Facet calls realmRing with the handle it already has; no
		// caller passes a realm in, so no caller can pass a wrong one or forget
		// to pass one at all. realmOf is the same answer as a number, for the
		// badge and progress bar that must agree with the ring beside them.
		// Backed by realm.Index: an in-memory lookup, no query per row, and a
		// single defined default for unknown or out-of-scale values.
		"realmRing": func(v interface{}) (string, error) {
			h, err := templateHandle("realmRing", v)
			if err != nil {
				return "", err
			}
			return realm.RingClass(h), nil
		},
		"realmOf": func(v interface{}) (int, error) {
			h, err := templateHandle("realmOf", v)
			if err != nil {
				return 0, err
			}
			return realm.Of(h), nil
		},
		"realmName": realm.RealmName,
		// The admin surface's one-line answer to "where does this standing come
		// from", decided in Go and interpolated by the facet.
		"realmStanding": realmStandingLabel,
		"realmNextXP":   realm.NextThreshold,
		"realmPct":      realm.Progress,
		"markdownHTML":  renderMarkdown,
		"replyPreviewLabel": func(handles []string, count int) string {
			if len(handles) == 0 || count == 0 {
				return ""
			}
			switch len(handles) {
			case 1:
				return "@" + handles[0] + " replied"
			case 2:
				return "@" + handles[0] + " and @" + handles[1] + " replied"
			default:
				others := count - len(handles)
				if others > 0 {
					return fmt.Sprintf("@%s, @%s and %d others replied", handles[0], handles[1], others)
				}
				return "@" + handles[0] + ", @" + handles[1] + " and @" + handles[2] + " replied"
			}
		},
		"todayDate": func() string { return time.Now().Format("2006-01-02") },
		// Analytics helpers
		"fmtNum": func(n int64) string {
			if n >= 1_000_000 {
				return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
			}
			if n >= 1_000 {
				return fmt.Sprintf("%.1fK", float64(n)/1_000)
			}
			return fmt.Sprintf("%d", n)
		},
		"absFloat": func(f float64) float64 {
			if f < 0 {
				return -f
			}
			return f
		},
		"pct64": func(val, max int64) int {
			if max <= 0 {
				return 0
			}
			p := int(float64(val) / float64(max) * 100)
			if p > 100 {
				return 100
			}
			if p < 0 {
				return 0
			}
			return p
		},
		"fmtDuration64": func(secs int64) string {
			if secs == 0 {
				return "0:00"
			}
			m := secs / 60
			s := secs % 60
			return fmt.Sprintf("%d:%02d", m, s)
		},
		"div64": func(a, b float64) int {
			if b == 0 {
				return 0
			}
			p := int(a / b * 100)
			if p > 100 {
				return 100
			}
			if p < 0 {
				return 0
			}
			return p
		},
		"derefF64": func(p *float64) float64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"notNil": func(p *float64) bool { return p != nil },
		"bioHTML": func(bio string) template.HTML {
			// Make URLs clickable in profile bios. Escapes everything else.
			urlRe := regexp.MustCompile(`https?://[^\s<>"']+`)
			safe := template.HTMLEscapeString(bio)
			linked := urlRe.ReplaceAllStringFunc(safe, func(u string) string {
				return `<a href="` + u + `" target="_blank" rel="noopener noreferrer nofollow" style="color:var(--accent);text-decoration:underline;text-underline-offset:2px">` + u + `</a>`
			})
			return template.HTML(linked)
		},
		// signedMediaURL — D-005 NSFW media URL gating.
		// For NSFW media, appends a short-lived HMAC token as ?tok=<token>.
		// For non-NSFW media or when the URL is empty, returns the URL unchanged.
		// pialID is the current viewer's PIAL UUID (available as .User.PIALID in templates).
		"signedMediaURL": signedMediaURLFunc,
	}
	// Facet(sports_*) id builders. A facet id is an address, not decoration: it
	// is built by one function so a typo in markup cannot silently produce a
	// facet the stream can never reach.
	for name, fn := range sportsFacetFuncs() {
		h.funcMap[name] = fn
	}
	// Facet(frequency_*) id builder, same rule.
	h.funcMap["frequencyFacetID"] = frequencyFacetID

	// Glob all EventType partial files — every page and partial set gets them all.
	partialGlob := filepath.Join("web", "templates", "partials", "*.html")
	partialFiles, err := filepath.Glob(partialGlob)
	if err != nil || len(partialFiles) == 0 {
		log.Printf("[templates] warning: no partial files found at %s", partialGlob)
	}

	// Shared base files included in every page template set.
	sharedFiles := []string{
		filepath.Join("web", "templates", "base.html"),
		filepath.Join("web", "templates", "_state.html"),
		filepath.Join("web", "templates", "nexus", "persona-switcher.html"),
	}
	sharedFiles = append(sharedFiles, partialFiles...)

	// missingKey controls what happens when a template references a data key the
	// handler never populated. When TemplateStrict is set we fail loudly
	// ("missingkey=error") so the buffered renderer surfaces a clean 500 and the
	// wiring bug is caught at the source; otherwise we keep the default (render
	// "<no value>") so a single un-set key never hard-fails a page. This is an
	// opt-in tripwire (TEMPLATE_STRICT=1), deliberately independent of DEV_MODE
	// so normal local browsing is not broken by pre-existing data-map drift.
	missingKey := "missingkey=default"
	if h.cfg.TemplateStrict {
		missingKey = "missingkey=error"
	}

	h.pages = make(map[string]*template.Template, len(pageFiles))
	h.shellPages = make(map[string]bool)
	for _, page := range pageFiles {
		files := make([]string, len(sharedFiles))
		copy(files, sharedFiles)
		files = append(files, filepath.Join("web", "templates", page))
		tmpl, err := template.New("base.html").Funcs(h.funcMap).Option(missingKey).ParseFiles(files...)
		if err != nil {
			log.Fatalf("[templates] %s: %v", page, err)
		}
		h.pages[page] = tmpl
		if pageShellClass(tmpl) != "" {
			h.shellPages[page] = true
		}
	}

	// Partial template set — used by HTMX partial endpoints and feed item rendering.
	// persona-switcher.html lives outside partials/ but defines a Facet fragment
	// that a partial endpoint serves, so the partial set needs it by name.
	partialBase := []string{
		filepath.Join("web", "templates", "_state.html"),
		filepath.Join("web", "templates", "nexus", "persona-switcher.html"),
	}
	partialBase = append(partialBase, partialFiles...)
	partial, err := template.New("").Funcs(h.funcMap).Option(missingKey).ParseFiles(partialBase...)
	if err != nil {
		log.Fatalf("[templates] partial set: %v", err)
	}
	h.partial = partial
	h.tracksHTML = partial

	// Facet Atlas — a standalone dev page (its own chrome, not base.html). It
	// includes the partial files so it can render facets through the same set.
	atlasFiles := []string{filepath.Join("web", "templates", "atlas.html")}
	atlasFiles = append(atlasFiles, partialFiles...)
	atlasTmpl, err := template.New("atlas.html").Funcs(h.funcMap).Option(missingKey).ParseFiles(atlasFiles...)
	if err != nil {
		log.Fatalf("[templates] atlas set: %v", err)
	}
	h.atlas = atlasTmpl

	log.Printf("[templates] loaded %d page sets + %d partials", len(h.pages), len(partialFiles))
}

// Routes returns the complete HTTP mux.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	// Register HLS MIME types — Go's stdlib doesn't include these.
	// Without correct types, Firefox (strict nosniff) rejects .ts segments
	// and cannot feed them into MSE, causing complete playback failure.
	_ = mime.AddExtensionType(".m3u8", "application/x-mpegURL")
	_ = mime.AddExtensionType(".ts", "video/mp2t")
	_ = mime.AddExtensionType(".wasm", "application/wasm") // browser WASM crypto core

	// sw.js must be served at the root scope so the service worker can intercept all pages.
	// The file lives at web/static/sw.js but browsers require it at /sw.js.
	mux.HandleFunc("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Service-Worker-Allowed", "/")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Content-Type", "application/javascript")
		http.ServeFile(w, r, filepath.Join("web", "static", "sw.js"))
	})

	// RFC 9116 security contact disclosure.
	mux.HandleFunc("GET /.well-known/security.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFile(w, r, filepath.Join("web", "static", ".well-known", "security.txt"))
	})

	// User media is the one static subtree with per-viewer rules — registered
	// ahead of /static/ so it cannot be read around the wall (see media_gate.go).
	mux.Handle("/static/media/", h.staticMediaHandler())

	// The JSON surface the native clients read; see api_v1.go.
	h.registerAPIV1(mux)

	// Directories do not exist under /static/: FileServer would otherwise
	// render an index of web/static to anyone who asks for a trailing slash.
	staticFS := http.FileServer(middleware.NoDirListing(http.Dir("web/static")))
	mux.Handle("/static/", http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, ".m3u8") || strings.HasSuffix(path, ".ts"):
			// HLS manifests and segments: short cache, NOT immutable.
			// Browsers must be able to re-fetch if the stream changes.
			w.Header().Set("Cache-Control", "public, max-age=3600")
		default:
			// CSS/JS (cache-busted via ?v=), images, fonts — safe to cache long.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		staticFS.ServeHTTP(w, r)
	})))

	// /media/* → MinIO reverse proxy with NSFW token gate (D-005).
	// Caeor generates URLs like /media/media-derived/posts/xxx/master.m3u8 (MINIO_PUBLIC_BASE=/media).
	// Proxying through feed-engine keeps media on the same origin as the app — eliminates CORS for
	// hls.js and mixed-content errors when the app is served over HTTPS via Caddy.
	//
	// Gate: if the media path belongs to an NSFW post, the request must carry a valid
	// ?tok= HMAC-SHA256 token issued for the current viewer's PIAL (see internal/mediatoken).
	// Non-NSFW media is served with no token requirement.
	{
		minioEndpoint := os.Getenv("MINIO_ENDPOINT")
		if minioEndpoint == "" {
			minioEndpoint = "http://minio:9000"
		}
		if minioTarget, err := url.Parse(minioEndpoint); err == nil {
			mediaProxy := httputil.NewSingleHostReverseProxy(minioTarget)
			mux.Handle("/media/", h.mediaGate(http.StripPrefix("/media", mediaProxy)))
		}
	}

	// Auth — GET pages use rlRead (just rendering HTML, no brute-force risk).
	// POST mutations (login attempts, signup) use rlAuth (brute-force protection).
	mux.HandleFunc("GET /onboard", h.rlRead.Limit(h.onboardPage))
	mux.HandleFunc("POST /onboard", h.rlAuth.Limit(h.handleOnboard))
	// Onboard multi-step flow
	// Retired single-step onboarding URLs — redirects only. See onboard.go.
	mux.HandleFunc("GET /onboard/role", h.rlRead.Limit(h.requireHandle(h.onboardAgePage)))
	mux.HandleFunc("GET /onboard/age", h.rlRead.Limit(h.requireHandle(h.onboardAgePage)))
	mux.HandleFunc("GET /onboard/content", h.rlRead.Limit(h.requireHandle(h.onboardAgePage)))
	mux.HandleFunc("GET /onboard/verify", h.rlRead.Limit(h.requireHandle(h.onboardVerifyPage)))
	mux.HandleFunc("GET /onboard/setup", h.rlRead.Limit(h.requireHandle(h.onboardSetupPage)))
	mux.HandleFunc("GET /login", h.rlRead.Limit(h.loginPage))
	mux.HandleFunc("POST /login", h.rlAuth.Limit(h.rlAccount.LimitKeyed(loginAccountKey, h.handleLogin)))
	mux.HandleFunc("/logout", h.handleLogout)
	mux.HandleFunc("GET /deactivated", h.deactivatedPage)
	mux.HandleFunc("GET /backup-codes/setup", h.rlRead.Limit(h.requireHandle(h.backupCodesSetupPage)))
	mux.HandleFunc("GET /forgot-password", h.rlRead.Limit(h.forgotPasswordPage))
	mux.HandleFunc("POST /forgot-password", h.rlAuth.Limit(h.forgotPasswordSubmit))
	mux.HandleFunc("POST /forgot-password/verify", h.rlAuth.Limit(h.rlAccount.LimitKeyed(loginAccountKey, h.forgotPasswordVerifyCode)))
	mux.HandleFunc("POST /forgot-password/reset", h.rlAuth.Limit(h.forgotPasswordResetSubmit))

	// Pages (require auth, read rate limit)
	// "/" is the catch-all that also dispatches the clean root URLs (/{handle},
	// /{handle}/work/{id}, /{handle}/followers|following) — see rootDispatch. It must be
	// the ONLY first-segment-wildcard-equivalent at the root; see the note at its block below.
	mux.HandleFunc("/", h.rlRead.Limit(h.rootDispatch))
	mux.HandleFunc("/explore", h.rlRead.Limit(h.requireHandle(h.explorePage)))
	mux.HandleFunc("/music", h.rlRead.Limit(h.requireHandle(h.musicPage)))
	// Creator Studio
	mux.HandleFunc("GET /create", h.rlRead.Limit(h.requireHandle(h.creatorDashboardPage)))
	// Creator section deep-link routes — full-page render so back/forward and hard refresh work.
	mux.HandleFunc("GET /create/overview", h.rlRead.Limit(h.requireHandle(h.creatorDashboardPage)))
	mux.HandleFunc("GET /create/music", h.rlRead.Limit(h.requireHandle(h.creatorDashboardPage)))
	mux.HandleFunc("GET /create/content", h.rlRead.Limit(h.requireHandle(h.creatorDashboardPage)))
	mux.HandleFunc("GET /create/memberships", h.rlRead.Limit(h.requireHandle(h.creatorDashboardPage)))
	mux.HandleFunc("GET /create/earnings", h.rlRead.Limit(h.requireHandle(h.creatorDashboardPage)))
	mux.HandleFunc("GET /create/audience", h.rlRead.Limit(h.requireHandle(h.creatorDashboardPage)))
	mux.HandleFunc("GET /create/analytics", h.rlRead.Limit(h.requireHandle(h.creatorDashboardPage)))
	mux.HandleFunc("GET /create/settings", h.rlRead.Limit(h.requireHandle(h.creatorDashboardPage)))
	mux.HandleFunc("GET /create/partials/overview", h.rlRead.Limit(h.requireHandle(h.creatorPanelOverview)))
	mux.HandleFunc("GET /create/partials/music", h.rlRead.Limit(h.requireHandle(h.creatorPanelMusic)))
	mux.HandleFunc("GET /create/partials/memberships", h.rlRead.Limit(h.requireHandle(h.creatorPanelMemberships)))
	mux.HandleFunc("GET /create/partials/earnings", h.rlRead.Limit(h.requireHandle(h.creatorPanelEarnings)))
	mux.HandleFunc("GET /create/partials/analytics", h.rlRead.Limit(h.requireHandle(h.creatorPanelAnalytics)))
	mux.HandleFunc("GET /create/partials/content", h.rlRead.Limit(h.requireHandle(h.creatorPanelContent)))
	mux.HandleFunc("GET /create/partials/audience", h.rlRead.Limit(h.requireHandle(h.creatorPanelAudience)))
	mux.HandleFunc("GET /create/partials/settings", h.rlRead.Limit(h.requireHandle(h.creatorPanelSettings)))
	mux.HandleFunc("GET /create/marketplace", h.rlRead.Limit(h.requireHandle(h.sellerDashboardMarketPage)))
	mux.HandleFunc("GET /create/welcome", h.rlRead.Limit(h.requireHandle(h.creatorWelcomePage)))
	mux.HandleFunc("GET /create/setup", h.rlRead.Limit(h.requireHandle(h.creatorSetupPage)))
	mux.HandleFunc("GET /facets/marketplace_listing_form", h.rlRead.Limit(h.requireHandle(h.facetMarketplaceListingForm)))
	mux.HandleFunc("GET /facets/marketplace/feed", h.rlRead.Limit(h.requireHandle(h.facetMarketplaceFeed)))
	// NSFW surface — Adult Creator content only; gated server-side and client-side.
	// /feed?surface=nsfw is handled inside feedPartial with its own 403 gate.
	mux.HandleFunc("/nsfw", h.rlRead.Limit(h.requireHandle(h.nsfwFeedPage)))
	// New surfaces — video feed + coming-soon pages
	mux.HandleFunc("/video", h.rlRead.Limit(h.requireHandle(h.videoFeedPage)))
	// /visions is the Vision lane's Playground. It reads the visions lane, where
	// expiry is enforced in SQL on every read — kind='vision' left the Work lane
	// in migration 0008 and nothing writes it any more.
	mux.HandleFunc("GET /visions", h.rlRead.Limit(h.requireHandle(h.visionsFeedPage)))
	mux.HandleFunc("GET /visions/camera", h.rlRead.Limit(h.requireHandle(h.visionCameraPage)))
	mux.HandleFunc("GET /works", h.worksPage)
	mux.HandleFunc("GET /facets/works/feed", h.facetWorksFeed)
	// ── Live lane — watch surfaces, chat, presence, broadcast ────────────────
	// The watch page is public: a stream plays for a logged-out visitor and only
	// chat is gated. Broadcast, start and end require a real account.
	mux.HandleFunc("GET /live/{id}", h.rlRead.Limit(h.liveWatchPage))
	mux.HandleFunc("GET /live/{id}/broadcast", h.rlRead.Limit(h.requireHandle(h.liveBroadcastPage)))
	mux.HandleFunc("GET /golive", h.rlRead.Limit(h.requireHandle(h.liveGoPage)))
	mux.HandleFunc("POST /live/start", h.rlWrite.Limit(h.requireHandle(h.liveStartHandler)))
	mux.HandleFunc("POST /live/{id}/end", h.rlWrite.Limit(h.requireHandle(h.liveEndHandler)))
	mux.HandleFunc("POST /live/{id}/heartbeat", h.rlRead.Limit(h.liveHeartbeat))
	mux.HandleFunc("POST /live/{id}/chat", h.rlWrite.Limit(h.requireHandle(h.liveChatSend)))
	// Encoder credentials. POST only and owner-gated: the ingest key is answered
	// into this one request and never appears in a URL or an FA Live Fragment.
	mux.HandleFunc("POST /live/{id}/encoder", h.rlWrite.Limit(h.requireHandle(h.liveEncoderReveal)))
	// WebRTC ingest for the phone browser. Same-origin so the publish credential
	// stays on the server; only the SDP handshake passes through this process.
	mux.HandleFunc("POST /live/{id}/whip", h.rlWrite.Limit(h.requireHandle(h.liveWhipPublish)))
	mux.HandleFunc("DELETE /live/{id}/whip/{session}", h.rlWrite.Limit(h.requireHandle(h.liveWhipTeardown)))
	mux.HandleFunc("GET /facets/live/cards", h.rlRead.Limit(h.facetLiveCards))
	// Frequencies — live audio, under the Go Live tab. The object lives in
	// Auralis; these routes render it. Mutations go through POST /events
	// (apiV1FrequencyEvent).
	mux.HandleFunc("GET /frequencies", h.rlRead.Limit(h.frequenciesPage))
	mux.HandleFunc("GET /frequencies/{id}", h.rlRead.Limit(h.requireHandle(h.frequencyPage)))
	mux.HandleFunc("GET /facets/frequency/cards", h.rlRead.Limit(h.facetFrequencyCards))
	mux.HandleFunc("GET /facets/frequency/{id}/{slot}", h.rlRead.Limit(h.facetFrequencySlot))
	// ── Vision lane — the ephemeral compose path ─────────────────────────────
	// One writer for the whole lane. Every artboard choice is a preset key the
	// server resolves; the artboard re-render is a server round trip.
	mux.HandleFunc("GET /facets/vision/composer", h.rlRead.Limit(h.requireHandle(h.facetVisionComposer)))
	mux.HandleFunc("POST /facets/vision/artboard", h.rlRead.Limit(h.requireHandle(h.facetVisionArtboardHandler)))
	mux.HandleFunc("POST /visions", h.rlWrite.Limit(h.requireHandle(h.visionCreate)))
	// ── Vision lane — the read path ──────────────────────────────────────────
	// Ring state, play order and every step of the sequential viewer are decided
	// on the server. One step is one round trip; the browser holds no sequence.
	mux.HandleFunc("GET /facets/vision/rings", h.rlRead.Limit(h.requireHandle(h.facetVisionRings)))
	mux.HandleFunc("GET /facets/vision/ring", h.rlRead.Limit(h.requireHandle(h.facetVisionProfileRing)))
	mux.HandleFunc("GET /facets/vision/reels", h.rlRead.Limit(h.requireHandle(h.facetVisionReels)))
	mux.HandleFunc("GET /facets/vision/viewer", h.rlRead.Limit(h.requireHandle(h.facetVisionViewer)))
	// Contact addresses — public, revocable credentials that resolve to an
	// identity through Manhattan. The address is the capability to open an
	// encrypted channel; the identity behind it never moves.
	mux.HandleFunc("GET /facets/identity/addresses", h.facetContactAddresses)
	mux.HandleFunc("POST /identity/addresses/mint", h.rlWrite.Limit(h.mintContactAddress))
	mux.HandleFunc("POST /identity/addresses/revoke", h.rlWrite.Limit(h.revokeContactAddress))
	mux.HandleFunc("GET /api/contact/{address}", h.rlContactInit.Limit(h.contactKeyBundle))

	// F33D3R Numbers — a speakable, rotatable contact name. Resolving one yields
	// a policy decision and nothing else; key material follows only an allow.
	mux.HandleFunc("GET /facets/identity/numbers", h.rlRead.Limit(h.requireHandle(h.facetNumbers)))
	mux.HandleFunc("POST /identity/numbers/mint", h.rlWrite.Limit(h.requireHandle(h.mintNumber)))
	mux.HandleFunc("POST /identity/numbers/revoke", h.rlWrite.Limit(h.requireHandle(h.revokeNumber)))
	mux.HandleFunc("POST /identity/numbers/policy", h.rlWrite.Limit(h.requireHandle(h.setNumberPolicy)))
	mux.HandleFunc("POST /identity/contact/policy", h.rlWrite.Limit(h.requireHandle(h.setContactPolicy)))
	mux.HandleFunc("POST /identity/contact/capability/mint", h.rlWrite.Limit(h.requireHandle(h.mintContactCapability)))
	mux.HandleFunc("POST /identity/contact/capability/revoke", h.rlWrite.Limit(h.requireHandle(h.revokeContactCapability)))
	mux.HandleFunc("POST /identity/contact/requests/decide", h.rlWrite.Limit(h.requireHandle(h.decideContactRequest)))
	mux.HandleFunc("POST /contact/resolve", h.rlNumberResolve.Limit(h.requireHandle(h.resolveNumber)))
	mux.HandleFunc("POST /contact/keys", h.rlContactInit.Limit(h.requireHandle(h.numberKeyBundle)))
	mux.HandleFunc("POST /contact/start", h.rlContactInit.Limit(h.requireHandle(h.startContactByNumber)))
	mux.HandleFunc("GET /facets/works/replies/{id}", h.facetWorkReplies)
	mux.HandleFunc("GET /facets/works/conversation/{id}", h.facetWorkConversation)
	mux.HandleFunc("GET /facets/work/{id}/editions", h.facetWorkEditions)
	mux.HandleFunc("GET /work/{id}", h.workDetailPage)
	mux.HandleFunc("GET /react-video/{id}", h.rlRead.Limit(h.requireHandle(h.reactVideoStudio)))
	mux.HandleFunc("/communities", h.rlRead.Limit(h.requireHandle(h.comingSoonPage("Communities", "Connect around shared interests. Coming soon."))))
	mux.HandleFunc("/lists", h.rlRead.Limit(h.requireHandle(h.listsPage)))
	mux.HandleFunc("GET /lists/{id}", h.rlRead.Limit(h.requireHandle(h.listDetailPage)))
	mux.HandleFunc("GET /facets/list/create_modal", h.rlRead.Limit(h.requireHandle(h.facetListCreateModal)))
	mux.HandleFunc("GET /facets/list/{id}/add_member_modal", h.rlRead.Limit(h.requireHandle(h.facetListAddMemberModal)))
	mux.HandleFunc("GET /facets/list/{id}/works", h.rlRead.Limit(h.requireHandle(h.facetListWorks)))
	mux.HandleFunc("GET /api/post/{id}/analytics", h.rlRead.Limit(h.requireHandle(h.postAnalyticsPartial)))
	// Video download — streams the watermarked MP4; subscriber-only content gets a forensic watermark.
	mux.HandleFunc("GET /api/post/{id}/download", h.rlRead.Limit(h.requireHandle(h.downloadPost)))
	// Admin: founding creator badge
	mux.HandleFunc("POST /api/admin/grant-founding-creator", h.rlWrite.Limit(h.requireHandle(h.adminGrantFoundingCreator)))
	// Referral system
	mux.HandleFunc("GET /api/referral/code", h.rlRead.Limit(h.requireHandle(h.referralCode)))
	mux.HandleFunc("GET /api/referral/stats", h.rlRead.Limit(h.requireHandle(h.referralStats)))
	mux.HandleFunc("GET /facets/referral_panel", h.rlRead.Limit(h.requireHandle(h.facetReferralPanel)))
	// The old placeholder names for live audio. Frequencies is the product.
	mux.HandleFunc("/spheres", h.rlRead.Limit(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/frequencies", http.StatusMovedPermanently)
	}))
	mux.HandleFunc("/spaces", h.rlRead.Limit(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/frequencies", http.StatusMovedPermanently)
	}))
	mux.HandleFunc("/marketplace", h.rlRead.Limit(h.requireHandle(h.marketplacePage)))
	mux.HandleFunc("/buyer-library", h.rlRead.Limit(h.requireHandle(h.buyerLibraryPage)))
	// Themis, projected as Facets. The browser never receives Themis JSON: each
	// route asks Themis on the viewer's behalf and renders the answer.
	mux.HandleFunc("GET /themis/v1/library", h.rlRead.Limit(h.requireHandle(h.buyerLibraryFacet)))
	mux.HandleFunc("GET /themis/v1/qr/{id}", h.rlRead.Limit(h.requireHandle(h.purchaseQR)))
	mux.HandleFunc("GET /facets/marketplace/purchase/{id}/status", h.rlRead.Limit(h.requireHandle(h.facetPurchaseStatus)))
	mux.HandleFunc("GET /facets/marketplace/seller/stats", h.rlRead.Limit(h.requireHandle(h.facetSellerStats)))
	mux.HandleFunc("GET /facets/marketplace/seller/listings", h.rlRead.Limit(h.requireHandle(h.facetSellerListings)))
	mux.HandleFunc("/notifications", h.rlRead.Limit(h.requireHandle(h.notificationsPage)))
	mux.HandleFunc("GET /facets/notifications", h.rlRead.Limit(h.requireHandle(h.facetNotifications)))
	// ── Gnosis messaging ──────────────────────────────────────────────────────
	mux.HandleFunc("GET /messages", h.rlRead.Limit(h.requireHandle(h.messagesPage)))
	mux.HandleFunc("GET /facets/messages_convos", h.rlRead.Limit(h.requireHandle(h.facetConvoList)))
	mux.HandleFunc("GET /facets/messages_thread", h.rlRead.Limit(h.requireHandle(h.facetThread)))
	mux.HandleFunc("POST /messages/new", h.rlWrite.Limit(h.requireHandle(h.handleCreateConvo)))
	// The second way into a conversation: a F33D3R Number instead of an @handle.
	// Rate-limited as a contact initiation, not as an ordinary write, because it
	// is one — the same limiter /contact/start answers to.
	mux.HandleFunc("POST /messages/new-number", h.rlContactInit.Limit(h.requireHandle(h.handleNumberContact)))
	mux.HandleFunc("GET /facets/messages_requests", h.rlRead.Limit(h.requireHandle(h.facetGnosisContactRequests)))
	mux.HandleFunc("POST /messages/send", h.rlWrite.Limit(h.requireHandle(h.handleSendMessage)))
	mux.HandleFunc("POST /messages/add-member", h.rlWrite.Limit(h.requireHandle(h.handleAddMember)))
	// Gnosis sealed mode (E2EE) — blind relay; server stores/routes only ciphertext.
	mux.HandleFunc("GET /api/gnosis/bootstrap", h.rlRead.Limit(h.requireHandle(h.gnosisBootstrap)))
	mux.HandleFunc("GET /api/gnosis/directory", h.rlRead.Limit(h.requireHandle(h.gnosisDirectory)))
	mux.HandleFunc("POST /api/gnosis/provision", h.rlWrite.Limit(h.requireHandle(h.gnosisProvision)))
	mux.HandleFunc("POST /api/gnosis/send-sealed", h.rlWrite.Limit(h.requireHandle(h.gnosisSendSealed)))
	mux.HandleFunc("/bookmarks", h.rlRead.Limit(h.requireHandle(h.bookmarksPage)))
	mux.HandleFunc("/bookmarks/items", h.rlRead.Limit(h.requireHandle(h.bookmarksItemsPartial)))
	mux.HandleFunc("/profile", h.rlRead.Limit(h.requireHandle(h.profilePage)))
	mux.HandleFunc("/settings", h.rlRead.Limit(h.requireHandle(h.settingsPage)))
	mux.HandleFunc("/post/{id}", h.legacyPostRedirect)
	// ── Clean root URLs ──────────────────────────────────────────────────────────
	// f33d3r.com/{handle}, /{handle}/work/{id}, /{handle}/followers, /{handle}/following.
	// These are NOT registered as first-segment wildcard patterns: Go 1.22's ServeMux
	// panics at boot if a "/{handle}" pattern coexists with any root subtree/literal route
	// ("/static/", "/explore", …), because "/static/work/x" is ambiguous between the route
	// and a user named "static". Instead h.rootDispatch (wired to "/" above) parses the path
	// itself — "/" is always the least-specific pattern, so it never conflicts and every
	// literal route still wins. Handles that collide with a real route are rejected at signup
	// (reservedHandle), so a literal route can never shadow a real user's profile.
	//
	// Legacy /u/{handle}[/...] → permanent redirect to the clean URL (old links stay alive).
	// First segment is the literal "u", so these never conflict with anything.
	mux.HandleFunc("/u/{handle}", h.rlRead.Limit(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/"+r.PathValue("handle"), http.StatusMovedPermanently)
	}))
	mux.HandleFunc("/u/{handle}/followers", h.rlRead.Limit(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/"+r.PathValue("handle")+"/followers", http.StatusMovedPermanently)
	}))
	mux.HandleFunc("/u/{handle}/following", h.rlRead.Limit(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/"+r.PathValue("handle")+"/following", http.StatusMovedPermanently)
	}))
	mux.HandleFunc("/search", h.rlRead.Limit(h.requireHandle(h.searchPage)))
	mux.HandleFunc("/wallet", h.rlRead.Limit(h.requireHandle(h.walletPage)))
	// Wallet section deep-link routes — full-page render so back/forward and hard refresh work.
	mux.HandleFunc("GET /wallet/overview", h.rlRead.Limit(h.requireHandle(h.walletPage)))
	mux.HandleFunc("GET /wallet/send", h.rlRead.Limit(h.requireHandle(h.walletPage)))
	mux.HandleFunc("GET /wallet/history", h.rlRead.Limit(h.requireHandle(h.walletPage)))
	mux.HandleFunc("GET /wallet/governance", h.rlRead.Limit(h.requireHandle(h.walletPage)))
	mux.HandleFunc("GET /wallet/partials/overview", h.rlRead.Limit(h.requireHandle(h.walletPanelOverview)))
	mux.HandleFunc("GET /wallet/partials/history", h.rlRead.Limit(h.requireHandle(h.walletPanelHistory)))
	mux.HandleFunc("GET /wallet/partials/governance", h.rlRead.Limit(h.requireHandle(h.walletPanelGovernance)))
	mux.HandleFunc("GET /wallet/partials/send", h.rlRead.Limit(h.requireHandle(h.walletPanelSend)))
	// Facet Atlas — read-only live catalog of every facet. Gated inside the
	// handler (DEV_MODE or admin); 404 otherwise. Never publicly reachable.
	mux.HandleFunc("GET /_atlas", h.rlRead.Limit(h.atlasPage))
	mux.HandleFunc("POST /_atlas/review", h.rlWrite.Limit(h.atlasReview))
	mux.HandleFunc("/admin", h.rlRead.Limit(h.requireHandle(h.adminPage)))
	mux.HandleFunc("/analytics", h.rlRead.Limit(h.requireHandle(h.analyticsPage)))
	mux.HandleFunc("/admin/analytics", h.rlRead.Limit(h.requireHandle(h.adminUserAnalytics)))
	mux.HandleFunc("/shop/{handle}", h.rlRead.Limit(h.requireHandle(h.shopPage)))
	mux.HandleFunc("/achievements", h.rlRead.Limit(h.requireHandle(h.achievementsPage)))
	mux.HandleFunc("/kyc", h.rlRead.Limit(h.requireHandle(h.kycPage)))
	mux.HandleFunc("/kyc/submit", h.rlWrite.Limit(h.requireHandle(h.kycSubmit)))
	mux.HandleFunc("/kyc/adult/enable", h.rlWrite.Limit(h.requireHandle(h.kycAdultEnable)))
	mux.HandleFunc("POST /kyc/liveness/challenge", h.rlWrite.Limit(h.requireHandle(h.kycLivenessChallenge)))
	mux.HandleFunc("POST /kyc/age-verify", h.rlWrite.Limit(h.requireHandle(h.handleMRZAgeVerify)))
	mux.HandleFunc("GET /kyc/creator/statement", h.rlRead.Limit(h.requireHandle(h.kycCreatorStatement)))
	mux.HandleFunc("POST /kyc/creator/accept", h.rlWrite.Limit(h.requireHandle(h.kycCreatorAccept)))
	mux.HandleFunc("/legal/2257", h.rlRead.Limit(h.legal2257Page))

	// ── Legal / Compliance (public — no auth required) ───────────────────────
	mux.HandleFunc("/privacy", h.rlRead.Limit(h.privacyPage))
	mux.HandleFunc("/terms", h.rlRead.Limit(h.termsPage))
	mux.HandleFunc("/dmca", h.rlRead.Limit(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			h.dmcaSubmit(w, r)
			return
		}
		h.dmcaPage(w, r)
	}))
	mux.HandleFunc("/dmca/submit", h.rlWrite.Limit(h.dmcaSubmit))
	mux.HandleFunc("/admin/dmca", h.rlRead.Limit(h.requireHandle(h.dmcaAdminQueue)))
	// ── GDPR / CCPA ──────────────────────────────────────────────────────────
	mux.HandleFunc("/api/data/delete", h.rlWrite.Limit(h.requireHandle(h.requestDataDeletion)))
	mux.HandleFunc("/api/data/export", h.rlRead.Limit(h.requireHandle(h.requestDataExport)))

	// Ain Soph proxy — ETHRA/AET wallet + governance (injects PIAL identity header)
	mux.HandleFunc("/ainsoph/", h.rlRead.Limit(h.requireHandle(h.ainSophProxy)))
	// Verity proxy — KYC/compliance decisions (injects PIAL identity header)
	mux.HandleFunc("/verity/", h.rlRead.Limit(h.requireHandle(h.verityProxy)))
	mux.HandleFunc("/library/", h.rlRead.Limit(h.requireHandle(h.alexandriaProxy)))
	// Aethyr Ledger proxy — AET settlement fabric (write rate limited)
	mux.HandleFunc("/ledger/", h.rlWrite.Limit(h.requireHandle(h.ledgerProxy)))
	// Thessalon proxy — subscriptions, PPV, tips (write rate limited)
	mux.HandleFunc("/thessalon/", h.rlWrite.Limit(h.requireHandle(h.thessalonProxy)))

	// ── D-070 read lane — GET /facets/{name} returns HTML fragments only ────────
	mux.HandleFunc("GET /facets/link_preview", h.rlRead.Limit(h.requireHandle(h.facetLinkPreview)))
	mux.HandleFunc("GET /facets/quoted_post", h.rlRead.Limit(h.requireHandle(h.facetQuotedPost)))
	mux.HandleFunc("GET /facets/cashtag/search", h.rlRead.Limit(h.requireHandle(h.facetCashtagSearch)))
	mux.HandleFunc("GET /facets/cashtag/card", h.rlRead.Limit(h.requireHandle(h.facetCashtagCard)))

	// ── Sports lane — game cards, one league per strip ───────────────────────
	// Public, like the watch surface: a scoreboard is not a private fact, and a
	// logged-out visitor landing on a game page must see the game. Every one of
	// these answers 204/404 when SPORTS_PROVIDER is blank.
	mux.HandleFunc("GET /facets/sports/{league}/strip", h.rlRead.Limit(h.facetSportsStrip))
	mux.HandleFunc("GET /facets/sports/{league}/card", h.rlRead.Limit(h.facetSportsCard))
	mux.HandleFunc("GET /facets/hashtag/search", h.rlRead.Limit(h.requireHandle(h.facetHashtagSearch)))
	mux.HandleFunc("POST /facets/compose_highlight", h.rlRead.Limit(h.requireHandle(h.facetComposeHighlight)))
	mux.HandleFunc("GET /facets/edit_profile_modal", h.rlRead.Limit(h.requireHandle(h.facetEditProfileModal)))
	mux.HandleFunc("GET /facets/share_sheet", h.rlRead.Limit(h.requireHandle(h.facetShareSheet)))
	mux.HandleFunc("GET /facets/overlay/close", h.rlRead.Limit(h.facetOverlayClose))
	mux.HandleFunc("GET /facets/tip_modal", h.rlRead.Limit(h.requireHandle(h.facetTipModal)))
	mux.HandleFunc("GET /facets/report_sheet", h.rlRead.Limit(h.requireHandle(h.facetReportSheet)))
	mux.HandleFunc("GET /facets/work/{id}/translate_slot", h.rlRead.Limit(h.requireHandle(h.facetTranslateSlot)))
	mux.HandleFunc("GET /facets/explore/canvas", h.rlRead.Limit(h.requireHandle(h.facetExploreCanvas)))
	mux.HandleFunc("GET /facets/tip_external_links", h.rlRead.Limit(h.requireHandle(h.facetTipExternalLinks)))
	mux.HandleFunc("GET /facets/post_replies/{id}", h.rlRead.Limit(h.requireHandle(h.facetPostReplies)))
	mux.HandleFunc("GET /facets/post/{id}/engagement", h.rlRead.Limit(h.requireHandle(h.facetPostEngagement)))
	mux.HandleFunc("POST /api/events/watch", h.rlRead.Limit(h.requireHandle(h.watchHandler)))
	mux.HandleFunc("GET /facets/org_search", h.rlRead.Limit(h.requireHandle(h.facetOrgSearch)))
	mux.HandleFunc("GET /org/panel", h.rlRead.Limit(h.requireHandle(h.orgPanelPage)))
	mux.HandleFunc("POST /api/org/verify/apply", h.rlWrite.Limit(h.requireHandle(h.orgVerifyApply)))
	mux.HandleFunc("GET /admin/partials/org-verifications", h.rlRead.Limit(h.requireHandle(h.adminOrgVerificationsPanel)))
	mux.HandleFunc("POST /api/admin/org/approve", h.rlWrite.Limit(h.requireHandle(h.adminApproveOrgVerification)))
	mux.HandleFunc("POST /api/admin/org/reject", h.rlWrite.Limit(h.requireHandle(h.adminRejectOrgVerification)))
	mux.HandleFunc("POST /api/admin/org/grant", h.rlWrite.Limit(h.requireHandle(h.adminGrantOrgBadge)))
	mux.HandleFunc("POST /api/admin/org/revoke", h.rlWrite.Limit(h.requireHandle(h.adminRevokeOrgBadge)))

	// ── D-070 mutation lane — ALL state changes go through POST /events ────────
	mux.HandleFunc("POST /events", h.rlWrite.Limit(h.requireHandle(h.eventsPost)))
	// Brain-to-brain: reachable only from the container network, and then only
	// with INTERNAL_API_KEY (internalCallerOK inside the handler).
	mux.Handle("POST /api/internal/balance-update", middleware.InternalOnly(http.HandlerFunc(h.balanceUpdateWebhook)))

	// HTMX partials (write rate limit)
	mux.HandleFunc("/feed", h.rlRead.Limit(h.feedPartial))
	mux.HandleFunc("/partials/reply-preview/{post_id}", h.rlRead.Limit(h.replyPreviewPartial))
	mux.HandleFunc("/partials/sidebar-right", h.rlRead.Limit(h.sidebarRightPartial))
	mux.HandleFunc("GET /partials/push-prompt", h.rlRead.Limit(h.partialPushPrompt))
	mux.HandleFunc("GET /partials/follow-list", h.rlRead.Limit(h.followListPartial))
	mux.HandleFunc("GET /api/push/vapid-key", h.vapidPublicKey)
	mux.HandleFunc("/partials/nexus/add-persona-flow", h.rlRead.Limit(h.requireHandle(h.nexusAddPersonaFlow)))
	mux.HandleFunc("/partials/nexus/persona-switcher", h.rlRead.Limit(h.requireHandle(h.nexusPersonaSwitcherPartial)))
	mux.HandleFunc("/partials/nexus/handle-search", h.rlRead.Limit(h.requireHandle(h.nexusHandleSearch)))
	mux.HandleFunc("GET /facets/add_account_modal", h.rlRead.Limit(h.requireHandle(h.facetAddAccountModal)))
	mux.HandleFunc("GET /partials/add-account-modal/fork", h.rlRead.Limit(h.requireHandle(h.partialAamFork)))
	mux.HandleFunc("GET /partials/add-account-modal/create", h.rlRead.Limit(h.requireHandle(h.partialAamCreate)))
	mux.HandleFunc("GET /partials/add-account-modal/link", h.rlRead.Limit(h.requireHandle(h.partialAamLink)))
	mux.HandleFunc("POST /partials/add-account-modal/create", h.rlAuth.Limit(h.requireHandle(h.handleAamCreate)))
	mux.HandleFunc("POST /partials/link-account", h.rlAuth.Limit(h.requireHandle(h.handleLinkAccount)))
	mux.HandleFunc("/settings/personas", h.rlRead.Limit(h.requireHandle(h.settingsPersonas)))
	mux.HandleFunc("GET /settings/add-account", h.rlRead.Limit(h.requireHandle(h.addAccountPage)))
	mux.HandleFunc("POST /settings/add-account", h.rlAuth.Limit(h.requireHandle(h.handleAddAccount)))
	mux.HandleFunc("/settings/{section}", h.rlRead.Limit(h.requireHandle(h.settingsPage)))
	mux.HandleFunc("/partials/settings/{section}", h.rlRead.Limit(h.requireHandle(h.settingsSection)))
	mux.HandleFunc("POST /api/settings/notifications", h.rlWrite.Limit(h.requireHandle(h.saveNotificationPrefs)))
	// The device list the security section swaps in, and the two silence lists.
	// All three had handlers and no route: the section fetched a 404 and the
	// links in it led nowhere.
	mux.HandleFunc("GET /api/sessions", h.rlRead.Limit(h.requireHandle(h.sessionsPartial)))
	mux.HandleFunc("GET /blocked", h.rlRead.Limit(h.requireHandle(h.blockedPage)))
	mux.HandleFunc("GET /muted", h.rlRead.Limit(h.requireHandle(h.mutedPage)))
	mux.HandleFunc("/profile/save", h.rlWrite.Limit(h.saveProfile))
	mux.HandleFunc("/api/settings/password", h.rlWrite.Limit(h.requireHandle(h.changePasswordAPI)))
	mux.HandleFunc("/music/tracks", h.rlRead.Limit(h.tracksPartial))
	mux.HandleFunc("GET /feed/surfaces", h.rlRead.Limit(h.feedSurfacesPartial))
	mux.HandleFunc("GET /partials/surface-sheet", h.rlRead.Limit(h.requireHandle(h.surfaceSheetPartial)))
	mux.HandleFunc("GET /partials/compose/quote-picker/bookmarks", h.rlRead.Limit(h.requireHandle(h.composeQuotePickerBookmarks)))
	mux.HandleFunc("GET /partials/compose/quote-picker/likes", h.rlRead.Limit(h.requireHandle(h.composeQuotePickerLikes)))
	mux.HandleFunc("GET /partials/compose/quote-picker/yours", h.rlRead.Limit(h.requireHandle(h.composeQuotePickerYours)))

	// User lists
	mux.HandleFunc("/api/user/", h.rlRead.Limit(h.userListAPI))

	// JSON API (write rate limit on mutations)
	mux.HandleFunc("/api/post/edit", h.rlWrite.Limit(h.editPost))
	mux.HandleFunc("POST /api/work/edit", h.rlWrite.Limit(h.requireHandle(h.editWork)))
	mux.HandleFunc("/api/thread/{id}", h.rlRead.Limit(h.requireHandle(h.threadChainAPI)))
	mux.HandleFunc("/api/poll/vote", h.rlWrite.Limit(h.requireHandle(h.castPollVote)))
	// Creator
	mux.HandleFunc("/api/creator/enable", h.rlWrite.Limit(h.requireHandle(h.enableCreatorAPI)))
	mux.HandleFunc("/api/subscribe", h.rlWrite.Limit(h.requireHandle(h.subscribeToCreator)))
	mux.HandleFunc("/api/unsubscribe", h.rlWrite.Limit(h.requireHandle(h.unsubscribeFromCreator)))
	mux.HandleFunc("/api/creator/plans", h.rlRead.Limit(h.requireHandle(h.getCreatorPlans)))
	mux.HandleFunc("/api/track/like", h.rlWrite.Limit(h.trackLikeAction))
	mux.HandleFunc("/api/track/play", h.rlWrite.Limit(h.trackPlayAction))
	mux.HandleFunc("/api/track/delete", h.rlWrite.Limit(h.requireHandle(h.deleteTrackAPI)))
	mux.HandleFunc("/api/follow", h.rlWrite.Limit(h.followAPI))
	mux.HandleFunc("/api/feedback", h.rlWrite.Limit(h.feedbackAPI))
	mux.HandleFunc("/api/notifications/read-all", h.rlWrite.Limit(h.requireHandle(h.markNotificationsRead)))
	mux.HandleFunc("/api/notifications/count", h.rlRead.Limit(h.requireHandle(h.notificationsCount)))
	// SSE — real-time push for notifications + wallet balance (no polling, no JSON)
	mux.HandleFunc("/api/events", h.requireHandle(h.sseEvents))
	mux.HandleFunc("/api/online/", h.rlRead.Limit(h.userOnlineStatus))
	mux.HandleFunc("GET /api/profiles/mini", h.rlRead.Limit(h.profilesMini))
	mux.HandleFunc("/api/report", h.rlWrite.Limit(h.requireHandle(h.reportContent)))
	mux.HandleFunc("/api/admin/report/resolve", h.rlWrite.Limit(h.requireHandle(h.resolveReport)))
	mux.HandleFunc("/api/admin/set-role", h.rlWrite.Limit(h.requireHandle(h.adminSetRole)))
	mux.HandleFunc("/api/admin/inject-aet", h.rlWrite.Limit(h.requireHandle(h.adminInjectAET)))
	mux.HandleFunc("/api/admin/reset-password", h.rlWrite.Limit(h.requireHandle(h.adminResetPassword)))
	mux.HandleFunc("/api/admin/set-email", h.rlWrite.Limit(h.requireHandle(h.adminSetEmail)))
	mux.HandleFunc("/api/admin/set-cap", h.rlWrite.Limit(h.requireHandle(h.adminSetCap)))
	mux.HandleFunc("POST /api/admin/set-realm-grant", h.rlWrite.Limit(h.requireHandle(h.adminSetRealmGrant)))
	mux.HandleFunc("POST /api/admin/scan-sweep", h.rlWrite.Limit(h.requireHandle(h.adminScanSweep)))
	mux.HandleFunc("POST /api/admin/media-fingerprint-sweep", h.rlWrite.Limit(h.requireHandle(h.adminMediaFingerprintSweep)))
	mux.HandleFunc("POST /api/admin/clear-orphan-hashes", h.rlWrite.Limit(h.requireHandle(h.adminClearOrphanHashes)))
	mux.HandleFunc("POST /api/admin/delete-post", h.rlWrite.Limit(h.requireHandle(h.adminDeletePost)))
	mux.HandleFunc("POST /api/admin/purge-user", h.rlWrite.Limit(h.requireHandle(h.adminPurgeUser)))
	mux.HandleFunc("GET /api/admin/human-review-count", h.rlRead.Limit(h.requireHandle(h.adminHumanReviewCount)))
	mux.HandleFunc("POST /api/admin/bulk-approve-review", h.rlWrite.Limit(h.requireHandle(h.adminBulkApproveReview)))
	mux.HandleFunc("POST /api/admin/resync-follow-counts", h.rlWrite.Limit(h.requireHandle(h.adminResyncFollowCounts)))
	mux.HandleFunc("POST /api/admin/user-action", h.rlWrite.Limit(h.requireHandle(h.adminUserAction)))
	mux.HandleFunc("POST /api/admin/user/nsfw-toggle", h.rlWrite.Limit(h.requireHandle(h.adminNSFWToggle)))
	mux.HandleFunc("POST /api/admin/user/content-setting", h.rlWrite.Limit(h.requireHandle(h.adminSetContentSetting)))
	mux.HandleFunc("POST /api/admin/abraxas-config", h.rlWrite.Limit(h.requireHandle(h.abraxasConfigPost)))
	mux.HandleFunc("POST /api/admin/celebrations", h.rlWrite.Limit(h.requireHandle(h.celebrationsConfigPost)))
	// Admin panel section deep-link routes — same full-page render so back/forward and hard refresh work.
	mux.HandleFunc("GET /admin/overview", h.rlRead.Limit(h.requireHandle(h.adminPage)))
	mux.HandleFunc("GET /admin/users", h.rlRead.Limit(h.requireHandle(h.adminPage)))
	mux.HandleFunc("GET /admin/analytics", h.rlRead.Limit(h.requireHandle(h.adminPage)))
	mux.HandleFunc("GET /admin/moderation", h.rlRead.Limit(h.requireHandle(h.adminPage)))
	mux.HandleFunc("GET /admin/dmca", h.rlRead.Limit(h.requireHandle(h.adminPage)))
	mux.HandleFunc("GET /admin/treasury", h.rlRead.Limit(h.requireHandle(h.adminPage)))
	mux.HandleFunc("GET /admin/devtools", h.rlRead.Limit(h.requireHandle(h.adminPage)))
	mux.HandleFunc("GET /admin/abraxas", h.rlRead.Limit(h.requireHandle(h.adminPage)))
	mux.HandleFunc("GET /admin/celebrations", h.rlRead.Limit(h.requireHandle(h.adminPage)))
	mux.HandleFunc("GET /admin/org-verifications", h.rlRead.Limit(h.requireHandle(h.adminPage)))
	// Admin panel HTMX partial routes
	mux.HandleFunc("GET /admin/partials/overview", h.rlRead.Limit(h.requireHandle(h.adminPanelOverview)))
	mux.HandleFunc("GET /admin/partials/users", h.rlRead.Limit(h.requireHandle(h.adminPanelUsers)))
	mux.HandleFunc("GET /admin/partials/moderation", h.rlRead.Limit(h.requireHandle(h.adminPanelModeration)))
	mux.HandleFunc("GET /admin/partials/dmca", h.rlRead.Limit(h.requireHandle(h.adminPanelDMCA)))
	mux.HandleFunc("GET /admin/partials/devtools", h.rlRead.Limit(h.requireHandle(h.adminPanelDevtools)))
	mux.HandleFunc("GET /admin/partials/analytics", h.rlRead.Limit(h.requireHandle(h.adminPanelAnalytics)))
	mux.HandleFunc("GET /admin/partials/abraxas", h.rlRead.Limit(h.requireHandle(h.abraxasConfigGet)))
	mux.HandleFunc("GET /admin/partials/celebrations", h.rlRead.Limit(h.requireHandle(h.adminPanelCelebrations)))
	mux.HandleFunc("GET /admin/partials/treasury", h.rlRead.Limit(h.requireHandle(h.adminPanelTreasury)))
	mux.HandleFunc("POST /api/admin/sweep-fees", h.rlWrite.Limit(h.requireHandle(h.adminSweepFees)))
	// Trust & Safety moderation queue and admin actions
	mux.HandleFunc("GET /admin/moderation/queue", h.rlRead.Limit(h.requireHandle(h.ModerationQueueGet)))
	mux.HandleFunc("POST /admin/moderation/action", h.rlWrite.Limit(h.requireHandle(h.ModerationActionPost)))
	mux.HandleFunc("POST /admin/moderation/hash-ban", h.rlWrite.Limit(h.requireHandle(h.ModerationHashBan)))
	mux.HandleFunc("/api/users/search", h.rlRead.Limit(h.mentionSearchAPI))
	mux.HandleFunc("GET /api/vault/recovery", h.rlRead.Limit(h.requireHandle(h.vaultRecoveryGet)))
	mux.HandleFunc("POST /api/vault/recovery", h.rlWrite.Limit(h.requireHandle(h.vaultRecoverySet)))
	// Malkuth: PIAL signing key registration (ECDSA-P256, browser-generated)
	mux.HandleFunc("POST /api/pial/signing-key/register", h.rlWrite.Limit(h.requireHandle(h.registerPIALSigningKey)))
	mux.HandleFunc("GET /api/pial/by-handle/{handle}", h.rlRead.Limit(h.requireHandle(h.pialByHandle)))
	mux.HandleFunc("/api/health", h.healthAPI)

	// Hashtag and cashtag dedicated pages
	mux.HandleFunc("GET /tag/{name}", h.rlRead.Limit(h.tagPage))
	mux.HandleFunc("GET /stocks/{ticker}", h.rlRead.Limit(h.stocksPage))

	// Sports — the slate and one game, each as a durable destination. A score in
	// a work scrolls away; these addresses do not.
	// The cross-league board. A rail whose rows span two leagues links here, not
	// to whichever league happened to sort first.
	mux.HandleFunc("GET /sports", h.rlRead.Limit(h.sportsHubPage))
	mux.HandleFunc("GET /sports/{$}", h.rlRead.Limit(h.sportsHubPage))
	mux.HandleFunc("GET /sports/{league}", h.rlRead.Limit(h.sportsHubPage))
	mux.HandleFunc("GET /sports/{league}/{game}", h.rlRead.Limit(h.sportsGamePage))

	// 2FA TOTP
	mux.HandleFunc("GET /api/2fa/setup", h.rlWrite.Limit(h.requireHandle(h.totpSetup)))
	mux.HandleFunc("POST /api/2fa/enable", h.rlWrite.Limit(h.requireHandle(h.rlAccount.LimitKeyed(sessionAccountKey, h.totpEnable))))
	mux.HandleFunc("POST /api/2fa/disable", h.rlWrite.Limit(h.requireHandle(h.rlAccount.LimitKeyed(sessionAccountKey, h.totpDisable))))

	// GIF search proxy
	mux.HandleFunc("GET /api/gif/search", h.rlRead.Limit(h.requireHandle(h.gifSearch)))

	// Prometheus metrics — scraped by infra/observability/prometheus.yml over
	// the container network. Caddy proxies every path on f33d3r.com here, so
	// "bound to the internal network" is not a property of the listener; it is
	// enforced per request: anything that crossed the edge is answered 404.
	mux.Handle("/metrics", middleware.InternalOnly(observ.MetricsHandler()))

	// File uploads (write rate limit)
	mux.HandleFunc("/upload/avatar", h.rlWrite.Limit(h.uploadAvatar))
	mux.HandleFunc("/upload/header", h.rlWrite.Limit(h.uploadHeader))
	mux.HandleFunc("/upload/track", h.rlWrite.Limit(h.uploadTrack))
	mux.HandleFunc("/upload/post-media", h.rlWrite.Limit(h.requireHandle(h.uploadPostMedia)))
	mux.HandleFunc("/upload/voice", h.rlWrite.Limit(h.requireHandle(h.uploadVoicePost)))
	mux.HandleFunc("POST /upload/react-video", h.rlWrite.Limit(h.requireHandle(h.uploadReactVideo)))
	mux.HandleFunc("POST /api/media/upload-encrypted", h.rlWrite.Limit(h.requireHandle(h.uploadEncryptedMedia)))
	// Pre-upload hash gates — client computes SHA-256 locally and checks here before upload starts.
	mux.HandleFunc("POST /upload/check-dedup", h.rlRead.Limit(h.requireHandle(h.checkVideoDedupHandler)))
	mux.HandleFunc("POST /upload/check-video-hash", h.rlRead.Limit(h.requireHandle(h.checkVideoHashHandler)))
	mux.HandleFunc("POST /upload/check-image-hash", h.rlRead.Limit(h.requireHandle(h.checkImageHashHandler)))
	// TUS resumable upload (S0.7)
	mux.HandleFunc("POST /upload/tus/", h.rlWrite.Limit(h.requireHandle(h.tusCreate)))
	mux.HandleFunc("HEAD /upload/tus/{id}", h.rlRead.Limit(h.requireHandle(h.tusHead)))
	mux.HandleFunc("PATCH /upload/tus/{id}", h.rlWrite.Limit(h.requireHandle(h.tusPatch)))
	mux.HandleFunc("GET /upload/tus/status/{id}", h.rlRead.Limit(h.requireHandle(h.tusJobStatus)))

	// Security: admin panel security routes
	mux.HandleFunc("GET /admin/security", h.rlRead.Limit(h.requireHandle(h.adminPage)))
	mux.HandleFunc("GET /admin/partials/security", h.rlRead.Limit(h.requireHandle(h.adminPanelSecurity)))
	mux.HandleFunc("POST /api/admin/security/block-ip", h.rlWrite.Limit(h.requireHandle(h.adminBlockIP)))
	mux.HandleFunc("POST /api/admin/security/unblock-ip", h.rlWrite.Limit(h.requireHandle(h.adminUnblockIP)))

	// Post translation
	mux.HandleFunc("POST /api/post/{id}/translate", h.rlWrite.Limit(h.requireHandle(h.translatePostHandler)))

	// Articles
	mux.HandleFunc("GET /articles", h.rlRead.Limit(h.requireHandle(h.articlesPage)))
	mux.HandleFunc("GET /articles/new", h.rlRead.Limit(h.requireHandle(h.newArticlePage)))
	mux.HandleFunc("GET /articles/{id}/edit", h.rlRead.Limit(h.requireHandle(h.editArticlePage)))
	mux.HandleFunc("GET /article/{slug}", h.rlRead.Limit(h.articleReaderPage))
	mux.HandleFunc("POST /api/article/save", h.rlWrite.Limit(h.requireHandle(h.saveArticle)))
	mux.HandleFunc("POST /api/article/{id}/delete", h.rlWrite.Limit(h.requireHandle(h.deleteArticle)))
	mux.HandleFunc("POST /api/article/preview", h.rlRead.Limit(h.requireHandle(h.previewArticle)))
	mux.HandleFunc("GET /facets/article/{id}/viewcount", h.rlRead.Limit(h.facetArticleViewcount))
	mux.HandleFunc("GET /facets/leaderboard_panel", h.rlRead.Limit(h.facetLeaderboardPanel))

	// Apply CSRF check globally then wrap with IP block check and request logging.
	csrfHandler := middleware.CSRF(mux)
	securityHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip IP block check for static assets — they don't need it and we don't
		// want to hit the DB cache on every image/CSS/font request.
		if !strings.HasPrefix(r.URL.Path, "/static/") && !strings.HasPrefix(r.URL.Path, "/media/") {
			ip := requestIP(r)
			if IsIPBlocked(h.db, ip) {
				http.Error(w, "403 Forbidden", http.StatusForbidden)
				return
			}
		}
		csrfHandler.ServeHTTP(w, r)
	})
	// Compress sits inside RequestLog (so logs still see the real status) and wraps
	// everything else — gzip cuts the ~1.2 MB of uncompressed CSS/JS per cold load
	// (styles.css 547 KB → ~70 KB) on both the direct :8081 path and behind Caddy.
	// SecurityHeaders sits just inside Recover so every response — including a
	// 429 from a limiter, a 403 from CSRF and the 500 Recover itself writes —
	// carries the CSP and the rest of the hardening set.
	// Recover is outermost so it catches panics from every other layer and
	// turns them into a clean 500 instead of a dropped/half-written connection.
	return middleware.Recover(middleware.SecurityHeaders(middleware.RequestLog(middleware.Compress(securityHandler))))
}

// requireHandle redirects to /login if no handle cookie is set.
// It also sets Cache-Control: no-store so browsers never serve stale
// authenticated pages from cache after a Docker rebuild or logout.
func (h *Handler) requireHandle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if HandleFromCookie(r) == "" {
			// The browser Shell cannot take a login page into its Playground: the
			// login page is a different Shell. It is told to load the document.
			if playgroundRequest(r) {
				hxRedirect(w, "/login")
				return
			}
			// A facet caller cannot use a login page: it has a shell of its own and
			// would project the page into it. The honest answer is the status.
			if facetRequest(r) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		next(w, r)
	}
}

// rootDispatch is the "/" catch-all. It serves the home feed for "/" and dispatches the
// clean root URLs that would otherwise need first-segment wildcard patterns (which Go's
// ServeMux refuses to register alongside root subtree routes — see the Routes() note):
//
//	/                       → home feed (auth required)
//	/{handle}               → public profile
//	/{handle}/followers     → followers list (auth required)
//	/{handle}/following     → following list (auth required)
//	/{handle}/work/{id}     → public work detail
//
// The downstream handlers read r.PathValue("handle"/"id"); we populate those via
// SetPathValue so they work unchanged. Anything else is a genuine 404.
func (h *Handler) rootDispatch(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		h.requireHandle(h.indexPage)(w, r)
		return
	}
	// Clean-URL profile surface is read-only — GET (and HEAD) only.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(segs) == 1 && segs[0] != "":
		r.SetPathValue("handle", segs[0])
		h.userProfilePage(w, r)
	case len(segs) == 2 && segs[1] == "followers":
		r.SetPathValue("handle", segs[0])
		h.requireHandle(h.followersPage)(w, r)
	case len(segs) == 2 && segs[1] == "following":
		r.SetPathValue("handle", segs[0])
		h.requireHandle(h.followingPage)(w, r)
	case len(segs) == 3 && segs[1] == "work":
		r.SetPathValue("handle", segs[0])
		r.SetPathValue("id", segs[2])
		h.workDetailPage(w, r)
	default:
		http.NotFound(w, r)
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

		// Slow path: DB lookup — resolves PIAL + active account in one query.
		if ident, _ := dbpkg.GetSessionIdentity(h.db, tok); ident.UserID != "" {
			if u, err := dbpkg.GetUserByID(h.db, ident.UserID); err == nil && u != nil {
				// Prefer PIAL from session; fall back to account lookup / bootstrap.
				if ident.PIALID != "" {
					u.PIALID = ident.PIALID
				} else if u.PIALID == "" {
					if pid := dbpkg.BootstrapPIAL(h.db, u.ID, u.Handle); pid != "" {
						u.PIALID = pid
						// The handle predates this identity. Tell the authority.
						h.ensureHandleAllocated(r.Context(), u.Handle, pid)
					}
				}
				// The identity authority governs this PIAL and keys every contact
				// row and messaging key to it, so it has to know the identity
				// exists before anyone tries to reach this person. Detached: the
				// person is not waiting on it.
				h.ensurePIALAsserted(r.Context(), u.PIALID)
				// Identity/authz resolved from PIAL — the single system of record.
				// age_verified is the age-gate fact (self-report is enough). The verified
				// badge is the stricter identity fact — kyc_tier=='full' — never age_verified.
				// Role drives IsAdmin().
				if ps := dbpkg.LoadPIALState(h.db, u.PIALID); ps != nil {
					u.IsAgeVerified = ps.AgeVerified
					u.IsVerified = model.KYCTierIsIdentity(ps.KYCTier)
					u.KYCTier = ps.KYCTier
					u.IsMinor = ps.IsMinor
					u.IsAdult = ps.IsAdult
					u.Role = ps.Role
				}
				// Birthday is authoritative — override DB flags if birthday is set
				ApplyBirthdayAgeGate(u)
				// Interest vector on the Jung axes: the stored online average of
				// engaged works' vectors, or — when none has been stored yet — a
				// fresh computation from recent engagement and the declared
				// archetype, persisted so the next request reads it.
				if stored, ok := jung.FromSlice(dbpkg.LoadInterestVector(h.db, u.ID)); ok {
					u.InterestVector = stored.Slice()
				} else {
					u.InterestVector = h.computeInterestVector(u)
					go dbpkg.SaveInterestVector(h.db, u.ID, u.InterestVector)
				}
				u.RecentContentIDs = []string{}
				u.CreatorAffinities = map[string]float32{}
				u.IsColdStart = u.PostCount == 0
				u.SafetyEpsilon = SafetyEpsilon(u.ContentSetting)
				h.sessionCache.Store(tok, sessionEntry{user: u, exp: time.Now().Add(30 * time.Second)})
				return u
			}
		}
		// Session token present but session not found or expired.
		// Only clear the cookie on full-page responses — never on HTMX partials,
		// which would silently destroy the session mid-navigation.
		if r.Header.Get("HX-Request") != "true" {
			ClearSessionCookie(w)
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
					// The handle predates this identity. Tell the authority.
					h.ensureHandleAllocated(r.Context(), u.Handle, pid)
				} else {
					log.Printf("[pial] auto-bootstrap failed for account %s", u.ID)
				}
			}
			if stored, ok := jung.FromSlice(dbpkg.LoadInterestVector(h.db, u.ID)); ok {
				u.InterestVector = stored.Slice()
			} else {
				u.InterestVector = h.computeInterestVector(u)
				go dbpkg.SaveInterestVector(h.db, u.ID, u.InterestVector)
			}
			u.RecentContentIDs = []string{}
			u.CreatorAffinities = map[string]float32{}
			u.IsColdStart = u.PostCount == 0
			u.SafetyEpsilon = SafetyEpsilon(u.ContentSetting)
			if u.ThemeID == "" {
				u.ThemeID = "void"
			}
			if u.Tier == "" {
				u.Tier = "free"
			}
			if u.ContentSetting == "" {
				u.ContentSetting = "default"
			}
			// Identity/authz resolved from PIAL — the single system of record.
			// age_verified is the age-gate fact; the verified badge is the stricter
			// kyc_tier=='full' identity fact.
			if ps := dbpkg.LoadPIALState(h.db, u.PIALID); ps != nil {
				u.IsAgeVerified = ps.AgeVerified
				u.IsVerified = model.KYCTierIsIdentity(ps.KYCTier)
				u.KYCTier = ps.KYCTier
				u.IsMinor = ps.IsMinor
				u.IsAdult = ps.IsAdult
				u.Role = ps.Role
			}
			// Birthday is authoritative — override DB flags if birthday is set
			ApplyBirthdayAgeGate(u)
			h.sessionCache.Store(legacyKey, sessionEntry{user: u, exp: time.Now().Add(30 * time.Second)})
			return u
		}
	}

	u := DemoUser()
	u.Handle = handle
	return u
}

// baseData returns a map pre-populated with keys that every authenticated page
// needs: User, Nexus (persona switcher context), SessionID, ShowScores, Themes.
// Callers merge their own keys on top. Nexus is nil for single-account users —
// the sidebar template handles that gracefully.
func (h *Handler) baseData(user *model.User) map[string]interface{} {
	var nexusCtx *model.NexusContext
	if user != nil && user.PIALID != "" {
		sess := nexus.GetSessionForUser(user.PIALID, h.nexusClient)
		nexusCtx = buildNexusContext(user, sess, h)
	}
	return map[string]interface{}{
		"User":       user,
		"Nexus":      nexusCtx,
		"SessionID":  uuid.New().String(),
		"ShowScores": h.cfg.ShowScores,
		"Themes": ThemesWithActive(func() string {
			if user != nil {
				return user.ThemeID
			}
			return ""
		}()),
	}
}

// facetRequest reports whether the caller wants a Facet rather than a page: the
// native Shell (Android) and HTMX both say so with HX-Request, and neither has any
// use for base.html — one already has a shell, the other is swapping into one.
func facetRequest(r *http.Request) bool {
	return r != nil && r.Header.Get("HX-Request") == "true"
}

// pageFacetID is the address of a page rendered as a Facet: the page's whole body
// block, delivered as one fragment. The name is the template file without its
// extension, so /lists renders as facet:f33d3r:page:lists:body.
func pageFacetID(page string) string {
	return "facet:f33d3r:page:" + strings.TrimSuffix(filepath.Base(page), ".html") + ":body"
}

// pageShellClass executes a page set's shell-class block. A non-empty value
// means the page draws a different Shell than the standing one; the block is a
// constant per page, so it is evaluated once at boot with no data.
func pageShellClass(tmpl *template.Template) string {
	t := tmpl.Lookup("shell-class")
	if t == nil {
		return ""
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, nil); err != nil {
		return ""
	}
	return strings.TrimSpace(buf.String())
}

// render draws a page. For a page load it executes base.html — nav, rails, overlay
// slots and all. For a facet request it executes only the page's body block and
// wraps it in a single addressable root, because the caller already owns the shell
// and a second one arriving inside the content plane is the one thing a
// server-authoritative surface must never do. Pages without a body block (the
// bare-shell ones: login, onboard) render whole either way.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, page string, data interface{}) {
	tmpl, ok := h.pages[page]
	if !ok {
		http.Error(w, "unknown page: "+page, http.StatusInternalServerError)
		return
	}
	// Default-fill the top-level keys that base.html and the nav/shell templates
	// reference on EVERY page (User, Nexus, SessionID, ShowScores, Themes). A
	// handler that builds its own data map and forgets one of these would
	// otherwise render "<no value>" (and hard-500 under TEMPLATE_STRICT). Filling
	// them here makes every full-page render consistent regardless of the handler.
	if m, ok2 := data.(map[string]interface{}); ok2 {
		var user *model.User
		if u, uok := m["User"].(*model.User); uok {
			user = u
		}
		if _, has := m["User"]; !has {
			m["User"] = user // typed-nil is fine; templates guard nil .User
		}
		if _, has := m["Nexus"]; !has {
			if user != nil && user.PIALID != "" {
				sess := nexus.GetSessionForUser(user.PIALID, h.nexusClient)
				m["Nexus"] = buildNexusContext(user, sess, h)
			} else {
				m["Nexus"] = nil
			}
		}
		if _, has := m["SessionID"]; !has {
			m["SessionID"] = uuid.New().String()
		}
		if _, has := m["ShowScores"]; !has {
			m["ShowScores"] = h.cfg.ShowScores
		}
		if _, has := m["Themes"]; !has {
			themeID := ""
			if user != nil {
				themeID = user.ThemeID
			}
			m["Themes"] = ThemesWithActive(themeID)
		}
		// The Frequencies dock lives in the Shell, so it is decided once per
		// document load: the person's live Frequency, or the empty mount.
		// Playground swaps and facet answers keep the Shell they have.
		if _, has := m["FrequencyDock"]; !has {
			if playgroundRequest(r) || facetRequest(r) {
				m["FrequencyDock"] = template.HTML("")
			} else {
				m["FrequencyDock"] = h.frequencyDock(r.Context(), user)
			}
		}
	}
	// Render into a buffer first so the write is atomic. If template execution
	// fails partway through (e.g. a nil-pointer deref in any nested template),
	// the client gets a clean 500 instead of a half-written page streamed onto
	// the wire — which is what silently drops the right rail and everything else
	// downstream of the failure point. Nothing is written until render succeeds.
	var buf bytes.Buffer
	// Every page answer varies on these headers: the same URL is a document for
	// a page load, a Playground for the browser Shell and a body facet for a
	// native one. No cache may hand one caller another's shape.
	w.Header().Add("Vary", "HX-Request")
	w.Header().Add("Vary", "HX-Boosted")
	w.Header().Add("Vary", "HX-Target")
	if playgroundRequest(r) {
		// A page that draws its own Shell (login, onboard, deactivated…) cannot
		// be placed inside the standing Shell. The Shell is told to load it as a
		// document — the one navigation that is allowed to be a document load.
		if h.shellPages[page] {
			hxRedirect(w, r.URL.RequestURI())
			return
		}
		if err := tmpl.ExecuteTemplate(&buf, "playground", data); err != nil {
			log.Printf("[render] %s playground: %v", page, err)
			http.Error(w, "internal render error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(buf.Bytes())
		return
	}
	if facetRequest(r) && tmpl.Lookup("body") != nil {
		fmt.Fprintf(&buf, `<div data-facet-id="%s" data-fa-page="%s">`,
			pageFacetID(page), strings.TrimSuffix(filepath.Base(page), ".html"))
		if err := tmpl.ExecuteTemplate(&buf, "body", data); err != nil {
			log.Printf("[render] %s body: %v", page, err)
			http.Error(w, "internal render error", http.StatusInternalServerError)
			return
		}
		buf.WriteString("</div>")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(buf.Bytes())
		return
	}
	if err := tmpl.ExecuteTemplate(&buf, "base.html", data); err != nil {
		log.Printf("[render] %s: %v", page, err)
		http.Error(w, "internal render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// renderPartial is the partial-endpoint counterpart of render(): it executes a
// named partial into a buffer before writing, so a template failure yields a
// clean 500 rather than a half-written HTML fragment streamed to the browser
// (which corrupts HTMX swaps and leaves elements blank/truncated). Any headers
// the caller already set (Cache-Control, etc.) are preserved because nothing is
// written to w until execution succeeds.
func (h *Handler) renderPartial(w http.ResponseWriter, name string, data interface{}) {
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("[partial] %s: %v", name, err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	_, _ = w.Write(buf.Bytes())
}

// ── D-070 mutation lane ───────────────────────────────────────────────────────
// eventsPost is the single entry point for all state changes from the browser.
// event_type field dispatches to the correct handler.
func (h *Handler) eventsPost(w http.ResponseWriter, r *http.Request) {
	// Buffer the body once so push subscription events (nested JSON objects) can be
	// parsed correctly. r.FormValue() only reads the body for form-encoded requests;
	// JSON bodies must be read here and restored for downstream handlers.
	var rawBodyMap map[string]json.RawMessage
	eventType := r.FormValue("event_type")
	if eventType == "" {
		bodyBytes, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		if err := json.Unmarshal(bodyBytes, &rawBodyMap); err == nil {
			if v, ok := rawBodyMap["event_type"]; ok {
				json.Unmarshal(v, &eventType)
			}
		}
	}
	if eventType == "" {
		http.Error(w, "event_type required", http.StatusBadRequest)
		return
	}

	// Dispatch marketplace.* events to Themis brain.
	if strings.HasPrefix(eventType, "marketplace.") {
		h.marketplaceEvent(w, r, rawBodyMap)
		return
	}

	// Dispatch nexus.* events to the NEXUS handler package.
	// Pass the authenticated user's PIAL ID so handlers can validate persona ownership.
	if strings.HasPrefix(eventType, "nexus.") {
		user := h.userFromRequest(w, r)
		pialID := ""
		if user != nil {
			pialID = user.PIALID
		}
		nexus.HandleEvent(w, r, eventType, h.nexusClient, pialID, h.db)
		return
	}

	// Herald push notification events — subscription lifecycle.
	switch eventType {
	case "push.subscription.registered", "push.subscription.refreshed", "push.subscription.rotated":
		user := h.userFromRequest(w, r)
		if user != nil && user.PIALID != "" && h.cfg.HeraldURL != "" {
			var subRaw json.RawMessage
			var deviceID string
			if rawBodyMap != nil {
				subRaw = rawBodyMap["subscription"]
				json.Unmarshal(rawBodyMap["device_id"], &deviceID)
			}
			go h.heraldSubscribeJSON(user.PIALID, subRaw, deviceID, r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
		return
	case "push.subscription.removed":
		if h.cfg.HeraldURL != "" {
			deviceID := r.FormValue("device_id")
			if deviceID == "" && rawBodyMap != nil {
				json.Unmarshal(rawBodyMap["device_id"], &deviceID)
			}
			if deviceID != "" {
				go h.heraldUnsubscribe(deviceID)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
		return
	case "permission.push.granted":
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
		return
	}

	// Malkuth-signed work: cid field present → route to workEvent regardless of event_type.
	if rawBodyMap != nil {
		if cidRaw, ok := rawBodyMap["cid"]; ok {
			var cidStr string
			if json.Unmarshal(cidRaw, &cidStr) == nil && strings.HasPrefix(cidStr, "sha256:") {
				h.workEvent(w, r, rawBodyMap)
				return
			}
		}
	}

	// D-070 migrated mutation events
	switch eventType {
	case "follow", "unfollow":
		h.followEvent(w, r)
		return

	case "tip":
		h.tipEvent(w, r)
		return

	case "pin_surface", "unpin_surface":
		h.pinSurfaceEvent(w, r, eventType)
		return

	case "notif_read":
		// The id arrives as a form field from the web and as JSON from the
		// apps, and the row it names is marked read only for the account that
		// asked — an unscoped UPDATE by id let any signed-in account clear
		// anyone's badge. The id is a group head, so the rows the group stands
		// for are marked with it; a group of five likes that clears one row is
		// a badge that comes back on the next read.
		user := h.userFromRequest(w, r)
		if user == nil || user.ID == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		notifID := eventField(r, rawBodyMap, "notif_id", "id")
		if notifID == "" {
			http.Error(w, "notif_id required", http.StatusBadRequest)
			return
		}
		if h.db != nil {
			ids, err := dbpkg.NotificationGroupMemberIDs(h.db, user.ID, notifID)
			if err != nil {
				http.Error(w, "notification not found", http.StatusNotFound)
				return
			}
			if err := dbpkg.MarkNotificationsRead(h.db, user.ID, ids); err != nil {
				log.Printf("[notif] mark read for %s: %v", user.Handle, err)
				http.Error(w, "could not mark notification read", http.StatusInternalServerError)
				return
			}
			h.publishUnreadBadge(user.ID)
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case "notif_read_all":
		user := h.userFromRequest(w, r)
		if user == nil || user.ID == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if h.db != nil {
			if err := dbpkg.MarkAllNotificationsRead(h.db, user.ID); err != nil {
				log.Printf("[notif] mark all read for %s: %v", user.Handle, err)
				http.Error(w, "could not mark notifications read", http.StatusInternalServerError)
				return
			}
			// Every other device this person has open is still showing the old
			// count. The badge is server-owned state, so the server says so.
			h.publishUnreadBadge(user.ID)
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case "poll_vote":
		// The ballot as the native clients cast it: work_id + option_idx, 204.
		// The web's ballot posts to /polls/vote and wants the poll_card
		// fragment back, so it keeps its own route; the vote itself is the
		// same idempotent row.
		user := h.userFromRequest(w, r)
		if user == nil || user.ID == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		workID := eventField(r, rawBodyMap, "work_id", "post_id")
		optionIdx, ok := eventIntField(r, rawBodyMap, "option_idx")
		if workID == "" || !ok || optionIdx < 0 {
			http.Error(w, "work_id and option_idx required", http.StatusBadRequest)
			return
		}
		if h.db == nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		poll, perr := dbpkg.GetPollResults(h.db, workID, user.ID)
		if perr != nil && !errors.Is(perr, sql.ErrNoRows) {
			log.Printf("[poll] lookup work=%s: %v", workID, perr)
			http.Error(w, "vote failed", http.StatusInternalServerError)
			return
		}
		if poll == nil {
			http.Error(w, "work not found or not a poll", http.StatusNotFound)
			return
		}
		if optionIdx >= len(poll.Options) {
			http.Error(w, "option out of range", http.StatusBadRequest)
			return
		}
		if poll.Closed {
			http.Error(w, "poll closed", http.StatusBadRequest)
			return
		}
		if _, err := dbpkg.CastPollVote(h.db, workID, user.ID, optionIdx); err != nil {
			log.Printf("[poll] cast vote work=%s voter=%s: %v", workID, user.ID, err)
			http.Error(w, "vote failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case "report":
		// A report from the native clients: post_id (or target_handle for an
		// account) + reason, 204. Filed through the same sequence as the web
		// report form — see fileContentReport.
		user := h.userFromRequest(w, r)
		if user == nil || user.ID == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		reason := eventField(r, rawBodyMap, "reason")
		switch reason {
		case "spam", "harassment", "hate", "nsfw", "violence", "illegal", "other":
		default:
			http.Error(w, "reason required", http.StatusBadRequest)
			return
		}
		contentID := eventField(r, rawBodyMap, "post_id", "work_id", "content_id")
		contentType := "post"
		if contentID == "" {
			targetHandle := strings.TrimPrefix(strings.ToLower(eventField(r, rawBodyMap, "target_handle")), "@")
			if targetHandle == "" {
				http.Error(w, "post_id or target_handle required", http.StatusBadRequest)
				return
			}
			if h.db == nil {
				http.Error(w, "database unavailable", http.StatusServiceUnavailable)
				return
			}
			target, terr := dbpkg.GetUserByHandle(h.db, targetHandle)
			if terr != nil || target == nil {
				http.Error(w, "user not found", http.StatusNotFound)
				return
			}
			contentID, contentType = target.ID, "user"
		}
		detail := truncate(eventField(r, rawBodyMap, "detail"), 500)
		if err := h.fileContentReport(user, contentID, contentType, reason, detail); err != nil {
			http.Error(w, "could not file report", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case "not_interested":
		// The reader's "show me less of this". It is a ranking signal and
		// nothing else: written to feedback_events as the negative_feedback
		// event AethyrRank's taxonomy defines, and sent to the ranker, which
		// lowers what it resembles. The author never hears about it.
		user := h.userFromRequest(w, r)
		if user == nil || user.ID == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		contentID := eventField(r, rawBodyMap, "work_id", "post_id")
		if contentID == "" {
			http.Error(w, "work_id required", http.StatusBadRequest)
			return
		}
		surface := eventField(r, rawBodyMap, "surface")
		if surface == "" {
			surface = "feed"
		}
		feedback := model.AethyrFeedbackRequest{
			UserID:    user.ID,
			SessionID: eventField(r, rawBodyMap, "session_id"),
			Surface:   surface,
			Events: []model.AethyrFeedbackEvent{{
				ContentID: contentID,
				EventType: "negative_feedback",
				Timestamp: time.Now().UTC(),
			}},
		}
		if h.db != nil {
			if err := dbpkg.InsertFeedbackEvents(h.db, feedback.UserID, feedback.SessionID, feedback.Surface, feedback.Events); err != nil {
				log.Printf("[feedback] not_interested by %s on %s not recorded: %v", user.Handle, contentID, err)
				http.Error(w, "could not record", http.StatusInternalServerError)
				return
			}
		}
		if h.aethyr != nil {
			h.aethyr.SendFeedback(&feedback)
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case "set_mobile_feed_view":
		user := h.userFromRequest(w, r)
		if user == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		view := r.FormValue("value")
		if view == "" && rawBodyMap != nil {
			json.Unmarshal(rawBodyMap["value"], &view)
		}
		if err := dbpkg.SetMobileFeedView(h.db, user.ID, view); err != nil {
			http.Error(w, "invalid value", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case "settings.privacy.show_sensitive":
		// 18+ "show sensitive content" (gore) toggle. An unchecked checkbox
		// submits nothing, which is off; a native client sends "0" or "1" and
		// means them. eventBool is the one reading of both.
		user := h.userFromRequest(w, r)
		if user == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		val, _ := eventBool(r, rawBodyMap, "value")
		if err := dbpkg.SetShowSensitive(h.db, user.ID, val); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case "settings.privacy.celebrations":
		// Per-user celebration opt-out. On by default; an unchecked checkbox
		// submits nothing and a client sends "0" — both mean hide.
		user := h.userFromRequest(w, r)
		if user == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		val, _ := eventBool(r, rawBodyMap, "value")
		if err := dbpkg.SetCelebrationsEnabled(h.db, user.ID, val); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case "settings.privacy.account_private":
		// Account visibility toggle. ON = private (hidden from public/logged-out
		// lookup, followers only); off is the default, whether that arrives as an
		// unchecked checkbox or as "0".
		user := h.userFromRequest(w, r)
		if user == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		val, _ := eventBool(r, rawBodyMap, "value")
		if err := dbpkg.SetIsPrivate(h.db, user.ID, val); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case "settings.privacy.content_setting":
		// Porn-axis content visibility (safe_mode|default|adult_enabled). Lets a
		// verified adult switch out of "standard" so the content_gate clears
		// directly instead of showing the 18+ click-to-reveal card. Minors and
		// non-age-verified users may not select adult_enabled — same rule as onboarding.
		user := h.userFromRequest(w, r)
		if user == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// The web's radio group names it "setting"; the clients name it "value".
		// One field read, both names.
		setting := eventField(r, rawBodyMap, "setting", "value")
		if setting == "adult_enabled" && (user.IsMinor || !(user.IsAgeVerified || user.IsVerified)) {
			setting = "default"
		}
		if err := dbpkg.SetContentSetting(h.db, user.ID, setting); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case "view_time":
		// Gap 1: view_time_seconds instrumentation.
		// JS sends this when a post card leaves the viewport after being watched.
		// Stored in feedback_events.dwell_ms; queried in buildRankRequest to patch
		// ViewTimeSeconds on AethyrContent so the quality signal is live.
		user := h.userFromRequest(w, r)
		if user == nil || user.ID == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		contentID := r.FormValue("post_id") // JS sends data-content-id as post_id
		secsStr := r.FormValue("seconds")
		if contentID == "" || secsStr == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var secs float64
		fmt.Sscanf(secsStr, "%f", &secs)
		if secs <= 0 || secs > 7200 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if h.db != nil {
			dwellMs := int64(secs * 1000)
			h.db.Exec(`
				INSERT INTO feedback_events (user_id, session_id, surface, content_id, event_type, position, dwell_ms, is_explore)
				VALUES ($1, $2, $3, $4, 'view_time', 0, $5, false)
			`, user.ID, r.FormValue("session_id"), r.FormValue("surface"), contentID, dwellMs)
			h.db.Exec(`UPDATE works SET view_count = view_count + 1 WHERE id = $1::uuid`, contentID)
		}
		// Sitra Achra: publish view ranking event (fire-and-forget).
		if user.PIALID != "" && contentID != "" {
			go h.publishRankingEvent("view", contentID, user.PIALID)
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case "block", "unblock":
		user := h.userFromRequest(w, r)
		if user == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		targetHandle := strings.TrimPrefix(strings.ToLower(eventField(r, rawBodyMap, "target_handle")), "@")
		if targetHandle == "" {
			http.Error(w, "target_handle required", http.StatusBadRequest)
			return
		}
		target, err := dbpkg.GetUserByHandle(h.db, targetHandle)
		if err != nil || target == nil {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		if eventType == "block" {
			dbpkg.BlockUser(h.db, user.ID, target.ID)
			go dbpkg.TryAwardTrollOnBlocked(h.db, target.ID)
		} else {
			dbpkg.UnblockUser(h.db, user.ID, target.ID)
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case "mute", "unmute":
		user := h.userFromRequest(w, r)
		if user == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		targetHandle := strings.TrimPrefix(strings.ToLower(eventField(r, rawBodyMap, "target_handle")), "@")
		if targetHandle == "" {
			http.Error(w, "target_handle required", http.StatusBadRequest)
			return
		}
		target, err := dbpkg.GetUserByHandle(h.db, targetHandle)
		if err != nil || target == nil {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		if eventType == "mute" {
			dbpkg.MuteUser(h.db, user.ID, target.ID)
			go dbpkg.TryAwardTrollOnMuted(h.db, target.ID)
		} else {
			dbpkg.UnmuteUser(h.db, user.ID, target.ID)
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case "set_reply_restriction":
		user := h.userFromRequest(w, r)
		if user == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		postID := r.FormValue("post_id")
		restriction := r.FormValue("restriction")
		if postID == "" && rawBodyMap != nil {
			json.Unmarshal(rawBodyMap["post_id"], &postID)
		}
		if restriction == "" && rawBodyMap != nil {
			json.Unmarshal(rawBodyMap["restriction"], &restriction)
		}
		// Map public values to DB values
		gatingMap := map[string]string{
			"everyone":  "open",
			"open":      "open",
			"followers": "followers",
			"verified":  "verified",
			"none":      "none",
		}
		gating, ok := gatingMap[restriction]
		if !ok {
			http.Error(w, "invalid restriction value", http.StatusBadRequest)
			return
		}
		if err := dbpkg.SetCommentGating(h.db, postID, user.ID, gating); err != nil {
			htmxError(w, r, "Could not update reply restriction", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case "pin_post", "unpin_post", "pin_work", "unpin_work":
		// pin_work/unpin_work are the same event under the name the native
		// clients use (they speak in works; the web's buttons predate the rename).
		user := h.userFromRequest(w, r)
		if user == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if eventType == "unpin_post" || eventType == "unpin_work" {
			if err := dbpkg.UnpinPost(h.db, user.ID); err != nil {
				htmxError(w, r, "Could not unpin post", http.StatusInternalServerError)
				return
			}
			w.Header().Set("HX-Trigger", `{"f33Toast":"Unpinned from profile"}`)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		postID := eventField(r, rawBodyMap, "post_id", "work_id")
		if postID == "" {
			http.Error(w, "post_id required", http.StatusBadRequest)
			return
		}
		if err := dbpkg.PinPost(h.db, user.ID, postID); err != nil {
			htmxError(w, r, "Could not pin post", http.StatusForbidden)
			return
		}
		w.Header().Set("HX-Trigger", `{"f33Toast":"Pinned to profile"}`)
		w.WriteHeader(http.StatusNoContent)
		return

	case "follow_topic", "unfollow_topic":
		user := h.userFromRequest(w, r)
		if user == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		tag := strings.TrimPrefix(r.FormValue("tag"), "#")
		if rawBodyMap != nil && tag == "" {
			json.Unmarshal(rawBodyMap["tag"], &tag)
		}
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			http.Error(w, "tag required", http.StatusBadRequest)
			return
		}
		// The write decides what the button says. This used to discard the error
		// and answer 204 No Content into an outerHTML swap, which deleted the
		// button from the page on every click.
		isFollow := eventType == "follow_topic"
		var terr error
		if isFollow {
			terr = dbpkg.FollowTopic(h.db, user.ID, tag)
		} else {
			terr = dbpkg.UnfollowTopic(h.db, user.ID, tag)
		}
		if terr != nil {
			log.Printf("[follow-topic] %s %s #%s: %v", eventType, user.ID, tag, terr)
			htmxError(w, r, "could not save that — try again", http.StatusInternalServerError)
			return
		}
		fragment, rerr := h.renderTopicFollowButton(tag, isFollow)
		if rerr != nil {
			log.Printf("[follow-topic] %v", rerr)
			htmxError(w, r, "render error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(fragment))
		return

	case "create_list", "delete_list", "list_add_member", "list_remove_member":
		h.listEvent(w, r, eventType)
		return

	case "org_apply", "org_invite", "org_approve", "org_reject", "org_revoke",
		"org_leave", "org_set_primary", "org_accept_invite":
		h.orgEvent(w, r, eventType)
		return

	case "deactivate_account":
		user := h.userFromRequest(w, r)
		if user == nil || user.ID == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		reason := eventField(r, rawBodyMap, "reason")
		if err := dbpkg.DeactivateUser(h.db, user.ID, reason); err != nil {
			htmxError(w, r, "Could not deactivate account", http.StatusInternalServerError)
			return
		}
		// Award Emotional Damage to users whose posts this person argued with recently
		go dbpkg.TryAwardTrollOnDeactivation(h.db, user.ID)
		if !facetRequest(r) {
			// A client with no cookie to clear is signed out by ending the
			// sessions themselves — otherwise the bearer token it still holds
			// keeps working on a deactivated account.
			h.endEverySession(r, user)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Clear their session cookie and redirect to the goodbye page
		ClearSessionCookie(w)
		w.Header().Set("HX-Redirect", "/deactivated")
		w.WriteHeader(http.StatusOK)
		return

	case "switch_account":
		user := h.userFromRequest(w, r)
		if user == nil || user.PIALID == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		accountID := r.FormValue("account_id")
		if accountID == "" && rawBodyMap != nil {
			json.Unmarshal(rawBodyMap["account_id"], &accountID)
		}
		if accountID == "" {
			http.Error(w, "account_id required", http.StatusBadRequest)
			return
		}
		rawToken := GetSessionToken(r)
		if err := dbpkg.SwitchActiveAccount(h.db, rawToken, accountID, user.PIALID); err != nil {
			htmxError(w, r, "Could not switch account", http.StatusForbidden)
			return
		}
		// Bust the session cache so the very next request resolves the new active account.
		h.sessionCache.Delete(rawToken)
		// Stay on the accounts page so the user sees the active badge move.
		w.Header().Set("HX-Redirect", "/settings/personas")
		w.WriteHeader(http.StatusNoContent)
		return

	// D-070: article create/update — same logic as /api/article/save.
	// event_type=publish_article is the correct mutation lane for article saves.
	case "publish_article":
		h.saveArticle(w, r)
		return

	case "watch_post", "unwatch_post":
		// FA Live: the client says which works are on screen, so engagement
		// events are delivered for those and no others. Both halves are set
		// operations on the person's watch context — watching what is already
		// watched, or dropping what was never there, is the same answer as
		// doing it once, which is what lets the client fire these off a
		// debounced scroll without keeping a ledger of what it has sent.
		user := h.userFromRequest(w, r)
		if user == nil || user.PIALID == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		postID := eventField(r, rawBodyMap, "post_id", "work_id")
		if postID == "" {
			http.Error(w, "post_id required", http.StatusBadRequest)
			return
		}
		if eventType == "watch_post" {
			AddPostWatcher(user.PIALID, postID)
		} else {
			RemovePostWatcher(user.PIALID, postID)
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case "work_like", "work_unlike", "work_repost", "work_unrepost", "work_bookmark", "work_unbookmark", "work_dislike", "work_undislike":
		h.workReactionEvent(w, r, eventType, rawBodyMap)
		return

	case "work_delete":
		h.workDeleteEvent(w, r, rawBodyMap)
		return

	case "content.reveal":
		h.contentRevealEvent(w, r)
		return

	case "creator.setup.complete":
		h.handleCreatorSetupComplete(w, r, rawBodyMap)
		return

	case "creator.plan.create":
		h.creatorPlanCreateEvent(w, r, rawBodyMap)
		return
	case "creator.plan.update":
		h.creatorPlanUpdateEvent(w, r, rawBodyMap)
		return
	case "creator.plan.delete":
		h.creatorPlanDeleteEvent(w, r, rawBodyMap)
		return

	default:
		// The events the native clients send that the web has no button for
		// yet: visions' room, live rooms, and the account settings screens.
		// Each dispatcher answers and returns true only for its own names.
		if h.apiV1VisionEvent(w, r, eventType, rawBodyMap) ||
			h.apiV1LiveEvent(w, r, eventType, rawBodyMap) ||
			h.apiV1FrequencyEvent(w, r, eventType, rawBodyMap) ||
			h.apiV1MessageEvent(w, r, eventType, rawBodyMap) ||
			h.apiV1NumberEvent(w, r, eventType, rawBodyMap) ||
			h.apiV1AccountEvent(w, r, eventType, rawBodyMap) {
			return
		}
		http.Error(w, "unknown event_type: "+eventType, http.StatusNotFound)
	}
}

// contentRevealEvent: TODO_WORKS — migrate to works-based age-gate reveal.
func (h *Handler) contentRevealEvent(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotFound)
}

// nexusAddPersonaFlow is kept for backward compat — redirects to personas settings.
func (h *Handler) nexusAddPersonaFlow(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("HX-Redirect", "/settings/personas")
	w.WriteHeader(http.StatusOK)
}

// nexusHandleSearch returns a list of handles matching ?q= for the link-account search box.
// Returns up to 8 results, excludes the current user and already-linked shards.
func (h *Handler) nexusHandleSearch(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	q := r.URL.Query().Get("q")
	if len(q) > 0 && q[0] == '@' {
		q = q[1:]
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(q) < 1 {
		fmt.Fprint(w, `<div class="nexus-search-empty">Type a handle to search</div>`)
		return
	}

	rows, err := h.db.Query(
		`SELECT u.handle, COALESCE(p.display_name,''), COALESCE(p.avatar_url,'')
		   FROM users u
		   LEFT JOIN user_profiles p ON p.user_id = u.id
		  WHERE u.handle ILIKE $1
		    AND u.id != $2
		    AND u.pial_id IS NOT NULL
		  ORDER BY u.handle
		  LIMIT 8`,
		q+"%", user.ID,
	)
	if err != nil {
		fmt.Fprint(w, `<div class="nexus-search-empty">Search unavailable</div>`)
		return
	}
	defer rows.Close()

	type result struct{ Handle, DisplayName, AvatarURL string }
	var results []result
	for rows.Next() {
		var res result
		rows.Scan(&res.Handle, &res.DisplayName, &res.AvatarURL)
		results = append(results, res)
	}
	if len(results) == 0 {
		fmt.Fprintf(w, `<div class="nexus-search-empty">No accounts found for "@%s"</div>`, q)
		return
	}

	for _, res := range results {
		name := res.DisplayName
		if name == "" {
			name = "@" + res.Handle
		}
		avatarHTML := ""
		if res.AvatarURL != "" {
			avatarHTML = fmt.Sprintf(`<img class="nexus-search-avatar" src="%s" alt="">`, res.AvatarURL)
		} else {
			avatarHTML = fmt.Sprintf(`<span class="nexus-search-avatar nexus-search-initials">%s</span>`, string([]rune(res.Handle)[0:1]))
		}
		fmt.Fprintf(w, `<button class="nexus-search-row"
  hx-post="/events"
  hx-vals="{&quot;event_type&quot;:&quot;nexus.persona.link.initiated&quot;,&quot;target_handle&quot;:&quot;@%s&quot;}"
  hx-target="#nexus-link-result"
  hx-swap="innerHTML"
  type="button">
  %s
  <div class="nexus-search-info">
    <span class="nexus-search-handle">@%s</span>
    <span class="nexus-search-name">%s</span>
  </div>
  <span class="nexus-search-add">Send request</span>
</button>`, res.Handle, avatarHTML, res.Handle, name)
	}
}

// nexusPersonaSwitcherPartial renders the persona switcher partial for HTMX.
// Returns the persona-switcher.html fragment populated with the user's NEXUS personas.
func (h *Handler) nexusPersonaSwitcherPartial(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	sess := nexus.GetSessionForUser(user.PIALID, h.nexusClient)
	if sess == nil || len(sess.AllPersonaShards) < 2 {
		// Single-account user — return an empty fragment.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!-- no nexus -->`)
		return
	}

	// A Facet fragment, delivered as one. It was going through h.render, which
	// wants a registered full page and answers "unknown page" for anything else —
	// and even had it been registered, the page path executes base.html, so the
	// whole shell would have come back where a switcher panel was asked for.
	// persona-switcher.html defines "persona_switcher" and takes the Nexus
	// context directly (.Personas), not a page data map.
	ctx := buildNexusContext(user, sess, h)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "persona_switcher", ctx)
}

// settingsPersonas renders the persona management settings page.
// Also fetches incoming pending link requests so the user can accept/decline.
func (h *Handler) settingsPersonas(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	sess := nexus.GetSessionForUser(user.PIALID, h.nexusClient)
	ctx := buildNexusContext(user, sess, h)

	// Fetch incoming link requests (target = this user).
	type IncomingRequest struct {
		RequestID   string
		FromHandle  string
		FromAvatar  string
		InitiatedAt string
	}
	var incoming []IncomingRequest
	if user.PIALID != "" {
		targetShard := nexus.PersonaShardFromPIAL(user.PIALID)
		if pending, err := h.nexusClient.ListPendingLinks(targetShard); err == nil && pending != nil {
			for _, req := range pending.Requests {
				ir := IncomingRequest{
					RequestID:   req.RequestID,
					InitiatedAt: req.InitiatedAt,
				}
				// Resolve source shard → handle + avatar via pgcrypto.
				if req.SourcePIALShardID != "" && h.db != nil {
					var handle, avatar string
					h.db.QueryRow(
						`SELECT u.handle, COALESCE(p.avatar_url,'')
						   FROM users u
						   LEFT JOIN user_profiles p ON p.user_id = u.id
						  WHERE encode(digest(u.pial_id::text || ':nexus-pial-shard-v1','sha256'),'hex') = $1`,
						req.SourcePIALShardID,
					).Scan(&handle, &avatar)
					ir.FromHandle = handle
					ir.FromAvatar = avatar
				}
				incoming = append(incoming, ir)
			}
		}
	}

	var linkedAccounts []model.LinkedAccount
	if user.PIALID != "" && h.db != nil {
		linkedAccounts, _ = dbpkg.GetAccountsForPIAL(h.db, user.PIALID, user.ID)
	}

	data := h.baseData(user)
	data["Title"] = "Accounts · F33D3R"
	data["NexusCtx"] = ctx
	data["HasNexus"] = sess != nil
	data["Incoming"] = incoming
	data["LinkedAccounts"] = linkedAccounts
	h.render(w, r, "settings/personas.html", data)
}

// addAccountPage renders the add-account form.
func (h *Handler) addAccountPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	data := h.baseData(user)
	data["Title"] = "Add Account · F33D3R"
	h.render(w, r, "settings/add_account.html", data)
}

// handleAddAccount creates a new account bound to the current user's PIAL.
// No new PIAL is created — the new account shares identity with the existing one.
func (h *Handler) handleAddAccount(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	renderErr := func(msg string) {
		data := h.baseData(user)
		data["Title"] = "Add Account · F33D3R"
		data["Error"] = msg
		h.render(w, r, "settings/add_account.html", data)
	}

	handle := strings.TrimSpace(strings.ToLower(r.FormValue("handle")))
	if !handleRe.MatchString(handle) {
		renderErr("Handle must be 1–30 characters, letters/numbers/underscore only.")
		return
	}
	if reservedHandle(handle) {
		renderErr("That handle is reserved. Try another.")
		return
	}
	password := r.FormValue("password")
	confirmPassword := r.FormValue("confirm_password")
	if len(password) < 8 {
		renderErr("Password must be at least 8 characters.")
		return
	}
	if password != confirmPassword {
		renderErr("Passwords don't match.")
		return
	}

	taken, _ := dbpkg.IsHandleTaken(h.db, handle)
	if taken {
		renderErr("That handle is already taken. Try another.")
		return
	}

	// The handle is allocated at the registrar before the local row exists.
	// This account joins an identity that already has a PIAL, so there is
	// nothing to mint — only the grant to obtain.
	if aerr := h.allocateHandle(r, handle, user.PIALID); aerr != nil {
		renderErr(aerr.Error())
		return
	}

	newID, err := dbpkg.UpsertUserByHandle(h.db, handle)
	if err != nil {
		log.Printf("[add-account] upsert error: %v", err)
		renderErr("Could not create account — try again.")
		return
	}
	if err := dbpkg.SetPassword(h.db, newID, password); err != nil {
		log.Printf("[add-account] SetPassword error: %v", err)
		renderErr("Could not save password — try again.")
		return
	}

	displayName := strings.TrimSpace(r.FormValue("display_name"))
	if displayName == "" {
		displayName = handle
	}
	if err := dbpkg.SaveProfile(h.db, &model.ProfileSave{
		UserID:      newID,
		DisplayName: truncate(displayName, 100),
		ThemeID:     user.ThemeID,
	}); err != nil {
		log.Printf("[add-account] SaveProfile error for %s: %v", handle, err)
		renderErr("Could not save the new account's profile — try again.")
		return
	}

	if err := dbpkg.BindAdditionalAccount(h.db, user.PIALID, newID); err != nil {
		log.Printf("[add-account] BindAdditionalAccount error: %v", err)
		renderErr("Could not link account to identity — try again.")
		return
	}

	if err := dbpkg.UpsertUserRole(h.db, &dbpkg.UserRole{
		UserID:      newID,
		RoleType:    "user",
		IsAdult:     user.IsAdult,
		IsMinor:     user.IsMinor,
		OnboardDone: true,
	}); err != nil {
		log.Printf("[add-account] UpsertUserRole error for %s: %v", handle, err)
		renderErr("Could not finish setting up the new account — try again.")
		return
	}

	dbpkg.LogPIALEvent(h.db, user.PIALID, newID, "account_added", map[string]interface{}{
		"handle": handle,
	}, "nantar")

	http.Redirect(w, r, "/settings/personas", http.StatusSeeOther)
}

// ── Add-account modal handlers ────────────────────────────────────────────────

func (h *Handler) facetAddAccountModal(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	h.renderPartial(w, "add_account_modal", map[string]interface{}{
		"User": user,
	})
}

func (h *Handler) partialAamFork(w http.ResponseWriter, r *http.Request) {
	if h.userFromRequest(w, r) == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	h.renderPartial(w, "add_account_modal_fork", nil)
}

func (h *Handler) partialAamCreate(w http.ResponseWriter, r *http.Request) {
	if h.userFromRequest(w, r) == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	h.renderPartial(w, "add_account_modal_create_step", nil)
}

func (h *Handler) partialAamLink(w http.ResponseWriter, r *http.Request) {
	if h.userFromRequest(w, r) == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	h.renderPartial(w, "add_account_modal_link_step", nil)
}

// handleAamCreate creates a new account in the modal context, returning
// fragments instead of a full-page redirect.
func (h *Handler) handleAamCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	renderErr := func(msg string) {
		w.Header().Set("Content-Type", "text/html")
		h.renderPartial(w, "add_account_modal_create_step", map[string]interface{}{
			"Error": msg,
		})
	}

	handle := strings.TrimSpace(strings.ToLower(r.FormValue("handle")))
	if !handleRe.MatchString(handle) {
		renderErr("Handle must be 1–30 characters, letters/numbers/underscore only.")
		return
	}
	password := r.FormValue("password")
	confirm := r.FormValue("confirm_password")
	if len(password) < 8 {
		renderErr("Password must be at least 8 characters.")
		return
	}
	if password != confirm {
		renderErr("Passwords don't match.")
		return
	}
	taken, _ := dbpkg.IsHandleTaken(h.db, handle)
	if taken {
		renderErr("That handle is already taken.")
		return
	}
	// Allocated at the registrar first, for the same reason as everywhere else:
	// the local row records a grant, it does not make one.
	if aerr := h.allocateHandle(r, handle, user.PIALID); aerr != nil {
		renderErr(aerr.Error())
		return
	}

	newID, err := dbpkg.UpsertUserByHandle(h.db, handle)
	if err != nil {
		log.Printf("[aam-create] upsert: %v", err)
		renderErr("Could not create account — try again.")
		return
	}
	if err := dbpkg.SetPassword(h.db, newID, password); err != nil {
		log.Printf("[aam-create] set password: %v", err)
		renderErr("Could not save password — try again.")
		return
	}
	displayName := strings.TrimSpace(r.FormValue("display_name"))
	if displayName == "" {
		displayName = handle
	}
	if err := dbpkg.SaveProfile(h.db, &model.ProfileSave{
		UserID:      newID,
		DisplayName: truncate(displayName, 100),
		ThemeID:     user.ThemeID,
	}); err != nil {
		log.Printf("[aam-create] SaveProfile error for %s: %v", handle, err)
		renderErr("Could not save the new account's profile — try again.")
		return
	}
	if err := dbpkg.BindAdditionalAccount(h.db, user.PIALID, newID); err != nil {
		log.Printf("[aam-create] bind: %v", err)
		renderErr("Could not link account — try again.")
		return
	}
	if err := dbpkg.UpsertUserRole(h.db, &dbpkg.UserRole{
		UserID:      newID,
		RoleType:    "user",
		IsAdult:     user.IsAdult,
		IsMinor:     user.IsMinor,
		OnboardDone: true,
	}); err != nil {
		log.Printf("[aam-create] UpsertUserRole error for %s: %v", handle, err)
		renderErr("Could not finish setting up the new account — try again.")
		return
	}
	dbpkg.LogPIALEvent(h.db, user.PIALID, newID, "account_added", map[string]interface{}{
		"handle": handle,
	}, "nantar")

	// Generate backup codes for the new account and show them in the modal.
	// The account has no codes yet; the backup-codes gate only fires at password
	// login, which linked accounts never go through.
	newCodes := make([]string, 8)
	for i := range newCodes {
		c, err := dbpkg.GenerateBackupCode()
		if err != nil {
			log.Printf("[aam-create] gen code: %v", err)
			continue
		}
		newCodes[i] = c
	}
	// These codes are about to be shown to the person as their way back into the
	// account. If the store failed they would save eight strings that unlock
	// nothing and only find out when they are already locked out.
	if err := dbpkg.StoreBackupCodes(h.db, newID, newCodes); err != nil {
		log.Printf("[aam-create] CRITICAL: backup codes not stored for %s: %v", handle, err)
		htmxError(w, r, "Your account was created but its recovery codes could not be saved. "+
			"Generate new codes from Settings before signing out.", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	h.renderPartial(w, "add_account_modal_codes_step", map[string]interface{}{
		"Codes":  newCodes,
		"Handle": handle,
	})
}

// handleLinkAccount links an existing independently-created account to the
// current user's PIAL. Ownership is proved by a valid backup code for the
// target account. On success the target's codes are rotated and returned
// for the user to save.
func (h *Handler) handleLinkAccount(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	handle := strings.TrimSpace(strings.ToLower(r.FormValue("handle")))
	code := strings.TrimSpace(r.FormValue("backup_code"))

	renderErr := func(msg string) {
		w.Header().Set("Content-Type", "text/html")
		h.renderPartial(w, "add_account_modal_link_step", map[string]interface{}{
			"Error":  msg,
			"Handle": handle,
		})
	}

	if handle == "" || code == "" {
		renderErr("Enter the account handle and a backup code.")
		return
	}

	target, err := dbpkg.GetUserByHandle(h.db, handle)
	// Always spend bcrypt time to prevent timing-based enumeration.
	if err != nil || target == nil {
		dbpkg.GenerateBackupCode() //nolint:errcheck
		renderErr("Invalid handle or backup code.")
		return
	}
	if target.ID == user.ID {
		renderErr("That's your current account.")
		return
	}

	existing, _ := dbpkg.GetAccountsForPIAL(h.db, user.PIALID, user.ID)
	for _, a := range existing {
		if a.ID == target.ID {
			renderErr("That account is already linked to yours.")
			return
		}
	}

	// Validate link eligibility before consuming the backup code.
	var otherCount int
	h.db.QueryRow(`
		SELECT COUNT(*) FROM pial_account_bindings
		WHERE pial_id = (SELECT pial_id FROM users WHERE id = $1)
		  AND account_id != $1 AND status = 'active'
	`, target.ID).Scan(&otherCount)
	if otherCount > 0 {
		renderErr("That account already has linked accounts.")
		return
	}

	ok, err := dbpkg.VerifyAndConsumeBackupCode(h.db, target.ID, code)
	if err != nil {
		log.Printf("[link-account] verify: %v", err)
		renderErr("Something went wrong — try again.")
		return
	}
	if !ok {
		renderErr("Invalid backup code.")
		return
	}

	if err := dbpkg.LinkExistingAccount(h.db, user.PIALID, target.ID); err != nil {
		log.Printf("[link-account] link: %v", err)
		renderErr("Could not link account — try again.")
		return
	}

	newCodes := make([]string, 8)
	for i := range newCodes {
		c, err := dbpkg.GenerateBackupCode()
		if err != nil {
			log.Printf("[link-account] gen code: %v", err)
			continue
		}
		newCodes[i] = c
	}
	// Rotated codes replace the ones just spent to prove ownership. Shown to the
	// person as their new way in, so they must exist before they are shown.
	if err := dbpkg.StoreBackupCodes(h.db, target.ID, newCodes); err != nil {
		log.Printf("[link-account] CRITICAL: rotated backup codes not stored for %s: %v", handle, err)
		htmxError(w, r, "The account was linked but its new recovery codes could not be saved. "+
			"Generate new codes from Settings before signing out.", http.StatusInternalServerError)
		return
	}

	dbpkg.LogPIALEvent(h.db, user.PIALID, target.ID, "account_linked", map[string]interface{}{
		"handle": handle,
	}, "nantar")

	w.Header().Set("Content-Type", "text/html")
	h.renderPartial(w, "add_account_modal_codes_step", map[string]interface{}{
		"Codes":  newCodes,
		"Handle": handle,
	})
}

// buildNexusContext assembles a model.NexusContext from a nexus.Session.
// Returns nil when sess is nil (single-account path).
// Enriches each persona shard with user profile data via a pgcrypto shard query.
func buildNexusContext(user *model.User, sess *nexus.Session, h *Handler) *model.NexusContext {
	if sess == nil {
		return nil
	}

	// Build a map of pial_shard → user info using pgcrypto to match shards.
	type shardUser struct {
		handle      string
		displayName string
		avatarURL   string
	}
	shardMap := map[string]shardUser{}
	if h.db != nil && len(sess.AllPersonaShards) > 0 {
		rows, err := h.db.Query(
			`SELECT encode(digest(u.pial_id::text || ':nexus-pial-shard-v1', 'sha256'), 'hex'),
			        u.handle,
			        COALESCE(p.display_name,''),
			        COALESCE(p.avatar_url,'')
			   FROM users u
			   LEFT JOIN user_profiles p ON p.user_id = u.id
			  WHERE encode(digest(u.pial_id::text || ':nexus-pial-shard-v1', 'sha256'), 'hex') = ANY($1)`,
			pq.Array(sess.AllPersonaShards),
		)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var shard, handle, display, avatar string
				if rows.Scan(&shard, &handle, &display, &avatar) == nil {
					shardMap[shard] = shardUser{handle: handle, displayName: display, avatarURL: avatar}
				}
			}
		}
	}

	active := &model.NexusPersona{
		PIALShardID: sess.ActivePIALShard,
		Handle:      user.Handle,
		DisplayName: user.DisplayName,
		AvatarURL:   user.AvatarURL,
		IsActive:    true,
		IsPrimary:   true,
		PersonaType: "personal",
	}

	personas := make([]model.NexusPersona, 0, len(sess.AllPersonaShards))
	for _, shard := range sess.AllPersonaShards {
		p := model.NexusPersona{
			PIALShardID: shard,
			PersonaType: "personal",
			IsActive:    shard == sess.ActivePIALShard,
		}
		if info, ok := shardMap[shard]; ok {
			p.Handle = info.handle
			p.DisplayName = info.displayName
			p.AvatarURL = info.avatarURL
		}
		// Mark primary as the first shard (Verity returns them ordered primary-first).
		if len(personas) == 0 {
			p.IsPrimary = true
		}
		personas = append(personas, p)
		if shard == sess.ActivePIALShard {
			*active = p
			active.IsActive = true
		}
	}

	return &model.NexusContext{
		ActivePersona: active,
		Personas:      personas,
	}
}

// ── Legal ─────────────────────────────────────────────────────────────────────

func (h *Handler) legal2257Page(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	h.render(w, r, "legal_2257.html", map[string]interface{}{
		"User":       user,
		"Title":      "18 U.S.C. § 2257 Compliance · F33D3R",
		"SessionID":  uuid.New().String(),
		"ShowScores": h.cfg.ShowScores,
		"Themes":     ThemesWithActive(user.ThemeID),
	})
}

// ── Herald push notification helpers ─────────────────────────────────────────

// vapidPublicKey serves GET /api/push/vapid-key — returns the VAPID public key
// so the browser can subscribe to push. Proxied to Herald in production.
func (h *Handler) vapidPublicKey(w http.ResponseWriter, r *http.Request) {
	// TODO: proxy to Herald /v1/vapid-public-key when HERALD_URL is configured.
	heraldURL := h.cfg.HeraldURL
	if heraldURL == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"public_key": "not-configured"})
		return
	}
	// Proxy the request to Herald.
	resp, err := http.Get(heraldURL + "/v1/vapid-public-key")
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "herald_unavailable"})
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// partialPushPrompt serves GET /partials/push-prompt — returns the
// EventType(push_prompt_modal) fragment for HTMX injection into #portal-modal.
func (h *Handler) partialPushPrompt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "push_prompt", nil)
}

// heraldSubscribeJSON forwards a push subscription to Herald.
// Takes the pre-parsed subscription RawMessage so the body is not re-read.
func (h *Handler) heraldSubscribeJSON(pialID string, subRaw json.RawMessage, deviceID, userAgent string) {
	type subJSON struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	var sub subJSON
	if err := json.Unmarshal(subRaw, &sub); err != nil || sub.Endpoint == "" {
		log.Printf("[herald] subscription parse: %v (raw=%s)", err, subRaw)
		return
	}
	if deviceID == "" {
		deviceID = pialID + "-default"
	}
	payload, _ := json.Marshal(map[string]string{
		"pial_shard_id": pialID,
		"device_id":     deviceID,
		"endpoint":      sub.Endpoint,
		"p256dh_key":    sub.Keys.P256dh,
		"auth_key":      sub.Keys.Auth,
		"user_agent":    userAgent,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.HeraldURL+"/v1/subscriptions", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[herald] subscribe: %v", err)
		return
	}
	resp.Body.Close()
	log.Printf("[herald] subscribe: device=%s status=%d", deviceID, resp.StatusCode)
}

// heraldUnsubscribe calls Herald DELETE /v1/subscriptions/{device_id}.
func (h *Handler) heraldUnsubscribe(deviceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, h.cfg.HeraldURL+"/v1/subscriptions/"+deviceID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[herald] unsubscribe: %v", err)
		return
	}
	resp.Body.Close()
}

// HeraldNotify sends a push notification via Herald for a given PIAL.
// Call this fire-and-forget (go h.HeraldNotify(...)) after creating a DB notification.
func (h *Handler) HeraldNotify(pialID, notifType, title, body, iconURL, actionURL string) {
	if h.cfg.HeraldURL == "" || pialID == "" {
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"pial_shard_id":     pialID,
		"notification_type": notifType,
		"title":             title,
		"body":              body,
		"icon_url":          iconURL,
		"action_url":        actionURL,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.HeraldURL+"/v1/notify", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[herald] notify: %v", err)
		return
	}
	resp.Body.Close()
}
