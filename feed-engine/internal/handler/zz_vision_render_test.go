package handler

import (
	"strings"
	"testing"
	"time"

	"github.com/f33d3r/feed-engine/internal/model"
)

// visionTestComposer builds a composer view the way the handler does, without a
// request behind it.
func visionTestComposer(mode string) visionComposerView {
	u := &model.User{
		ID: "11111111-1111-1111-1111-111111111111", Handle: "nantar",
		PIALID:  "22222222-2222-2222-2222-222222222222",
		IsAdult: true, ContentSetting: "adult_enabled",
	}
	return buildVisionComposer(mode, u, nil)
}

func TestVisionFacetsRender(t *testing.T) {
	h := liveTestHandler(t)

	art := buildVisionArtboard("33333333-3333-3333-3333-333333333333",
		"Twitter users are missing Visions", "ember", "display", "auto", "center")
	created := visionCreatedView{
		VisionID:     art.EntityID,
		Handle:      "nantar",
		ContentType: "text",
		ExpiryLabel: "gone in 24h",
		Artboard:    art,
		HasArtboard: true,
	}
	createdMedia := visionCreatedView{
		VisionID:     "44444444-4444-4444-4444-444444444444",
		Handle:      "nantar",
		ContentType: "image",
		ExpiryLabel: "gone in 24h",
		Artboard:    buildVisionArtboard("44444444-4444-4444-4444-444444444444", "", "void", "grotesk", "auto", "center"),
		PreviewURL:  "/static/uploads/visions/x.jpg",
	}

	cases := []struct {
		name string
		data interface{}
		want string
	}{
		{"vision_artboard", art, "facet:f33d3r:vision:" + art.EntityID + ":artboard"},
		{"vision_artboard", buildVisionArtboard("new", "", "void", "grotesk", "auto", "left"),
			"facet:f33d3r:vision:new:artboard"},
		{"vision_text_composer", visionTestComposer("text"), "facet:f33d3r:vision:new:text_composer"},
		{"vision_media_composer", visionTestComposer("media"), "facet:f33d3r:vision:new:media_composer"},
		{"vision_created", created, "facet:f33d3r:vision:" + created.VisionID + ":created"},
		{"vision_created", createdMedia, "/static/uploads/visions/x.jpg"},
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
	}
}

// TestVisionArtboardIsServerStyled proves the artboard carries only preset-keyed
// classes: a style attribute here would mean the browser was handed the look.
func TestVisionArtboardIsServerStyled(t *testing.T) {
	h := liveTestHandler(t)
	for _, bg := range visionBackgrounds {
		art := buildVisionArtboard("new", "a much longer body than the huge rung allows for, deliberately",
			bg.Key, "serif", "auto", "right")
		frag, err := h.renderVisionFragment("vision_artboard", art)
		if err != nil {
			t.Fatalf("vision_artboard %s: %v", bg.Key, err)
		}
		if strings.Contains(frag, "style=") {
			t.Errorf("vision_artboard %s: fragment carries a style attribute", bg.Key)
		}
		for _, want := range []string{
			"vision-artboard--bg-" + bg.Key,
			"vision-artboard--face-serif",
			"vision-artboard--align-right",
		} {
			if !strings.Contains(frag, want) {
				t.Errorf("vision_artboard %s: missing class %q", bg.Key, want)
			}
		}
		if strings.Contains(frag, "vision-artboard--scale-auto") {
			t.Errorf("vision_artboard %s: 'auto' reached the browser unresolved", bg.Key)
		}
	}
}

func TestResolveVisionTypeScale(t *testing.T) {
	cases := []struct {
		body, scale, want string
	}{
		{"short", "auto", "xl"},
		{strings.Repeat("a", 60), "auto", "l"},
		{strings.Repeat("a", 120), "auto", "m"},
		{strings.Repeat("a", 200), "auto", "s"},
		{"short", "s", "s"},
		{"short", "nonsense", "xl"}, // unknown key falls back to auto, then resolves
		{"", "auto", "xl"},
	}
	for _, c := range cases {
		if got := resolveVisionTypeScale(c.body, c.scale); got != c.want {
			t.Errorf("resolveVisionTypeScale(%d runes, %q) = %q, want %q",
				len([]rune(c.body)), c.scale, got, c.want)
		}
	}
}

// TestVisionContentTypeIsClosed guards the defect this lane exists to fix: the
// content type is checked against a closed set, never taken on trust.
func TestVisionContentTypeIsClosed(t *testing.T) {
	for _, ok := range []string{"text", "image", "video", "share"} {
		if !visionComposeContentTypes[ok] {
			t.Errorf("content_type %q should be accepted", ok)
		}
	}
	for _, bad := range []string{"vision", "post", "reply", "", "TEXT", "share "} {
		if visionComposeContentTypes[bad] {
			t.Errorf("content_type %q should be refused", bad)
		}
	}
}

