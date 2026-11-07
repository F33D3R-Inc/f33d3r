package handler

// work_gate.go — the one answer to "may this viewer see these works?".
//
// Every read of other people's works passes through gateWorks before it is shown, on
// every surface, page or stream or API. A surface that does not call it is a bug.
// Liveness (expiry, scheduling, moderation) is already applied in SQL by
// workNotBlockedSQL; what is here depends on who is looking.
//
// It removes a work when:
//   - a block stands between the viewer and the author, or whoever reposted it
//   - the viewer muted either of them (a person's own profile still shows them)
//   - the author is private and the viewer neither owns nor follows the account
//   - the work is adult and this viewer may never see adult content
//
// It fails closed: if it cannot look, only the viewer's own works pass. It never
// changes the slice it is given, so a caller can still read where a page ended.

import (
	"errors"
	"log"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

var errGateNoDB = errors.New("no database")

// gateOptions are the few ways surfaces legitimately differ.
type gateOptions struct {
	// keepMuted is for a person's own profile: muting hides someone from your feeds,
	// not from a page you opened to look at them.
	keepMuted bool
}

func gateViewerID(viewer *model.User) string {
	if viewer == nil || viewer.ID == "" || viewer.ID == "demo_user" {
		return ""
	}
	return viewer.ID
}

// gateWorks applies the read rules for one viewer.
func (h *Handler) gateWorks(viewer *model.User, works []*model.Work) []*model.Work {
	return h.gateWorksWith(viewer, works, gateOptions{})
}

func (h *Handler) gateWorksWith(viewer *model.User, works []*model.Work, opts gateOptions) []*model.Work {
	if len(works) == 0 {
		return nil
	}
	viewerID := gateViewerID(viewer)
	hideAdult := excludeNSFW(viewer)

	authors := make([]string, 0, len(works))
	reposters := make([]string, 0)
	for _, w := range works {
		if w == nil {
			continue
		}
		authors = append(authors, w.AuthorID)
		if w.RepostedByHandle != "" {
			reposters = append(reposters, w.RepostedByHandle)
		}
	}

	failClosed := func(what string, err error) []*model.Work {
		log.Printf("[work-gate] %s: %v", what, err)
		own := make([]*model.Work, 0)
		for _, w := range works {
			if w != nil && viewerID != "" && w.AuthorID == viewerID {
				own = append(own, w)
			}
		}
		return own
	}

	if h.db == nil {
		return failClosed("no database", errGateNoDB)
	}
	private, err := dbpkg.PrivateAmong(h.db, authors)
	if err != nil {
		return failClosed("private authors", err)
	}
	blocked, muted, followed := map[string]bool{}, map[string]bool{}, map[string]bool{}
	if viewerID != "" {
		if blocked, err = h.blockedFor(viewerID); err != nil {
			return failClosed("blocks", err)
		}
		if muted, err = dbpkg.GetMutedUserIDs(h.db, viewerID); err != nil {
			return failClosed("mutes", err)
		}
		if len(private) > 0 {
			if followed, err = dbpkg.FollowedAmong(h.db, viewerID, authors); err != nil {
				return failClosed("follows", err)
			}
		}
	}
	reposterIDs := map[string]string{}
	if len(reposters) > 0 && (len(blocked) > 0 || len(muted) > 0) {
		if reposterIDs, err = dbpkg.UserIDsByHandles(h.db, reposters); err != nil {
			return failClosed("reposters", err)
		}
	}

	subscribed := map[string]bool{}
	out := make([]*model.Work, 0, len(works))
	for _, w := range works {
		if w == nil {
			continue
		}
		own := viewerID != "" && w.AuthorID == viewerID
		// An adult creator's works are adult, flagged or not, so every later check
		// and template treats them the same way.
		if w.AuthorIsAdultCreator {
			w.IsNSFW = true
		}
		if !own {
			if blocked[w.AuthorID] || (muted[w.AuthorID] && !opts.keepMuted) {
				continue
			}
			if private[w.AuthorID] && !followed[w.AuthorID] {
				continue
			}
			if hideAdult && (w.IsNSFW || w.IsGore) {
				continue
			}
			if id := reposterIDs[w.RepostedByHandle]; id != "" && id != viewerID && (blocked[id] || muted[id]) {
				continue
			}
		}
		if w.SubscriberOnly && viewerID != "" && !own {
			has, seen := subscribed[w.AuthorID]
			if !seen {
				has = dbpkg.IsSubscribed(h.db, viewerID, w.AuthorID)
				subscribed[w.AuthorID] = has
			}
			w.ViewerSubscribed = has
		}
		out = append(out, w)
	}
	h.gateQuotes(viewer, out)
	return out
}

// gateQuotes removes quoted works this viewer may not see. The work quoting them stays:
// it is the quoter's own words. Quoted works load after the list itself, so anything
// that loads them must call this afterwards.
func (h *Handler) gateQuotes(viewer *model.User, works []*model.Work) {
	var quoted []*model.Work
	for _, w := range works {
		if w != nil && w.QuotedWork != nil {
			quoted = append(quoted, w.QuotedWork)
		}
	}
	if len(quoted) == 0 {
		return
	}
	allowed := map[*model.Work]bool{}
	for _, q := range h.gateWorks(viewer, quoted) {
		allowed[q] = true
	}
	for _, w := range works {
		if w != nil && w.QuotedWork != nil && !allowed[w.QuotedWork] {
			w.QuotedWork = nil
		}
	}
}

// surfaceKeepsMuted says whether a surface is a page about one person.
func surfaceKeepsMuted(surface string) bool {
	return strings.HasPrefix(surface, "profile_")
}

// adultMediaOpen says whether a work's media can be drawn with no gate in front of it.
// The mobile layout has no room for a gate, so anything less draws no media there.
func adultMediaOpen(w *model.Work, u *model.User) bool {
	if w == nil || !(w.IsNSFW || w.IsGore || w.AuthorIsAdultCreator) {
		return true
	}
	return u != nil && u.IsAdult && u.ContentSetting == "adult_enabled" &&
		(u.IsAgeVerified || u.IsVerified) && (!w.IsGore || u.ShowSensitive) && !excludeNSFW(u)
}
