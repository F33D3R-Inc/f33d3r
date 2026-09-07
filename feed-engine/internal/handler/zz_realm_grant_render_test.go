package handler

import (
	"os"
	"strings"
	"testing"

	"github.com/f33d3r/feed-engine/internal/config"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/realm"
)

// The realm grant is assigned from one facet — the admin lookup result — and
// this file proves that facet renders, that the control it draws is founder
// only, and that the standing sentence it shows is decided in Go.

func realmGrantTestHandler(t *testing.T) *Handler {
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

func renderAdminLookup(t *testing.T, h *Handler, caller, target *model.User) string {
	t.Helper()
	var b strings.Builder
	err := h.partial.ExecuteTemplate(&b, "admin_lookup_result", map[string]interface{}{
		"LookupUser":   target,
		"LookupHandle": target.Handle,
		"LookupCaps":   model.PIALCapabilityMap{},
		"AllCaps":      model.DefaultCapabilities,
		"CurrentUser":  caller,
	})
	if err != nil {
		t.Fatalf("admin_lookup_result: render: %v", err)
	}
	return b.String()
}

// TestTheGrantControlIsDrawnForTheFounderAlone pins the surface to the same
// gate the server enforces. An admin who cannot use the control is never shown
// it, so the panel never offers an action the server will refuse.
func TestTheGrantControlIsDrawnForTheFounderAlone(t *testing.T) {
	h := realmGrantTestHandler(t)

	target := &model.User{
		ID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Handle: "granted_probe",
		DisplayName: "Granted Probe", Role: model.RoleUser, XP: 125, RealmGrant: 5, Realm: 5,
	}

	founder := &model.User{Handle: "tehanibentley", Role: model.RoleFounder}
	out := renderAdminLookup(t, h, founder, target)
	if !strings.Contains(out, `hx-post="/api/admin/set-realm-grant"`) {
		t.Error("the founder is not shown the realm standing control")
	}
	if !strings.Contains(out, "Realm standing") {
		t.Error("the realm standing card is missing for the founder")
	}

	admin := &model.User{Handle: "an_admin", Role: model.RoleAdmin}
	out = renderAdminLookup(t, h, admin, target)
	if strings.Contains(out, "/api/admin/set-realm-grant") {
		t.Error("an admin is shown a realm grant control the server will refuse")
	}
	// The role control it sits beside is still there — this is a narrower gate
	// on one card, not the admin losing the panel.
	if !strings.Contains(out, `hx-post="/api/admin/set-role"`) {
		t.Error("the admin lost the role control")
	}
}

// TestTheGrantedRingIsShownWithItsSource proves the facet interpolates a
// sentence the server decided. The template never compares a realm to a number
// or works out for itself whether standing was earned.
func TestTheGrantedRingIsShownWithItsSource(t *testing.T) {
	h := realmGrantTestHandler(t)
	founder := &model.User{Handle: "tehanibentley", Role: model.RoleFounder}

	granted := &model.User{
		ID: "a", Handle: "granted_probe", DisplayName: "Granted Probe",
		Role: model.RoleUser, XP: 125, RealmGrant: 5, Realm: 5,
	}
	if out := renderAdminLookup(t, h, founder, granted); !strings.Contains(out, "held by a granted R5 floor") {
		t.Errorf("granted standing is not named as granted:\n%s", realmStandingLabel(granted.XP, granted.RealmGrant, granted.Role))
	}

	earner := &model.User{
		ID: "b", Handle: "earner_probe", DisplayName: "Earner Probe",
		Role: model.RoleUser, XP: 20000, RealmGrant: 0, Realm: 5,
	}
	if out := renderAdminLookup(t, h, founder, earner); !strings.Contains(out, "earned on 20000 XP") {
		t.Errorf("earned standing is not named as earned:\n%s", realmStandingLabel(earner.XP, earner.RealmGrant, earner.Role))
	}

	self := &model.User{
		ID: "c", Handle: "tehanibentley", DisplayName: "Tehani",
		Role: model.RoleFounder, XP: 195, RealmGrant: 3, Realm: 5,
	}
	if out := renderAdminLookup(t, h, founder, self); !strings.Contains(out, "the permanent R5 floor the founder role carries") {
		t.Errorf("the founder floor is not named as the source:\n%s", realmStandingLabel(self.XP, self.RealmGrant, self.Role))
	}
}

// TestTheStandingSentenceNamesTheFallback proves the label always says what the
// account would drop to, so nobody has to guess what clearing a grant does.
func TestTheStandingSentenceNamesTheFallback(t *testing.T) {
	got := realmStandingLabel(125, 5, model.RoleUser)
	for _, want := range []string{"R5 Guardian", "granted R5 floor", "earned R1 Wanderer", "125 XP"} {
		if !strings.Contains(got, want) {
			t.Errorf("standing label %q is missing %q", got, want)
		}
	}
	if got := realmStandingLabel(0, realm.GrantNone, model.RoleUser); !strings.Contains(got, "earned on 0 XP") {
		t.Errorf("an ungranted account reads %q", got)
	}
}