func TestVisionPresetOrError(t *testing.T) {
	if got, err := visionPresetOrError("artboard_background", "", visionBackgroundSet, visionDefaultBackground); err != nil || got != visionDefaultBackground {
		t.Errorf("empty key should default: got %q, %v", got, err)
	}
	if got, err := visionPresetOrError("artboard_background", "ember", visionBackgroundSet, visionDefaultBackground); err != nil || got != "ember" {
		t.Errorf("known key should pass: got %q, %v", got, err)
	}
	if _, err := visionPresetOrError("artboard_background", "url(evil)", visionBackgroundSet, visionDefaultBackground); err == nil {
		t.Error("unknown key should be refused on the write path")
	}
	// The read path is the opposite: an unknown stored key still renders.
	if got := visionPresetOrDefault("retired-preset", visionBackgroundSet, visionDefaultBackground); got != visionDefaultBackground {
		t.Errorf("unknown stored key should render as default, got %q", got)
	}
}

func TestVisionSharedWorkID(t *testing.T) {
	const id = "55555555-5555-5555-5555-555555555555"
	for _, raw := range []string{
		id,
		"https://f33d3r.com/nantar/status/" + id,
		"/nantar/status/" + id + "?ref=vision",
		"https://f33d3r.com/nantar/status/" + id + "/",
	} {
		got, err := visionSharedWorkID(raw)
		if err != nil {
			t.Errorf("visionSharedWorkID(%q): %v", raw, err)
			continue
		}
		if got != id {
			t.Errorf("visionSharedWorkID(%q) = %q, want %q", raw, got, id)
		}
	}
	for _, raw := range []string{"", "not-a-work", "https://f33d3r.com/nantar"} {
		if _, err := visionSharedWorkID(raw); err == nil {
			t.Errorf("visionSharedWorkID(%q) should have failed", raw)
		}
	}
}

func TestVisionLocalAssetURL(t *testing.T) {
	if got, err := visionLocalAssetURL("/static/media/posts/a.webp"); err != nil || got != "/static/media/posts/a.webp" {
		t.Errorf("same-origin path should pass: %q %v", got, err)
	}
	for _, raw := range []string{"", "https://evil.example/x.jpg", "//evil.example/x.jpg", "/static/../../etc/passwd"} {
		if _, err := visionLocalAssetURL(raw); err == nil {
			t.Errorf("visionLocalAssetURL(%q) should have failed", raw)
		}
	}
}

func TestVisionSniffImage(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0}
	if _, err := visionSniffImage(jpeg, "vision.jpg"); err != nil {
		t.Errorf("real jpeg should pass: %v", err)
	}
	if _, err := visionSniffImage([]byte("<html><body>hi</body>"), "vision.jpg"); err == nil {
		t.Error("html renamed to .jpg should be refused")
	}
	if _, err := visionSniffImage(jpeg, "vision.mp4"); err == nil {
		t.Error("a video extension should not enter the image path")
	}
	if _, err := visionSniffImage([]byte{0xFF}, "vision.jpg"); err == nil {
		t.Error("a truncated file should be refused")
	}
}

func TestVisionExpiryLabel(t *testing.T) {
	now := time.Now()
	cases := []struct {
		at   time.Time
		want string
	}{
		{now.Add(-time.Second), "expired"},
		{now.Add(30 * time.Second), "gone in under a minute"},
		{now.Add(2 * time.Hour), "gone in 2h"},
		{now.Add(24 * time.Hour), "gone in 24h"},
		{now.Add(72 * time.Hour), "gone in 3d"},
	}
	for _, c := range cases {
		if got := visionExpiryLabel(c.at); got != c.want {
			t.Errorf("visionExpiryLabel(%v) = %q, want %q", c.at, got, c.want)
		}
	}
}

func TestVisionActorRequiresPIAL(t *testing.T) {
	if visionActor(nil) != nil {
		t.Error("a nil viewer cannot author a Vision")
	}
	if visionActor(&model.User{ID: "demo_user", PIALID: "p"}) != nil {
		t.Error("the demo account cannot author a Vision")
	}
	if visionActor(&model.User{ID: "u1"}) != nil {
		t.Error("an account with no PIAL cannot author a Vision — the row requires one")
	}
	if visionActor(&model.User{ID: "u1", PIALID: "p"}) == nil {
		t.Error("a real account with a PIAL should be able to author a Vision")
	}
}
