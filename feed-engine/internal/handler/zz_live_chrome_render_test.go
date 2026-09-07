package handler

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/f33d3r/feed-engine/internal/model"
)

// The shell chrome — the left rail, which responsive.css keeps as the primary
// navigation at every breakpoint — is present on every page unless a broadcast
// is genuinely in progress. It used to be hidden by a class the live page baked
// in from its Mode, which is fixed for the life of the document while a
// broadcast is not: a broadcaster who ended a stream was left on the terminal
// surface with no navigation at all.
//
// The decision now rides on live_broadcast_stage's rendered data-status, so the
// Fragment the server pushes when a broadcast ends brings the chrome back on a
// page that is already open. That contract is split across two files — the
// attribute the Facet renders and the selector the stylesheet reads — and these
// tests hold the two halves together.

// broadcastStageFor renders live_broadcast_stage in one status/source pairing.
func broadcastStageFor(t *testing.T, h *Handler, status, source string) string {
	t.Helper()
	s := liveTestStream()
	s.Status = status
	var buf bytes.Buffer
	err := h.partial.ExecuteTemplate(&buf, "live_broadcast_stage", map[string]interface{}{
		"S": s, "AuthorHandle": s.AuthorHandle, "Facing": "user", "Source": source,
		"Viewers": "0", "ViewerCount": 0, "ShareURL": "/live/" + s.ID,
		"Encoder": encoderSetupData(s.ID, nil),
	})
	if err != nil {
		t.Fatalf("live_broadcast_stage %s/%s: %v", status, source, err)
	}
	return buf.String()
}

// TestLivePageNeverHidesShellChrome — no mode of the live page may hide the nav
// by itself. Mode says which surface this is, never whether a broadcast is
// running, and the ended stage proves the two come apart.
func TestLivePageNeverHidesShellChrome(t *testing.T) {
	h := liveTestHandler(t)
	v := liveTestView()

	modes := []map[string]interface{}{
		{"Mode": "watch", "V": v, "ChatMessages": []liveChatMessage{}, "PageTitle": "t",
			"OGTitle": "t", "OGDesc": "d", "OGImage": "i", "OGUrl": "u"},
		{"Mode": "compose", "ComposerHandle": "nantar", "CanNSFW": false, "Error": "",
			"Title": "", "Description": "", "Source": "browser", "PageTitle": "t"},
		{"Mode": "broadcast", "B": map[string]interface{}{
			"S": liveTestStream(), "AuthorHandle": "nantar", "Facing": "user", "Source": "browser",
			"Viewers": "0", "ViewerCount": 0, "ShareURL": "/live/x",
			"Encoder": encoderSetupData(liveTestStream().ID, nil)}, "PageTitle": "t"},
		{"Mode": "missing", "PageTitle": "t"},
	}
	for _, data := range modes {
		data["User"] = (*model.User)(nil)
		data["RailContext"] = "default"
		for _, block := range []string{"shell-class", "sidebar-hidden", "mobile-nav-hidden"} {
			var buf bytes.Buffer
			if err := h.pages["live.html"].ExecuteTemplate(&buf, block, data); err != nil {
				t.Fatalf("mode %v block %s: %v", data["Mode"], block, err)
			}
			for _, banned := range []string{"sidebar-onboard-hidden", "mobile-nav-onboard-hidden", "shell-live-full"} {
				if strings.Contains(buf.String(), banned) {
					t.Errorf("mode %v block %s hides the shell chrome from the page mode (%q); "+
						"the broadcast's own state decides that", data["Mode"], block, banned)
				}
			}
		}
	}
}

