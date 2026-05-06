package handler

import (
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/f33d3r/feed-engine/internal/model"
)

var handleRe = regexp.MustCompile(`^[a-zA-Z0-9_\-]{1,30}$`)

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
		Secure:   false, // set true in production (HTTPS)
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
		ContentSetting:    "default",
		SafetyEpsilon:     0.10,
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
