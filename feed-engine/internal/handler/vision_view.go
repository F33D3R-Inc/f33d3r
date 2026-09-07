package handler

// vision_view.go — the Vision read path.
//
// Everything a viewer sees of the ephemeral lane is decided here and delivered
// as a finished Fragment: which Visions are live, what order they play in, which
// avatar carries a ring, where a ring resumes, and what "next" means once one
// author's run ends. The browser walks the sequence by asking the server for the
// next frame; it never holds the sequence, computes a ring, or measures expiry.
//
// Expiry is enforced in SQL on every read (db.visionLiveSQL). Nothing on this
// path hides an expired Vision with CSS or a timer.
//
// A Vision has no id of its own: it is addressed as (author_pial, seq), a
// position in its author's PIAL-owned lane (db.VisionRef). PIAL is the only
// standalone identity this lane ever carries.

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── scopes ───────────────────────────────────────────────────────────────────

// The two scopes the reels Playground and the sequential viewer share. A scope
// decides the sequence, so the viewer's "next" and the feed's order are the same
// answer to the same question.
const (
	visionScopeFollowing = "following"
	visionScopeForYou    = "for_you"
)

// visionReelsMax caps how many reels one Playground render carries. The sequence
// behind it can be longer; the viewer walks the whole of it one frame at a time.
const visionReelsMax = 30

// visionRingLimit caps the avatar rail.
const visionRingLimit = 60

// visionScope normalises a requested scope to the closed set. An unknown value is
// the following scope, never a predicate assembled from what the browser sent.
func visionScope(raw string) string {
	if strings.TrimSpace(raw) == visionScopeForYou {
		return visionScopeForYou
	}
	return visionScopeFollowing
}

// visionRef is the address of one Vision, built from the fields every row
// carries — the small twin of dbpkg.ParseVisionRef for a row already in hand.
func visionRef(v *model.Vision) dbpkg.VisionRef {
	return dbpkg.VisionRef{AuthorPIAL: v.AuthorPIAL, Seq: v.Seq}
}

// ── views ────────────────────────────────────────────────────────────────────

// visionStillView is one Vision as any surface shows it. The content type has
// already chosen the Facet that renders it and every look is a resolved preset,
// so no surface below this decides what a Vision looks like.
type visionStillView struct {
	V           *model.Vision
	Ref         string // the canonical `<author_pial>.<seq>` address — templates cannot call visionRef themselves
	Artboard    visionArtboardView
	HasArtboard bool
	IsImage     bool
	IsVideo     bool
	IsHLS       bool
	MediaURL    string
	SharedWork  *model.Work
	HasShare    bool
	AuthorURL   string
	TimeAgo     string
	ExpiryLabel string
	RingElemID  string
}

// visionRingView is one author's avatar ring. State, unseen count and the resume
// point all arrive from the server already decided — this Facet renders an
// answer, never the inputs to one.
type visionRingView struct {
	R       *model.VisionRing
	Scope   string
	OpenURL string
	ElemID  string
	Label   string
}

// visionRailView is the ring rail across the top of the Playground and the home
// timeline. The viewer's own entry is separated out by the server: it is always
// the first item, it carries the add affordance, and it carries a ring of its
// own once the viewer has a live Vision — so an author never appears twice.
type visionRailView struct {
	Rings      []visionRingView
	Own        visionRingView
	HasOwn     bool
	Scope      string
	Handle     string
	AvatarURL  string
	ComposeURL string
	CameraURL  string
	IsEmpty    bool
}

// visionProfileRingView is one author's avatar on their own profile. The ring is
// the same server answer the rail carries; when the author has nothing live the
// Facet renders the plain avatar, so the profile head is identical either way.
type visionProfileRingView struct {
	Ring      visionRingView
	HasRing   bool
	AuthorID  string
	Handle    string
	Name      string
	AvatarURL string
	IsOnline  bool
	// IsLive is a broadcast in progress, not a Vision — it takes the ring over
	// outright (LiveURL, not a Vision resume point) because a live camera is
	// more urgent than a story and the two never need to be told apart by
	// looking at the same ring.
	IsLive  bool
	LiveURL string
}