// TestBroadcastStageCarriesChromeStatus — the stage's root element is where the
// server's answer lands. Every status has to reach it, and the ended state has
// to be distinguishable from the two that publish.
func TestBroadcastStageCarriesChromeStatus(t *testing.T) {
	h := liveTestHandler(t)
	for _, c := range []struct{ status, source string }{
		{model.LiveStatusIdle, "browser"},
		{model.LiveStatusIdle, "encoder"},
		{model.LiveStatusLive, "browser"},
		{model.LiveStatusLive, "encoder"},
		{model.LiveStatusEnded, "browser"},
		{model.LiveStatusEnded, "encoder"},
	} {
		frag := broadcastStageFor(t, h, c.status, c.source)
		if !strings.Contains(frag, `data-status="`+c.status+`"`) {
			t.Errorf("stage %s/%s: root carries no data-status=%q", c.status, c.source, c.status)
		}
		if !strings.Contains(frag, ":broadcast\"") {
			t.Errorf("stage %s/%s: root carries no broadcast facet id", c.status, c.source)
		}
	}
}

// TestShellChromeRuleTracksBroadcastStatus — the stylesheet half of the
// contract. The chrome may stand aside for idle and live and for nothing else;
// an ended broadcast is a page like any other and keeps its navigation.
func TestShellChromeRuleTracksBroadcastStatus(t *testing.T) {
	h := liveTestHandler(t) // chdirs to the repo root for the life of the test
	_ = h

	css := shellCSS(t)

	for _, sel := range []string{
		`.f33d3r-shell:has(.live-bcast[data-status="idle"]) .f33d3r-sidebar`,
		`.f33d3r-shell:has(.live-bcast[data-status="live"]) .f33d3r-sidebar`,
	} {
		if !strings.Contains(css, sel) {
			t.Errorf("styles.css lost the chrome rule %q — the broadcast surface "+
				"would keep its navigation while the camera is open", sel)
		}
	}
	if strings.Contains(css, `.live-bcast[data-status="ended"]`) {
		t.Error("styles.css special-cases the ended stage; the rule is positive — " +
			"the chrome stands aside only while a broadcast is in progress")
	}
	if strings.Contains(css, ".shell-live-full") {
		t.Error("styles.css still carries .shell-live-full, the page-mode class the " +
			"broadcast surface no longer renders")
	}
}

// broadcastBody renders live.html's body block in broadcast mode for one stream.
func broadcastBody(t *testing.T, h *Handler, s *model.LiveStream) string {
	t.Helper()
	data := map[string]interface{}{
		"User": (*model.User)(nil), "RailContext": "default",
		"Mode": "broadcast", "PageTitle": "t",
		"B": map[string]interface{}{
			"S": s, "AuthorHandle": "nantar", "Facing": "user", "Source": "browser",
			"Viewers": "0", "ViewerCount": 0, "ShareURL": "/live/" + s.ID,
			"Encoder": encoderSetupData(s.ID, nil)},
	}
	var buf bytes.Buffer
	if err := h.pages["live.html"].ExecuteTemplate(&buf, "body", data); err != nil {
		t.Fatalf("live.html body (broadcast, %s): %v", s.Status, err)
	}
	return buf.String()
}

