package handler

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// parseLease runs the real parser over a urlencoded body.
//
// The bodies are written out literally rather than built from a map, because
// what is being tested is which fields are PRESENT: a field that is absent and a
// field that is empty mean different things to a lease.
func parseLease(t *testing.T, body string) (numberLeaseInput, error) {
	t.Helper()
	r := httptest.NewRequest("POST", "/identity/numbers/policy", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return parseNumberLease(r)
}

// ── Saying nothing is not saying "no limit" ──────────────────────────────────

func TestALeaseFormThatMentionsNothingLeavesTheLeaseAlone(t *testing.T) {
	// This is the policy control beside a Number: it posts the policy and the
	// label and nothing else. If "absent" collapsed into "no limit", changing a
	// conference Number's policy would silently throw away its expiry and its
	// budget — the two things the owner set it up for.
	in, err := parseLease(t, "number=H8K2-9QRT-4VMXC&label=ACL&policy=number_only")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if in.ExpirySpecified || in.BudgetSpecified {
		t.Fatalf("a form that said nothing must specify nothing: %+v", in)
	}
	body := map[string]interface{}{}
	in.applyTo(body)
	if len(body) != 0 {
		t.Fatalf("nothing said must send nothing, got %v", body)
	}
}

func TestAnEmptyLeaseFieldIsAnExplicitNoLimit(t *testing.T) {
	in, err := parseLease(t, "expiry=&max_admissions=")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if !in.ExpirySpecified || !in.BudgetSpecified {
		t.Fatalf("both fields were present and must count as specified: %+v", in)
	}
	if in.ExpiresInSeconds != nil || in.MaxAdmissions != nil {
		t.Fatalf("an empty field means no limit, not a value: %+v", in)
	}
	body := map[string]interface{}{}
	in.applyTo(body)
	for _, k := range []string{"expires_in_seconds", "max_admissions"} {
		v, ok := body[k]
		if !ok {
			t.Fatalf("%s must be sent so the authority clears the column", k)
		}
		if v != nil {
			t.Fatalf("%s must be sent as null, got %#v", k, v)
		}
	}
}

func TestLeaveUnchangedDropsTheExpiryFromTheRequestEntirely(t *testing.T) {
	// The form that edits an existing Number always carries an expiry select,
	// so "leave unchanged" has to be expressible, not merely absent.
	in, err := parseLease(t, "expiry="+numberExpiryKeep+"&max_admissions=40")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if in.ExpirySpecified {
		t.Fatal("leave-unchanged must not specify an expiry")
	}
	if !in.BudgetSpecified || in.MaxAdmissions == nil || *in.MaxAdmissions != 40 {
		t.Fatalf("the budget beside it must still be read: %+v", in)
	}
	body := map[string]interface{}{}
	in.applyTo(body)
	if _, ok := body["expires_in_seconds"]; ok {
		t.Fatalf("leave-unchanged must send no expiry at all, got %v", body)
	}
	if body["max_admissions"] != 40 {
		t.Fatalf("budget: got %#v", body["max_admissions"])
	}
}

// ── Presets and a custom number of days ──────────────────────────────────────

func TestEveryPresetIsAWholeNumberOfSecondsOrASentinel(t *testing.T) {
	// A preset is a DURATION, never a date: a date would be read against the
	// browser's clock and its timezone and then reconciled with the authority's.
	seen := map[string]bool{}
	for _, o := range numberExpiryOptions() {
		if seen[o.Value] {
			t.Fatalf("duplicate expiry option %q", o.Value)
		}
		seen[o.Value] = true
		if o.Label == "" {
			t.Fatalf("expiry option %q has no label", o.Value)
		}
		if o.Value == "" || o.Value == numberExpiryCustom {
			continue
		}
		in, err := parseLease(t, "expiry="+o.Value)
		if err != nil {
			t.Fatalf("preset %q is not readable by the parser that receives it: %v", o.Value, err)
		}
		if in.ExpiresInSeconds == nil || *in.ExpiresInSeconds <= 0 {
			t.Fatalf("preset %q did not resolve to a positive duration", o.Value)
		}
	}
	if !seen[""] {
		t.Fatal("no-expiry must be offered")
	}
	if !seen[numberExpiryCustom] {
		t.Fatal("a custom number of days must be offered")
	}
}

func TestChangingAnExistingNumberDefaultsToLeavingTheExpiryAlone(t *testing.T) {
	opts := numberExpiryChangeOptions()
	if len(opts) == 0 || opts[0].Value != numberExpiryKeep {
		t.Fatalf("leave-unchanged must be first and therefore the default: %+v", opts)
	}
	if len(opts) != len(numberExpiryOptions())+1 {
		t.Fatalf("the change vocabulary must be the mint vocabulary plus one: %+v", opts)
	}
}

func TestACustomNumberOfDaysBecomesSeconds(t *testing.T) {
	in, err := parseLease(t, "expiry="+numberExpiryCustom+"&expiry_days=3")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if in.ExpiresInSeconds == nil || *in.ExpiresInSeconds != 3*secondsPerDay {
		t.Fatalf("three days: got %+v", in.ExpiresInSeconds)
	}
	// Opening the field and leaving it blank is no expiry, not a refusal.
	in, err = parseLease(t, "expiry="+numberExpiryCustom+"&expiry_days=")
	if err != nil {
		t.Fatalf("a blank day count is not an error: %v", err)
	}
	if !in.ExpirySpecified || in.ExpiresInSeconds != nil {
		t.Fatalf("a blank day count is no expiry: %+v", in)
	}
}

func TestALeaseThatIsNotAWholeNumberIsRefused(t *testing.T) {
	for _, body := range []string{
		"expiry=" + numberExpiryCustom + "&expiry_days=0",
		"expiry=" + numberExpiryCustom + "&expiry_days=-4",
		"expiry=" + numberExpiryCustom + "&expiry_days=tuesday",
		"expiry=nonsense",
		"expiry=-1",
		"max_admissions=0",
		"max_admissions=-5",
		"max_admissions=forty",
	} {
		if _, err := parseLease(t, body); err == nil {
			t.Fatalf("must refuse: %q", body)
		}
	}
}

// ── What the owner is shown ──────────────────────────────────────────────────

func TestTimeLeftIsRenderedOnTheServerInWords(t *testing.T) {
	cases := []struct {
		seconds int64
		want    string
	}{
		{-1, "expired"},
		{0, "expired"},
		{30, "under a minute left"},
		{60, "1 minute left"},
		{120, "2 minutes left"},
		{3600, "1 hour left"},
		{7200, "2 hours left"},
		{secondsPerDay, "1 day left"},
		{6 * secondsPerDay, "6 days left"},
	}
	for _, c := range cases {
		if got := humaniseTimeLeft(c.seconds); got != c.want {
			t.Fatalf("%ds: got %q want %q", c.seconds, got, c.want)
		}
	}
}

func TestALeaseCountsWhatIsLeftAndNeverGoesNegative(t *testing.T) {
	budget := 40
	secs := int64(6 * secondsPerDay)
	l := numberLeaseFrom("live", &secs, &budget, 12)
	if !l.HasBudget || l.Remaining != 28 || l.Budget != 40 || l.Used != 12 {
		t.Fatalf("remaining budget: %+v", l)
	}
	if l.Expires != "6 days left" {
		t.Fatalf("countdown: %q", l.Expires)
	}
	if !l.Live() || l.Spent() || l.Expired() {
		t.Fatalf("a live lease: %+v", l)
	}

	// Defensive: a count above the cap reads as nothing left, never as a
	// negative number of people.
	l = numberLeaseFrom("exhausted", nil, &budget, 41)
	if l.Remaining != 0 {
		t.Fatalf("remaining must floor at zero: %+v", l)
	}
	if !l.Spent() || l.Live() {
		t.Fatalf("an exhausted lease: %+v", l)
	}
	if l.Expires != "" {
		t.Fatalf("a Number with no expiry has no countdown: %q", l.Expires)
	}

	// A Number minted before leases existed carries neither, and is live.
	l = numberLeaseFrom("", nil, nil, 0)
	if !l.Live() || l.HasBudget || l.Expires != "" {
		t.Fatalf("a Number with no lease at all: %+v", l)
	}
}

func TestNumbersFacetShowsTheOwnerTheirLabelCountdownAndRemainingBudget(t *testing.T) {
	tmpl := partialSet(t, "_f33d3r_numbers.html")
	secs := int64(6 * secondsPerDay)
	budget := 40
	out := render(t, tmpl, "f33d3r_numbers", map[string]interface{}{
		"PIALID":   "pial-1",
		"Policies": []string{"number_only", "mutuals"},
		"Numbers": []map[string]interface{}{{
			"Number":  "H8K29QRT4VMXC",
			"Display": "H8K2 9QRT 4VMXC",
			"Live":    true,
			"Policy":  "number_only",
			"Label":   "ACL Conference 2026",
			"Lease":   numberLeaseFrom("live", &secs, &budget, 12),
		}},
		"ExpiryOptions":       numberExpiryOptions(),
		"ExpiryChangeOptions": numberExpiryChangeOptions(),
		"ExpiryDaysMax":       numberExpiryDaysMax,
		"BudgetMax":           numberBudgetMax,
	})
	for _, want := range []string{
		`data-facet-id="facet:f33d3r:identity:pial-1:numbers"`,
		"ACL Conference 2026",
		"6 days left",
		"28 of 40 left",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the owner must see %q\n%s", want, out)
		}
	}
}

