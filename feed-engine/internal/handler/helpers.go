package handler

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/jung"
	"github.com/f33d3r/feed-engine/internal/model"
)

var handleRe = regexp.MustCompile(`^[a-zA-Z0-9_\-]{1,30}$`)

// reservedHandles are top-level path segments owned by real routes. Because profiles
// live at the clean root URL f33d3r.com/{handle}, a literal route (/settings, /explore,
// /wallet, …) is always more specific than the "/" dispatcher and would silently shadow
// a user who grabbed that handle — the user could never reach their own profile. We block
// these at handle creation (signup + add-account) so the collision can never happen.
// Keep in sync with the top-level routes registered in Routes(). Lowercase; handles are
// lowercased before the check. A few non-route words (root/home/admin/www/etc.) are
// reserved defensively for future routes and to prevent impersonation.
var reservedHandles = map[string]bool{
	// Auth / onboarding
	"onboard": true, "login": true, "logout": true, "signin": true, "signup": true,
	"register": true, "auth": true, "oauth": true, "deactivated": true,
	"forgot-password": true, "backup-codes": true, "kyc": true, "verity": true,
	// Primary surfaces
	"explore": true, "music": true, "create": true, "nsfw": true, "video": true,
	"visions": true, "works": true, "work": true, "communities": true, "lists": true,
	"spheres": true, "spaces": true, "marketplace": true, "buyer-library": true,
	"notifications": true, "messages": true, "bookmarks": true, "profile": true,
	"settings": true, "search": true, "wallet": true, "post": true, "feed": true,
	"shop": true, "stocks": true, "events": true, "library": true, "ledger": true,
	"upload": true, "react-video": true, "tag": true, "org": true, "achievements": true,
	"analytics": true, "leaderboard": true, "ainsoph": true, "thessalon": true,
	// Infra / system namespaces
	"api": true, "static": true, "media": true, "facets": true, "partials": true,
	"_atlas": true, "u": true, "admin": true, "article": true, "articles": true,
	"sw": true, "robots": true, "sitemap": true, "favicon": true, "assets": true,
	"app": true, "www": true,
	// Legal / info
	"legal": true, "privacy": true, "terms": true, "dmca": true, "about": true,
	"help": true, "support": true, "home": true, "root": true,
}

// reservedHandle reports whether a handle collides with a reserved route segment.
func reservedHandle(handle string) bool {
	return reservedHandles[strings.ToLower(handle)]
}

