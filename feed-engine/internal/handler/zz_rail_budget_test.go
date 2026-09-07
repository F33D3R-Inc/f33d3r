package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/rss"
)

// zz_rail_budget_test.go — the convergence guard for the right rail's length.
//
// The rail once ran to twenty-two rows and nobody had chosen twenty-two. Five
// panels in three files each picked a limit at the call site, every one of them
// defensible on its own, and no code anywhere added them up. The fix was to give
// the rail one owner — railManifest in rail_budget.go — and the fix is only
// worth anything while every panel keeps reading from it.
//
// These tests are what make it keep. The first one is the important one: it
// reads the handler sources and FAILS a panel that sizes itself with a number
// written at the call site, so the next panel cannot sidestep the budget the way
// the last five did. The rest pin what the budget promises — that the caps add
// up, that a capped panel says where the rest is, and that a cap never turns
// "nothing to show" into an empty box.

// railSizedFuncs are the functions that decide how long a rail panel is. Every
// one of them must take its length from railCap.
var railSizedFuncs = map[string]string{
	"helpers.go": "railData",
	"sports.go":  "sportsRailData",
}

// TestEveryRailPanelTakesItsCapFromTheBudget is the guard. A rail panel that
// asks a query for a literal number of rows fails here, whatever the number is.
//
// A bare integer in one of these functions is the exact shape of the original
// defect: GetTrendingTags(h.db, 6), Limit: 3, rssCache.Get("news", 4). Each read
// fine in isolation, which is why five of them shipped. The rail's height is not
// a call-site decision.
func TestEveryRailPanelTakesItsCapFromTheBudget(t *testing.T) {
	for file, fn := range railSizedFuncs {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		body := railFuncBody(t, parsed, fn, file)

		sawRailCap := false
		complain := func(pos token.Pos, lit *ast.BasicLit) {
			t.Errorf("%s: %s sizes a rail panel with the literal %s at %s.\n"+
				"The rail is one column with one height budget: every panel's cap is "+
				"declared in railManifest (rail_budget.go) and read with "+
				"railCap(railPanel…). A number chosen here is invisible to the rail's "+
				"total, which is how the rail came to be twenty-two rows long without "+
				"anyone deciding it should be. Declare this panel's share and read it "+
				"from the budget.",
				file, fn, lit.Value, fset.Position(pos))
		}
		ast.Inspect(body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				// How a panel asks for rows: it hands a length to whatever
				// fetches them. GetTrendingTags(h.db, 6), rssCache.Get("news", 4).
				if ident, ok := node.Fun.(*ast.Ident); ok && ident.Name == "railCap" {
					sawRailCap = true
				}
				for _, arg := range node.Args {
					if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.INT {
						complain(lit.Pos(), lit)
					}
				}
			case *ast.KeyValueExpr:
				// The other way it asks: a query struct's own length field.
				// dbpkg.SuggestionQuery{Limit: 3}.
				key, ok := node.Key.(*ast.Ident)
				if !ok || !railLimitField(key.Name) {
					return true
				}
				if lit, ok := node.Value.(*ast.BasicLit); ok && lit.Kind == token.INT {
					complain(lit.Pos(), lit)
				}
			}
			return true
		})
		if !sawRailCap && fn == "railData" {
			t.Errorf("%s: %s no longer reads a single cap from the rail budget; the rail "+
				"has stopped being owned", file, fn)
		}
	}
}

// TestNoRailLimitIsDeclaredOutsideTheBudget catches the other way around a
// shared budget: hoisting the literal out of the function into a package
// constant of its own. That is precisely what sports.go did with
// sportsRailLimit = 6 — a well-argued number, invisible to every other panel.
func TestNoRailLimitIsDeclaredOutsideTheBudget(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing handler sources: %v", err)
	}
	for _, file := range files {
		// The same rule the Go toolchain itself applies: a name beginning with
		// "." or "_" is not part of the package (editor and archive metadata
		// lands beside sources and is not Go).
		if strings.HasSuffix(file, "_test.go") || file == "rail_budget.go" ||
			strings.HasPrefix(file, ".") || strings.HasPrefix(file, "_") {
			continue
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !railLimitName(name.Name) || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.INT {
						continue
					}
					t.Errorf("%s: %s = %s is a rail limit declared outside the rail's "+
						"budget, at %s.\nA panel's share of the rail belongs in railManifest "+
						"(rail_budget.go) beside every other panel's, where the total can be "+
						"checked. Move the number and the argument for it there, and read it "+
						"back with railCap(railPanel…).",
						file, name.Name, lit.Value, fset.Position(name.Pos()))
				}
			}
		}
	}
}

