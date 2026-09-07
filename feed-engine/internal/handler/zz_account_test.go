package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/f33d3r/feed-engine/internal/config"
	"github.com/f33d3r/feed-engine/internal/model"
)

// The settings surface, pinned.
//
// Everything here is either pure or reachable without a database. What needs a
// live Postgres or a live Herald — the toggle actually writing the column, the
// preference actually reaching Herald, a login against an account that really
// has TOTP on — is pinned at the level this package can honestly reach: the
// parsing rule, the dispatch, the DTO, and the source of the branch itself.

const accountSessionToken = "test-session-account"

func accountHandler(t *testing.T, user *model.User) *Handler {
	t.Helper()
	// sql.Open does not dial. Nothing below issues a query it depends on.
	db, err := sql.Open("postgres", "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Skipf("postgres driver unavailable in this build: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := &Handler{cfg: &config.Config{}, db: db}
	h.sessionCache.Store(accountSessionToken, sessionEntry{user: user, exp: time.Now().Add(time.Hour)})
	return h
}

func accountEvent(t *testing.T, h *Handler, body string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: accountSessionToken})
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("test body is not JSON: %v", err)
	}
	var eventType string
	_ = json.Unmarshal(raw["event_type"], &eventType)
	rec := httptest.NewRecorder()
	handled := h.apiV1AccountEvent(rec, req, eventType, raw)
	return rec, handled
}

// ── S1: the toggle that could be turned on but never off ─────────────────────

// TestSettingsToggleParsing is the rule behind the three privacy toggles. They
// each used to read `val != ""`, so "0" — the only way a native client can say
// "off" in a string field — turned the setting ON. Off is "", "0", "false",
// "off" and "no"; everything else is on.
func TestSettingsToggleParsing(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    bool
		present bool
	}{
		{"json string zero", `{"value":"0"}`, false, true},
		{"json string one", `{"value":"1"}`, true, true},
		{"json empty string", `{"value":""}`, false, false},
		{"json literal false", `{"value":false}`, false, true},
		{"json literal true", `{"value":true}`, true, true},
		{"json word false", `{"value":"false"}`, false, true},
		{"json word off", `{"value":"off"}`, false, true},
		{"json word on", `{"value":"on"}`, true, true},
		{"absent", `{}`, false, false},
	}
	for _, c := range cases {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(c.body), &raw); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		r := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(c.body))
		r.Header.Set("Content-Type", "application/json")
		got, present := eventBool(r, raw, "value")
		if got != c.want || present != c.present {
			t.Errorf("%s: eventBool = (%v, %v), want (%v, %v)", c.name, got, present, c.want, c.present)
		}
	}

	// The web's form: an unchecked checkbox submits nothing at all, which is
	// off, and a checked one submits its value, which is on.
	off := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader("event_type=settings.privacy.celebrations"))
	off.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if v, _ := eventBool(off, nil, "value"); v {
		t.Error("an unchecked checkbox submitted nothing and was read as ON")
	}
	on := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader("event_type=settings.privacy.celebrations&value=on"))
	on.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if v, present := eventBool(on, nil, "value"); !v || !present {
		t.Error("a checked checkbox was read as OFF")
	}
}

// TestSettingsToggleCasesUseEventBool holds the three cases to the rule above.
// The bug was not in a helper — there was no helper. It was three hand-written
// readings of the same field, each wrong in the same way, so the guard is on
// the source of the cases themselves.
func TestSettingsToggleCasesUseEventBool(t *testing.T) {
	src := readSource(t, "handlers.go")
	for _, event := range []string{
		"settings.privacy.show_sensitive",
		"settings.privacy.celebrations",
		"settings.privacy.account_private",
	} {
		body := eventCaseBody(t, src, event)
		if !strings.Contains(body, `eventBool(r, rawBodyMap, "value")`) {
			t.Errorf("%s does not read its value with eventBool — a client sending \"0\" turns it ON", event)
		}
		if strings.Contains(body, `val != ""`) {
			t.Errorf("%s still reads the value as `val != \"\"`", event)
		}
	}

	// The web's radio group names the field "setting"; the clients name it
	// "value". Reading only one name loses every change the other sends.
	body := eventCaseBody(t, src, "settings.privacy.content_setting")
	if !strings.Contains(body, `eventField(r, rawBodyMap, "setting", "value")`) {
		t.Error(`content_setting does not read both "setting" and "value"`)
	}
}