// countryName maps ISO 3166-1 alpha-2 codes to display names. Used by the
// leaderboard so users see "Japan" instead of "JP".
func countryName(code string) string {
	names := map[string]string{
		"AF": "Afghanistan", "AL": "Albania", "DZ": "Algeria", "AR": "Argentina",
		"AU": "Australia", "AT": "Austria", "AZ": "Azerbaijan", "BS": "Bahamas",
		"BH": "Bahrain", "BD": "Bangladesh", "BY": "Belarus", "BE": "Belgium",
		"BZ": "Belize", "BO": "Bolivia", "BA": "Bosnia & Herzegovina", "BR": "Brazil",
		"BG": "Bulgaria", "KH": "Cambodia", "CA": "Canada", "CL": "Chile",
		"CN": "China", "CO": "Colombia", "HR": "Croatia", "CU": "Cuba",
		"CY": "Cyprus", "CZ": "Czech Republic", "DK": "Denmark", "DO": "Dominican Republic",
		"EC": "Ecuador", "EG": "Egypt", "SV": "El Salvador", "ET": "Ethiopia",
		"FI": "Finland", "FR": "France", "GE": "Georgia", "DE": "Germany",
		"GH": "Ghana", "GR": "Greece", "GT": "Guatemala", "HN": "Honduras",
		"HK": "Hong Kong", "HU": "Hungary", "IS": "Iceland", "IN": "India",
		"ID": "Indonesia", "IR": "Iran", "IQ": "Iraq", "IE": "Ireland",
		"IL": "Israel", "IT": "Italy", "JM": "Jamaica", "JP": "Japan",
		"JO": "Jordan", "KZ": "Kazakhstan", "KE": "Kenya", "KW": "Kuwait",
		"LB": "Lebanon", "LY": "Libya", "LT": "Lithuania", "LU": "Luxembourg",
		"MY": "Malaysia", "MX": "Mexico", "MA": "Morocco", "MM": "Myanmar",
		"NP": "Nepal", "NL": "Netherlands", "NZ": "New Zealand", "NG": "Nigeria",
		"NO": "Norway", "OM": "Oman", "PK": "Pakistan", "PA": "Panama",
		"PY": "Paraguay", "PE": "Peru", "PH": "Philippines", "PL": "Poland",
		"PT": "Portugal", "QA": "Qatar", "RO": "Romania", "RU": "Russia",
		"SA": "Saudi Arabia", "SN": "Senegal", "RS": "Serbia", "SG": "Singapore",
		"SK": "Slovakia", "ZA": "South Africa", "KR": "South Korea", "ES": "Spain",
		"LK": "Sri Lanka", "SD": "Sudan", "SE": "Sweden", "CH": "Switzerland",
		"SY": "Syria", "TW": "Taiwan", "TZ": "Tanzania", "TH": "Thailand",
		"TN": "Tunisia", "TR": "Turkey", "UA": "Ukraine", "AE": "UAE",
		"GB": "United Kingdom", "US": "United States", "UY": "Uruguay",
		"UZ": "Uzbekistan", "VE": "Venezuela", "VN": "Vietnam", "YE": "Yemen",
		"ZM": "Zambia", "ZW": "Zimbabwe",
	}
	if n, ok := names[code]; ok {
		return n
	}
	return ""
}

// ageFromBirthday returns the viewer's age in whole years.
func ageFromBirthday(bday time.Time) int {
	now := time.Now()
	years := now.Year() - bday.Year()
	if now.Month() < bday.Month() || (now.Month() == bday.Month() && now.Day() < bday.Day()) {
		years--
	}
	return years
}

// enrichWithPresence sets IsAuthorOnline on each post from the in-memory SSE presence map.
func (h *Handler) enrichWithPresence(posts []*model.Post) {
	for _, p := range posts {
		if ts, ok := h.onlineUsers.Load(p.AuthorHandle); ok {
			if t, ok := ts.(time.Time); ok && time.Since(t) < 45*time.Second {
				p.IsAuthorOnline = true
			}
		}
	}
}

// isHandleOnline reports whether handle has a live SSE connection (heartbeat < 45s ago).
func (h *Handler) isHandleOnline(handle string) bool {
	if ts, ok := h.onlineUsers.Load(handle); ok {
		if t, ok := ts.(time.Time); ok && time.Since(t) < 45*time.Second {
			return true
		}
	}
	return false
}

// ApplyBirthdayAgeGate overrides IsMinor/IsAdult on the user based on birthday.
// Birthday is the authoritative source; DB flags are only a fallback when no birthday is set.
func ApplyBirthdayAgeGate(u *model.User) {
	if u == nil || u.Birthday == nil {
		return
	}
	age := ageFromBirthday(*u.Birthday)
	if age < 18 {
		u.IsMinor = true
		u.IsAdult = false
	} else {
		u.IsMinor = false
		u.IsAdult = true
	}
}

// excludeNSFW returns true when the viewer must not see NSFW posts at the DB level.
// Minors, safe-mode users, and government/business official accounts are always excluded.
// Standard/adult users see NSFW in the template (blurred or gated by verification status).
func excludeNSFW(u *model.User) bool {
	if u == nil {
		return true
	}
	// Government and business accounts are always in safe mode — they represent institutions.
	if u.OfficialType == "government" || u.OfficialType == "business" {
		return true
	}
	return u.IsMinor || u.ContentSetting == "safe_mode"
}

