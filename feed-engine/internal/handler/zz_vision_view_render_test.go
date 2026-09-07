package handler

import (
	"strings"
	"testing"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

const (
	visionTestAuthorA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	visionTestAuthorB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

// visionTestVision builds one live Vision row the way a read query hands it
// back, addressed by (authorPIAL, seq) rather than a standalone id.
func visionTestVision(authorPIAL string, seq int64, handle, contentType string, viewed bool) *model.Vision {
	return &model.Vision{
		AuthorPIAL: authorPIAL, Seq: seq,
		ContentType: contentType, Body: "Visions are back",
		ArtboardBackground: "ember", ArtboardTypeface: "display",
		ArtboardTypeScale: "auto", ArtboardAlign: "center",
		MediaURLs:      []string{"/static/uploads/visions/a.jpg"},
		CreatedAt:      time.Now().Add(-30 * time.Minute),
		ExpiresAt:      time.Now().Add(23 * time.Hour),
		AuthorHandle:   handle,
		AuthorName:     strings.ToUpper(handle),
		ViewedByViewer: viewed,
		ViewCount:      7,
	}
}

// visionTestSequence is two authors' runs, in the order the SQL hands them
// back: authors newest-first, and one author's Visions oldest-first inside
// their run.
func visionTestSequence() []*model.Vision {
	return []*model.Vision{
		visionTestVision(visionTestAuthorA, 1, "nantar", "text", true),
		visionTestVision(visionTestAuthorA, 2, "nantar", "image", false),
		visionTestVision(visionTestAuthorA, 3, "nantar", "text", false),
		visionTestVision(visionTestAuthorB, 1, "hiiro", "text", false),
	}
}

func visionTestRing(authorPIAL, handle, state string, count, unseen int, next string) *model.VisionRing {
	return &model.VisionRing{
		AuthorPIAL:   authorPIAL,
		AuthorHandle: handle, AuthorName: strings.ToUpper(handle),
		State: state, Count: count, UnseenCount: unseen,
		NextVisionRef: next, LatestAt: time.Now(),
	}
}

// TestVisionViewFacetsRender proves every read-path Facet parses against the
// real template set and carries the Facet id the atlas addresses it by.
func TestVisionViewFacetsRender(t *testing.T) {
	h := liveTestHandler(t)
	seq := visionTestSequence()
	ref0, ref1 := visionRef(seq[0]).String(), visionRef(seq[1]).String()

	still := buildVisionStill(seq[0], nil)
	frame := visionFrameView{
		Still: still, Scope: visionScopeFollowing,
		Position: 1, Total: 3, Segments: visionSegments(1, 3),
		NextURL:  visionViewerURL(ref1, visionScopeFollowing),
		CloseURL: "/visions?scope=following", IsFirst: true,
		ViewCount: 7, IsAuthor: true,
	}
	rail := visionRailView{
		Scope: visionScopeFollowing, Handle: "nantar", AvatarURL: "",
		ComposeURL: "/facets/vision/composer?mode=text", CameraURL: "/visions/camera",
		Rings: []visionRingView{{
			R:       visionTestRing(visionTestAuthorA, "nantar", model.VisionRingUnseen, 3, 2, ref1),
			Scope:   visionScopeFollowing,
			OpenURL: visionViewerURL(ref1, visionScopeFollowing),
			ElemID:  visionRingElemID(visionTestAuthorA),
			Label:   "Open NANTAR's Visions — 2 new",
		}},
	}
	reels := visionReelsView{
		Scope: visionScopeForYou, FollowingURL: "/facets/vision/reels?scope=following",
		ForYouURL: "/facets/vision/reels?scope=for_you", ComposeURL: "/facets/vision/composer?mode=text",
		Reels: []visionReelView{{
			Still: buildVisionStill(seq[1], nil), Scope: visionScopeForYou,
			OpenURL: visionViewerURL(ref1, visionScopeForYou), Index: 1, Total: 1,
		}},
	}

	cases := []struct {
		name string
		data interface{}
		want string
	}{
		{"vision_still", still, "facet:f33d3r:vision:" + ref0 + ":still"},
		{"vision_still", buildVisionStill(seq[1], nil), "/static/uploads/visions/a.jpg"},
		{"vision_progress", map[string]interface{}{
			"VisionID": ref0, "Position": 1, "Total": 3, "Segments": visionSegments(1, 3),
		}, "facet:f33d3r:vision:" + ref0 + ":progress"},
		{"vision_ring", rail.Rings[0], "facet:f33d3r:vision:" + visionTestAuthorA + ":ring"},
		{"vision_ring_rail", rail, "facet:f33d3r:vision:rail:rings"},
		{"vision_reel", reels.Reels[0], "facet:f33d3r:vision:" + ref1 + ":reel"},
		{"vision_reels", reels, "facet:f33d3r:vision:reels:playground"},
		{"vision_reels", visionReelsView{Scope: visionScopeFollowing, IsEmpty: true,
			ComposeURL: "/facets/vision/composer?mode=text"}, "No live Visions"},
		{"vision_viewer", frame, "facet:f33d3r:vision:" + ref0 + ":viewer"},
		{"vision_viewer", visionFrameView{Gone: true, Scope: visionScopeFollowing,
			CloseURL: "/visions?scope=following"}, "facet:f33d3r:vision:gone:viewer"},
	}

	for _, c := range cases {
		frag, err := h.renderVisionFragment(c.name, c.data)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !strings.Contains(frag, c.want) {
			t.Errorf("%s: fragment missing %q", c.name, c.want)
		}
		if strings.Contains(frag, "<no value>") {
			t.Errorf("%s: fragment contains an unfilled key", c.name)
		}
		if strings.Contains(frag, "<html") || strings.Contains(frag, "<body") {
			t.Errorf("%s: fragment carries a document tag", c.name)
		}
		if strings.Contains(frag, "<script") {
			t.Errorf("%s: fragment carries inline script", c.name)
		}
	}
}

// TestVisionViewerCarriesServerNavigation proves the browser is handed
// finished addresses for every step, never the sequence it would have to
// walk itself.
func TestVisionViewerCarriesServerNavigation(t *testing.T) {
	h := liveTestHandler(t)
	seq := visionTestSequence()
	ref0, ref2 := visionRef(seq[0]).String(), visionRef(seq[2]).String()
	frame := visionFrameView{
		Still: buildVisionStill(seq[1], nil), Scope: visionScopeFollowing,
		Position: 2, Total: 3, Segments: visionSegments(2, 3),
		PrevURL:  visionViewerURL(ref0, visionScopeFollowing),
		NextURL:  visionViewerURL(ref2, visionScopeFollowing),
		CloseURL: "/visions?scope=following",
	}
	frag, err := h.renderVisionFragment("vision_viewer", frame)
	if err != nil {
		t.Fatalf("vision_viewer: %v", err)
	}
	// The template escapes the query separator, so the addresses are compared in
	// the form they actually reach the browser in.
	esc := func(u string) string { return strings.ReplaceAll(u, "&", "&amp;") }
	for _, want := range []string{
		"data-vision-prev", "data-vision-next", "data-vision-close",
		esc(visionViewerURL(ref0, visionScopeFollowing)),
		esc(visionViewerURL(ref2, visionScopeFollowing)),
		`data-vision-return="` + visionRingElemID(seq[1].AuthorPIAL) + `"`,
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("vision_viewer: missing %q", want)
		}
	}
	// The progress ticks are the server's answer, not a countdown the browser runs.
	if strings.Contains(frag, "data-expires") {
		t.Error("vision_viewer: an expiry timestamp reached the browser to be computed from")
	}
}

// TestVisionStillTextIsServerStyled proves a text Vision renders through the
// same artboard path everywhere: no style attribute, and no unresolved
// 'auto' rung.
func TestVisionStillTextIsServerStyled(t *testing.T) {
	h := liveTestHandler(t)
	v := visionTestVision(visionTestAuthorA, 5, "nantar", "text", false)
	for _, name := range []string{"vision_still", "vision_viewer"} {
		var data interface{} = buildVisionStill(v, nil)
		if name == "vision_viewer" {
			data = visionFrameView{
				Still: buildVisionStill(v, nil), Scope: visionScopeFollowing,
				Position: 1, Total: 1, Segments: visionSegments(1, 1),
				CloseURL: "/visions?scope=following",
			}
		}
		frag, err := h.renderVisionFragment(name, data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(frag, "vision-artboard--scale-auto") {
			t.Errorf("%s: 'auto' reached the browser unresolved", name)
		}
		if !strings.Contains(frag, "vision-artboard--bg-ember") {
			t.Errorf("%s: the stored background preset did not reach the class", name)
		}
	}
}

// TestVisionStillSelectsRenderer proves the content type picks the Facet path
// on the server, and that a media Vision with no object still renders its
// words.
func TestVisionStillSelectsRenderer(t *testing.T) {
	text := buildVisionStill(visionTestVision(visionTestAuthorA, 1, "n", "text", false), nil)
	if !text.HasArtboard || text.IsImage || text.IsVideo {
		t.Error("a text Vision must render as an artboard")
	}
	img := buildVisionStill(visionTestVision(visionTestAuthorA, 2, "n", "image", false), nil)
	if !img.IsImage || img.MediaURL == "" {
		t.Error("an image Vision must render its stored object")
	}
	vid := visionTestVision(visionTestAuthorA, 3, "n", "video", false)
	vid.MediaURLs = []string{"/static/uploads/visions/m.m3u8"}
	v := buildVisionStill(vid, nil)
	if !v.IsVideo || !v.IsHLS {
		t.Error("an HLS master must be marked for the decoder")
	}
	orphan := visionTestVision(visionTestAuthorA, 4, "n", "image", false)
	orphan.MediaURLs = nil
	o := buildVisionStill(orphan, nil)
	if !o.HasArtboard {
		t.Error("a media Vision with no object must still render something readable")
	}
	share := visionTestVision(visionTestAuthorA, 5, "n", "share", false)
	s := buildVisionStill(share, nil)
	if !s.HasShare {
		t.Error("a share Vision must render the share path")
	}
}

// TestLocateVisionRuns proves the sequence walk: an author's run is
// contiguous, and stepping off the end of one run lands in the next
// author's.
func TestLocateVisionRuns(t *testing.T) {
	seq := visionTestSequence()

	loc := locateVision(seq, visionRef(seq[1]))
	if !loc.Found || loc.Index != 1 || loc.RunStart != 0 || loc.RunEnd != 3 {
		t.Fatalf("run for the middle of author A = %+v", loc)
	}
	if pos := loc.Index - loc.RunStart + 1; pos != 2 {
		t.Errorf("position in run = %d, want 2", pos)
	}

	last := locateVision(seq, visionRef(seq[2]))
	if last.Index+1 >= len(seq) {
		t.Fatal("the tail of author A's run must have a next")
	}
	if seq[last.Index+1].AuthorPIAL != visionTestAuthorB {
		t.Error("stepping off the end of a run must cross into the next author")
	}

	b := locateVision(seq, visionRef(seq[3]))
	if !b.Found || b.RunStart != 3 || b.RunEnd != 4 {
		t.Fatalf("author B's run = %+v", b)
	}

	if locateVision(seq, dbpkg.VisionRef{AuthorPIAL: "not-in-this-scope", Seq: 1}).Found {
		t.Error("a Vision outside the scope must not be located in it")
	}
}

func TestVisionSegments(t *testing.T) {
	got := visionSegments(2, 3)
	want := []string{"done", "current", "todo"}
	if len(got) != len(want) {
		t.Fatalf("segments = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].State != want[i] {
			t.Errorf("segment %d = %q, want %q", i, got[i].State, want[i])
		}
	}
	if visionSegments(1, 0) != nil {
		t.Error("an empty run has no ticks")
	}
}

// TestVisionScopeIsClosed proves the scope is never a value taken on trust.
func TestVisionScopeIsClosed(t *testing.T) {
	if visionScope("for_you") != visionScopeForYou {
		t.Error("for_you should be honoured")
	}
	for _, raw := range []string{"", "following", "FOR_YOU", "'; DROP TABLE visions;--", "fleet"} {
		if visionScope(raw) != visionScopeFollowing {
			t.Errorf("scope %q should fall back to the following scope", raw)
		}
	}
}

// TestVisionRingsGatedToRestatesFromSequence proves a viewer who must not see
// 18+ content is handed rings recomputed from the sequence they are allowed
// to read, so a ring never advertises a Vision its owner would be refused.
func TestVisionRingsGatedToRestatesFromSequence(t *testing.T) {
	seq := visionTestSequence()
	rings := []*model.VisionRing{
		visionTestRing(visionTestAuthorA, "nantar", model.VisionRingUnseen, 9, 9, "gone"),
		visionTestRing("cccccccc-cccc-cccc-cccc-cccccccccccc", "walled", model.VisionRingUnseen, 4, 4, "walled-1"),
	}
	// Gating restates the rings the query already returned; it never invents one.
	// Author B has live Visions but no ring here, so only author A survives.
	got := visionRingsGatedTo(rings, seq)
	if len(got) != 1 {
		t.Fatalf("gated rings = %d, want 1 (author A; the walled author drops)", len(got))
	}
	a := got[0]
	if a.AuthorPIAL != visionTestAuthorA {
		t.Fatalf("first ring = %s", a.AuthorPIAL)
	}
	if a.Count != 3 {
		t.Errorf("gated count = %d, want 3", a.Count)
	}
	if a.UnseenCount != 2 {
		t.Errorf("gated unseen = %d, want 2", a.UnseenCount)
	}
	if want := visionRef(seq[1]).String(); a.NextVisionRef != want {
		t.Errorf("resume point = %s, want the first unseen %s", a.NextVisionRef, want)
	}
	if a.State != model.VisionRingUnseen {
		t.Errorf("state = %s, want unseen", a.State)
	}
	for _, r := range got {
		if r.AuthorHandle == "walled" {
			t.Error("an author with nothing readable must drop off the rail")
		}
	}

	// Every Vision seen collapses the ring to seen and resumes at the run's head.
	allSeen := []*model.Vision{visionTestVision(visionTestAuthorA, 9, "nantar", "text", true)}
	seen := visionRingsGatedTo(rings[:1], allSeen)
	if len(seen) != 1 || seen[0].State != model.VisionRingSeen || seen[0].UnseenCount != 0 {
		t.Fatalf("all-seen ring = %+v", seen)
	}
	if want := visionRef(allSeen[0]).String(); seen[0].NextVisionRef != want {
		t.Errorf("a fully seen run resumes at its head, got %s, want %s", seen[0].NextVisionRef, want)
	}
	// The source rings are never mutated — the rail is a restatement, not an edit.
	if rings[0].Count != 9 {
		t.Error("gating must not write back into the query's answer")
	}
}

func TestVisionRingLabelStatesTheAnswer(t *testing.T) {
	cases := []struct {
		r    *model.VisionRing
		want string
	}{
		{visionTestRing(visionTestAuthorA, "nantar", model.VisionRingUnseen, 3, 1, "x"), "1 new"},
		{visionTestRing(visionTestAuthorA, "nantar", model.VisionRingUnseen, 3, 2, "x"), "2 new"},
		{visionTestRing(visionTestAuthorA, "nantar", model.VisionRingSeen, 1, 0, "x"), "already seen"},
		{visionTestRing(visionTestAuthorA, "nantar", model.VisionRingSeen, 4, 0, "x"), "all seen"},
	}
	for _, c := range cases {
		if got := visionRingLabel(c.r); !strings.Contains(got, c.want) {
			t.Errorf("visionRingLabel = %q, want it to contain %q", got, c.want)
		}
	}
	anon := visionTestRing(visionTestAuthorA, "nantar", model.VisionRingUnseen, 1, 1, "x")
	anon.AuthorName = ""
	if !strings.Contains(visionRingLabel(anon), "@nantar") {
		t.Error("an author with no display name is named by handle")
	}
}

func TestVisionViewerURLCarriesScope(t *testing.T) {
	got := visionViewerURL("abc.1", visionScopeForYou)
	if !strings.Contains(got, "vision=abc.1") || !strings.Contains(got, "scope=for_you") {
		t.Errorf("visionViewerURL = %q", got)
	}
}
