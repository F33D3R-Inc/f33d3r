package handler

import (
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// Celebrations are admin-scheduled seasonal themes (see the `celebrations` table
// and db.GetActiveCelebration). The active theme tags the shell as
// body.celebrate-<theme>; the like-button effect + flair CSS live in
// static/css/celebrations.css. They are ON for everyone by default — a logged-in
// user opts out via user.CelebrationsEnabled == false. Adding a theme is a CSS
// block + a `celebrations` row; no Go change.

// celebrationClass returns the body class for a viewer ("" when none applies):
// empty unless an admin-enabled celebration covers today AND the viewer hasn't
// opted out. Logged-out viewers (user == nil) always see the active celebration.
func (h *Handler) celebrationClass(user *model.User) string {
	c, err := dbpkg.GetActiveCelebration(h.db)
	if err != nil || c == nil {
		return ""
	}
	if user != nil && !user.CelebrationsEnabled {
		return ""
	}
	return "celebrate-" + c.Theme
}

// currentCelebrationName returns the active celebration's name ("" when none) —
// used to label the settings opt-out. Independent of the viewer's opt-out.
func (h *Handler) currentCelebrationName() string {
	c, err := dbpkg.GetActiveCelebration(h.db)
	if err != nil || c == nil {
		return ""
	}
	return c.Name
}