// TestBroadcasterSeesTheirOwnChat — reading chat is most of the reason a person
// is live. The stage left the column under it empty and the panel was rendered
// only for watchers.
//
// Exactly one panel. live-chat.js appends an arriving line to the FIRST
// .live-chat for the stream in the document, and both copies the watch surface
// renders share the id live-chat-log; a second copy on this surface would put
// the broadcaster's chat in whichever one the stylesheet hid.
func TestBroadcasterSeesTheirOwnChat(t *testing.T) {
	h := liveTestHandler(t)

	for _, status := range []string{model.LiveStatusIdle, model.LiveStatusLive} {
		s := liveTestStream()
		s.Status = status
		body := broadcastBody(t, h, s)
		if n := strings.Count(body, `class="live-chat"`); n != 1 {
			t.Errorf("broadcast/%s renders %d chat panels, want exactly 1", status, n)
		}
		if !strings.Contains(body, "live-chat-bcast") {
			t.Errorf("broadcast/%s: the chat panel is not in the column under the "+
				"stage, which is the only space the surface has for it", status)
		}
		// The broadcaster is a participant, not an audience: the composer, not
		// the logged-out gate.
		if !strings.Contains(body, "live_chat_composer") &&
			!strings.Contains(body, `class="live-chat__composer"`) {
			t.Errorf("broadcast/%s: no composer — the broadcaster could read chat "+
				"but not answer it", status)
		}
		if strings.Contains(body, "live-chat__gate") {
			t.Errorf("broadcast/%s: the broadcaster is shown the logged-out chat "+
				"gate on their own stream", status)
		}
	}

	// Chat belongs to a running broadcast. The server refuses lines for a stream
	// that has ended or been blocked, so the surface must not offer a composer
	// that can only be answered with an error.
	for _, tc := range []struct {
		name string
		mut  func(*model.LiveStream)
	}{
		{"ended", func(s *model.LiveStream) { s.Status = model.LiveStatusEnded }},
		{"blocked", func(s *model.LiveStream) { s.IsBlocked = true }},
	} {
		s := liveTestStream()
		tc.mut(s)
		if body := broadcastBody(t, h, s); strings.Contains(body, "live-chat-bcast") {
			t.Errorf("broadcast/%s still offers chat; the server answers a line "+
				"for it with an error", tc.name)
		}
	}
}

// shellCSS reads the served stylesheet from the repo root the test handler
// chdir'd into.
func shellCSS(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("web", "static", "css", "styles.css"))
	if err != nil {
		t.Fatalf("read styles.css: %v", err)
	}
	return string(raw)
}

// takeoverBoundary is the width at which the shell becomes a desktop shell:
// .f33d3r-right appears and the nav rail takes its labels. The broadcast
// takeover belongs below it and the ordinary three-column shell at and above it.
const takeoverBoundary = "@media (max-width:1219px) {"

// mediaBlock returns every brace-balanced block opened by open, joined. The
// stylesheet states a breakpoint wherever it needs it rather than in one place,
// so "inside this breakpoint" means inside any block that declares it.
func mediaBlock(t *testing.T, css, open string) string {
	t.Helper()
	var found []string
	for at := 0; ; {
		i := strings.Index(css[at:], open)
		if i < 0 {
			break
		}
		start := at + i + len(open)
		depth, end := 1, -1
		for j, r := range css[start:] {
			switch r {
			case '{':
				depth++
			case '}':
				depth--
			}
			if depth == 0 {
				end = start + j
				break
			}
		}
		if end < 0 {
			t.Fatalf("unbalanced braces after %q", open)
		}
		found = append(found, css[start:end])
		at = end
	}
	if len(found) == 0 {
		t.Fatalf("styles.css has no %q block", open)
	}
	return strings.Join(found, "\n")
}

