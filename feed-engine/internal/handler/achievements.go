package handler

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/f33d3r/feed-engine/internal/model"
)

func (h *Handler) achievementsPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)

	type achDef struct {
		id, name, desc, icon, tier string
		earned                     func(*model.User) bool
	}
	catalog := []achDef{
		{"first_post",    "First Post",      "Posted your first thought on F33D3R",  "✍️",  "bronze",   func(u *model.User) bool { return u.PostCount >= 1 }},
		{"getting_social","Getting Social",  "Reached 10 followers",                 "🤝",  "bronze",   func(u *model.User) bool { return u.FollowerCount >= 10 }},
		{"prolific",      "Prolific",        "Posted 100 times",                     "📝",  "silver",   func(u *model.User) bool { return u.PostCount >= 100 }},
		{"initiate",      "Initiate",        "Reached Initiate realm",               "🌱",  "bronze",   func(u *model.User) bool { return u.Realm >= 2 }},
		{"seeker",        "Seeker",          "Reached Seeker realm",                 "🔭",  "silver",   func(u *model.User) bool { return u.Realm >= 3 }},
		{"adept",         "Adept",           "Reached Adept realm",                  "⚡",  "silver",   func(u *model.User) bool { return u.Realm >= 4 }},
		{"guardian",      "Guardian",        "Reached Guardian realm — top tier",    "🛡️",  "gold",     func(u *model.User) bool { return u.Realm >= 5 }},
		{"popular",       "Popular",         "Gained 100 followers",                 "🌟",  "silver",   func(u *model.User) bool { return u.FollowerCount >= 100 }},
		{"celebrity",     "Celebrity",       "Gained 1,000 followers",               "💫",  "gold",     func(u *model.User) bool { return u.FollowerCount >= 1000 }},
		{"influencer",    "Influencer",      "Gained 10,000 followers",              "🚀",  "platinum", func(u *model.User) bool { return u.FollowerCount >= 10000 }},
		{"verified",      "Verified",        "Completed identity verification",      "✅",  "gold",     func(u *model.User) bool { return u.IsVerified }},
		{"creator",       "Creator",         "Enabled creator monetization",         "💰",  "gold",     func(u *model.User) bool { return u.IsCreator }},
	}

	now := time.Now()
	achievements := make([]model.Achievement, len(catalog))
	for i, d := range catalog {
		a := model.Achievement{ID: d.id, Name: d.name, Description: d.desc, Icon: d.icon, Tier: d.tier}
		if d.earned(user) { a.EarnedAt = &now }
		achievements[i] = a
	}

	h.render(w, "achievements.html", map[string]interface{}{
		"User":         user,
		"Achievements": achievements,
		"Title":        "Achievements · F33D3R",
		"SessionID":    uuid.New().String(),
		"ShowScores":   h.cfg.ShowScores,
		"Themes":       ThemesWithActive(user.ThemeID),
	})
}