// visionReelView is one reel in the vertical Playground.
type visionReelView struct {
	Still   visionStillView
	Scope   string
	OpenURL string
	IsNSFW  bool
	Index   int
	Total   int
}

// visionReelsView is the reels Playground itself.
type visionReelsView struct {
	Scope        string
	Reels        []visionReelView
	IsEmpty      bool
	FollowingURL string
	ForYouURL    string
	ComposeURL   string
}

// visionSegment is one tick of the progress indicator. State is decided here so
// the browser never counts anything.
type visionSegment struct {
	State string // done | current | todo
}

// visionFrameView is one frame of the sequential viewer: this Vision, its place
// in its author's run, and where the sequence goes from here.
type visionFrameView struct {
	Still     visionStillView
	Scope     string
	Position  int
	Total     int
	Segments  []visionSegment
	PrevURL   string
	NextURL   string
	CloseURL  string
	IsFirst   bool
	IsLast    bool
	ViewCount int
	IsAuthor  bool
	Gone      bool
}

// ── still assembly ───────────────────────────────────────────────────────────

// buildVisionStill resolves one Vision into the finished shape a Facet renders.
// A text Vision goes through the same buildVisionArtboard the composer uses, so
// an artboard looks identical wherever it appears.
func buildVisionStill(v *model.Vision, shared *model.Work) visionStillView {
	sv := visionStillView{
		V:           v,
		Ref:         visionRef(v).String(),
		AuthorURL:   "/" + v.AuthorHandle,
		TimeAgo:     TimeAgo(v.CreatedAt),
		ExpiryLabel: visionExpiryLabel(v.ExpiresAt),
		RingElemID:  visionRingElemID(v.AuthorPIAL),
	}
	switch v.ContentType {
	case "image":
		sv.IsImage = true
		if len(v.MediaURLs) > 0 {
			sv.MediaURL = v.MediaURLs[0]
		}
	case "video":
		sv.IsVideo = true
		if len(v.MediaURLs) > 0 {
			sv.MediaURL = v.MediaURLs[0]
			sv.IsHLS = strings.HasSuffix(sv.MediaURL, ".m3u8")
		}
	case "share":
		sv.HasShare = true
		sv.SharedWork = shared
	default:
		sv.HasArtboard = true
	}
	// A media Vision whose object never landed still has to render something the
	// viewer can read, so it falls back to the artboard carrying its words.
	if !sv.HasArtboard && !sv.HasShare && sv.MediaURL == "" {
		sv.IsImage, sv.IsVideo = false, false
		sv.HasArtboard = true
	}
	if sv.HasArtboard {
		sv.Artboard = buildVisionArtboard(visionRef(v).String(), v.Body,
			v.ArtboardBackground, v.ArtboardTypeface, v.ArtboardTypeScale, v.ArtboardAlign)
	}
	return sv
}

// visionRingElemID is the id the avatar carries so the sequential viewer can put
// focus back exactly where it was opened from. Derived from the author's PIAL,
// so nothing the browser supplies is ever echoed into a Fragment.
func visionRingElemID(authorPIAL string) string {
	return "vision-ring-" + authorPIAL
}

// visionViewerURL is the address of one frame of the sequential viewer.
func visionViewerURL(ref, scope string) string {
	return fmt.Sprintf("/facets/vision/viewer?vision=%s&scope=%s", ref, scope)
}

// ── sequences ────────────────────────────────────────────────────────────────

// visionSequence returns the whole ordered play sequence for a scope: authors
// newest-first, and inside one author oldest-first. Ordering is the server's,
// resolved in SQL, and every surface on this path walks the same one.
func (h *Handler) visionSequence(viewer *model.User, scope string) ([]*model.Vision, error) {
	if h.db == nil {
		return nil, errors.New("vision: db unavailable")
	}
	viewerPIAL, viewerID := "", ""
	if viewer != nil {
		viewerPIAL, viewerID = viewer.PIALID, viewerAccountID(viewer)
	}
	includeNSFW := !excludeNSFW(viewer)
	if scope == visionScopeForYou || viewerID == "" {
		return dbpkg.GetActiveVisions(h.db, viewerPIAL, includeNSFW, 0)
	}
	return dbpkg.GetActiveVisionsForViewer(h.db, viewerID, viewerPIAL, includeNSFW, 0)
}

