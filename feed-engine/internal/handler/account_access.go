package handler

import (
	"github.com/f33d3r/feed-engine/internal/model"
)

// mayPublish is the one check in front of putting anything new in front of people:
// posts, Visions, live, Frequencies, articles and music all answer to it. It reads
// the capabilities elohim-veni resolved for this session — the viewer's own,
// current to within the session cache.
func (h *Handler) mayPublish(user *model.User) bool {
	return user.Can(model.CapPosting)
}

const publishRestricted = "Posting is restricted on this account"
