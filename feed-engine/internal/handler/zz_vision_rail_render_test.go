package handler

// zz_vision_rail_render_test.go — the Vision lane's discovery surface.
//
// Two things are proved here. First, that the rail the home timeline and the
// profile mount is assembled entirely on the server: the viewer's own entry is
// separated out of the one ring answer, an author is never listed twice, and a
// ring with nowhere to resume is not rendered as a dead target. Second, that no
// surface a user can read ever shows the retired name "Fleet" — this lane and
// the code that renders it are Visions end to end now.

import (
	"strings"
	"testing"

	"github.com/f33d3r/feed-engine/internal/model"
)

const visionTestViewerID = "dddddddd-dddd-dddd-dddd-dddddddddddd"

// visionTestRail is the rail as the server hands it to the Facet.
func visionTestRail(rings []*model.VisionRing) visionRailView {
	own, hasOwn, row := visionRailEntries(rings, visionTestViewerID, visionScopeFollowing)
	return visionRailView{
		Rings: row, Own: own, HasOwn: hasOwn,
		Scope:      visionScopeFollowing,
		Handle:     "miiyazuko",
		ComposeURL: "/facets/vision/composer?mode=text",
		CameraURL:  "/visions/camera",
		IsEmpty:    len(row) == 0 && !hasOwn,
	}
}

// TestVisionRailSeparatesTheViewersOwnEntry proves the viewer's own ring leaves
// the ordered row and becomes the rail's first entry, so an author who posts and
// then looks at their own timeline sees themselves once, with a ring.
func TestVisionRailSeparatesTheViewersOwnEntry(t *testing.T) {
	rings := []*model.VisionRing{
		visionTestRing(visionTestViewerID, "miiyazuko", model.VisionRingUnseen, 1, 1, "11111111-1111-1111-1111-111111111111.1"),
		visionTestRing(visionTestAuthorA, "nantar", model.VisionRingUnseen, 3, 2, "22222222-2222-2222-2222-222222222221.1"),
		// No resume point — nothing to open, so nothing to draw.
		visionTestRing(visionTestAuthorB, "hiiro", model.VisionRingSeen, 1, 0, ""),
	}
	own, hasOwn, row := visionRailEntries(rings, visionTestViewerID, visionScopeFollowing)
	if !hasOwn {
		t.Fatal("the viewer's own live Vision must produce the rail's own entry")
	}
	if own.R.AuthorPIAL != visionTestViewerID {
		t.Fatalf("own entry = %s", own.R.AuthorPIAL)
	}
	if own.OpenURL == "" || !strings.Contains(own.OpenURL, "/facets/vision/viewer?vision=") {
		t.Fatalf("own entry must carry a server-resolved resume address, got %q", own.OpenURL)
	}
	if own.Label != "Open your Vision" {
		t.Fatalf("own label = %q", own.Label)
	}
	if len(row) != 1 || row[0].R.AuthorPIAL != visionTestAuthorA {
		t.Fatalf("row = %+v, want the followed author alone", row)
	}
	for _, e := range row {
		if e.R.AuthorPIAL == visionTestViewerID {
			t.Error("the viewer appears twice on their own rail")
		}
	}
}

// TestVisionRailIsDiscoverableWhenEmpty proves a viewer nobody has posted to
// still gets a rail: their own avatar and an add affordance. An invisible
// feature is worse than an empty one.
func TestVisionRailIsDiscoverableWhenEmpty(t *testing.T) {
	h := liveTestHandler(t)
	rail := visionTestRail(nil)
	if !rail.IsEmpty {
		t.Fatal("a rail with no rings at all must report itself empty")
	}
	frag, err := h.renderVisionFragment("vision_ring_rail", rail)
	if err != nil {
		t.Fatalf("vision_ring_rail: %v", err)
	}
	for _, want := range []string{
		"facet:f33d3r:vision:rail:rings",
		`aria-label="Post a Vision"`,
		"vision-ring--compose",
		"Your Vision",
		"No live Visions yet",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("empty rail missing %q", want)
		}
	}
}

// TestVisionRailRendersTheOwnRing proves the own entry is drawn as a ring that
// opens the sequence, with the add affordance still beside it.
func TestVisionRailRendersTheOwnRing(t *testing.T) {
	h := liveTestHandler(t)
	rail := visionTestRail([]*model.VisionRing{
		visionTestRing(visionTestViewerID, "miiyazuko", model.VisionRingUnseen, 2, 2, "11111111-1111-1111-1111-111111111111.1"),
	})
	if rail.IsEmpty {
		t.Fatal("a rail carrying the viewer's own live Vision is not empty")
	}
	frag, err := h.renderVisionFragment("vision_ring_rail", rail)
	if err != nil {
		t.Fatalf("vision_ring_rail: %v", err)
	}
	for _, want := range []string{
		"vision-ring--unseen",
		`id="` + visionRingElemID(visionTestViewerID) + `"`,
		"data-vision-open",
		`hx-target="#vision-viewer-slot"`,
		"vision-rail__add",
		`aria-label="Post another Vision"`,
		"Open your 2 Visions",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("own-ring rail missing %q", want)
		}
	}
	if strings.Contains(frag, "vision-ring--compose") {
		t.Error("a viewer with a live Vision must not also be offered the compose ring")
	}
}