// visionRuns describes where one Vision sits in the sequence: the bounds of its
// author's contiguous run and its own index. Found is false when the Vision is
// not in this scope at all.
type visionRuns struct {
	Index    int
	RunStart int
	RunEnd   int // exclusive
	Found    bool
}

// locateVision finds a Vision and its author's run inside a sequence. The
// sequence is grouped by author in SQL, so an author's Visions are always
// contiguous.
func locateVision(seq []*model.Vision, ref dbpkg.VisionRef) visionRuns {
	idx := -1
	for i, v := range seq {
		if v.AuthorPIAL == ref.AuthorPIAL && v.Seq == ref.Seq {
			idx = i
			break
		}
	}
	if idx < 0 {
		return visionRuns{}
	}
	author := seq[idx].AuthorPIAL
	start := idx
	for start > 0 && seq[start-1].AuthorPIAL == author {
		start--
	}
	end := idx + 1
	for end < len(seq) && seq[end].AuthorPIAL == author {
		end++
	}
	return visionRuns{Index: idx, RunStart: start, RunEnd: end, Found: true}
}

// gateVisionsFor drops the Visions a viewer may not see. The scope queries gate
// in SQL; this is for the author read, which has no clearance argument.
func gateVisionsFor(viewer *model.User, in []*model.Vision) []*model.Vision {
	if !excludeNSFW(viewer) {
		return in
	}
	out := make([]*model.Vision, 0, len(in))
	for _, v := range in {
		if v.IsNSFW || v.IsBlocked {
			continue
		}
		out = append(out, v)
	}
	return out
}

// visionSegments renders the progress indicator's ticks. Position is 1-based.
func visionSegments(position, total int) []visionSegment {
	if total <= 0 {
		return nil
	}
	out := make([]visionSegment, total)
	for i := range out {
		switch {
		case i+1 < position:
			out[i].State = "done"
		case i+1 == position:
			out[i].State = "current"
		default:
			out[i].State = "todo"
		}
	}
	return out
}

// ── rings ────────────────────────────────────────────────────────────────────

// visionRings returns the avatar rings for one viewer, already gated. The ring
// set itself is one SQL answer; a viewer who must not see 18+ content has that
// answer restated from their own gated sequence, so a ring can never advertise
// a Vision its owner would be refused.
func (h *Handler) visionRings(viewer *model.User, scope string) ([]*model.VisionRing, error) {
	viewerID := viewerAccountID(viewer)
	if viewerID == "" {
		return nil, nil
	}
	rings, err := dbpkg.GetVisionRingsForViewer(h.db, viewerID, viewer.PIALID, visionRingLimit)
	if err != nil {
		return nil, err
	}
	if !excludeNSFW(viewer) {
		return rings, nil
	}
	gated, err := h.visionSequence(viewer, visionScopeFollowing)
	if err != nil {
		return nil, err
	}
	return visionRingsGatedTo(rings, gated), nil
}

// visionRingsGatedTo restates a ring set from a sequence the viewer is actually
// allowed to read. Counts, state and the resume point are recomputed from that
// sequence; an author with nothing left in it drops off the rail entirely.
func visionRingsGatedTo(rings []*model.VisionRing, seq []*model.Vision) []*model.VisionRing {
	byAuthor := make(map[string][]*model.Vision, len(rings))
	for _, v := range seq {
		byAuthor[v.AuthorPIAL] = append(byAuthor[v.AuthorPIAL], v)
	}
	out := make([]*model.VisionRing, 0, len(rings))
	for _, r := range rings {
		run := byAuthor[r.AuthorPIAL]
		if len(run) == 0 {
			continue
		}
		gated := *r
		gated.Count = len(run)
		gated.UnseenCount = 0
		gated.NextVisionRef = visionRef(run[0]).String()
		resumed := false
		for _, v := range run {
			if v.ViewedByViewer {
				continue
			}
			gated.UnseenCount++
			if !resumed {
				gated.NextVisionRef = visionRef(v).String()
				resumed = true
			}
		}
		gated.State = model.VisionRingSeen
		if gated.UnseenCount > 0 {
			gated.State = model.VisionRingUnseen
		}
		out = append(out, &gated)
	}
	return out
}