func TestNumbersFacetTellsTheOwnerWhyANumberStoppedAdmittingPeople(t *testing.T) {
	tmpl := partialSet(t, "_f33d3r_numbers.html")
	budget := 40
	for _, c := range []struct{ state, want string }{
		{"expired", "Expired"},
		{"exhausted", "Full"},
	} {
		out := render(t, tmpl, "f33d3r_numbers", map[string]interface{}{
			"PIALID":   "pial-1",
			"Policies": []string{"number_only"},
			"Numbers": []map[string]interface{}{{
				"Number": "H8K29QRT4VMXC", "Display": "H8K2 9QRT 4VMXC",
				"Live": true, "Policy": "number_only", "Label": "ACL",
				"Lease": numberLeaseFrom(c.state, nil, &budget, 40),
			}},
			"ExpiryOptions":       numberExpiryOptions(),
			"ExpiryChangeOptions": numberExpiryChangeOptions(),
			"ExpiryDaysMax":       numberExpiryDaysMax,
			"BudgetMax":           numberBudgetMax,
		})
		if !strings.Contains(out, c.want) {
			t.Fatalf("state %q must read %q to its owner\n%s", c.state, c.want, out)
		}
	}
}

// ── What a resolver is shown ─────────────────────────────────────────────────

func TestResolveFacetRendersOneIdenticalRefusalWhateverCausedIt(t *testing.T) {
	// Expired, exhausted, retired and never-allocated are FOUR different facts
	// about a Number and ONE answer to whoever typed it. The authority collapses
	// them to "deny" before this surface ever sees them, and this surface has a
	// single fallthrough branch, so the fragment cannot differ. Pin both halves:
	// nothing that names a cause reaches the template, and the bytes match.
	tmpl := partialSet(t, "_number_resolve.html")
	base := render(t, tmpl, "number_resolve", map[string]interface{}{
		"PIALID": "pial-1", "Decision": "deny", "Number": "H8K2-9QRT-4VMXC",
	})
	for _, cause := range []string{"expired", "exhausted", "revoked", "unknown", "closed"} {
		out := render(t, tmpl, "number_resolve", map[string]interface{}{
			"PIALID": "pial-1", "Decision": "deny", "Number": "H8K2-9QRT-4VMXC",
			// Even if a future caller mistakenly passed the cause through, the
			// facet must not render it.
			"LeaseState": cause,
			"Label":      "ACL Conference 2026",
			"Lease":      numberLeaseFrom(cause, nil, nil, 40),
		})
		if out != base {
			t.Fatalf("cause %q produced a different fragment:\n%s\nvs\n%s", cause, out, base)
		}
		if strings.Contains(out, cause) {
			t.Fatalf("cause %q leaked into the fragment:\n%s", cause, out)
		}
	}
}

func TestResolveFacetNeverRendersALabelACountdownOrABudget(t *testing.T) {
	// A label is the owner's private note to themselves. It must not appear on
	// a resolve, in a refusal, or beside an allow — and neither must how much of
	// the budget is left, which would tell a stranger how many other people hold
	// the same Number.
	tmpl := partialSet(t, "_number_resolve.html")
	secs := int64(6 * secondsPerDay)
	budget := 40
	for _, decision := range []string{"allow", "request", "deny"} {
		out := render(t, tmpl, "number_resolve", map[string]interface{}{
			"PIALID": "pial-1", "Decision": decision, "Number": "H8K2-9QRT-4VMXC",
			"Label": "ACL Conference 2026",
			"Lease": numberLeaseFrom("live", &secs, &budget, 12),
		})
		for _, forbidden := range []string{
			"ACL Conference 2026", "6 days left", "28 of 40", "40", "28",
		} {
			if strings.Contains(out, forbidden) {
				t.Fatalf("decision %q leaked %q:\n%s", decision, forbidden, out)
			}
		}
	}
}
