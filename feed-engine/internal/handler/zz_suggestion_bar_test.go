package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// zz_suggestion_bar_test.go — the convergence guard for people suggestions.
//
// The rail's "Who to follow" once recommended anything that could hold a row in
// the users table: automation probes with no avatar, no works and no followers
// rendered above real people. The fix was not to remove those handles, it was to
// give the platform one definition of who may be suggested (db.SuggestPeople)
// and to route every unprompted people surface through it.
//
// Convergence is only worth anything while it holds. This test is what makes it
// hold: it reads the handler sources and refuses a surface that fills a
// suggestion facet from anywhere else. The next "people you may know" panel
// therefore cannot ship without the quality bar, which is the actual fix.

// suggestionFacetKeys are the template keys that put a stranger's face in front
// of somebody who did not ask for one.
var suggestionFacetKeys = []string{"SuggestedUsers", "RailCreators"}

// TestEverySuggestionSurfaceGoesThroughTheOneOwner fails if a handler file feeds
// a suggestion facet without calling the one suggestion owner.
func TestEverySuggestionSurfaceGoesThroughTheOneOwner(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing handler sources: %v", err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		body := string(raw)

		feeds := ""
		for _, key := range suggestionFacetKeys {
			// The assignment forms a surface can use: a map literal entry and an
			// index write. A bare mention in a comment is not a surface.
			if strings.Contains(body, `"`+key+`":`) || strings.Contains(body, `["`+key+`"] =`) {
				feeds = key
				break
			}
		}
		if feeds == "" {
			continue
		}
		checked++
		if !strings.Contains(body, "dbpkg.SuggestPeople(") {
			t.Errorf("%s fills the %q facet without calling dbpkg.SuggestPeople.\n"+
				"Every surface that suggests people goes through the one quality bar "+
				"— alive, presentable, has published, permitted for this viewer — so a "+
				"new panel cannot ship recommending empty accounts. Narrow the one set "+
				"with dbpkg.SuggestionQuery instead of fetching people another way.",
				f, feeds)
		}
	}
	if checked == 0 {
		t.Fatal("no handler file feeds a suggestion facet — this guard is watching " +
			"nothing. If the facet keys were renamed, rename them in suggestionFacetKeys too.")
	}
}

// TestTheRailAlwaysResolvesItsSuggestionSet pins the honest empty state.
//
// sidebar_right renders the "Who to follow" panel — including its "no one to
// suggest yet" line — only when the surface actually resolved a suggestion set.
// railData is what says so, and it must say so unconditionally: a viewer-gated
// resolution is how the panel came to be silently absent for logged-out
// visitors, which reads as "the platform has nobody" rather than "we did not
// ask".
func TestTheRailAlwaysResolvesItsSuggestionSet(t *testing.T) {
	raw, err := os.ReadFile("helpers.go")
	if err != nil {
		t.Fatalf("reading helpers.go: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `out["SuggestedUsersResolved"] = true`) {
		t.Fatal("railData no longer marks its suggestion set as resolved; the rail " +
			"panel's empty state cannot tell 'nobody clears the bar' from 'this " +
			"surface never asked'")
	}
	// The marker and the set are written together, unconditionally, at the same
	// nesting depth — one tab inside railData's body.
	if !strings.Contains(body, "\n\tout[\"SuggestedUsers\"] = suggested\n") {
		t.Error("railData no longer resolves its suggestion set unconditionally; a " +
			"logged-out visitor must be served suggestions, not skipped")
	}
}

// TestTheRailPanelRendersItsOwnEmptyState pins the facet half: the panel must
// have somewhere honest to land when nobody clears the bar, rather than
// disappearing or being padded with whatever rows exist.
func TestTheRailPanelRendersItsOwnEmptyState(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "templates", "partials", "_sidebar_right.html"))
	if err != nil {
		t.Skipf("sidebar_right facet not readable from here: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "{{if .SuggestedUsersResolved}}") {
		t.Error("sidebar_right no longer gates the who-to-follow panel on a resolved " +
			"suggestion set")
	}
	if !strings.Contains(body, "sidebar-panel-empty") {
		t.Error("sidebar_right no longer renders an empty state for the who-to-follow " +
			"panel; an empty suggestion set must be said out loud, not hidden")
	}
}
