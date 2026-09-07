package handler

import (
	"bytes"
	"fmt"
	"html/template"
	"path/filepath"
	"strings"
	"testing"

	"github.com/f33d3r/feed-engine/internal/realm"
)

// partialSet parses the named partial files as one set, the way the real
// renderer does.
func partialSet(t *testing.T, names ...string) *template.Template {
	t.Helper()
	paths := make([]string, 0, len(names))
	for _, n := range names {
		paths = append(paths, filepath.Join("..", "..", "web", "templates", "partials", n))
	}
	// Only the funcs the parsed facets actually reference; a facet needing more
	// than these is a facet whose dependencies are worth noticing.
	//
	// realmRing is the real one: it reads the realm index, which answers the
	// defined default when no index has been started, so a facet under test
	// draws exactly the ring an unknown handle draws in production.
	funcs := template.FuncMap{
		"avatarColors": func(string) []string { return []string{"#000", "#fff"} },
		"firstChar":    func(s string) string { return s },
		"realmRing":    realm.RingClass,
		"dict": func(values ...interface{}) (map[string]interface{}, error) {
			if len(values)%2 != 0 {
				return nil, fmt.Errorf("dict requires an even number of arguments")
			}
			m := make(map[string]interface{}, len(values)/2)
			for i := 0; i < len(values); i += 2 {
				k, ok := values[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict keys must be strings, got %T", values[i])
				}
				m[k] = values[i+1]
			}
			return m, nil
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFiles(paths...)
	if err != nil {
		t.Fatalf("parsing %v: %v", names, err)
	}
	return tmpl
}

func render(t *testing.T, tmpl *template.Template, name string, data interface{}) string {
	t.Helper()
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		t.Fatalf("rendering %s: %v", name, err)
	}
	return buf.String()
}

func TestNumberDisplayGroupsSpokenSymbols(t *testing.T) {
	if got := numberDisplay("H8K2-9QRT-4VMXC"); got != "H8K2 9QRT 4VMXC" {
		t.Fatalf("hyphenated form: got %q", got)
	}
	if got := numberDisplay("h8k29qrt4vmxc"); got != "H8K2 9QRT 4VMXC" {
		t.Fatalf("bare lowercase form: got %q", got)
	}
	// Anything that is not a Number is returned untouched rather than sliced.
	for _, s := range []string{"", "SHORT", "WAY-TOO-LONG-TO-BE-A-NUMBER"} {
		if got := numberDisplay(s); got != s {
			t.Fatalf("non-Number %q was rewritten to %q", s, got)
		}
	}
}

func TestNumbersFacetCarriesItsFacetIDAndNeverLeaksARetiredNumbersControls(t *testing.T) {
	tmpl := partialSet(t, "_f33d3r_numbers.html")
	out := render(t, tmpl, "f33d3r_numbers", map[string]interface{}{
		"PIALID":   "11111111-2222-3333-4444-555555555555",
		"Policies": []string{"open", "number_only", "closed"},
		"Numbers": []numberRow{
			{Number: "H8K2-9QRT-4VMXC", Display: "H8K2 9QRT 4VMXC", Live: true, Policy: "number_only", Label: "Badge"},
			{Number: "0000-0000-00000", Display: "0000 0000 00000", Live: false, Policy: "closed"},
		},
		"HandlePolicy":        "open",
		"DefaultNumberPolicy": "number_only",
	})

	if !strings.Contains(out, `data-facet-id="facet:f33d3r:identity:11111111-2222-3333-4444-555555555555:numbers"`) {
		t.Fatal("facet id missing or malformed")
	}
	if !strings.Contains(out, "H8K2 9QRT 4VMXC") {
		t.Fatal("live Number is not rendered in spoken form")
	}
	if strings.Count(out, "Retire</button>") != 1 {
		t.Fatal("a retired Number must not offer a Retire control")
	}
	if !strings.Contains(out, "Retired") {
		t.Fatal("retired state is not rendered")
	}
}

func TestNumbersFacetShowsAMintedSecretExactlyOnce(t *testing.T) {
	tmpl := partialSet(t, "_f33d3r_numbers.html")
	out := render(t, tmpl, "f33d3r_numbers", map[string]interface{}{
		"PIALID":        "abc",
		"Policies":      []string{"open"},
		"NewCapability": "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ",
	})
	if strings.Count(out, "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ") != 1 {
		t.Fatal("a freshly minted capability must be rendered exactly once")
	}
}

func TestResolveFacetNeverDisclosesAnIdentity(t *testing.T) {
	tmpl := partialSet(t, "_number_resolve.html")

	// Not even an allow names anyone. Contact proceeds by the Number the caller
	// already typed; the identity behind it surfaces only once a conversation
	// opens.
	allow := render(t, tmpl, "number_resolve", map[string]interface{}{
		"PIALID": "abc", "Decision": "allow", "Number": "H8K2-9QRT-4VMXC",
	})
	if !strings.Contains(allow, "/contact/start") {
		t.Fatal("an allow must offer to open a conversation by Number")
	}
	if !strings.Contains(allow, "H8K2-9QRT-4VMXC") {
		t.Fatal("the Number must be carried forward so the gate is re-evaluated")
	}
	if strings.Contains(allow, "@") {
		t.Fatal("resolution disclosed a handle")
	}

	// A refusal must not distinguish unknown from retired from closed.
	denied := render(t, tmpl, "number_resolve", map[string]interface{}{
		"PIALID": "abc", "Decision": "deny", "Number": "H8K2-9QRT-4VMXC",
	})
	for _, leak := range []string{"retired", "unknown", "closed", "does not exist"} {
		if strings.Contains(strings.ToLower(denied), leak) {
			t.Fatalf("a refusal leaked why it refused: %q", leak)
		}
	}
	if strings.Contains(denied, "/contact/start") {
		t.Fatal("a refusal must not offer to open a conversation")
	}
}

func TestContactGateRefusesWhenThePolicyAuthorityIsUnreachable(t *testing.T) {
	tmpl := partialSet(t, "_contact_gate.html")
	out := render(t, tmpl, "contact_gate", map[string]interface{}{
		"Handle": "someone", "Decision": "unavailable",
	})
	if !strings.Contains(out, `data-facet-id="facet:f33d3r:contact:someone:gate"`) {
		t.Fatal("facet id missing or malformed")
	}
	if strings.Contains(out, "not accepting") {
		t.Fatal("an unreachable authority must not be reported as a refusal by the user")
	}
}

func TestNotifBadgeRendersAFragmentAndHidesAtZero(t *testing.T) {
	tmpl := partialSet(t, "_notif_badge.html")

	zero := render(t, tmpl, "notif_badge", map[string]interface{}{"PIALID": "abc", "UnreadCount": 0})
	if !strings.Contains(zero, "hidden") {
		t.Fatal("an empty badge must be hidden rather than rendered blank")
	}
	if !strings.Contains(zero, `data-facet-id="facet:f33d3r:identity:abc:notif_badge"`) {
		t.Fatal("facet id missing — the fragment could not be located for mutation")
	}

	many := render(t, tmpl, "notif_badge", map[string]interface{}{"PIALID": "abc", "UnreadCount": 42})
	if !strings.Contains(many, "9+") {
		t.Fatal("counts above nine must render as 9+")
	}
}

func TestNotifItemFallsBackForAnUnrecognisedType(t *testing.T) {
	tmpl := partialSet(t, "_notif_item.html", "_user_avatar.html", "_status_dot.html")
	out := render(t, tmpl, "notif_item", map[string]interface{}{
		"Type": "a_type_nobody_has_written_yet", "ActorHandle": "someone",
		"ActorName": "Someone", "ActorAvatar": "", "IsRead": true,
		"TargetID": "", "TimeAgo": "now", "CreatedAt": testTime{},
	})
	if !strings.Contains(out, "sent you a notification") {
		t.Fatal("an unrecognised notification type must not render an empty sentence")
	}

	known := render(t, tmpl, "notif_item", map[string]interface{}{
		"Type": "contact_accepted", "ActorHandle": "someone",
		"ActorName": "Someone", "ActorAvatar": "", "IsRead": true,
		"TargetID": "", "TimeAgo": "now", "CreatedAt": testTime{},
	})
	if !strings.Contains(known, "accepted your contact request") {
		t.Fatal("contact_accepted has no rendering")
	}
	if strings.Contains(known, "sent you a notification") {
		t.Fatal("a known type fell through to the fallback")
	}

	// The other half of the pair: a request that reaches nobody is a request
	// nobody answers, so the arrival has a rendering and it points at the inbox
	// that can settle it.
	asked := render(t, tmpl, "notif_item", map[string]interface{}{
		"Type": "contact_request", "ActorHandle": "someone",
		"ActorName": "Someone", "ActorAvatar": "", "IsRead": false,
		"TargetID": "", "TimeAgo": "now", "CreatedAt": testTime{},
	})
	if !strings.Contains(asked, "asked to reach you") {
		t.Fatal("contact_request has no rendering")
	}
	if !strings.Contains(asked, "/settings/contact") {
		t.Fatal("a contact request must lead to the inbox that can answer it")
	}
	if strings.Contains(asked, "sent you a notification") {
		t.Fatal("contact_request fell through to the fallback")
	}
}

// testTime satisfies the .CreatedAt.Unix call the notif_item facet makes.
type testTime struct{}

func (testTime) Unix() int64 { return 0 }
