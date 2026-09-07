package handler

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/f33d3r/feed-engine/internal/config"
)

// followTestHandler builds a Handler against the real template tree. cwd-dependent,
// like the atlas integration test.
func followTestHandler(t *testing.T) *Handler {
	t.Helper()
	cwd, _ := os.Getwd()
	if err := os.Chdir("../.."); err != nil {
		t.Skipf("cannot chdir to repo root: %v", err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })
	if _, err := os.Stat("web/templates/partials"); err != nil {
		t.Skipf("no web/templates: %v", err)
	}
	h := &Handler{cfg: &config.Config{}}
	h.loadTemplates()
	return h
}

// TestFollowButtonSingleEntryPoint pins the contract of the one follow control:
// every surface renders one addressable, correctly-classed, correctly-labelled
// btn_follow, and the fragment the stream carries is the same fragment a click
// swaps in.
func TestFollowButtonSingleEntryPoint(t *testing.T) {
	h := followTestHandler(t)

	const pial = "00000000-0000-0000-0000-0000000000bb"
	cases := []struct {
		surface   string
		following bool
		wantClass string
		wantText  string
	}{
		{"profile", false, `class="btn-follow"`, "Follow"},
		{"profile", true, `class="btn-follow is-on"`, "Following"},
		{"compact", false, `class="btn-follow btn-follow--compact"`, "Follow"},
		{"compact", true, `class="btn-follow btn-follow--compact is-on"`, "Following"},
		{"card", false, `class="btn-follow btn-follow--card"`, "Follow"},
		{"dot", false, `class="btn-follow btn-follow--dot"`, "+"},
		{"dot", true, `class="btn-follow btn-follow--dot is-on"`, "✓"},
		// An unreported or unknown surface must still produce a real button.
		{"", false, `class="btn-follow"`, "Follow"},
		{"nonsense", false, `class="btn-follow"`, "Follow"},
	}
	for _, c := range cases {
		frag, err := h.renderFollowButton(pial, "mia", c.following, c.surface)
		if err != nil {
			t.Fatalf("surface %q following=%v: %v", c.surface, c.following, err)
		}
		got := string(frag)
		if !strings.Contains(got, c.wantClass) {
			t.Errorf("surface %q following=%v: missing %s in:\n%s", c.surface, c.following, c.wantClass, got)
		}
		if !strings.Contains(got, ">"+c.wantText+"<") {
			t.Errorf("surface %q following=%v: missing label %q in:\n%s", c.surface, c.following, c.wantText, got)
		}
		// Addressable by the stream, and keyed on the person, not the page.
		if !strings.Contains(got, `data-facet-id="facet:f33d3r:identity:`+pial+`:btn_follow"`) {
			t.Errorf("surface %q: missing/incorrect facet id in:\n%s", c.surface, got)
		}
		if !strings.Contains(got, `data-pial="`+pial+`"`) {
			t.Errorf("surface %q: missing data-pial in:\n%s", c.surface, got)
		}
		// The surface must be stamped on the element: the fan-out sends one
		// fragment per surface and the runtime must not cross them over.
		wantSurface := c.surface
		if c.wantClass == `class="btn-follow"` || c.wantClass == `class="btn-follow is-on"` {
			wantSurface = "profile"
		}
		if !strings.Contains(got, `data-surface="`+wantSurface+`"`) {
			t.Errorf("surface %q: expected data-surface=%q in:\n%s", c.surface, wantSurface, got)
		}
		// The next click must be the opposite mutation.
		wantEvent := "follow"
		if c.following {
			wantEvent = "unfollow"
		}
		if !strings.Contains(got, `event_type&#34;:&#34;`+wantEvent) && !strings.Contains(got, `"event_type":"`+wantEvent) {
			t.Errorf("surface %q following=%v: hx-vals does not emit %s:\n%s", c.surface, c.following, wantEvent, got)
		}
	}
}

