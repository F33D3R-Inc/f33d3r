package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAccountFields: the settings screen's fields round-trip through
// profile_update and come back on /me in the shapes the Swift CurrentUser
// decodes; the closed vocabularies are enforced; a seeded creator can be paid.
func TestAccountFields(t *testing.T) {
	h := newHarness(t)
	token, me := h.login("dev")

	if me["has_password"] != true || me["payout_enabled"] != true || me["kyc_status"] != "full" ||
		me["birthday_md_visibility"] != "everyone" || me["birthday_year_visibility"] != "only_me" {
		t.Fatalf("seeded dev standing: %v", me)
	}
	if _, ok := me["country_code"]; !ok || me["country_code"] != nil {
		t.Fatalf("country_code must be present and null before it is set: %v", me["country_code"])
	}
	if links, ok := me["social_links"].(map[string]any); !ok || len(links) != 0 {
		t.Fatalf("social_links must be an empty object, got %v", me["social_links"])
	}

	_, guest := h.login("guest")
	if guest["payout_enabled"] != false || guest["kyc_status"] != nil {
		t.Fatalf("guest standing: payout=%v kyc=%v", guest["payout_enabled"], guest["kyc_status"])
	}

	status, body := h.do("POST", "/events", token, map[string]any{
		"event_type":               "profile_update",
		"birthday_md_visibility":   "followers",
		"birthday_year_visibility": "only_me",
		"country_code":             "us",
		"is_adult_creator":         true,
		"social_links":             map[string]string{"youtube": "https://youtube.com/@dev", "twitch": "", "kick": " https://kick.com/dev "},
		"external_tip_links":       map[string]string{"cashapp": "devcash", "xrp": "rDEV"},
	}, "")
	if status != 200 {
		t.Fatalf("profile_update: %d %s", status, body)
	}
	var fresh map[string]any
	json.Unmarshal(body, &fresh)
	if fresh["birthday_md_visibility"] != "followers" || fresh["country_code"] != "US" || fresh["is_adult_creator"] != true {
		t.Fatalf("after update: %v", fresh)
	}
	social := fresh["social_links"].(map[string]any)
	if len(social) != 2 || social["youtube"] != "https://youtube.com/@dev" || social["kick"] != "https://kick.com/dev" {
		t.Fatalf("social_links = %v; blanks drop, values trim", social)
	}
	tips := fresh["external_tip_links"].(map[string]any)
	if tips["cashapp"] != "devcash" || tips["xrp"] != "rDEV" {
		t.Fatalf("external_tip_links = %v", tips)
	}

	// /me says the same thing the event answered.
	_, body = h.do("GET", "/api/v1/me", token, nil, "")
	var again map[string]any
	json.Unmarshal(body, &again)
	if again["country_code"] != "US" || again["social_links"].(map[string]any)["youtube"] != "https://youtube.com/@dev" {
		t.Fatalf("/me after update: %v", again)
	}

	// Clearing: an empty country is null again; an empty map is an empty object.
	status, body = h.do("POST", "/events", token, map[string]any{
		"event_type": "profile_update", "country_code": "", "social_links": map[string]string{},
	}, "")
	json.Unmarshal(body, &fresh)
	if status != 200 || fresh["country_code"] != nil || len(fresh["social_links"].(map[string]any)) != 0 {
		t.Fatalf("clear: %d %v", status, fresh)
	}

	// The vocabularies are closed.
	for _, bad := range []map[string]any{
		{"event_type": "profile_update", "birthday_md_visibility": "friends"},
		{"event_type": "profile_update", "country_code": "USA"},
		{"event_type": "profile_update", "social_links": map[string]string{"myspace": "https://x"}},
		{"event_type": "profile_update", "social_links": map[string]string{"youtube": "youtube.com/@dev"}},
		{"event_type": "profile_update", "external_tip_links": map[string]string{"stripe": "x"}},
		{"event_type": "profile_update", "is_adult_creator": "yes"},
	} {
		if status, body = h.do("POST", "/events", token, bad, ""); status != 400 {
			t.Errorf("%v: %d %s", bad, status, strings.TrimSpace(string(body)))
		}
	}
}
