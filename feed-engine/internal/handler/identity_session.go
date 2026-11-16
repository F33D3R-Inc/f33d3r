package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/identity"
	"github.com/f33d3r/feed-engine/internal/jung"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/realm"
	"github.com/f33d3r/feed-engine/internal/sitra"
)

// ── Sessions: elohim-veni resolves, this brain renders ──────────────────────
//
// The session cookie (or the native clients' bearer token) carries
// elohim-veni's opaque session token and nothing else. Who is behind it is
// asked of the authority on every request that misses the 30-second cache, and
// the answer is accepted only when the access token it carries verifies
// against the authority's published keys and names the same identity.
//
// The session path FAILS CLOSED. If elohim-veni cannot be asked, nobody is
// signed in: a public page renders for an anonymous visitor, and a page that
// needs an account answers 503 with the identity_unavailable Fragment. There
// is no fallback to any local table, because there is no local table that
// knows who anyone is.

// sessionCacheTTL bounds how long a resolved session is reused without asking
// the authority. A revocation, ban or role change made through this process
// evicts at once (evictCachedAccount); one made elsewhere takes effect within
// this window.
const sessionCacheTTL = 30 * time.Second

var (
	// errSessionStopped: the session resolved to an identity that may no longer
	// act. The session is ended and the request is anonymous.
	errSessionStopped = errors.New("session identity is stopped")
	// errAccountNotProjected: the authority knows the account and this brain's
	// projection does not yet. Transient — the identity.events consumer is
	// behind — and answered like an outage, never as a sign-out.
	errAccountNotProjected = errors.New("account not yet projected")
)

// viewer resolves the request's session. (nil, nil) is an anonymous request.
// A non-nil error means the session could not be confirmed either way; the
// caller fails closed on it.
func (h *Handler) viewer(w http.ResponseWriter, r *http.Request) (*model.User, error) {
	tok := GetSessionToken(r)
	if tok == "" || h.db == nil {
		return nil, nil
	}
	u, err := h.sessionUser(r.Context(), tok)
	switch {
	case err == nil:
		return u, nil
	case errors.Is(err, identity.ErrSessionInvalid):
		// Only a document load may clear the cookie — never an HTMX partial,
		// which would sign the page out mid-navigation.
		if r.Header.Get("HX-Request") != "true" && w != nil {
			ClearSessionCookie(w)
		}
		return nil, nil
	case errors.Is(err, errSessionStopped):
		if rerr := h.identity.Client().RevokeSession(r.Context(), tok); rerr != nil {
			log.Printf("[auth] ending a stopped identity's session: %v", rerr)
		}
		if w != nil {
			ClearSessionCookie(w)
		}
		return nil, nil
	default:
		log.Printf("[auth] session could not be confirmed, failing closed: %v", err)
		return nil, err
	}
}

// sessionUser resolves a token to the signed-in account. The authority's
// answer comes through the cache; the account's profile is read fresh from
// this brain's database every time (see sessionEntry).
func (h *Handler) sessionUser(ctx context.Context, tok string) (*model.User, error) {
	sess, err := h.cachedSession(ctx, tok)
	if err != nil {
		return nil, err
	}
	if h.accountLoader != nil {
		return h.accountLoader(sess)
	}
	return h.loadAccount(sess)
}

// loadAccount builds the signed-in account for one request: this brain's
// account and profile row, read now, with the identity authority's answer
// folded on top.
func (h *Handler) loadAccount(sess *identity.Session) (*model.User, error) {
	u, err := dbpkg.GetUserByID(h.db, sess.AccountID)
	if err != nil {
		return nil, fmt.Errorf("loading account %s: %w", sess.AccountID, err)
	}
	if u == nil {
		return nil, fmt.Errorf("%w: %s (@%s)", errAccountNotProjected, sess.AccountID, sess.Handle)
	}
	applySession(u, sess)
	// Interest vector on the Jung axes: the stored online average of engaged
	// works' vectors, or — when none has been stored yet — a fresh computation
	// from recent engagement and the declared archetype, persisted so the next
	// request reads it.
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
	return u, nil
}

// cachedSession is the identity authority's answer for a token, reused for
// sessionCacheTTL. Only the answer is cached, never an account row built from it.
func (h *Handler) cachedSession(ctx context.Context, tok string) (*identity.Session, error) {
	if v, ok := h.sessionCache.Load(tok); ok {
		if e := v.(sessionEntry); time.Now().Before(e.exp) {
			return e.sess, nil
		}
		h.sessionCache.Delete(tok)
	}
	sess, err := h.identity.Resolve(ctx, tok)
	if err != nil {
		return nil, err
	}
	if sess.Stopped() {
		return nil, errSessionStopped
	}
	h.sessionCache.Store(tok, sessionEntry{sess: sess, exp: time.Now().Add(sessionCacheTTL)})
	// Once per resolution, not once per request: the session's start is the
	// authority's fact and does not change between them.
	go dbpkg.TryAwardTrollOnSessionUpdate(h.db, sess.AccountID, sess.SessionCreatedAt)
	return sess, nil
}