// TestFollowButtonHandleAddressing covers the panels that know a handle and no
// PIAL. They used to POST target_handle at a handler that read only target_pial,
// so every follow from them failed.
func TestFollowButtonHandleAddressing(t *testing.T) {
	h := followTestHandler(t)
	frag, err := h.renderFollowButton("", "mia", false, "compact")
	if err != nil {
		t.Fatalf("handle-addressed render: %v", err)
	}
	got := string(frag)
	if !strings.Contains(got, `data-facet-id="facet:f33d3r:identity:mia:btn_follow"`) {
		t.Errorf("handle fallback missing from facet id:\n%s", got)
	}
	if !strings.Contains(got, "target_handle&#34;:&#34;mia") && !strings.Contains(got, `"target_handle":"mia`) {
		t.Errorf("handle-addressed button does not carry target_handle:\n%s", got)
	}
}

// TestFollowButtonRejectsNoTarget — an unaddressable control must be an error,
// never a rendered button that posts nowhere.
func TestFollowButtonRejectsNoTarget(t *testing.T) {
	h := followTestHandler(t)
	if _, err := h.renderFollowButton("", "", false, "profile"); err == nil {
		t.Fatal("expected an error for a follow button with no target")
	}
}

// TestTopicFollowButton pins the topic control. Its handler used to answer 204
// No Content into an outerHTML swap, which deleted the button on click.
func TestTopicFollowButton(t *testing.T) {
	h := followTestHandler(t)
	for _, following := range []bool{false, true} {
		frag, err := h.renderTopicFollowButton("aethyr", following)
		if err != nil {
			t.Fatalf("following=%v: %v", following, err)
		}
		got := string(frag)
		if !strings.Contains(got, `data-facet-id="facet:f33d3r:topic:aethyr:btn_follow"`) {
			t.Errorf("following=%v: bad facet id:\n%s", following, got)
		}
		want := "follow_topic"
		if following {
			want = "unfollow_topic"
		}
		if !strings.Contains(got, want) {
			t.Errorf("following=%v: hx-vals does not emit %s:\n%s", following, want, got)
		}
	}
}

// TestNoHandWrittenFollowControls is the regression guard for the whole task: a
// follow control may exist in exactly one place, _btn_follow.html (and its topic
// twin). Any template that reintroduces one of the retired private classes, or
// that posts a follow event without going through the Facet, puts the site back
// in the state where the button a page draws and the button a click swaps in are
// different buttons.
func TestNoHandWrittenFollowControls(t *testing.T) {
	followTestHandler(t) // chdir to repo root

	retiredClasses := []string{
		"post-follow-btn",
		"suggested-follow-btn",
		"profile-follow-btn",
		"pdf-follow-btn",
		"focus-follow-dot",
		"sidebar-follow-btn",
		"user-list-sheet-follow",
		"user-list-sheet-unfollow",
		"btn-following",
	}
	// Only the Facet definitions may name the follow mutations directly.
	allowedEmitters := map[string]bool{
		"_btn_follow.html":       true,
		"_btn_follow_topic.html": true,
	}

	roots := []string{"web/templates", "web/static/css", "web/static/js"}
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			// Scan code, not prose. The comments that record WHY these classes
			// were retired are worth keeping, and a guard that cannot tell a
			// selector from an explanation of a selector forces them to be
			// deleted. Strip comments first, then the check means what it says:
			// no retired class is USED anywhere.
			src := stripComments(path, string(body))
			for _, cls := range retiredClasses {
				if strings.Contains(src, cls) {
					t.Errorf("%s still references retired follow class %q — the follow control has one definition, _btn_follow.html", path, cls)
				}
			}
			if strings.HasSuffix(path, ".html") && !allowedEmitters[filepath.Base(path)] {
				for _, ev := range []string{`"event_type":"follow"`, `"event_type":"unfollow"`} {
					if strings.Contains(src, ev) {
						t.Errorf("%s hand-writes a follow control (%s) — call {{template \"btn_follow\" …}} instead", path, ev)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}

// stripComments removes comment bodies so a scan tests usage rather than
// mention. Covers the three comment syntaxes in web/: {{/* … */}} and /* … */
// share the block form, <!-- … --> is HTML, and // … is JavaScript.
func stripComments(path, src string) string {
	src = blockComment.ReplaceAllString(src, " ")
	src = htmlComment.ReplaceAllString(src, " ")
	if strings.HasSuffix(path, ".js") {
		src = lineComment.ReplaceAllString(src, " ")
	}
	return src
}

var (
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	htmlComment  = regexp.MustCompile(`(?s)<!--.*?-->`)
	lineComment  = regexp.MustCompile(`(?m)//.*$`)
)
