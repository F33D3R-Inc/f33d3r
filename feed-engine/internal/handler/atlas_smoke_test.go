package handler

import "testing"

const btnFollowSrc = `{{/*
FACET btn_follow
  version: 1.0 | status: stable | layer: 5

  inputs:
    TargetPIAL    string — PIAL ID of the user to follow/unfollow
    TargetHandle  string — handle (for aria-label)
    IsFollowing   bool   — current follow state

  states:
    default | following (is-on)

  children: none

  signals:
    emits:    follow (POST /events) | unfollow (POST /events)
    receives: none

  context:
    profile_header → action row
*/}}
{{define "btn_follow"}}<button>x</button>{{end}}`

func TestParseFacetHeader(t *testing.T) {
	m := parseFacetHeader("_btn_follow.html", btnFollowSrc)
	if !m.HasHeader {
		t.Fatal("expected HasHeader")
	}
	if m.Name != "btn_follow" {
		t.Fatalf("name = %q", m.Name)
	}
	if m.Version != "1.0" || m.Status != "stable" || m.LayerRaw != "5" {
		t.Fatalf("meta = %q %q %q", m.Version, m.Status, m.LayerRaw)
	}
	if len(m.Inputs) != 3 {
		t.Fatalf("inputs = %d: %+v", len(m.Inputs), m.Inputs)
	}
	if m.Inputs[0].Name != "TargetPIAL" || m.Inputs[0].Type != "string" {
		t.Fatalf("input0 = %+v", m.Inputs[0])
	}
	if m.States != "default | following (is-on)" {
		t.Fatalf("states = %q", m.States)
	}
	if len(m.Signals) == 0 || len(m.Context) == 0 {
		t.Fatalf("signals=%v context=%v", m.Signals, m.Context)
	}
	label, rank := tierInfo(m.LayerRaw)
	if label != "Atomic" || rank != 0 {
		t.Fatalf("tier = %q %d", label, rank)
	}
}

func TestTierInfoTolerant(t *testing.T) {
	cases := map[string]struct {
		label string
		rank  int
	}{
		"5":                   {"Atomic", 0},
		"atomic":              {"Atomic", 0},
		"2":                   {"Template", 3},
		"1":                   {"Wire / Page", 4},
		"3 (template facet)":  {"Content", 2},
		"composite":           {"Composite", 1},
		"":                    {"Unclassified", 9},
	}
	for in, want := range cases {
		l, r := tierInfo(in)
		if l != want.label || r != want.rank {
			t.Errorf("tierInfo(%q) = %q,%d want %q,%d", in, l, r, want.label, want.rank)
		}
	}
}
