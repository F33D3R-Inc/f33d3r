package handler

// atlas.go — Facet Atlas: a read-only, auto-discovering live catalog of every
// Facet in the registry. Dev tool only (DEV_MODE or admin). Route: GET /_atlas.
//
// The atlas reads the facet store; it never modifies, moves, or rewrites any
// facet. It renders facets through the SAME template set the app uses
// (h.partial) — never a parallel renderer.
//
// Discovery is automatic: every partial opens with a structured
// `{{/* FACET ... */}}` header (name, version, status, layer, inputs, states,
// signals, context). Adding a partial with such a header makes it appear here
// with zero atlas-code edits. The composition graph (which facet renders which)
// is read directly from the parsed `{{template "child"}}` calls, so it is always
// current.

import (
	"bytes"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template/parse"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// ── Parsed manifest ───────────────────────────────────────────────────────────

// ContractField is one declared input from a FACET header `inputs:` block.
type ContractField struct {
	Name string
	Type string
	Desc string
}

// FacetMeta is the parsed FACET header for a single facet.
type FacetMeta struct {
	Name      string
	File      string
	Version   string
	Status    string
	LayerRaw  string
	Inputs    []ContractField
	States    string
	Signals   []string
	Context   []string
	HasHeader bool
}

// ── View model handed to atlas.html ───────────────────────────────────────────

type atlasVariantView struct {
	Label string
	HTML  template.HTML
	Err   string
}

type atlasCard struct {
	Name      string
	File      string
	Version   string
	Status    string
	TierLabel string
	LayerRaw  string
	Inputs    []ContractField
	States    string
	Signals   []string
	Context   []string
	Slots     []string // children, from the live {{template}} graph
	UsedIn    []string // reverse links
	Variants  []atlasVariantView
	Rendered  bool // true when at least one variant rendered live
	// Canonical citation: facet:f33d3r:<tier>:<name>
	FacetID string
	// Admin review state (persisted in facet_reviews).
	Review     string // approved | needs_work | unreviewed
	ReviewNote string
}

type atlasTier struct {
	Label    string
	Rank     int
	Count    int
	Approved int
	Cards    []atlasCard
}

type atlasView struct {
	Tiers      []atlasTier
	TierLabels []string
	Total      int
	Approved   int  // facets marked approved
	NeedsWork  int  // facets marked needs_work
	IsAdmin    bool // viewer can approve
}

// ── Header parsing ────────────────────────────────────────────────────────────

var (
	reFacetName  = regexp.MustCompile(`(?m)^\s*FACET\s+(\S+)`)
	reMetaLine   = regexp.MustCompile(`version:\s*([^|]*)\|\s*status:\s*([^|]*)\|\s*layer:\s*(.+)`)
	reDefine     = regexp.MustCompile(`{{-?\s*define\s+"([^"]+)"`)
	reHeaderBlk  = regexp.MustCompile(`(?s)\{\{/\*(.*?)\*/\}\}`)
	reInputField = regexp.MustCompile(`^(\S+)\s+(\S+)\s*(?:[—-]\s*)?(.*)$`)
)

// parseFacetHeader extracts a FacetMeta from the leading {{/* ... */}} comment.
func parseFacetHeader(file, src string) FacetMeta {
	m := FacetMeta{File: filepath.Base(file)}

	loc := reHeaderBlk.FindStringSubmatch(src)
	if loc == nil {
		return m
	}
	block := loc[1]
	if !strings.Contains(block, "FACET") {
		return m
	}
	m.HasHeader = true

	if nm := reFacetName.FindStringSubmatch(block); nm != nil {
		m.Name = strings.TrimSpace(nm[1])
	}
	if ml := reMetaLine.FindStringSubmatch(block); ml != nil {
		m.Version = strings.TrimSpace(ml[1])
		m.Status = strings.TrimSpace(ml[2])
		m.LayerRaw = strings.TrimSpace(ml[3])
	}

	// Section scan: "inputs:", "states:", "signals:", "context:" headed blocks,
	// each followed by indented lines until the next section header.
	lines := strings.Split(block, "\n")
	section := ""
	isHeader := func(l string) (string, bool) {
		t := strings.TrimSpace(l)
		for _, s := range []string{"inputs", "states", "signals", "context", "children"} {
			if strings.HasPrefix(strings.ToLower(t), s+":") {
				return s, true
			}
		}
		return "", false
	}
	for _, l := range lines {
		if name, ok := isHeader(l); ok {
			section = name
			// inline value after "states:" / single-line sections
			if v := strings.TrimSpace(l[strings.Index(l, ":")+1:]); v != "" {
				switch section {
				case "states":
					m.States = v
				case "signals":
					m.Signals = append(m.Signals, v)
				case "context":
					m.Context = append(m.Context, v)
				}
			}
			continue
		}
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		switch section {
		case "inputs":
			if f := reInputField.FindStringSubmatch(t); f != nil {
				m.Inputs = append(m.Inputs, ContractField{
					Name: f[1], Type: f[2], Desc: strings.TrimSpace(f[3]),
				})
			}
		case "states":
			if m.States == "" {
				m.States = t
			} else {
				m.States += " " + t
			}
		case "signals":
			m.Signals = append(m.Signals, t)
		case "context":
			m.Context = append(m.Context, t)
		}
	}
	return m
}

// discoverFacets reads every partial file and maps facet name -> parsed header.
// Each {{define}} in a file inherits the file's header layer so secondary
// templates group with their primary; only the FACET-named define gets the
// full contract.
func discoverFacets() map[string]FacetMeta {
	out := map[string]FacetMeta{}
	files, _ := filepath.Glob(filepath.Join("web", "templates", "partials", "*.html"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		src := string(raw)
		meta := parseFacetHeader(f, src)
		defines := reDefine.FindAllStringSubmatch(src, -1)
		for _, d := range defines {
			name := d[1]
			if name == meta.Name {
				out[name] = meta
				continue
			}
			// Secondary define in the same file: inherit grouping, no contract.
			out[name] = FacetMeta{
				Name:     name,
				File:     meta.File,
				LayerRaw: meta.LayerRaw,
				Status:   meta.Status,
			}
		}
		// Header names a facet that doesn't appear as a literal define here.
		if meta.Name != "" {
			if _, ok := out[meta.Name]; !ok {
				out[meta.Name] = meta
			}
		}
	}
	return out
}

// ── Composition graph (authoritative: from the parsed templates) ──────────────

func collectTemplateRefs(n parse.Node, out map[string]struct{}) {
	switch x := n.(type) {
	case *parse.ListNode:
		if x == nil {
			return
		}
		for _, c := range x.Nodes {
			collectTemplateRefs(c, out)
		}
	case *parse.TemplateNode:
		out[x.Name] = struct{}{}
	case *parse.IfNode:
		collectTemplateRefs(x.List, out)
		collectTemplateRefs(x.ElseList, out)
	case *parse.RangeNode:
		collectTemplateRefs(x.List, out)
		collectTemplateRefs(x.ElseList, out)
	case *parse.WithNode:
		collectTemplateRefs(x.List, out)
		collectTemplateRefs(x.ElseList, out)
	}
}

// slotGraph walks the parsed partial set and returns children + reverse parents.
func (h *Handler) slotGraph() (children, parents map[string][]string) {
	children = map[string][]string{}
	parents = map[string][]string{}
	if h.partial == nil {
		return
	}
	for _, t := range h.partial.Templates() {
		name := t.Name()
		if name == "" || t.Tree == nil || t.Tree.Root == nil {
			continue
		}
		refs := map[string]struct{}{}
		collectTemplateRefs(t.Tree.Root, refs)
		for r := range refs {
			children[name] = append(children[name], r)
			parents[r] = append(parents[r], name)
		}
	}
	for k := range children {
		sort.Strings(children[k])
	}
	for k := range parents {
		sort.Strings(parents[k])
	}
	return
}

// ── Tier classification ───────────────────────────────────────────────────────

// tierInfo normalizes the messy `layer:` field into a (label, rank). Lower rank
// renders first; the brief wants atoms first, containers last.
func tierInfo(layerRaw string) (string, int) {
	s := strings.ToLower(strings.TrimSpace(layerRaw))
	tok := s
	if i := strings.IndexAny(s, " ("); i > 0 {
		tok = s[:i]
	}
	switch tok {
	case "atomic", "5":
		return "Atomic", 0
	case "composite", "4":
		return "Composite", 1
	case "3":
		return "Content", 2
	case "2":
		return "Template", 3
	case "1":
		return "Wire / Page", 4
	case "":
		return "Unclassified", 9
	default:
		return "Layer " + tok, 8
	}
}

// ── View assembly ─────────────────────────────────────────────────────────────

func (h *Handler) buildAtlasView(isAdmin bool) atlasView {
	metas := discoverFacets()
	children, parents := h.slotGraph()
	reviews := dbpkg.GetFacetReviews(h.db)

	// Build the universe of facet names: everything the parsed set defines
	// (authoritative) plus anything a header named.
	names := map[string]struct{}{}
	for _, t := range h.partial.Templates() {
		n := t.Name()
		if n == "" || strings.HasSuffix(n, ".html") {
			continue
		}
		names[n] = struct{}{}
	}
	for n := range metas {
		if n != "" {
			names[n] = struct{}{}
		}
	}

	tiers := map[int]*atlasTier{}
	for name := range names {
		meta := metas[name]
		label, rank := tierInfo(meta.LayerRaw)
		review := reviews[name].Status
		if review == "" {
			review = "unreviewed"
		}
		card := atlasCard{
			Name:       name,
			File:       meta.File,
			Version:    meta.Version,
			Status:     meta.Status,
			TierLabel:  label,
			LayerRaw:   meta.LayerRaw,
			Inputs:     meta.Inputs,
			States:     meta.States,
			Signals:    meta.Signals,
			Context:    meta.Context,
			Slots:      dedupeFacetNames(children[name], names),
			UsedIn:     dedupeFacetNames(parents[name], names),
			FacetID:    "facet:f33d3r:" + strings.ToLower(strings.ReplaceAll(label, " / ", "_")) + ":" + name,
			Review:     review,
			ReviewNote: reviews[name].Note,
		}
		h.renderFacetVariants(&card) // live stage

		t, ok := tiers[rank]
		if !ok {
			t = &atlasTier{Label: label, Rank: rank}
			tiers[rank] = t
		}
		t.Cards = append(t.Cards, card)
	}

	view := atlasView{IsAdmin: isAdmin}
	ranks := make([]int, 0, len(tiers))
	for r := range tiers {
		ranks = append(ranks, r)
	}
	sort.Ints(ranks)
	for _, r := range ranks {
		t := tiers[r]
		sort.Slice(t.Cards, func(i, j int) bool { return t.Cards[i].Name < t.Cards[j].Name })
		t.Count = len(t.Cards)
		for _, c := range t.Cards {
			switch c.Review {
			case "approved":
				t.Approved++
				view.Approved++
			case "needs_work":
				view.NeedsWork++
			}
		}
		view.Tiers = append(view.Tiers, *t)
		view.TierLabels = append(view.TierLabels, t.Label)
		view.Total += t.Count
	}
	return view
}

// dedupeFacetNames keeps only references that are real facets in the universe,
// removing duplicates and self-references handled by the caller.
func dedupeFacetNames(in []string, universe map[string]struct{}) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range in {
		if seen[n] {
			continue
		}
		if _, ok := universe[n]; !ok {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// ── Handler ───────────────────────────────────────────────────────────────────

// atlasPage serves GET /_atlas. Gated to DEV_MODE or admin; 404 otherwise so the
// route is invisible in production.
func (h *Handler) atlasPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	isAdmin := user.IsAdmin()
	if !h.cfg.DevMode && !isAdmin {
		http.NotFound(w, r)
		return
	}

	view := h.buildAtlasView(isAdmin)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	var buf bytes.Buffer
	if h.atlas == nil {
		http.Error(w, "atlas template not loaded", http.StatusInternalServerError)
		return
	}
	if err := h.atlas.ExecuteTemplate(&buf, "atlas.html", view); err != nil {
		http.Error(w, "atlas render: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = buf.WriteTo(w)
}

// atlasReview persists an admin's review of a facet. Always admin-only — it
// writes, so it is gated regardless of DEV_MODE. Returns the new status as plain
// text; atlas.js updates the chip and counters client-side.
func (h *Handler) atlasReview(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	status := strings.TrimSpace(r.FormValue("status"))
	note := strings.TrimSpace(r.FormValue("note"))
	if name == "" {
		http.Error(w, "missing name", http.StatusBadRequest)
		return
	}
	switch status {
	case "approved", "needs_work", "unreviewed":
	default:
		http.Error(w, "bad status", http.StatusBadRequest)
		return
	}
	if err := dbpkg.SetFacetReview(h.db, name, status, note, user.Handle); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(status))
}
