package handler

import (
	"strings"
	"testing"

	"github.com/f33d3r/feed-engine/internal/model"
)

// The profile's live hero is a surface, not a stream: it has to exist and be
// addressable when nobody is broadcasting, and it has to disappear completely —
// not degrade into a placeholder — when it has nothing to show.

func TestProfileLiveStageRendersBroadcast(t *testing.T) {
	h := liveTestHandler(t)
	v := liveTestView()

	frag, err := h.renderLiveFragment("profile_live_stage", map[string]interface{}{
		"AuthorID": "author-1", "V": v,
	})
	if err != nil {
		t.Fatalf("render profile_live_stage: %v", err)
	}
	for _, want := range []string{
		`data-facet-id="facet:f33d3r:profile:author-1:live_stage"`,
		`data-facet-id="facet:f33d3r:live:` + v.S.ID + `:player"`,
		`data-f33d-live-hls="/live/` + v.S.ID + `/master.m3u8"`,
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("live hero missing %q\n%s", want, frag)
		}
	}
	// Only the surface's own tag is under test — the player it carries has
	// hidden children of its own.
	if strings.Contains(stageOpenTag(t, frag), "hidden") {
		t.Errorf("a hero carrying a broadcast must not be hidden\n%s", frag)
	}
}

func TestProfileLiveStageEmptyLeavesNoTrace(t *testing.T) {
	h := liveTestHandler(t)

	frag, err := h.renderLiveFragment("profile_live_stage", map[string]interface{}{
		"AuthorID": "author-1", "V": nil,
	})
	if err != nil {
		t.Fatalf("render empty profile_live_stage: %v", err)
	}
	if !strings.Contains(frag, `data-facet-id="facet:f33d3r:profile:author-1:live_stage"`) {
		t.Errorf("the surface must exist and stay addressable when empty\n%s", frag)
	}
	if !strings.Contains(stageOpenTag(t, frag), "hidden") {
		t.Errorf("an empty hero must reserve no space\n%s", frag)
	}
	for _, forbidden := range []string{"live-player", "<video", "live-badge"} {
		if strings.Contains(frag, forbidden) {
			t.Errorf("empty hero rendered %q — that is a placeholder\n%s", forbidden, frag)
		}
	}
}

// liveSurfaceVisible is the one gate the profile hero and the Playground strip
// answer to. It must never be looser than the watch surface.
func TestLiveSurfaceVisible(t *testing.T) {
	stream := func(mut func(*model.LiveStream)) *model.LiveStream {
		s := &model.LiveStream{
			ID: "11111111-1111-1111-1111-111111111111", Status: model.LiveStatusLive,
			ScanState: "clean", AuthorHandle: "nantar",
		}
		mut(s)
		return s
	}
	adult := &model.User{ID: "u1", ContentSetting: "adult_enabled"}
	safe := &model.User{ID: "u2", ContentSetting: "safe_mode"}
	minor := &model.User{ID: "u3", ContentSetting: "adult_enabled", IsMinor: true}

	cases := []struct {
		name   string
		s      *model.LiveStream
		viewer *model.User
		want   bool
	}{
		{"no stream", nil, adult, false},
		{"live and clean", stream(func(*model.LiveStream) {}), adult, true},
		{"live and clean, logged out", stream(func(*model.LiveStream) {}), nil, true},
		{"idle", stream(func(s *model.LiveStream) { s.Status = model.LiveStatusIdle }), adult, false},
		{"ended", stream(func(s *model.LiveStream) { s.Status = model.LiveStatusEnded }), adult, false},
		{"blocked", stream(func(s *model.LiveStream) { s.IsBlocked = true }), adult, false},
		{"scan blocked", stream(func(s *model.LiveStream) { s.ScanState = "blocked" }), adult, false},
		{"nsfw to cleared viewer", stream(func(s *model.LiveStream) { s.IsNSFW = true }), adult, true},
		{"nsfw to safe mode", stream(func(s *model.LiveStream) { s.IsNSFW = true }), safe, false},
		{"nsfw to minor", stream(func(s *model.LiveStream) { s.IsNSFW = true }), minor, false},
		{"nsfw to logged out", stream(func(s *model.LiveStream) { s.IsNSFW = true }), nil, false},
	}
	for _, c := range cases {
		if got := liveSurfaceVisible(c.s, c.viewer); got != c.want {
			t.Errorf("%s: liveSurfaceVisible = %v, want %v", c.name, got, c.want)
		}
	}
}

// stageOpenTag returns the hero's own opening tag, so an assertion about the
// surface is never answered by markup belonging to a Facet nested inside it.
func stageOpenTag(t *testing.T, frag string) string {
	t.Helper()
	i := strings.Index(frag, "<section")
	if i < 0 {
		t.Fatalf("no <section> in fragment:\n%s", frag)
	}
	j := strings.Index(frag[i:], ">")
	if j < 0 {
		t.Fatalf("unterminated <section> in fragment:\n%s", frag)
	}
	return frag[i : i+j+1]
}
