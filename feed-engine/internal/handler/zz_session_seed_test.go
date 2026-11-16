package handler

import (
	"time"

	"github.com/f33d3r/feed-engine/internal/identity"
	"github.com/f33d3r/feed-engine/internal/model"
)

// seedSession signs tok in as user for tests that render without a database.
//
// The session cache holds only the identity authority's answer, never an
// account row (see sessionEntry), so a test supplies both halves the running
// server would: the answer, cached under tok, and the account, through the
// handler's accountLoader. The answer is derived from user, so what the test
// declares about the account is what the request sees.
func (h *Handler) seedSession(tok string, user *model.User, ttl time.Duration) {
	caps := user.Caps
	if len(caps) == 0 {
		caps = []string{model.CapPosting, model.CapMessaging}
	}
	h.sessionCache.Store(tok, sessionEntry{
		sess: &identity.Session{
			AccountID: user.ID,
			PIALID:    user.PIALID,
			Handle:    user.Handle,
			Role:      user.Role,
			Caps:      caps,
			Status:    identity.StatusActive,
		},
		exp: time.Now().Add(ttl),
	})
	prev := h.accountLoader
	h.accountLoader = func(sess *identity.Session) (*model.User, error) {
		if sess.AccountID == user.ID {
			return user, nil
		}
		if prev != nil {
			return prev(sess)
		}
		return nil, errAccountNotProjected
	}
}