// eventCaseBody returns the source of one `case "<event>":` block in the
// events switch, up to the next case label.
func eventCaseBody(t *testing.T, src, event string) string {
	t.Helper()
	marker := `case "` + event + `":`
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("no case for %q in the events switch", event)
	}
	rest := src[i+len(marker):]
	if j := strings.Index(rest, "\n\tcase "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// ── S1: session.revoke, the name the web actually posts ──────────────────────

// TestSessionRevokeAliasDispatches: the session card has always posted
// "session.revoke" and only "session_revoke" was handled, so "sign out this
// device" answered 404 on every click. Both names reach the same rule now.
func TestSessionRevokeAliasDispatches(t *testing.T) {
	h := accountHandler(t, &model.User{ID: "u1", Handle: "dev", PIALID: "c0ffee00-0000-4000-8000-000000000001"})

	for _, name := range []string{"session_revoke", "session.revoke"} {
		rec, handled := accountEvent(t, h, `{"event_type":"`+name+`"}`)
		if !handled {
			t.Fatalf("%s was not dispatched — /events answers 404 for it", name)
		}
		// No session_id in the body: the shared rule refuses it the same way
		// under either name, which is what proves it is the same rule.
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s with no session_id: got %d, want 400", name, rec.Code)
		}
	}

	if _, handled := accountEvent(t, h, `{"event_type":"not_an_event"}`); handled {
		t.Error("the account dispatcher claimed an event it does not own")
	}
}

// TestAccountEventNamesDispatch: every settings event the contract names is
// owned by the account dispatcher rather than falling through to the 404.
func TestAccountEventNamesDispatch(t *testing.T) {
	h := accountHandler(t, &model.User{ID: "u1", Handle: "dev", PIALID: "c0ffee00-0000-4000-8000-000000000001"})
	for _, name := range []string{
		"notification_prefs", "two_fa_enable", "two_fa_disable",
		"two_fa_backup_codes", "account_delete", "profile_update",
	} {
		req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: accountSessionToken})
		if !h.apiV1AccountEvent(httptest.NewRecorder(), req, name, map[string]json.RawMessage{}) {
			t.Errorf("%s is not dispatched — the app's settings screen gets a 404", name)
		}
	}
}

// ── S4: the second factor at login ───────────────────────────────────────────

// TestLoginTwoFactorGate pins the branch in apiV1Login. An account with TOTP on
// must not be entered with a password alone, the demand for a code must come
// only after the password is verified — otherwise the endpoint tells a stranger
// which handles have 2FA on — and the two refusals must be distinguishable, so
// the app can show a code field for one and an error for the other.
//
// The branch itself needs a live Postgres to exercise end to end; what is
// checked here is that it exists, in that order, with those codes.
func TestLoginTwoFactorGate(t *testing.T) {
	src := readSource(t, "api_v1_auth.go")
	login := src[strings.Index(src, "func (h *Handler) apiV1Login("):]
	login = login[:strings.Index(login, "\nfunc ")]

	for _, want := range []string{"two_fa_required", "two_fa_invalid", "GetTOTPSecret", "totpVerify", "VerifyAndConsumeBackupCode"} {
		if !strings.Contains(login, want) {
			t.Errorf("apiV1Login does not mention %q — the second factor is not checked at login", want)
		}
	}
	password := strings.Index(login, "CheckPassword")
	gate := strings.Index(login, "two_fa_required")
	if password < 0 || gate < 0 || gate < password {
		t.Error("apiV1Login asks for a 2FA code before verifying the password — that tells a stranger which handles have 2FA on")
	}
	if strings.Index(login, "apiIssueSession") < gate {
		t.Error("apiV1Login issues the session before the 2FA gate")
	}
}

// TestTOTPVerify is the code check the gate rests on.
func TestTOTPVerify(t *testing.T) {
	secret, err := totpGenSecret()
	if err != nil {
		t.Fatal(err)
	}
	code, err := totpCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !totpVerify(secret, code) {
		t.Error("the current code was refused")
	}
	if totpVerify(secret, "000000") && code != "000000" {
		t.Error("a wrong code was accepted")
	}
	if totpVerify(secret, "") {
		t.Error("an empty code was accepted")
	}
}