// filterAdultForViewer strips adult content (porn IsNSFW, gore IsGore, and adult-creator
// authors) from a works slice for viewers who must never see it — minors, safe_mode, and
// government/business accounts (excludeNSFW). 18+ standard/nsfw viewers keep the works; they
// are gated per-item at render time by the content_gate Facet. This is what keeps adult
// creators out of a minor's Trending feed (which otherwise leaks then 403s on click).
func filterAdultForViewer(u *model.User, works []*model.Work) []*model.Work {
	if !excludeNSFW(u) {
		return works
	}
	out := works[:0]
	for _, w := range works {
		if w.IsNSFW || w.IsGore || w.AuthorIsAdultCreator {
			continue
		}
		out = append(out, w)
	}
	return out
}

// hideAdultCreators returns true when adult creator accounts and their content must be
// excluded at the DB level. Only users who have explicitly opted in (adult_enabled AND
// confirmed 18+) may see adult creator accounts — everyone else gets them filtered out.
func hideAdultCreators(u *model.User) bool {
	if u == nil {
		return true
	}
	return !(u.IsAdult && u.ContentSetting == "adult_enabled")
}

// htmxError returns a styled HTML error fragment for HTMX requests,
// or falls back to plain http.Error for non-HTMX requests.
func htmxError(w http.ResponseWriter, r *http.Request, msg string, code int) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(code)
		fmt.Fprintf(w, `<div style="padding:12px 16px;background:color-mix(in srgb,#ef4444 12%%,transparent);border:1px solid color-mix(in srgb,#ef4444 30%%,transparent);border-radius:10px;font-size:13px;color:#fca5a5;margin:8px 0">%s</div>`, msg)
		return
	}
	http.Error(w, msg, code)
}

// TimeAgo returns human-readable relative time.
func TimeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

// SafetyEpsilon maps content_setting to AethyrRank epsilon.
func SafetyEpsilon(contentSetting string) float64 {
	switch contentSetting {
	case "safe_mode":
		return 1e-6
	case "adult_enabled":
		return 1.0
	default:
		return 0.10
	}
}

// SessionCookieName is the name of the session token cookie.
const SessionCookieName = "f33d3r_session"

// GetSessionToken reads the session token a request presents.
//
// A browser carries it in the session cookie. A native client — the iOS app,
// the Android app's JSON lane — carries the same token as `Authorization:
// Bearer <token>`: it is issued by the same CreateAuthSession, stored in the
// same user_sessions row and revoked by the same DeleteSession, so every
// route that resolves a session (requireHandle, userFromRequest, the /events
// lane, the signing-key registration) accepts either presentation without
// knowing which one arrived. The header is read first because a device that
// sends one has chosen it deliberately; a cookie on such a request is a
// leftover from a web view and must not outrank it.
func GetSessionToken(r *http.Request) string {
	if tok := bearerSessionToken(r); tok != "" {
		return tok
	}
	if c, err := r.Cookie(SessionCookieName); err == nil {
		return c.Value
	}
	// Legacy fallback — old handle cookie for existing logged-in users
	if c, err := r.Cookie("f33d3r_handle"); err == nil {
		if handleRe.MatchString(c.Value) {
			return "__handle__" + c.Value
		}
	}
	return ""
}

// bearerSessionToken returns the token in an `Authorization: Bearer` header,
// or "" when the request carries none. A legacy handle token is never accepted
// this way: the header form exists for real sessions only.
func bearerSessionToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < 8 || !strings.EqualFold(h[:7], "Bearer ") {
		return ""
	}
	tok := strings.TrimSpace(h[7:])
	if tok == "" || len(tok) > 200 || strings.HasPrefix(tok, "__handle__") {
		return ""
	}
	return tok
}

// SetSessionCookie writes the session token cookie (30-day expiry, HttpOnly).
func SetSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   86400 * 30,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(),
	})
}

// ClearSessionCookie removes the session cookie on logout.
//
// A clear is only honoured when its attributes match the cookie being cleared:
// a Secure cookie is not deleted by a non-Secure Set-Cookie of the same name.
// Every attribute here mirrors the corresponding setter.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(),
	})
	// Also clear legacy handle cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "f33d3r_handle",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(),
	})
}