// TestVisionProfileRingRendersBothStates proves the profile head is the same
// avatar either way, and that the ring — when there is one — is an entry point
// into the sequence rather than a link back to the profile already open.
func TestVisionProfileRingRendersBothStates(t *testing.T) {
	h := liveTestHandler(t)
	ring := visionTestRing(visionTestAuthorA, "miiyazuko", model.VisionRingUnseen, 1, 1, "11111111-1111-1111-1111-111111111111.1")

	withRing := visionProfileRingView{
		HasRing:  true,
		AuthorID: visionTestAuthorA,
		Handle:   "miiyazuko",
		Name:     "Miiyazuko",
		Ring: visionRingView{
			R: ring, Scope: visionScopeFollowing,
			OpenURL: visionViewerURL(ring.NextVisionRef, visionScopeFollowing),
			ElemID:  visionRingElemID(visionTestAuthorA),
			Label:   visionRingLabel(ring),
		},
	}
	plain := visionProfileRingView{
		AuthorID: visionTestAuthorA, Handle: "miiyazuko", Name: "Miiyazuko",
	}

	on, err := h.renderVisionFragment("vision_profile_ring", withRing)
	if err != nil {
		t.Fatalf("vision_profile_ring: %v", err)
	}
	for _, want := range []string{
		"facet:f33d3r:vision:" + visionTestAuthorA + ":profile_ring",
		"vision-ring--profile",
		"vision-ring--unseen",
		"data-vision-open",
		`hx-target="#vision-viewer-slot"`,
		"user-av--xl",
		"Visions — 1 new",
	} {
		if !strings.Contains(on, want) {
			t.Errorf("profile ring missing %q", want)
		}
	}

	off, err := h.renderVisionFragment("vision_profile_ring", plain)
	if err != nil {
		t.Fatalf("vision_profile_ring plain: %v", err)
	}
	if !strings.Contains(off, "facet:f33d3r:vision:"+visionTestAuthorA+":profile_ring") {
		t.Error("the plain profile avatar is still a Facet with an id")
	}
	if !strings.Contains(off, "user-av--xl") {
		t.Error("the plain profile avatar must still render")
	}
	if strings.Contains(off, "data-vision-open") {
		t.Error("an author with nothing live must not offer an entry point")
	}
}

// TestVisionFacetsNeverSayFleetToTheViewer walks every Facet on the lane and
// proves no rendered Fragment shows the reader the retired name "Fleet".
// Internal tokens — class names, Facet ids, addresses — are lower case by
// construction, so the reader's word is the only capitalised one that could
// appear.
func TestVisionFacetsNeverSayFleetToTheViewer(t *testing.T) {
	h := liveTestHandler(t)
	seq := []*model.Vision{
		visionTestVision(visionTestAuthorA, 1, "miiyazuko", "text", false),
		visionTestVision(visionTestAuthorA, 2, "miiyazuko", "image", false),
	}
	for _, v := range seq {
		v.Body = "fgfgf"
	}
	ref0, ref1 := visionRef(seq[0]).String(), visionRef(seq[1]).String()
	still := buildVisionStill(seq[0], nil)
	frame := visionFrameView{
		Still: still, Scope: visionScopeFollowing,
		Position: 1, Total: 2, Segments: visionSegments(1, 2),
		NextURL:  visionViewerURL(ref1, visionScopeFollowing),
		CloseURL: "/visions?scope=following", IsFirst: true, IsAuthor: true,
	}
	ring := visionTestRing(visionTestAuthorA, "miiyazuko", model.VisionRingUnseen, 2, 1, ref1)
	ringView := visionRingView{
		R: ring, Scope: visionScopeFollowing,
		OpenURL: visionViewerURL(ref1, visionScopeFollowing),
		ElemID:  visionRingElemID(visionTestAuthorA),
		Label:   visionRingLabel(ring),
	}

	cases := []struct {
		name string
		data interface{}
	}{
		{"vision_still", still},
		{"vision_still", buildVisionStill(seq[1], nil)},
		{"vision_artboard", still.Artboard},
		{"vision_progress", map[string]interface{}{
			"VisionID": ref0, "Position": 1, "Total": 2, "Segments": visionSegments(1, 2),
		}},
		{"vision_ring", ringView},
		{"vision_ring_rail", visionTestRail([]*model.VisionRing{ring})},
		{"vision_ring_rail", visionTestRail(nil)},
		{"vision_profile_ring", visionProfileRingView{
			HasRing: true, AuthorID: visionTestAuthorA, Handle: "miiyazuko",
			Name: "Miiyazuko", Ring: ringView,
		}},
		{"vision_reel", visionReelView{
			Still: still, Scope: visionScopeFollowing,
			OpenURL: visionViewerURL(ref0, visionScopeFollowing), Index: 1, Total: 2,
		}},
		{"vision_reels", visionReelsView{
			Scope: visionScopeFollowing, IsEmpty: true,
			ComposeURL: "/facets/vision/composer?mode=text",
		}},
		{"vision_viewer", frame},
		{"vision_viewer", visionFrameView{
			Gone: true, Scope: visionScopeFollowing, CloseURL: "/visions?scope=following",
		}},
		{"vision_text_composer", buildVisionComposer("text", &model.User{Handle: "miiyazuko"}, nil)},
		{"vision_media_composer", buildVisionComposer("media", &model.User{Handle: "miiyazuko"}, nil)},
	}

	for _, c := range cases {
		frag, err := h.renderVisionFragment(c.name, c.data)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if strings.Contains(frag, "Fleet") {
			t.Errorf("%s: a reader is shown the retired name — %q", c.name, visionOffendingLine(frag))
		}
	}
}

// visionOffendingLine returns the first line of a Fragment carrying the
// retired name, so a failure names the string instead of dumping the whole
// Fragment.
func visionOffendingLine(frag string) string {
	for _, line := range strings.Split(frag, "\n") {
		if strings.Contains(line, "Fleet") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