// ── S3: the owner's own view ─────────────────────────────────────────────────

// TestMeDTOCarriesSettingsFacts: the settings screens are drawn from /me, so
// every fact they draw has to be in it — and none of them may be invented on
// the device.
func TestMeDTOCarriesSettingsFacts(t *testing.T) {
	submitted := time.Date(2026, 8, 1, 9, 15, 0, 0, time.UTC)
	u := &model.User{
		Handle: "dev", DisplayName: "Dev", Role: model.RoleUser,
		ContentSetting: "default", Tier: "free", ThemeID: "void",
		CountryCode:            "US",
		KYCTier:                model.KYCTierFull,
		BirthdayMdVisibility:   "followers",
		BirthdayYearVisibility: "only_me",
		SocialLinksRaw:         `{"youtube":"https://youtube.com/@dev","twitch":""}`,
		ExternalTipLinksRaw:    `{"bitcoin":"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"}`,
		PIALID:                 "c0ffee00-0000-4000-8000-000000000001",
	}
	dto := meDTO(u, meStanding{Unread: 3, TwoFA: true, HasPassword: true, KYCSubmittedAt: &submitted})

	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"has_password", "birthday_md_visibility", "birthday_year_visibility",
		"kyc_status", "kyc_submitted_at", "payout_enabled",
		"social_links", "external_tip_links", "country_code",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("/me does not carry %q — the settings screen would have to invent it", key)
		}
	}

	if dto.BirthdayMDVisibility != "followers" || dto.BirthdayYearVisibility != "only_me" {
		t.Errorf("birthday visibility not carried through: %q / %q", dto.BirthdayMDVisibility, dto.BirthdayYearVisibility)
	}
	if dto.KYCStatus == nil || *dto.KYCStatus != model.KYCTierFull {
		t.Errorf("kyc_status = %v, want %q", dto.KYCStatus, model.KYCTierFull)
	}
	if dto.KYCSubmittedAt == nil || !dto.KYCSubmittedAt.Equal(submitted) {
		t.Errorf("kyc_submitted_at = %v, want %v", dto.KYCSubmittedAt, submitted)
	}
	if !dto.PayoutEnabled {
		t.Error("a fully verified identity reads as payout_enabled false")
	}
	if dto.CountryCode == nil || *dto.CountryCode != "US" {
		t.Errorf("country_code = %v", dto.CountryCode)
	}
	if dto.SocialLinks["youtube"] != "https://youtube.com/@dev" {
		t.Errorf("social_links = %v", dto.SocialLinks)
	}
	if _, blank := dto.SocialLinks["twitch"]; blank {
		t.Error("an empty link is carried as a key — the client would draw an empty row")
	}
	if dto.ExternalTipLinks["bitcoin"] == "" {
		t.Errorf("external_tip_links = %v", dto.ExternalTipLinks)
	}

	// An account with no identity root and no links reads as nulls and empty
	// objects, never as a missing key.
	bare := meDTO(&model.User{Handle: "new", Role: model.RoleUser}, meStanding{})
	if bare.KYCStatus != nil || bare.KYCSubmittedAt != nil || bare.CountryCode != nil {
		t.Error("an unverified account reports an identity tier it does not have")
	}
	if bare.SocialLinks == nil || bare.ExternalTipLinks == nil {
		t.Error("the link maps are null rather than empty — the client cannot edit null")
	}
	if bare.BirthdayMDVisibility != "everyone" || bare.BirthdayYearVisibility != "only_me" {
		t.Errorf("birthday visibility defaults are %q / %q", bare.BirthdayMDVisibility, bare.BirthdayYearVisibility)
	}
	if bare.PayoutEnabled {
		t.Error("an unverified account reads as payout_enabled")
	}
}

// ── S2: the reads ────────────────────────────────────────────────────────────

