package handler

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/f33d3r/feed-engine/internal/config"
)

// TestAtlasDiscoveryReal builds the real template set from web/templates and runs
// the full discovery/graph/tier assembly. cwd-dependent; intended as a local
// confidence check.
func TestAtlasDiscoveryReal(t *testing.T) {
	cwd, _ := os.Getwd()
	if err := os.Chdir("../.."); err != nil {
		t.Skipf("cannot chdir to repo root: %v", err)
	}
	defer os.Chdir(cwd)

	if _, err := os.Stat("web/templates/partials"); err != nil {
		t.Skipf("no web/templates: %v", err)
	}

	h := &Handler{cfg: &config.Config{}}
	h.loadTemplates()

	view := h.buildAtlasView(true)
	if view.Total < 150 {
		t.Fatalf("expected 150+ facets, got %d", view.Total)
	}
	t.Logf("total facets: %d, tiers: %v", view.Total, view.TierLabels)
	for _, tr := range view.Tiers {
		t.Logf("  tier %-14s rank=%d count=%d", tr.Label, tr.Rank, tr.Count)
	}

	// work_card should compose several known children (slot graph).
	var wc *atlasCard
	for ti := range view.Tiers {
		for ci := range view.Tiers[ti].Cards {
			if view.Tiers[ti].Cards[ci].Name == "work_card" {
				wc = &view.Tiers[ti].Cards[ci]
			}
		}
	}
	if wc == nil {
		t.Fatal("work_card not discovered")
	}
	t.Logf("work_card slots: %v", wc.Slots)
	t.Logf("work_card usedIn: %v", wc.UsedIn)
	if len(wc.Slots) == 0 {
		t.Fatal("expected work_card to have slot children")
	}
	joined := strings.Join(wc.Slots, ",")
	if !strings.Contains(joined, "post_avatar") {
		t.Errorf("expected work_card to compose post_avatar, got %v", wc.Slots)
	}

	// Every fixture must render without error and produce non-empty HTML.
	rendered := 0
	for name := range atlasFixtures {
		c := atlasCard{Name: name}
		h.renderFacetVariants(&c)
		if len(c.Variants) == 0 {
			t.Errorf("%s: no variants produced", name)
			continue
		}
		for _, v := range c.Variants {
			if v.Err != "" {
				t.Errorf("%s [%s]: render error: %s", name, v.Label, v.Err)
			}
			if strings.TrimSpace(string(v.HTML)) == "" && v.Err == "" {
				t.Logf("%s [%s]: rendered empty (ok for conditional facets)", name, v.Label)
			}
		}
		rendered++
	}
	t.Logf("fixtures rendered: %d facets", rendered)

	// The atlas.html template must execute against the full view (admin path).
	var buf bytes.Buffer
	if err := h.atlas.ExecuteTemplate(&buf, "atlas.html", view); err != nil {
		t.Fatalf("atlas.html execute: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, "Facet Atlas") {
		t.Fatal("rendered atlas missing title")
	}
	if !strings.Contains(html, "facet:f33d3r:") {
		t.Fatal("rendered atlas missing canonical facet ids")
	}
	if !strings.Contains(html, "atlas-review-bar") {
		t.Fatal("admin view missing review controls")
	}
	t.Logf("atlas.html rendered: %d bytes", len(html))
}