// cookieSecure is the one place the Secure attribute is decided. Every cookie
// this process sets or clears is Secure except on a plain-HTTP dev listener,
// where a Secure cookie would never be stored at all.
//
// The cookie names keep their unprefixed form: f33d3r_session is read by the
// Android client (mobile/android …/net/Http.kt) and f33d3r_handle by the
// aethyr-walker crawler, so a __Host- rename is a cross-repository change and
// not one this file can make alone.
func cookieSecure() bool {
	return os.Getenv("DEV_MODE") != "true"
}

// HandleFromCookie reads the legacy f33d3r_handle cookie (kept for backward compat).
func HandleFromCookie(r *http.Request) string {
	tok := GetSessionToken(r)
	if tok == "" {
		return ""
	}
	// Legacy handle token
	if len(tok) > 10 && tok[:10] == "__handle__" {
		return tok[10:]
	}
	// For new session tokens, caller must resolve via DB
	return tok
}

// SetHandleCookie is kept for backward compatibility — now writes a handle token.
func SetHandleCookie(w http.ResponseWriter, handle string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "f33d3r_handle",
		Value:    handle,
		Path:     "/",
		MaxAge:   86400 * 30,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(),
	})
}

// DemoUser returns a baseline user for when DB is unavailable.
func DemoUser() *model.User {
	return &model.User{
		ID:          "demo_user",
		Handle:      "you",
		DisplayName: "You",
		ThemeID:     "void",
		Tier:        "free",
		// Anonymous / logged-out visitors are forced into safe_mode so that every
		// existing NSFW guard (excludeNSFW, filterAdultForViewer, hideAdultCreators)
		// strips adult content automatically — no NSFW ever leaks to the public.
		ContentSetting:    "safe_mode",
		SafetyEpsilon:     1e-6,
		IsCreator:         false,
		InterestVector:    jung.Neutral().Slice(),
		RecentContentIDs:  []string{},
		CreatorAffinities: map[string]float32{},
		IsColdStart:       true,
	}
}

// interestBlendPrior is the share of a freshly computed interest vector that
// comes from the declared archetype rather than from observed engagement.
const interestBlendPrior = 0.30

// interestEvidenceLimit is how many of a person's most recent positive
// reactions are read when the interest vector is recomputed from scratch.
const interestEvidenceLimit = 200

// computeInterestVector derives a person's interest vector on the Jung axes
// from evidence: the mean of the psych vectors of the works they most
// recently liked, reposted or bookmarked, blended 70/30 with the prior for
// the archetype they declared on their profile. A person who has engaged
// with nothing yet is exactly their archetype prior — and a person who
// declared none is jung.Neutral(), the honest position for someone about
// whom nothing is known. Nothing here is random and nothing is hashed.
//
// Works in the evidence set that predate the Jung layer get their vector
// computed here and written back, so the evidence converges to stored truth.
func (h *Handler) computeInterestVector(u *model.User) []float32 {
	if u == nil {
		return jung.Neutral().Slice()
	}
	prior := jung.ArchetypePrior(u.JungArchetype)
	if h.db == nil || viewerAccountID(u) == "" {
		return prior.Slice()
	}
	engaged, err := dbpkg.GetEngagedWorksForPsych(h.db, u.ID, interestEvidenceLimit)
	if err != nil {
		log.Printf("[jung] engaged works for %s: %v", u.ID, err)
		return prior.Slice()
	}
	if len(engaged) == 0 {
		return prior.Slice()
	}
	vectors := h.ensureWorkVectors(engaged)
	return jung.Blend(jung.Mean(vectors), prior, interestBlendPrior).Slice()
}