// buildVisionRail assembles the ring rail. Every ring arrives with its state,
// its unseen tally and the exact Vision it resumes at already decided.
func (h *Handler) buildVisionRail(viewer *model.User, scope string) (visionRailView, error) {
	rings, err := h.visionRings(viewer, scope)
	if err != nil {
		return visionRailView{}, err
	}
	v := visionRailView{
		Scope:      scope,
		ComposeURL: "/facets/vision/composer?mode=text",
		CameraURL:  "/visions/camera",
	}
	if viewer != nil {
		v.Handle = viewer.Handle
		v.AvatarURL = viewer.AvatarURL
	}
	viewerPIAL := ""
	if viewer != nil {
		viewerPIAL = viewer.PIALID
	}
	v.Own, v.HasOwn, v.Rings = visionRailEntries(rings, viewerPIAL, scope)
	v.IsEmpty = len(v.Rings) == 0 && !v.HasOwn
	return v, nil
}

// visionRailEntries splits the server's ring answer into the viewer's own entry
// and the ordered row of everyone else. An author appears once: the viewer is
// their own first item, never also one of the row. A ring with no resume point
// is not a ring — it is dropped rather than rendered as a dead target.
func visionRailEntries(rings []*model.VisionRing, viewerPIAL, scope string) (visionRingView, bool, []visionRingView) {
	var own visionRingView
	hasOwn := false
	row := make([]visionRingView, 0, len(rings))
	for _, r := range rings {
		if r == nil || r.NextVisionRef == "" {
			continue
		}
		rv := visionRingView{
			R:       r,
			Scope:   scope,
			OpenURL: visionViewerURL(r.NextVisionRef, scope),
			ElemID:  visionRingElemID(r.AuthorPIAL),
			Label:   visionRingLabel(r),
		}
		if viewerPIAL != "" && r.AuthorPIAL == viewerPIAL {
			rv.Label = visionOwnRingLabel(r)
			own, hasOwn = rv, true
			continue
		}
		row = append(row, rv)
	}
	return own, hasOwn, row
}

// visionRingForAuthor answers "does this author have a live Vision for this
// viewer, and where does it resume" for one author, named by PIAL. The
// viewer's own ring set is the answer wherever it already holds one — the same
// GetVisionRingsForViewer the rail is built from. An author the viewer neither
// is nor follows is not in that set, so their ring is restated from their own
// live run through the identical derivation the gated rail uses. No second
// definition of ring state exists.
func (h *Handler) visionRingForAuthor(authorPIAL string, viewer *model.User) (*model.VisionRing, error) {
	if h.db == nil {
		return nil, errors.New("vision: db unavailable")
	}
	rings, err := h.visionRings(viewer, visionScopeFollowing)
	if err != nil {
		return nil, err
	}
	for _, r := range rings {
		if r != nil && r.AuthorPIAL == authorPIAL && r.NextVisionRef != "" {
			return r, nil
		}
	}
	viewerPIAL := ""
	if viewer != nil {
		viewerPIAL = viewer.PIALID
	}
	run, err := dbpkg.GetActiveVisionsForAuthor(h.db, authorPIAL, viewerPIAL, 0)
	if err != nil {
		return nil, err
	}
	run = gateVisionsFor(viewer, run)
	if len(run) == 0 {
		return nil, nil
	}
	stub := &model.VisionRing{
		AuthorPIAL:   authorPIAL,
		AuthorHandle: run[0].AuthorHandle,
		AuthorName:   run[0].AuthorName,
		AvatarURL:    run[0].AvatarURL,
	}
	restated := visionRingsGatedTo([]*model.VisionRing{stub}, run)
	if len(restated) == 0 {
		return nil, nil
	}
	return restated[0], nil
}

