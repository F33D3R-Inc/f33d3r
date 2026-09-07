package handler

import (
	"bytes"
	"fmt"
	"html/template"
	"log"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── work_card — single render entry point ─────────────────────────────────────
//
// A work_card is a Facet: the stream transports its rendered fragment, never a
// state delta. Two places that both build that fragment are two sources of
// truth, and they drift the moment one of them forgets an enrichment step — the
// card delivered over Sitra Achra then visibly differs from the card the same
// work produces on an HTTP facet fetch. Everything below exists so that cannot
// happen: every surface (HTTP facet fetch, profile timeline, reply stream,
// bookmarks, stream fan-out) renders through renderWorkCardSet, which owns the
// complete enrichment sequence. Callers supply works and a viewer; they never
// enrich, and they never execute the work_card template themselves.

// workCardFragment pairs a rendered fragment with the work it was rendered from,
// so a caller that must interleave other facets (the reply stream) or derive a
// pagination cursor (the feed sentinel) can do so without re-deriving anything.
type workCardFragment struct {
	Work     *model.Work
	Fragment template.HTML
}

// viewerAccountID returns the account UUID to use for viewer-relative queries,
// or "" when there is no real account behind the viewer. The anonymous
// DemoUser sentinel and dev-mode non-UUID identifiers resolve to "" so a
// viewer-relative query is skipped instead of being sent to Postgres with an
// unparseable uuid parameter.
func viewerAccountID(viewer *model.User) string {
	if viewer == nil || viewer.ID == "" || viewer.ID == "demo_user" {
		return ""
	}
	if _, err := uuid.Parse(viewer.ID); err != nil {
		return ""
	}
	return viewer.ID
}

// surfaceVisions is the render surface that selects the vision_card Facet.
// Carried in the work_card context so one value decides the Facet for a whole
// batch, instead of every work being asked what it is.
const surfaceVisions = "visions"

// workCardCtx builds the standard template context for a work_card render.
// Templates below work_card read .Ctx.CurrentUserHandle (author-vs-viewer
// checks, subscriber-only lock) and .Ctx.User (content_gate dereferences it, so
// it must never be nil). SessionID / Surface / ShowScores are carried for the
// surfaces that report them. Building it here is what stops each call site from
// hand-rolling a map that is missing a key the templates require.
func (h *Handler) workCardCtx(viewer *model.User, surface string) map[string]interface{} {
	handle := ""
	tmplViewer := viewer
	if viewer == nil {
		// No viewer: render against the anonymous safe-mode projection so
		// content_gate has a real struct to read and gates adult media rather
		// than leaking it.
		tmplViewer = DemoUser()
	} else {
		handle = viewer.Handle
	}
	if surface == "" {
		surface = "feed"
	}
	showScores := false
	if h.cfg != nil {
		showScores = h.cfg.ShowScores
	}
	return map[string]interface{}{
		"CurrentUserHandle": handle,
		"User":              tmplViewer,
		"SessionID":         uuid.New().String(),
		"Surface":           surface,
		"ShowScores":        showScores,
	}
}

// enrichWorks is the complete enrichment sequence a work must pass through
// before it can be rendered — reactions, quotes, link previews, replier
// avatars, polls, the viewer's pinned marker, and relative time. It is the only
// definition of that sequence in the service; renderWorkCardSet applies it, and
// the full-page renderers that assemble works into a page template (work
// detail, bookmarks) call it directly so a page-rendered work and a
// fragment-rendered work carry identical data.
//
// Viewer-relative steps degrade instead of failing when there is no account
// behind the viewer: reactions and pinned state are simply skipped.
func (h *Handler) enrichWorks(works []*model.Work, viewer *model.User) {
	if len(works) == 0 {
		return
	}
	viewerID := viewerAccountID(viewer)
	if h.db != nil {
		dbpkg.EnrichWorksWithReactions(h.db, works, viewerID)
		dbpkg.EnrichWorksWithQuotes(h.db, works)
		dbpkg.EnrichWorksWithLinkPreviews(h.db, works)
		dbpkg.EnrichWorksWithRepliers(h.db, works)
		// Polls run after quotes so a quoted ballot is enriched too — the quote
		// card draws the poll, so the quote card needs it loaded.
		dbpkg.EnrichWorksWithPolls(h.db, dbpkg.WorksWithQuoted(works), viewerID)
		if viewerID != "" {
			dbpkg.EnrichWorksWithPinnedState(works, dbpkg.GetPinnedPostID(h.db, viewerID))
		}
	}
	for _, wk := range works {
		if wk == nil {
			continue
		}
		wk.TimeAgo = TimeAgo(wk.CreatedAt)
	}
}

// renderWorkCardSet enriches and renders a set of works, returning one entry per
// work that produced a complete fragment. This is the ONLY place a work_card
// fragment is produced. Every surface — the HTTP facet fetch, the profile
// timeline, the reply stream, bookmarks, and the Sitra Achra stream fan-out —
// goes through here, so a work renders identically no matter which path
// delivered it.
//
// Order of operations is fixed: adult content is stripped for viewers who must
// never see it, then the full enrichment sequence runs, then each work is
// rendered into its own buffer. A work whose template execution fails, or which
// produces an empty buffer, is logged with its id and dropped — a half-written
// or empty fragment on the stream is a broken card in the browser, and one bad
// work must never take the rest of the batch down with it.
//
// viewer may be nil. That is the stream fan-out: it renders one fragment and
// broadcasts it to every follower, because rendering per recipient would turn a
// single render into one render per follower and does not survive a large
// follow graph. A nil viewer therefore produces the complete
// non-viewer-relative card: everything that does not depend on who is looking
// is present (author, body, media, counts, quotes, link previews, repliers,
// relative time), while viewer-relative marks (the viewer's own like/repost/
// bookmark state, their pinned marker) are absent because there is no single
// viewer they could belong to, and adult media renders behind the gate rather
// than raw. Adult works are not dropped for a nil viewer — a broadcast has no
// one viewer to filter for, and dropping them would silently deny delivery to
// the followers entitled to them.
func (h *Handler) renderWorkCardSet(works []*model.Work, viewer *model.User, ctx map[string]interface{}) []workCardFragment {
	if len(works) == 0 || h.partial == nil {
		return nil
	}
	if viewer != nil {
		works = filterAdultForViewer(viewer, works)
	}
	h.enrichWorks(works, viewer)
	if ctx == nil {
		ctx = h.workCardCtx(viewer, "")
	}

	out := make([]workCardFragment, 0, len(works))
	for _, wk := range works {
		if wk == nil {
			continue
		}
		// The Visions surface renders the full-bleed vision_card Facet; every
		// other surface renders work_card. The choice is the surface's, not the
		// work's: kind='vision' left the Work lane in migration 0008, and the
		// Visions feed is now media-rich Works of any kind.
		name := "work_card"
		var data interface{} = map[string]interface{}{"W": wk, "Ctx": ctx}
		if s, _ := ctx["Surface"].(string); s == surfaceVisions {
			name = "vision_card"
			data = wk
		}
		var buf bytes.Buffer
		if err := h.partial.ExecuteTemplate(&buf, name, data); err != nil {
			log.Printf("[work-card] %s render failed for work %s: %v", name, wk.ID, err)
			continue
		}
		if buf.Len() == 0 {
			log.Printf("[work-card] %s produced an empty fragment for work %s", name, wk.ID)
			continue
		}
		out = append(out, workCardFragment{Work: wk, Fragment: template.HTML(buf.String())})
	}
	return out
}

// renderWorkCards enriches and renders a set of works into work_card fragments.
// Thin form of renderWorkCardSet for callers that only write fragments out in
// order and need nothing back about which work produced which fragment.
func (h *Handler) renderWorkCards(works []*model.Work, viewer *model.User, ctx map[string]interface{}) []template.HTML {
	set := h.renderWorkCardSet(works, viewer, ctx)
	out := make([]template.HTML, 0, len(set))
	for _, c := range set {
		out = append(out, c.Fragment)
	}
	return out
}

// renderWorkCardByID loads a work by id, enriches it, and returns the finished
// fragment. Used by the stream fan-out, which has a work id and nothing else.
func (h *Handler) renderWorkCardByID(workID string, viewer *model.User) (template.HTML, error) {
	if h.db == nil {
		return "", fmt.Errorf("work %s: no database", workID)
	}
	work, err := dbpkg.GetWorkByID(h.db, workID, viewerAccountID(viewer))
	if err != nil {
		return "", fmt.Errorf("work %s: %w", workID, err)
	}
	if work == nil {
		return "", fmt.Errorf("work %s not found", workID)
	}
	cards := h.renderWorkCardSet([]*model.Work{work}, viewer, h.workCardCtx(viewer, ""))
	if len(cards) == 0 {
		return "", fmt.Errorf("work %s produced no fragment", workID)
	}
	return cards[0].Fragment, nil
}