// TestBroadcastTakeoverIsPhoneOnly — the takeover is a phone answer to a phone
// problem. On a desktop the broadcaster keeps the whole shell, so the surface
// can never end up half-hidden: nav gone, rail present, dead space where the nav
// used to be. Both columns stand aside together, and only below the boundary.
func TestBroadcastTakeoverIsPhoneOnly(t *testing.T) {
	h := liveTestHandler(t)
	_ = h
	css := shellCSS(t)

	// Every rule that hides a shell column for a broadcast must sit inside the
	// narrow-width block. A rule that escaped it would hide chrome on a desktop.
	block := mediaBlock(t, css, takeoverBoundary)
	for _, sel := range []string{
		`.f33d3r-shell:has(.live-bcast[data-status="idle"]) .f33d3r-sidebar`,
		`.f33d3r-shell:has(.live-bcast[data-status="live"]) .f33d3r-sidebar`,
		`.f33d3r-shell:has(.live-bcast[data-status="idle"]) .f33d3r-right`,
		`.f33d3r-shell:has(.live-bcast[data-status="live"]) .f33d3r-right`,
		`.f33d3r-main:has(.live-bcast)`,
	} {
		if !strings.Contains(block, sel) {
			t.Errorf("%q is not inside %s — the broadcast surface would take the "+
				"viewport over at desktop widths, where the broadcaster needs the "+
				"rest of the site", sel, takeoverBoundary)
		}
		if strings.Count(css, sel) != strings.Count(block, sel) {
			t.Errorf("%q also appears outside %s", sel, takeoverBoundary)
		}
	}

	// The nav and the rail are one decision. Hiding one without the other is the
	// half-hidden shell the takeover exists to avoid.
	navRules := strings.Count(block, `]) .f33d3r-sidebar`)
	railRules := strings.Count(block, `]) .f33d3r-right`)
	if navRules != railRules {
		t.Errorf("the takeover hides the nav in %d rules and the rail in %d — "+
			"both columns stand aside together or neither does", navRules, railRules)
	}

	// The centre column's full-bleed treatment was keyed off the page's mode,
	// which is fixed for the life of the document. It is keyed off the Facet now.
	if strings.Contains(css, ".live-main--broadcast") {
		t.Error("styles.css still styles .live-main--broadcast, a page-mode class; " +
			"the column's shape follows the stage Facet that is actually in it")
	}
}

// TestBroadcastStageTakesItsShapeFromTheServer — the broadcaster's own preview
// is shaped the way the audience's stage is, from the source geometry the media
// server reported. A fixed 100dvh box in a centre column stands every source in
// bands of its own black.
func TestBroadcastStageTakesItsShapeFromTheServer(t *testing.T) {
	h := liveTestHandler(t)
	css := shellCSS(t)

	desktop := mediaBlock(t, css, "@media (min-width:1220px) {")
	if !strings.Contains(desktop, "aspect-ratio:var(--live-ar,16 / 9)") {
		t.Error("the desktop broadcast stage is still fixed-ratio; it must take " +
			"--live-ar with a 16/9 fallback, the way live_player does")
	}
	if !strings.Contains(desktop, ".live-bcast:not(.live-bcast--encoder)") {
		t.Error("the encoder stage is a document that scrolls, not a picture — it " +
			"must be excluded from the aspect-ratio box")
	}

	// The ratio itself is rendered by the server onto the Facet's root.
	s := liveTestStream()
	s.SourceWidth, s.SourceHeight = 1080, 1920
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, "live_broadcast_stage", map[string]interface{}{
		"S": s, "AuthorHandle": s.AuthorHandle, "Facing": "user", "Source": "browser",
		"Viewers": "0", "ViewerCount": 0, "ShareURL": "/live/" + s.ID,
		"Encoder": encoderSetupData(s.ID, nil),
	}); err != nil {
		t.Fatalf("live_broadcast_stage: %v", err)
	}
	if !strings.Contains(buf.String(), "--live-ar:1080 / 1920") {
		t.Error("the stage does not carry the source geometry the server holds; " +
			"the browser would have to measure the video to know its shape")
	}

	// A stream with no keyframe yet reports nothing, and the stylesheet falls
	// back. Rendering --live-ar:0 / 0 would collapse the stage.
	s2 := liveTestStream()
	s2.SourceWidth, s2.SourceHeight = 0, 0
	var buf2 bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf2, "live_broadcast_stage", map[string]interface{}{
		"S": s2, "AuthorHandle": s2.AuthorHandle, "Facing": "user", "Source": "browser",
		"Viewers": "0", "ViewerCount": 0, "ShareURL": "/live/" + s2.ID,
		"Encoder": encoderSetupData(s2.ID, nil),
	}); err != nil {
		t.Fatalf("live_broadcast_stage (no geometry): %v", err)
	}
	if strings.Contains(buf2.String(), "--live-ar") {
		t.Error("the stage renders --live-ar for a stream with no reported " +
			"geometry; the stylesheet's 16/9 fallback is what answers that")
	}
}