// AvatarColors returns a deterministic gradient pair from a handle seed.
func AvatarColors(seed string) [2]string {
	palettes := [][2]string{
		{"#00C853", "#00897B"}, {"#7C4DFF", "#3D5AFE"},
		{"#FF6D00", "#FF3D00"}, {"#0091EA", "#00B0FF"},
		{"#AA00FF", "#D500F9"}, {"#00BFA5", "#1DE9B6"},
		{"#FFD600", "#FF6F00"}, {"#C51162", "#F50057"},
	}
	h := 0
	for _, c := range seed {
		h = (h*31 + int(c)) % len(palettes)
	}
	return palettes[h]
}

// ThemeAccent returns the accent hex for a given theme ID.
func ThemeAccent(themeID string) string {
	for _, t := range model.AllThemes() {
		if t.ID == themeID {
			return t.Accent
		}
	}
	return "#7B68EE"
}

// ThemeSurface returns the surface hex for a given theme ID.
func ThemeSurface(themeID string) string {
	for _, t := range model.AllThemes() {
		if t.ID == themeID {
			return t.Surface
		}
	}
	return "#08080F"
}

// ThemesWithActive returns all themes with the Active flag set correctly.
func ThemesWithActive(currentThemeID string) []model.Theme {
	themes := model.AllThemes()
	for i := range themes {
		themes[i].Active = themes[i].ID == currentThemeID
	}
	return themes
}

// truncate clips s to maxRunes runes.
func truncate(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes])
}

// isValidTheme returns true if the theme ID is one of the 6 known themes.
func isValidTheme(id string) bool {
	for _, t := range model.AllThemes() {
		if t.ID == id {
			return true
		}
	}
	return false
}

var accentHexRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// suggestionViewer turns the viewer of a page into the identity the one
// people-suggestion owner takes.
//
// It is the single translation from "who is looking at this page" to "who may
// be suggested to them", so every suggestion surface asks the question the same
// way. Two things it settles that a surface must not settle for itself:
//
//   - A viewer with no account (the anonymous "demo_user" sentinel) is passed
//     through as-is. db.SuggestPeople recognises a non-account identifier and
//     simply writes no viewer-relative clauses, rather than sending it to a uuid
//     column and losing the whole statement.
//   - Adult creators are shown only to a viewer who has opted in, through the
//     same hideAdultCreators owner the feeds use. The rail used to be the one
//     people surface that did not ask.
func suggestionViewer(u *model.User) dbpkg.SuggestionViewer {
	if u == nil {
		return dbpkg.SuggestionViewer{}
	}
	return dbpkg.SuggestionViewer{
		ID:                u.ID,
		IsMinor:           u.IsMinor,
		ShowAdultCreators: !hideAdultCreators(u),
	}
}

