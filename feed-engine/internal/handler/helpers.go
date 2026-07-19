package handler

import (
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/f33d3r/feed-engine/internal/aethyr"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
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
	if u == nil { return true }
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
	if u == nil { return true }
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

// GetSessionToken reads the session token from the request cookie.
func GetSessionToken(r *http.Request) string {
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

// SetSessionCookie writes the session token cookie (30-day expiry, HttpOnly).
func SetSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   86400 * 30,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   os.Getenv("DEV_MODE") != "true",
	})
}

// ClearSessionCookie removes the session cookie on logout.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	// Also clear legacy handle cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "f33d3r_handle",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
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
	})
}

// DemoUser returns a baseline user for when DB is unavailable.
func DemoUser() *model.User {
	return &model.User{
		ID:                "demo_user",
		Handle:            "you",
		DisplayName:       "You",
		ThemeID:           "void",
		Tier:              "free",
		// Anonymous / logged-out visitors are forced into safe_mode so that every
		// existing NSFW guard (excludeNSFW, filterAdultForViewer, hideAdultCreators)
		// strips adult content automatically — no NSFW ever leaks to the public.
		ContentSetting:    "safe_mode",
		SafetyEpsilon:     1e-6,
		Realm:             1,
		IsCreator:         false,
		InterestVector:    randomVector(8),
		RecentContentIDs:  []string{},
		CreatorAffinities: map[string]float32{},
		IsColdStart:       true,
	}
}

func randomVector(dim int) []float32 {
	v := make([]float32, dim)
	var sum float32
	for i := range v {
		v[i] = rand.Float32()
		sum += v[i] * v[i]
	}
	if sum > 0 {
		norm := float32(math.Sqrt(float64(sum)))
		for i := range v {
			v[i] /= norm
		}
	}
	return v
}

// computeInterestVector derives a normalised interest vector from a set of author IDs
// (liked/bookmarked post authors and followed users). Each author contributes their
// deterministic topic vector via SeedVector. The result is the normalised average.
// Falls back to randomVector if the input set is empty.
func computeInterestVector(authorIDs []string, followedIDs map[string]bool) []float32 {
	const dim = 8
	// Collect unique IDs from engagements + follows
	seen := make(map[string]bool, len(authorIDs)+len(followedIDs))
	all := make([]string, 0, len(authorIDs)+len(followedIDs))
	for _, id := range authorIDs {
		if !seen[id] {
			seen[id] = true
			all = append(all, id)
		}
	}
	for id := range followedIDs {
		if !seen[id] {
			seen[id] = true
			all = append(all, id)
		}
	}
	if len(all) == 0 {
		return randomVector(dim)
	}

	sum := make([]float64, dim)
	for _, id := range all {
		v := aethyr.SeedVector(id, dim)
		for i, val := range v {
			sum[i] += float64(val)
		}
	}
	n := float64(len(all))
	result := make([]float32, dim)
	var norm float64
	for i := range result {
		result[i] = float32(sum[i] / n)
		norm += float64(result[i]) * float64(result[i])
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for i := range result {
			result[i] /= float32(norm)
		}
	}
	return result
}

// blendInterestVector updates an existing interest vector online using the topic vector
// of an engaged author (exponential moving average, learning rate 0.08).
func blendInterestVector(current []float32, authorID string) []float32 {
	const dim = 8
	const lr = float32(0.08)
	if len(current) != dim {
		return aethyr.SeedVector(authorID, dim)
	}
	update := aethyr.SeedVector(authorID, dim)
	blended := make([]float32, dim)
	var norm float32
	for i := range blended {
		blended[i] = (1-lr)*current[i] + lr*update[i]
		norm += blended[i] * blended[i]
	}
	norm = float32(math.Sqrt(float64(norm)))
	if norm > 0 {
		for i := range blended {
			blended[i] /= norm
		}
	}
	return blended
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

// sidebarData fetches trending tags, suggested users, and trending creators for the right rail.
// Pass context to control which sections are fetched ("feed","post","profile","music","explore","search","default").
func (h *Handler) sidebarData(user *model.User) ([]dbpkg.TrendingTag, []dbpkg.SuggestedUser) {
	if h.db == nil {
		return nil, nil
	}
	trending, err := dbpkg.GetTrendingTags(h.db, 6)
	if err != nil {
		log.Printf("[sidebar] trending tags: %v", err)
	}
	var suggested []dbpkg.SuggestedUser
	if user != nil {
		var sErr error
		suggested, sErr = dbpkg.GetSuggestedUsersFiltered(h.db, user.ID, 3, user.IsMinor)
		if sErr != nil {
			log.Printf("[sidebar] suggested users: %v", sErr)
		}
	}
	return trending, suggested
}

// railData returns the full right-rail dataset for a given page context.
// context values: "feed" | "explore" | "post" | "profile" | "music" | "search" | "default"
func (h *Handler) railData(user *model.User, context string) map[string]interface{} {
	out := map[string]interface{}{
		"RailContext": context,
	}
	if h.db == nil {
		return out
	}
	trending, err := dbpkg.GetTrendingTags(h.db, 6)
	if err != nil {
		log.Printf("[rail] trending tags: %v", err)
	}
	out["TrendingTags"] = trending

	if user != nil {
		suggested, err := dbpkg.GetSuggestedUsersFiltered(h.db, user.ID, 3, user.IsMinor)
		if err != nil {
			log.Printf("[rail] suggested users for %s: %v", user.ID, err)
		}
		out["SuggestedUsers"] = suggested
	}

	switch context {
	case "feed", "explore", "music", "profile":
		creators, cErr := dbpkg.GetTrendingCreators(h.db, 3)
		if cErr != nil {
			log.Printf("[rail] trending creators: %v", cErr)
		}
		out["RailCreators"] = creators
	}

	// RSS news items — category depends on context
	if h.rssCache != nil {
		switch context {
		case "music":
			out["RailNewsItems"] = h.rssCache.Get("music", 4)
			out["RailNewsLabel"] = "Music news"
		case "feed", "explore", "post", "profile", "default":
			out["RailNewsItems"] = h.rssCache.Get("news", 4)
			out["RailNewsLabel"] = "What's happening"
		}
	}

	return out
}