// TestBlockListShape: two arrays, always present, never null. An empty list is
// an answer; a missing key is a decode failure on the phone.
func TestBlockListShape(t *testing.T) {
	raw, err := json.Marshal(BlockListDTO{Blocked: userDTOs(nil), Muted: userDTOs(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"blocked":[],"muted":[]}` {
		t.Errorf("empty block list encodes as %s", raw)
	}

	full := BlockListDTO{
		Blocked: userDTOs([]*model.User{{Handle: "spammer", DisplayName: "Spam", Role: model.RoleUser, ThemeID: "void"}}),
		Muted:   userDTOs([]*model.User{{Handle: "loud", DisplayName: "Loud", Role: model.RoleUser, ThemeID: "void"}}),
	}
	if len(full.Blocked) != 1 || full.Blocked[0].Handle != "spammer" {
		t.Errorf("blocked list = %+v", full.Blocked)
	}
	if len(full.Muted) != 1 || full.Muted[0].Handle != "loud" {
		t.Errorf("muted list = %+v", full.Muted)
	}
	// No account UUID and no PIAL crosses on a list of other people.
	body, _ := json.Marshal(full)
	for _, forbidden := range []string{"pial", "user_id", `"id"`} {
		if strings.Contains(strings.ToLower(string(body)), forbidden) {
			t.Errorf("the block list leaks %q: %s", forbidden, body)
		}
	}
}

// TestNotificationPrefsDefaults: an account with no Herald row has turned
// nothing off, and that is what it must read as.
func TestNotificationPrefsDefaults(t *testing.T) {
	d := (&heraldPrefs{}).dto()
	if !d.PushEnabled || !d.MessagesEnabled || !d.LikesEnabled || !d.RepostsEnabled ||
		!d.RepliesEnabled || !d.FollowsEnabled || !d.AchievementsEnabled ||
		!d.MentionsEnabled || !d.FrequenciesEnabled {
		t.Errorf("a preference defaults to off: %+v", d)
	}
	if d.QuietHoursEnabled {
		t.Error("quiet hours default to on")
	}
	if d.QuietHoursStart != nil || d.QuietHoursEnd != nil {
		t.Error("a quiet-hours bound is invented where Herald holds none")
	}

	// The bounds Herald holds as hours are shown as wall-clock strings.
	start, end := 23, 7
	d = (&heraldPrefs{QuietHoursStart: &start, QuietHoursEnd: &end}).dto()
	if d.QuietHoursStart == nil || *d.QuietHoursStart != "23:00" {
		t.Errorf("quiet_hours_start = %v", d.QuietHoursStart)
	}
	if d.QuietHoursEnd == nil || *d.QuietHoursEnd != "07:00" {
		t.Errorf("quiet_hours_end = %v", d.QuietHoursEnd)
	}
	bad := 99
	if (&heraldPrefs{QuietHoursStart: &bad}).dto().QuietHoursStart != nil {
		t.Error("an hour outside the clock is shown rather than dropped")
	}
}

// TestNotificationPrefsPatch: only the preferences the body names change, and a
// literal false is a change — the whole reason the toggles could not be turned
// off in the first place.
func TestNotificationPrefsPatch(t *testing.T) {
	body := `{"likes_enabled":false,"mentions_enabled":true,"quiet_hours_start":"23:30","quiet_hours_end":"07:00"}`
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	patch := notificationPrefsPatch(r, raw)

	if patch.LikesEnabled == nil || *patch.LikesEnabled {
		t.Error("likes_enabled:false did not reach the patch")
	}
	if patch.MentionsEnabled == nil || !*patch.MentionsEnabled {
		t.Error("mentions_enabled:true did not reach the patch")
	}
	if patch.PushEnabled != nil || patch.FollowsEnabled != nil {
		t.Error("a preference the body never named was changed")
	}
	if patch.QuietHoursStart == nil || *patch.QuietHoursStart != 23 {
		t.Errorf("quiet_hours_start = %v, want 23", patch.QuietHoursStart)
	}
	if patch.QuietHoursEnd == nil || *patch.QuietHoursEnd != 7 {
		t.Errorf("quiet_hours_end = %v, want 7", patch.QuietHoursEnd)
	}

	// Nothing named: nothing sent, so a Herald row is never overwritten with
	// defaults by an empty body.
	empty := notificationPrefsPatch(httptest.NewRequest(http.MethodPost, "/events", nil), map[string]json.RawMessage{})
	encoded, _ := json.Marshal(empty)
	if string(encoded) != "{}" {
		t.Errorf("an empty body sends %s to Herald", encoded)
	}
}

// TestParseHour reads the bound the clients send.
func TestParseHour(t *testing.T) {
	cases := map[string]struct {
		want int
		ok   bool
	}{
		"23:00": {23, true},
		"07:30": {7, true},
		"7":     {7, true},
		"00:00": {0, true},
		"24:00": {0, false},
		"":      {0, false},
		"noon":  {0, false},
		"-1":    {0, false},
	}
	for in, want := range cases {
		got, ok := parseHour(in)
		if got != want.want || ok != want.ok {
			t.Errorf("parseHour(%q) = (%d, %v), want (%d, %v)", in, got, ok, want.want, want.ok)
		}
	}
}

// ── S4: the profile links, normalised on the server ──────────────────────────

// TestTipLinkNormalisation: a payment link is stored in the one form the
// profile can render, or it is not stored. An address that does not validate
// sends money nowhere, so it never reaches the row.
func TestTipLinkNormalisation(t *testing.T) {
	in := map[string]string{
		"cashapp":      "$devcash",
		"venmo":        "https://venmo.com/devvenmo",
		"paypal":       "paypal.me/devpay",
		"bitcoin":      "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4",
		"xrp":          "rEb8TK3gBgk5auZkwc6sHnwrGVJH8DuaLh",
		"kofi":         "ko-fi.com/devkofi",
		"buymeacoffee": "https://buymeacoffee.com/devbmc",
	}
	got := normalizeTipLinks(func(k string) string { return in[k] })
	want := map[string]string{
		"cashapp": "devcash", "venmo": "devvenmo", "paypal": "devpay",
		"kofi": "devkofi", "buymeacoffee": "devbmc",
		"bitcoin": "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4",
		"xrp":     "rEb8TK3gBgk5auZkwc6sHnwrGVJH8DuaLh",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s normalised to %q, want %q", k, got[k], v)
		}
	}

	junk := normalizeTipLinks(func(k string) string {
		switch k {
		case "bitcoin":
			return "not-an-address"
		case "xrp":
			return "0xdeadbeef"
		}
		return ""
	})
	if junk["bitcoin"] != "" || junk["xrp"] != "" {
		t.Errorf("an invalid crypto address was stored: %+v", junk)
	}
}

// TestLinkGroupReader: a JSON client sends one object per group, the web form
// sends flat fields, and a body that names neither leaves the group alone.
func TestLinkGroupReader(t *testing.T) {
	var raw map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"social_links":{"youtube":"https://youtube.com/@dev"}}`), &raw)
	r := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader("{}"))
	get, ok := linkGroupReader(r, raw, "social_links", "social_")
	if !ok {
		t.Fatal("a JSON social_links object was not read")
	}
	if get("youtube") != "https://youtube.com/@dev" {
		t.Errorf("youtube = %q", get("youtube"))
	}
	if _, ok := linkGroupReader(r, raw, "external_tip_links", "tip_"); ok {
		t.Error("a group the body never named was rewritten")
	}

	form := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader("tip_cashapp=%24devcash"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = form.ParseForm()
	get, ok = linkGroupReader(form, nil, "external_tip_links", "tip_")
	if !ok {
		t.Fatal("the form's tip_ fields were not read")
	}
	if get("cashapp") != "$devcash" {
		t.Errorf("cashapp = %q", get("cashapp"))
	}
}

// TestDecodeLinkMap: an unreadable or empty profile column is an empty object,
// never null and never a decode error.
func TestDecodeLinkMap(t *testing.T) {
	if m := decodeLinkMap(""); m == nil || len(m) != 0 {
		t.Errorf("empty raw decoded to %v", m)
	}
	if m := decodeLinkMap("not json"); m == nil || len(m) != 0 {
		t.Errorf("junk decoded to %v", m)
	}
	m := decodeLinkMap(`{"youtube":"u","twitch":"  "}`)
	if m["youtube"] != "u" {
		t.Errorf("youtube = %q", m["youtube"])
	}
	if _, ok := m["twitch"]; ok {
		t.Error("a blank link survived")
	}
}
