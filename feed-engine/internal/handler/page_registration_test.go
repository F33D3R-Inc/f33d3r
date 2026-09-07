package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The page registration guard.
//
// h.render looks a page up in h.pages, which is built from pageFiles. A page
// that is rendered but not in that list does not fail at boot, does not fail at
// build, and does not fail review — it fails in production, as a 500 reading
// "unknown page: X.html", on a route that is finished in every other respect.
//
// That is not a hypothetical. It shipped three times: stocks.html on
// /stocks/{ticker}, works.html on /works, and deactivated.html on /deactivated —
// the last two added straight past the comment left in pageFiles explaining the
// first. Three occurrences of one mistake, one of them under its own warning, is
// the signature of a structural trap rather than of careless people.
//
// So these two tests hold the list to the code instead of holding people to the
// list. They read the same pageFiles variable the server boots from and the same
// source the handlers are written in, so neither can drift from the other
// without a named, loud failure.

// renderedPage is one h.render call found in the package source.
type renderedPage struct {
	name string
	pos  string
}

// handlerPackageRenders walks every non-test source file in this package and
// returns the page named by each h.render call.
//
// A render whose page argument is not a plain string literal fails the test on
// the spot. This guard works by reading names out of the source, so a name
// assembled at runtime is a name it cannot check — and an unverifiable render is
// exactly the hole the guard exists to close.
func handlerPackageRenders(t *testing.T) []renderedPage {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		name := fi.Name()
		// The same names the go tool itself ignores. Without this, an editor or
		// filesystem leftover such as an AppleDouble "._post.go" is handed to the
		// parser as Go source and the guard dies on it instead of doing its job.
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return false
		}
		return !strings.HasSuffix(name, "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the handler package: %v", err)
	}

	var found []renderedPage
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "render" {
					return true
				}
				// h.render(w, r, page, data)
				if len(call.Args) < 3 {
					return true
				}
				lit, ok := call.Args[2].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Errorf("%s: render() called with a page name that is not a string literal.\n"+
						"The page registration guard reads page names out of the source; a name built at "+
						"runtime cannot be checked against pageFiles, which is how an unregistered page "+
						"reaches production as a 500. Pass a literal.",
						fset.Position(call.Pos()))
					return true
				}
				name, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					t.Errorf("%s: unparsable page name %s", fset.Position(call.Pos()), lit.Value)
					return true
				}
				found = append(found, renderedPage{name: name, pos: fset.Position(call.Pos()).String()})
				return true
			})
		}
	}

	if len(found) == 0 {
		t.Fatal("found no h.render calls in the handler package — the guard is not looking at anything, " +
			"which means it would pass no matter what was broken")
	}
	return found
}

// registeredPages is pageFiles as a set.
func registeredPages() map[string]bool {
	set := make(map[string]bool, len(pageFiles))
	for _, p := range pageFiles {
		set[p] = true
	}
	return set
}

// TestEveryRenderedPageIsRegistered is the forward direction: every page a
// handler renders must be in pageFiles, or that route answers 500.
func TestEveryRenderedPageIsRegistered(t *testing.T) {
	registered := registeredPages()

	missing := map[string][]string{}
	for _, r := range handlerPackageRenders(t) {
		if !registered[r.name] {
			missing[r.name] = append(missing[r.name], r.pos)
		}
	}
	if len(missing) == 0 {
		return
	}

	names := make([]string, 0, len(missing))
	for n := range missing {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("these pages are rendered by a handler but are NOT in pageFiles, so every request " +
		"that reaches them answers 500 \"unknown page\":\n")
	for _, n := range names {
		b.WriteString("\n  " + n + "\n")
		for _, pos := range missing[n] {
			b.WriteString("      rendered at " + pos + "\n")
		}
	}
	b.WriteString("\nfix: add each name to the pageFiles list in handlers.go " +
		"(or, if it is a fragment rather than a full page, serve it with h.renderPartial).")
	t.Fatal(b.String())
}

// TestEveryRegisteredPageIsRendered is the reverse direction: an entry nothing
// renders is dead weight, and dead weight in this list is what makes the list
// hard to trust when it matters.
func TestEveryRegisteredPageIsRendered(t *testing.T) {
	rendered := map[string]bool{}
	for _, r := range handlerPackageRenders(t) {
		rendered[r.name] = true
	}

	var dead []string
	for _, p := range pageFiles {
		if !rendered[p] {
			dead = append(dead, p)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("these pages are registered in pageFiles but no handler renders them:\n  %s\n\n"+
			"fix: either wire the handler that was meant to render it, or drop the entry. "+
			"A registration nothing uses is parsed on every boot and tells the next reader "+
			"a route exists that does not.", strings.Join(dead, "\n  "))
	}
}

// TestEveryRegisteredPageExistsOnDisk closes the third way this list goes wrong:
// a name that is in pageFiles and rendered but spelled differently from the file.
// ParseFiles fails at boot on a missing file, so this turns a production
// log.Fatalf into a test failure.
func TestEveryRegisteredPageExistsOnDisk(t *testing.T) {
	var missing []string
	for _, p := range pageFiles {
		// Tests run in the package directory; templates live at the module root.
		if _, err := os.Stat(filepath.Join("..", "..", "web", "templates", filepath.FromSlash(p))); err != nil {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("these pages are registered in pageFiles but have no template file — "+
			"the server calls log.Fatalf on this at boot:\n  %s", strings.Join(missing, "\n  "))
	}
}