// applySession folds the authority's answer onto the account row. Identity
// comes from the session, profile and content from this brain.
func applySession(u *model.User, sess *identity.Session) {
	u.PIALID = sess.PIALID
	u.Handle = sess.Handle
	u.Role = sess.Role
	u.Caps = sess.Caps
	u.AccessToken = sess.AccessToken
	u.SessionID = sess.SessionID
	u.SessionStartedAt = sess.SessionCreatedAt
	u.SessionExpiresAt = sess.SessionExpiresAt
	// age_verified is the age-gate fact (self-report is enough); the verified
	// badge is the stricter identity fact, kyc_tier 'full'.
	u.IsAgeVerified = sess.Facts.AgeVerified
	u.KYCTier = sess.Facts.KYCTier
	u.IsVerified = model.KYCTierIsIdentity(sess.Facts.KYCTier)
	u.IsMinor = sess.Facts.IsMinor
	u.IsAdult = sess.Facts.IsAdult
	if b := sess.Facts.Birthday(); b != nil {
		u.Birthday = b
	}
	ApplyBirthdayAgeGate(u)
}

// identityUnavailable answers a request that needs a signed-in account while
// the session cannot be confirmed.
func (h *Handler) identityUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "5")
	w.WriteHeader(http.StatusServiceUnavailable)
	if err := h.partial.ExecuteTemplate(w, "identity_unavailable", nil); err != nil {
		log.Printf("[auth] rendering identity_unavailable: %v", err)
	}
}

// openedSession is a session the authority has just opened for this device.
// The token goes into the cookie; nothing else about it is kept here.
func (h *Handler) openedSession(w http.ResponseWriter, token string) {
	h.sessionCache.Delete(token)
	SetSessionCookie(w, token)
}

// ── The identity projection, from identity.events ────────────────────────────

// ApplyIdentityEvent is the identity.events consumer's apply: it brings the
// projection up to one event and drops this replica's cached sessions for every
// account the event changed. Other replicas' caches lapse within
// sessionCacheTTL.
func (h *Handler) ApplyIdentityEvent(ctx context.Context, ev sitra.IdentityEvent) error {
	snap := dbpkg.IdentitySnapshot{
		EventID: ev.EventID, OccurredAt: ev.OccurredAt,
		PIALID: ev.PIALID, AccountID: ev.AccountID, Handle: ev.Handle,
		Role: ev.Role, Status: ev.Status, Caps: ev.Caps,
		Facts: dbpkg.IdentityFacts{
			AgeVerified: ev.Facts.AgeVerified, IsAdult: ev.Facts.IsAdult, IsMinor: ev.Facts.IsMinor,
			KYCTier: ev.Facts.KYCTier, KYCVerifiedAt: ev.Facts.KYCVerifiedAt,
		},
	}
	if ev.Facts.DOB != "" {
		dob, err := time.Parse("2006-01-02", ev.Facts.DOB)
		if err != nil {
			return fmt.Errorf("%w: event %s: dob %q: %v", sitra.ErrPermanent, ev.EventID, ev.Facts.DOB, err)
		}
		snap.Facts.DateOfBirth = &dob
	}
	affected, err := dbpkg.ApplyIdentityEvent(ctx, h.db, snap)
	if err != nil {
		if dbpkg.IsPermanentIdentityApplyError(err) {
			return fmt.Errorf("%w: %v", sitra.ErrPermanent, err)
		}
		return err
	}
	for _, id := range affected {
		h.evictCachedAccount(id, "")
		// The founder role carries a permanent realm floor, so a role change
		// moves standing: promotion raises the ring now, and demotion lets it
		// fall back to what the account has earned. The projection is already
		// applied; a failed restate is corrected by the account's next XP award
		// and is said here.
		if _, err := realm.Restate(h.db, id); err != nil {
			log.Printf("[identity] realm restate for %s after event %s: %v", id, ev.EventID, err)
		}
	}
	return nil
}

// IdentityDeadLetter keeps an identity.events record that can never apply.
func (h *Handler) IdentityDeadLetter(ctx context.Context, topic string, partition int32, offset int64, record []byte, reason string) error {
	return dbpkg.RecordIdentityDeadLetter(ctx, h.db, topic, partition, offset, record, reason)
}

// StartIdentityRelay drains the identity command outbox until ctx ends.
func (h *Handler) StartIdentityRelay(ctx context.Context) {
	go h.identityRelay.Run(ctx)
}