// buildVisionProfileRing assembles the author's avatar for their profile head.
func (h *Handler) buildVisionProfileRing(author, viewer *model.User, online, isLive bool, liveURL string) (visionProfileRingView, error) {
	v := visionProfileRingView{
		AuthorID:  author.ID,
		Handle:    author.Handle,
		Name:      author.DisplayName,
		AvatarURL: author.AvatarURL,
		IsOnline:  online,
		IsLive:    isLive,
		LiveURL:   liveURL,
	}
	if isLive {
		// A broadcast in progress wins the ring outright; a Vision underneath
		// it would only be reachable by a link this Facet no longer renders.
		return v, nil
	}
	ring, err := h.visionRingForAuthor(author.PIALID, viewer)
	if err != nil {
		return visionProfileRingView{}, err
	}
	if ring == nil {
		return v, nil
	}
	v.HasRing = true
	v.Ring = visionRingView{
		R:       ring,
		Scope:   visionScopeFollowing,
		OpenURL: visionViewerURL(ring.NextVisionRef, visionScopeFollowing),
		ElemID:  visionRingElemID(ring.AuthorPIAL),
		Label:   visionRingLabel(ring),
	}
	return v, nil
}

// visionRingLabel is the ring's accessible name, composed on the server so a
// screen reader is told the same state the ring is drawn in.
func visionRingLabel(r *model.VisionRing) string {
	name := r.AuthorName
	if strings.TrimSpace(name) == "" {
		name = "@" + r.AuthorHandle
	}
	switch {
	case r.UnseenCount == 1:
		return fmt.Sprintf("Open %s's Visions — 1 new", name)
	case r.UnseenCount > 1:
		return fmt.Sprintf("Open %s's Visions — %d new", name, r.UnseenCount)
	case r.Count == 1:
		return fmt.Sprintf("Open %s's Vision — already seen", name)
	default:
		return fmt.Sprintf("Open %s's Visions — all seen", name)
	}
}

// visionOwnRingLabel names the viewer's own ring. An author is never one of
// their own Vision's viewers, so their ring is never described as new or seen
// — only as how many of theirs are still live.
func visionOwnRingLabel(r *model.VisionRing) string {
	if r.Count == 1 {
		return "Open your Vision"
	}
	return fmt.Sprintf("Open your %d Visions", r.Count)
}

// ── shared works ─────────────────────────────────────────────────────────────

// loadSharedWork reads the Work behind a share Vision. A Work that has since
// gone is reported, not swallowed: the Vision then renders its own words
// instead.
func (h *Handler) loadSharedWork(v *model.Vision, viewer *model.User) *model.Work {
	if v.ContentType != "share" || v.SharedWorkID == "" || h.db == nil {
		return nil
	}
	work, err := dbpkg.GetWorkByID(h.db, v.SharedWorkID, viewerAccountID(viewer))
	if err != nil {
		log.Printf("[vision-view] shared work %s for vision %s: %v", v.SharedWorkID, visionRef(v), err)
		return nil
	}
	return work
}

// ── surfaces ─────────────────────────────────────────────────────────────────

// buildVisionReels assembles the reels Playground for a scope.
func (h *Handler) buildVisionReels(viewer *model.User, scope string) (visionReelsView, error) {
	seq, err := h.visionSequence(viewer, scope)
	if err != nil {
		return visionReelsView{}, err
	}
	v := visionReelsView{
		Scope:        scope,
		FollowingURL: "/facets/vision/reels?scope=" + visionScopeFollowing,
		ForYouURL:    "/facets/vision/reels?scope=" + visionScopeForYou,
		ComposeURL:   "/facets/vision/composer?mode=text",
	}
	total := len(seq)
	if total > visionReelsMax {
		total = visionReelsMax
	}
	for i := 0; i < total; i++ {
		vi := seq[i]
		v.Reels = append(v.Reels, visionReelView{
			Still:   buildVisionStill(vi, h.loadSharedWork(vi, viewer)),
			Scope:   scope,
			OpenURL: visionViewerURL(visionRef(vi).String(), scope),
			IsNSFW:  vi.IsNSFW,
			Index:   i + 1,
			Total:   total,
		})
	}
	v.IsEmpty = len(v.Reels) == 0
	return v, nil
}