// railLimitField reports whether a struct field asks a query for a length.
func railLimitField(name string) bool {
	switch strings.ToLower(name) {
	case "limit", "cap", "count", "max", "size", "n", "rows", "perpage", "pagesize":
		return true
	}
	return false
}

// railLimitName reports whether an identifier names a rail panel's length.
func railLimitName(name string) bool {
	lower := strings.ToLower(name)
	if !strings.Contains(lower, "rail") {
		return false
	}
	for _, sized := range []string{"limit", "cap", "count", "max", "size", "budget", "rows"} {
		if strings.Contains(lower, sized) {
			return true
		}
	}
	return false
}

// railFuncBody returns the named function's body, or fails the test. A renamed
// or deleted rail function must not silently retire this guard.
func railFuncBody(t *testing.T, file *ast.File, name, filename string) *ast.BlockStmt {
	t.Helper()
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name && fn.Body != nil {
			return fn.Body
		}
	}
	t.Fatalf("%s no longer defines %s — this guard is watching nothing. If the rail's "+
		"data path moved, point railSizedFuncs at where it went.", filename, name)
	return nil
}

// TestTheRailBudgetAddsUp runs the startup invariants as a test, so a manifest
// that stops adding up is a red build rather than a failed boot.
func TestTheRailBudgetAddsUp(t *testing.T) {
	if err := railBudgetInvariants(); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, p := range railManifest {
		total += p.Cap
	}
	if total != railItemBudget {
		t.Fatalf("the rail's panels total %d rows against a budget of %d", total, railItemBudget)
	}
}

// railBudgetExceptions are the panels allowed to exceed the rail's default
// share, and the reason each is allowed to.
//
// The map is the deliberate part. A new panel that wants more than three rows
// has to be added here, in a test, by someone who has read what the exception
// costs — it cannot arrive as a larger number in the manifest.
var railBudgetExceptions = map[railPanelID]string{
	railPanelScores: "ranked across every league at once with no seat reserved for " +
		"any of them, so a two-league night at the default cap hides an entire sport " +
		"by arithmetic; and its rows are the most compact the rail prints, two lines " +
		"on one right edge, with a working escape hatch in the head.",
}

// TestOnlyAnArguedPanelExceedsTheDefaultShare keeps the default the default.
func TestOnlyAnArguedPanelExceedsTheDefaultShare(t *testing.T) {
	for _, p := range railManifest {
		if p.Cap <= railPanelDefaultCap {
			continue
		}
		why, ok := railBudgetExceptions[p.ID]
		if !ok {
			t.Errorf("rail panel %q takes %d rows against a default share of %d without "+
				"being an argued exception.\nThe default is three because three is what a "+
				"panel is read at before the next head takes over. A panel that needs more "+
				"has to say what it buys with the extra height and what a reader does when "+
				"even that is not enough — add it to railBudgetExceptions with that argument.",
				p.ID, p.Cap, railPanelDefaultCap)
			continue
		}
		if len(why) < 40 {
			t.Errorf("rail panel %q's exception is asserted, not argued", p.ID)
		}
	}
	// And the exceptions list cannot outlive the panels it excuses.
	for id := range railBudgetExceptions {
		found := false
		for _, p := range railManifest {
			if p.ID == id && p.Cap > railPanelDefaultCap {
				found = true
			}
		}
		if !found {
			t.Errorf("railBudgetExceptions still excuses %q, which no longer exceeds the "+
				"default share", id)
		}
	}
}