// railData returns the full right-rail dataset for a given page context.
// context values: "feed" | "explore" | "post" | "profile" | "music" | "search" |
//
//	"sports" | "default"
//
// EVERY LIST BELOW IS SIZED BY railCap, NEVER BY A NUMBER WRITTEN HERE. The rail
// is one column with one height budget, declared panel by panel in
// rail_budget.go; a limit chosen at the call site is how the rail came to be
// twenty-two rows long without anyone deciding it should be. Each panel's
// escape hatch comes from the same declaration, so a panel can only link where
// the budget says a destination exists.
func (h *Handler) railData(user *model.User, context string) map[string]interface{} {
	out := map[string]interface{}{
		"RailContext": context,
	}
	if h.db == nil {
		return out
	}
	// Facet(trending_item) — the rail's own trending list, under its own key.
	// It is RailTrendingTags and not TrendingTags because the Explore surface
	// renders a full trending grid from a key of that name: while the rail and
	// a Playground surface shared one key they also shared one length, and
	// sizing the rail's panel silently resized the Explore grid. The rail's
	// dataset is the rail's own.
	trending, err := dbpkg.GetTrendingTags(h.db, railCap(railPanelTrending))
	if err != nil {
		log.Printf("[rail] trending tags: %v", err)
	}
	out["RailTrendingTags"] = trending
	out["RailTrendingSeeAll"] = railSeeAll(railPanelTrending)

	viewer := suggestionViewer(user)

	// Facet(rail_trending_creators) — "Creators to follow". The creator panel is
	// the SAME suggestion set narrowed to creator accounts, never a second bar:
	// it used to be its own query with its own rules, which is how two panels in
	// one rail came to disagree about who was worth showing.
	var spent []string
	switch context {
	case "feed", "explore", "music", "profile", "sports":
		creators, cErr := dbpkg.SuggestPeople(h.db, viewer, dbpkg.SuggestionQuery{
			Limit:        railCap(railPanelCreators),
			CreatorsOnly: true,
		})
		if cErr != nil {
			log.Printf("[rail] creators to follow: %v", cErr)
		}
		out["RailCreators"] = creators
		for _, c := range creators {
			spent = append(spent, c.ID)
		}
	}

	// Facet(suggested_user_row) — "Who to follow". Anyone already drawn by the
	// creator panel above is spent: the same face twice in one rail is the same
	// padding problem the quality bar exists to end.
	suggested, sErr := dbpkg.SuggestPeople(h.db, viewer, dbpkg.SuggestionQuery{
		Limit:      railCap(railPanelWhoToFollow),
		ExcludeIDs: spent,
	})
	if sErr != nil {
		log.Printf("[rail] who to follow: %v", sErr)
	}
	out["SuggestedUsers"] = suggested
	// The People tab with nothing typed is this panel unabridged — same
	// suggestion owner, same quality bar — so the cap hides nothing a reader
	// cannot still reach.
	out["RailPeopleSeeAll"] = railSeeAll(railPanelWhoToFollow)
	// The panel renders its own empty state, so it has to be able to tell "no
	// one clears the bar" from "this surface never asked". Only a surface that
	// actually resolved a suggestion set sets this.
	out["SuggestedUsersResolved"] = true

	// Facet(rail_news_section) — syndicated headlines. The category depends on
	// context; the length never does, because a headline row is the same height
	// whichever feed it came from.
	if h.rssCache != nil {
		news := railCap(railPanelNews)
		switch context {
		case "music":
			out["RailNewsItems"] = h.rssCache.Get("music", news)
			out["RailNewsLabel"] = "Music news"
		case "feed", "explore", "post", "profile", "sports", "default":
			out["RailNewsItems"] = h.rssCache.Get("news", news)
			out["RailNewsLabel"] = "What's happening"
		}
	}

	// Facet(rail_scorecards) — the right-rail scoreboard: what is being played
	// now and what is on next, ranked across every league the platform polls.
	// nil when the lane is off or no league has anything on, so the rail renders
	// no panel at all rather than an empty box.
	//
	// The "sports" context is deliberately absent: on /sports, /sports/<league>
	// and /sports/<league>/<game> the surface IS the board, and a compact copy
	// of the top of it in the rail beside them is the same information twice on
	// one screen.
	switch context {
	case "feed", "explore", "post", "profile", "default":
		if rail := h.sportsRailData(); rail != nil {
			out["RailScores"] = rail
		}
	}

	return out
}

// withRail merges the whole rail dataset into a surface's template data and
// returns it, so a page can be written as one literal.
//
// It exists because the rail is a single Facet with a single dataset, and a
// handler that hand-picks a few keys out of railData silently drops every panel
// it did not name. That is exactly how the scoreboard panel came to be built,
// styled, wired to the stream and then rendered on no page at all: railData
// produced RailScores and sixteen handlers forwarded four keys each. Merging the
// dataset whole makes the rail's contents the rail's business, and a panel added
// to railData appears on every surface that mounts sidebar_right without any
// handler being edited again.
//
// Keys already present in data win, so a surface that computes its own value for
// a rail key (the sports hub's own LeagueLabel, say) keeps it.
func (h *Handler) withRail(data map[string]interface{}, user *model.User, context string) map[string]interface{} {
	if data == nil {
		data = make(map[string]interface{})
	}
	for k, v := range h.railData(user, context) {
		if _, taken := data[k]; !taken {
			data[k] = v
		}
	}
	return data
}
