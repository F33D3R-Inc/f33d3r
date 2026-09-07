package handler

// author_wall.go — the account-level authorization authority.
//
// Every read path that hands a viewer somebody else's content has to answer the
// same three questions: is this viewer the owner, does a block stand between
// them, and — for a private account — does the viewer actually follow the author.
// workWall (works_feed.go) and the profile privateWall (profile.go) each answer
// them inline, which is how the /media/ read path came to answer none of them.
// authorWall is the one place that answer is computed.
//
// FA doctrine note: workWall (internal/handler/works_feed.go:457) and the
// privateWall/adultGate block (internal/handler/profile.go:85) still carry their
// own copies of this logic. They must delegate to authorWall so a fourth read
// path cannot drift again; those two files are owned elsewhere this session, so
// the consolidation is flagged here rather than applied.

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── Bounded TTL cache ────────────────────────────────────────────────────────

type ttlEntry struct {
	val any
	exp time.Time
}

// ttlCache is a bounded, expiry-aware memo shared by the authorization read
// paths. It holds decisions the server already computed; the server remains the
// sole authority — nothing here is client-supplied.
type ttlCache struct {
	m   sync.Map
	n   atomic.Int64
	max int64
}

func (c *ttlCache) get(key string) (any, bool) {
	v, ok := c.m.Load(key)
	if !ok {
		return nil, false
	}
	e := v.(ttlEntry)
	if time.Now().After(e.exp) {
		c.m.Delete(key)
		c.n.Add(-1)
		return nil, false
	}
	return e.val, true
}

func (c *ttlCache) put(key string, val any, exp time.Time) {
	if _, loaded := c.m.Swap(key, ttlEntry{val: val, exp: exp}); !loaded {
		if c.n.Add(1) > c.max {
			c.sweep()
		}
	}
}

// sweep drops expired entries; if the map is still over budget afterwards it is
// emptied outright. Losing memo entries only costs a recomputation.
func (c *ttlCache) sweep() {
	now := time.Now()
	var live int64
	c.m.Range(func(k, v any) bool {
		if now.After(v.(ttlEntry).exp) {
			c.m.Delete(k)
			return true
		}
		live++
		return true
	})
	if live > c.max {
		c.m.Range(func(k, _ any) bool { c.m.Delete(k); return true })
		live = 0
	}
	c.n.Store(live)
}

var (
	// viewer identity resolved from a session token hash (30 s, matches sessionCache).
	viewerCache = &ttlCache{max: 20000}
	// viewer→author relationship facts (30 s).
	authorAccessCache = &ttlCache{max: 50000}
	// viewer→block set, both directions (30 s).
	blockSetCache = &ttlCache{max: 20000}
)

const (
	viewerTTL       = 30 * time.Second
	authorAccessTTL = 30 * time.Second
)

// ── The wall ─────────────────────────────────────────────────────────────────

// authorAccess is the resolved viewer↔author relationship. Every field is
// server-computed; none of it is ever taken from the request.
type authorAccess struct {
	IsOwner      bool
	IsFollowing  bool
	IsBlocked    bool // a block in either direction
	IsSubscribed bool
}

// authorWall resolves the viewer's standing with one author. viewer may be nil
// or the anonymous DemoUser sentinel, in which case no relationship exists.
// A DB failure is returned as an error — the caller denies; an authorization
// primitive must never answer "allowed" because it could not look.
func (h *Handler) authorWall(authorID string, viewer *model.User) (authorAccess, error) {
	var a authorAccess
	if authorID == "" {
		return a, errors.New("authorWall: empty author id")
	}
	if viewer == nil || viewer.ID == "" || viewer.ID == "demo_user" {
		return a, nil // anonymous: not owner, not follower, no block, no subscription
	}
	if viewer.ID == authorID {
		a.IsOwner = true
		return a, nil
	}
	if h.db == nil {
		return a, errors.New("authorWall: no database")
	}

	key := viewer.ID + "|" + authorID
	if v, ok := authorAccessCache.get(key); ok {
		return v.(authorAccess), nil
	}

	blocked, err := h.blockedFor(viewer.ID)
	if err != nil {
		return authorAccess{}, err
	}
	a.IsBlocked = blocked[authorID]
	if a.IsBlocked {
		// A block ends the question; nothing else about the pair matters.
		authorAccessCache.put(key, a, time.Now().Add(authorAccessTTL))
		return a, nil
	}

	following, err := dbpkg.IsFollowing(h.db, viewer.ID, authorID)
	if err != nil {
		return authorAccess{}, err
	}
	a.IsFollowing = following
	a.IsSubscribed = dbpkg.IsSubscribed(h.db, viewer.ID, authorID)

	authorAccessCache.put(key, a, time.Now().Add(authorAccessTTL))
	return a, nil
}