// visionsFeedPage — GET /visions
// The reels Playground: the ring rail, then a vertical full-screen feed of every
// live Vision in scope. Assembled whole on the server.
func (h *Handler) visionsFeedPage(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	scope := visionScope(r.URL.Query().Get("scope"))

	rail, err := h.buildVisionRail(user, scope)
	if err != nil {
		log.Printf("[vision-view] rail for %s: %v", user.ID, err)
		http.Error(w, "the Vision rail could not be read", http.StatusInternalServerError)
		return
	}
	reels, err := h.buildVisionReels(user, scope)
	if err != nil {
		log.Printf("[vision-view] reels for %s: %v", user.ID, err)
		http.Error(w, "Visions could not be read", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"User":  user,
		"Rail":  rail,
		"Reels": reels,
		"Scope": scope,
	}
	for k, val := range h.railData(user, "default") {
		data[k] = val
	}
	h.render(w, r, "visions.html", data)
}

// facetVisionRings — GET /facets/vision/rings?scope=
// The ring rail on its own, so it can be refreshed after a Vision is opened
// without redrawing the Playground under it.
func (h *Handler) facetVisionRings(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rail, err := h.buildVisionRail(user, visionScope(r.URL.Query().Get("scope")))
	if err != nil {
		log.Printf("[vision-view] rings for %s: %v", user.ID, err)
		http.Error(w, "the Vision rail could not be read", http.StatusInternalServerError)
		return
	}
	h.renderPartial(w, "vision_ring_rail", rail)
}

// facetVisionProfileRing — GET /facets/vision/ring?handle=<handle>
// One author's avatar with its ring, for the head of their profile. The ring is
// the same server answer the rail carries, so a profile ring and a rail ring can
// never disagree. An author with nothing live renders the plain avatar.
func (h *Handler) facetVisionProfileRing(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	viewer := h.userFromRequest(w, r)
	if viewer == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	handle := strings.TrimSpace(r.URL.Query().Get("handle"))
	if handle == "" {
		http.Error(w, "handle is required", http.StatusBadRequest)
		return
	}
	author, err := dbpkg.GetUserByHandle(h.db, handle)
	if err != nil {
		// An unknown handle is a 404, not a swallowed error: every other failure
		// is reported rather than rendered as "this author has no Visions".
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "no such handle", http.StatusNotFound)
			return
		}
		log.Printf("[vision-view] profile ring author %q: %v", handle, err)
		http.Error(w, "the Vision ring could not be read", http.StatusInternalServerError)
		return
	}
	if author == nil {
		http.Error(w, "no such handle", http.StatusNotFound)
		return
	}
	isLive, liveURL := false, ""
	if s, err := dbpkg.GetStreamForAuthor(h.db, author.ID); err != nil {
		if !errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
			log.Printf("[vision-view] live status for %s: %v", author.ID, err)
		}
	} else if liveSurfaceVisible(s, viewer) {
		isLive, liveURL = true, "/live/"+s.ID
	}
	ring, err := h.buildVisionProfileRing(author, viewer, h.isHandleOnline(author.Handle), isLive, liveURL)
	if err != nil {
		log.Printf("[vision-view] profile ring for %s: %v", author.ID, err)
		http.Error(w, "the Vision ring could not be read", http.StatusInternalServerError)
		return
	}
	h.renderPartial(w, "vision_profile_ring", ring)
}

// facetVisionReels — GET /facets/vision/reels?scope=
// The reels Playground as a Fragment — how the scope switch is served.
func (h *Handler) facetVisionReels(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	reels, err := h.buildVisionReels(user, visionScope(r.URL.Query().Get("scope")))
	if err != nil {
		log.Printf("[vision-view] reels for %s: %v", user.ID, err)
		http.Error(w, "Visions could not be read", http.StatusInternalServerError)
		return
	}
	h.renderPartial(w, "vision_reels", reels)
}