// TestEveryRailEscapeHatchIsARouteThisServiceServes is the guard against the
// worst version of a cap: a "see more" that 404s. Every destination the budget
// names has to be a path the router actually registers.
func TestEveryRailEscapeHatchIsARouteThisServiceServes(t *testing.T) {
	routes, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatalf("reading the route table: %v", err)
	}
	table := string(routes)
	checked := 0
	for _, p := range railManifest {
		if p.SeeAll == "" {
			// Declared as having no destination. rail_budget.go's invariants
			// already refuse that without an explanation.
			continue
		}
		path := p.SeeAll
		if q := strings.IndexByte(path, '?'); q >= 0 {
			path = path[:q]
		}
		if !strings.Contains(table, `"`+path+`"`) && !strings.Contains(table, `"GET `+path+`"`) {
			t.Errorf("rail panel %q sends readers to %q, which this service does not "+
				"serve.\nA capped panel whose escape hatch 404s is worse than an uncapped "+
				"panel: it hides rows AND lies about where they went. Point it at a route "+
				"that exists, or declare the panel as having none and say why.",
				p.ID, p.SeeAll)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no rail panel declares an escape hatch — every panel in the rail now " +
			"hides rows with no way to reach them")
	}
}

// ── What the budget looks like once rendered ─────────────────────────────────

// railFullDataset builds a rail dataset holding exactly what the budget grants
// every panel, so the rendered rail can be counted against the budget.
func railFullDataset() map[string]interface{} {
	tags := make([]dbpkg.TrendingTag, 0, railCap(railPanelTrending))
	for i := 0; i < railCap(railPanelTrending); i++ {
		tags = append(tags, dbpkg.TrendingTag{Tag: "tag" + itoaRail(i), Count: i + 1})
	}
	people := func(n int, prefix string) []dbpkg.SuggestedUser {
		out := make([]dbpkg.SuggestedUser, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, dbpkg.SuggestedUser{
				ID:          prefix + itoaRail(i),
				Handle:      prefix + itoaRail(i),
				DisplayName: prefix + itoaRail(i),
				PIALID:      "pial-" + prefix + itoaRail(i),
			})
		}
		return out
	}
	news := make([]rss.Item, 0, railCap(railPanelNews))
	for i := 0; i < railCap(railPanelNews); i++ {
		news = append(news, rss.Item{Title: "head" + itoaRail(i), URL: "https://example.invalid/" + itoaRail(i), Source: "Wire"})
	}
	return map[string]interface{}{
		"RailContext":            "feed",
		"RailTrendingTags":       tags,
		"RailTrendingSeeAll":     railSeeAll(railPanelTrending),
		"RailCreators":           people(railCap(railPanelCreators), "creator"),
		"SuggestedUsers":         people(railCap(railPanelWhoToFollow), "person"),
		"SuggestedUsersResolved": true,
		"RailPeopleSeeAll":       railSeeAll(railPanelWhoToFollow),
		"RailNewsItems":          news,
		"RailNewsLabel":          "What's happening",
	}
}

// TestTheRenderedRailSpendsExactlyItsBudget counts the rows the rail actually
// prints when every panel is full. The scoreboard is counted by the sports lane's
// own tests; everything else is counted here.
func TestTheRenderedRailSpendsExactlyItsBudget(t *testing.T) {
	h := railTestHandler(t)
	out := renderRail(t, h, railFullDataset())

	counts := map[railPanelID]int{
		railPanelTrending:    strings.Count(out, `class="trending-item"`),
		railPanelCreators:    strings.Count(out, `class="rail-creator-row"`),
		railPanelWhoToFollow: strings.Count(out, `class="suggested-user-row"`),
		railPanelNews:        strings.Count(out, `class="rail-news-item"`),
	}
	spent := 0
	for id, got := range counts {
		if want := railCap(id); got != want {
			t.Errorf("rail panel %q rendered %d rows, want its budgeted %d", id, got, want)
		}
		spent += got
	}
	// Every panel but the scoreboard, which this dataset does not mount.
	if want := railItemBudget - railCap(railPanelScores); spent != want {
		t.Errorf("the rail spent %d rows, want %d — the budget and the page disagree", spent, want)
	}
}