// blockedFor returns the viewer's block set (both directions) through the one
// owner of that set, memoized per viewer so a page of many authors costs one read.
func (h *Handler) blockedFor(viewerID string) (map[string]bool, error) {
	if v, ok := blockSetCache.get(viewerID); ok {
		return v.(map[string]bool), nil
	}
	blocked, err := dbpkg.GetBlockedUserIDs(h.db, viewerID)
	if err != nil {
		return nil, err
	}
	blockSetCache.put(viewerID, blocked, time.Now().Add(authorAccessTTL))
	return blocked, nil
}

// privateWalled reports whether an account-private author's content must be
// withheld from this viewer. Same rule as profile.go's privateWall and
// workWall's privateWall — owner and approved follower pass, nobody else does.
func privateWalled(authorIsPrivate bool, a authorAccess) bool {
	return authorIsPrivate && !a.IsOwner && !a.IsFollowing
}

// ── Viewer resolution for non-page requests ──────────────────────────────────

// requestViewer resolves the requesting identity for asset requests, which have
// no response body to redirect and must never mutate the session. It reuses the
// entries userFromRequest stores in sessionCache, then its own 30 s memo, so an
// image burst costs at most one identity lookup per session per 30 s.
// Returns (nil, nil) for an anonymous request.
func (h *Handler) requestViewer(r *http.Request) (*model.User, error) {
	if h.db == nil {
		return nil, nil
	}
	tok := GetSessionToken(r)
	if tok != "" && !strings.HasPrefix(tok, "__handle__") {
		if v, ok := h.sessionCache.Load(tok); ok {
			if e := v.(sessionEntry); time.Now().Before(e.exp) {
				return e.user, nil
			}
			h.sessionCache.Delete(tok)
		}
		key := "s:" + dbpkg.HashToken(tok)
		if v, ok := viewerCache.get(key); ok {
			return v.(*model.User), nil
		}
		ident, err := dbpkg.GetSessionIdentity(h.db, tok)
		if err != nil {
			return nil, err
		}
		if ident.UserID != "" {
			u, err := dbpkg.GetUserByID(h.db, ident.UserID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if u != nil {
				if ident.PIALID != "" {
					u.PIALID = ident.PIALID
				}
				h.applyPIALIdentity(u)
				viewerCache.put(key, u, time.Now().Add(viewerTTL))
				return u, nil
			}
		}
	}

	// Legacy handle cookie — read only, never creates or bootstraps anything.
	handle := HandleFromCookie(r)
	if handle == "" || len(handle) > 30 || !handleRe.MatchString(handle) {
		return nil, nil
	}
	key := "h:" + handle
	if v, ok := viewerCache.get(key); ok {
		return v.(*model.User), nil
	}
	u, err := dbpkg.GetUserByHandle(h.db, handle)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if u == nil {
		return nil, nil
	}
	h.applyPIALIdentity(u)
	viewerCache.put(key, u, time.Now().Add(viewerTTL))
	return u, nil
}

// applyPIALIdentity folds PIAL state onto a user the same way userFromRequest
// does — PIAL is the single system of record for age and role.
func (h *Handler) applyPIALIdentity(u *model.User) {
	if u == nil {
		return
	}
	if ps := dbpkg.LoadPIALState(h.db, u.PIALID); ps != nil {
		u.IsAgeVerified = ps.AgeVerified
		u.IsVerified = model.KYCTierIsIdentity(ps.KYCTier)
		u.KYCTier = ps.KYCTier
		u.IsMinor = ps.IsMinor
		u.IsAdult = ps.IsAdult
		u.Role = ps.Role
	}
	ApplyBirthdayAgeGate(u)
}