// facetVisionViewer — GET /facets/vision/viewer?vision=<author_pial>.<seq>&scope=
// One frame of the sequential viewer. Every step is this round trip: the server
// decides the frame, the progress, and where prev and next lead — including the
// crossing into the next author's run. Opening a frame records the view.
func (h *Handler) facetVisionViewer(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	scope := visionScope(r.URL.Query().Get("scope"))
	rawRef := strings.TrimSpace(r.URL.Query().Get("vision"))
	if rawRef == "" {
		http.Error(w, "vision is required", http.StatusBadRequest)
		return
	}
	ref, err := dbpkg.ParseVisionRef(rawRef)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	v, err := dbpkg.GetVisionByRef(h.db, ref, user.PIALID)
	if err != nil {
		if errors.Is(err, dbpkg.ErrVisionNotLive) {
			h.renderVisionGone(w, scope)
			return
		}
		log.Printf("[vision-view] load %s: %v", ref, err)
		http.Error(w, "the Vision could not be read", http.StatusInternalServerError)
		return
	}
	// An 18+ Vision is never rendered to a viewer whose account is not cleared
	// for it. The gate is the same one the rest of the platform uses.
	if v.IsNSFW && excludeNSFW(user) {
		h.renderVisionGone(w, scope)
		return
	}
	if v.IsBlocked {
		h.renderVisionGone(w, scope)
		return
	}

	// Opening the frame is what marks it seen. Idempotent, so a step back and
	// forward over the same Vision writes once. An author reading their own
	// Vision is not one of its viewers, so it is not recorded as a view.
	if user.PIALID != "" && user.PIALID != v.AuthorPIAL {
		if mErr := dbpkg.MarkVisionViewed(h.db, ref, user.PIALID); mErr != nil {
			if errors.Is(mErr, dbpkg.ErrVisionNotLive) {
				h.renderVisionGone(w, scope)
				return
			}
			log.Printf("[vision-view] mark viewed %s: %v", ref, mErr)
			http.Error(w, "the Vision could not be opened", http.StatusInternalServerError)
			return
		}
	}

	frame, err := h.buildVisionFrame(v, user, scope)
	if err != nil {
		log.Printf("[vision-view] frame %s: %v", ref, err)
		http.Error(w, "the Vision could not be opened", http.StatusInternalServerError)
		return
	}
	h.renderPartial(w, "vision_viewer", frame)
}

// buildVisionFrame places one Vision in its sequence and resolves where the
// viewer goes from here. A Vision outside the current scope still plays: its
// author's own run becomes the sequence, so a direct open is never a dead end.
func (h *Handler) buildVisionFrame(v *model.Vision, viewer *model.User, scope string) (visionFrameView, error) {
	seq, err := h.visionSequence(viewer, scope)
	if err != nil {
		return visionFrameView{}, err
	}
	ref := visionRef(v)
	loc := locateVision(seq, ref)
	if !loc.Found {
		run, aErr := dbpkg.GetActiveVisionsForAuthor(h.db, v.AuthorPIAL, viewer.PIALID, 0)
		if aErr != nil {
			return visionFrameView{}, aErr
		}
		// The author read carries no clearance of its own, so the viewer's gate is
		// applied here — otherwise prev/next could point at a Vision this viewer
		// would be refused, and the sequence would dead-end on it.
		seq = gateVisionsFor(viewer, run)
		loc = locateVision(seq, ref)
		if !loc.Found {
			// The Vision is live but sits in no sequence — play it as a run of one
			// rather than rendering a frame with no place in anything.
			seq = []*model.Vision{v}
			loc = visionRuns{Index: 0, RunStart: 0, RunEnd: 1, Found: true}
		}
	}

	fv := visionFrameView{
		Still:     buildVisionStill(v, h.loadSharedWork(v, viewer)),
		Scope:     scope,
		Position:  loc.Index - loc.RunStart + 1,
		Total:     loc.RunEnd - loc.RunStart,
		ViewCount: v.ViewCount,
		IsAuthor:  viewer != nil && viewer.PIALID == v.AuthorPIAL,
		CloseURL:  "/visions?scope=" + scope,
	}
	fv.Segments = visionSegments(fv.Position, fv.Total)
	if loc.Index > 0 {
		fv.PrevURL = visionViewerURL(visionRef(seq[loc.Index-1]).String(), scope)
	}
	if loc.Index+1 < len(seq) {
		fv.NextURL = visionViewerURL(visionRef(seq[loc.Index+1]).String(), scope)
	}
	fv.IsFirst = fv.PrevURL == ""
	fv.IsLast = fv.NextURL == ""
	return fv, nil
}

// renderVisionGone answers a Vision that has expired, been deleted, been
// blocked or is walled from this viewer. It is a rendered Fragment like any
// other: the browser is never left to work out why a frame did not arrive.
func (h *Handler) renderVisionGone(w http.ResponseWriter, scope string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "vision_viewer", visionFrameView{
		Scope:    scope,
		Gone:     true,
		CloseURL: "/visions?scope=" + scope,
	})
}