// TestACapNeverTurnsNothingToShowIntoAnEmptyBox is the other half of a cap, and
// the half that is easy to break. A panel with no items must render NO panel:
// no head, no border, no box reporting its own emptiness. The rail self-shortens
// on a quiet surface, and it has to keep doing so.
func TestACapNeverTurnsNothingToShowIntoAnEmptyBox(t *testing.T) {
	h := railTestHandler(t)
	out := renderRail(t, h, map[string]interface{}{"RailContext": "feed"})

	for _, absent := range []struct{ what, marker string }{
		{"trending", "Trending</h3>"},
		{"creators to follow", "rail-creators-panel"},
		{"news", "rail-news-panel"},
		{"scoreboard", "rail-scores-panel"},
		{"who to follow", "Who to follow"},
	} {
		if strings.Contains(out, absent.marker) {
			t.Errorf("the rail grew an empty %s panel with nothing to put in it\n%s", absent.what, out)
		}
	}
	if strings.Contains(out, "rail-panel-see-all") {
		t.Error("the rail offered a way to see more of nothing")
	}
}

// TestTheSuggestionPanelStillSaysSoOutLoud pins the one panel whose empty state
// is deliberate. "Nobody clears the bar" is a fact worth printing; it is not the
// same as "this surface never asked", and it must not be padded into rows.
func TestTheSuggestionPanelStillSaysSoOutLoud(t *testing.T) {
	h := railTestHandler(t)
	out := renderRail(t, h, map[string]interface{}{
		"RailContext":            "feed",
		"SuggestedUsersResolved": true,
		"RailPeopleSeeAll":       railSeeAll(railPanelWhoToFollow),
	})
	if !strings.Contains(out, "Who to follow") {
		t.Fatal("the resolved suggestion panel vanished instead of stating its empty state")
	}
	if !strings.Contains(out, "sidebar-panel-empty") {
		t.Error("the suggestion panel lost its honest empty line")
	}
	if strings.Contains(out, "suggested-user-row") {
		t.Error("the empty suggestion panel was padded with rows")
	}
	if strings.Contains(out, "rail-panel-see-all") {
		t.Error("the empty suggestion panel offers 'show more' of nothing")
	}
}

// TestCappedPanelsKeepTheirEscapeHatch — a panel that hides rows has to say
// where they went, on the page, not only in the manifest.
func TestCappedPanelsKeepTheirEscapeHatch(t *testing.T) {
	h := railTestHandler(t)
	out := renderRail(t, h, railFullDataset())
	for _, id := range []railPanelID{railPanelTrending, railPanelWhoToFollow} {
		href := railSeeAll(id)
		if href == "" {
			t.Fatalf("rail panel %q lost the escape hatch it declared", id)
		}
		if !strings.Contains(out, `href="`+href+`"`) {
			t.Errorf("rail panel %q caps its list but renders no way to reach %s\n%s", id, href, out)
		}
	}
}

// TestTheRailNeverSizesTheExploreGrid pins the split that made cutting the rail's
// trending panel safe. The rail and the Explore surface once shared the
// TrendingTags key and therefore shared a length; shortening the rail would have
// silently halved a full-width grid on another surface.
func TestTheRailNeverSizesTheExploreGrid(t *testing.T) {
	rail, err := os.ReadFile("helpers.go")
	if err != nil {
		t.Fatalf("reading helpers.go: %v", err)
	}
	if strings.Contains(string(rail), `out["TrendingTags"]`) {
		t.Error("railData writes TrendingTags again — that key belongs to the Explore " +
			"surface, and sharing it makes the rail's cap resize a Playground grid")
	}
	explore, err := os.ReadFile(filepath.Join("..", "..", "web", "templates", "explore.html"))
	if err != nil {
		t.Fatalf("reading explore.html: %v", err)
	}
	if !strings.Contains(string(explore), ".TrendingTags") {
		t.Skip("the Explore surface no longer renders a trending grid")
	}
	feed, err := os.ReadFile("feed.go")
	if err != nil {
		t.Fatalf("reading feed.go: %v", err)
	}
	if !strings.Contains(string(feed), "exploreTrendingLimit") {
		t.Error("the Explore surface renders a trending grid but no longer resolves its " +
			"own list; it is reading the rail's again")
	}
}
