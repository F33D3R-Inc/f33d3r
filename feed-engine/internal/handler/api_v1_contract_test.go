package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// The /api/v1 contract has two authors: the Go DTOs in api_v1_dto.go and the
// Swift models in mobile/f33d3r_iOS/F33D3RKit. Neither side can be tested
// against the other on one machine — the Swift tests run on a Mac — so the
// Swift side's contract is read here as text: every Codable model's coding
// keys and which of them are optional. A key the Swift model requires that
// the Go DTO never emits is a decode failure on every phone; a key the Go DTO
// emits that no Swift model reads is drift. Both fail here, before a build.

var (
	swiftModelsDir  = filepath.Join("..", "..", "..", "mobile", "f33d3r_iOS", "F33D3RKit", "Sources", "F33D3RKit", "Models")
	swiftFixtureDir = filepath.Join("..", "..", "..", "mobile", "f33d3r_iOS", "F33D3RKit", "Tests", "F33D3RKitTests", "Fixtures")
	swiftNetworkDir = filepath.Join("..", "..", "..", "mobile", "f33d3r_iOS", "F33D3RKit", "Sources", "F33D3RKit", "Networking")
)

// swiftModel is one `struct X: Codable` as the Swift file declares it.
type swiftModel struct {
	keys     map[string]bool // JSON key -> required (non-optional)
	hasCoded bool
}

var (
	// A struct inside a `public extension` carries no modifier of its own
	// (APIClient.SignupRequest), so the modifier is optional.
	reSwiftStruct = regexp.MustCompile(`(?m)^\s*(?:public\s+)?struct (\w+)\b[^{]*\{`)
	reSwiftProp   = regexp.MustCompile(`(?m)^\s*public (?:let|var) (\w+)\s*:\s*([^\n{=]+?)\s*$`)
	reCodingKeys  = regexp.MustCompile(`enum CodingKeys\b[^{]*\{`)
	reCaseAssign  = regexp.MustCompile(`^(\w+)\s*=\s*"([^"]+)"$`)
)

// braceBody returns the text between the brace that ends at src[open] and
// its matching close brace.
func braceBody(src string, open int) string {
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[open+1 : i]
			}
		}
	}
	return src[open+1:]
}

// withoutNestedStructs cuts nested `public struct` declarations out of a
// struct body, so a parent's coding keys are its own (LiveStream.Tipper).
func withoutNestedStructs(body string) string {
	for {
		m := reSwiftStruct.FindStringIndex(body)
		if m == nil {
			return body
		}
		inner := braceBody(body, m[1]-1)
		end := m[1] + len(inner) + 1
		if end > len(body) {
			end = len(body)
		}
		body = body[:m[0]] + body[end:]
	}
}

func loadSwiftModels(t *testing.T) map[string]swiftModel {
	t.Helper()
	return loadSwiftModelsFrom(t, swiftModelsDir)
}

// loadSwiftModelsFrom reads every `public struct` with coding keys under dir:
// the Models directory for what the phone decodes, the Networking directory
// for the request bodies it encodes.
func loadSwiftModelsFrom(t *testing.T, dir string) map[string]swiftModel {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("Swift sources not found at %s: %v", dir, err)
	}
	models := map[string]swiftModel{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".swift") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		src := string(raw)
		for _, m := range reSwiftStruct.FindAllStringSubmatchIndex(src, -1) {
			name := src[m[2]:m[3]]
			body := withoutNestedStructs(braceBody(src, m[1]-1))
			// Property name -> optional?
			optional := map[string]bool{}
			var order []string
			for _, pm := range reSwiftProp.FindAllStringSubmatch(body, -1) {
				optional[pm[1]] = strings.HasSuffix(strings.TrimSpace(pm[2]), "?")
				order = append(order, pm[1])
			}
			model := swiftModel{keys: map[string]bool{}}
			if ck := reCodingKeys.FindStringIndex(body); ck != nil {
				model.hasCoded = true
				for _, line := range strings.Split(braceBody(body, ck[1]-1), "\n") {
					line = strings.TrimSpace(line)
					if !strings.HasPrefix(line, "case ") {
						continue
					}
					for _, item := range strings.Split(strings.TrimPrefix(line, "case "), ",") {
						item = strings.TrimSpace(item)
						if item == "" {
							continue
						}
						prop, key := item, item
						if am := reCaseAssign.FindStringSubmatch(item); am != nil {
							prop, key = am[1], am[2]
						}
						model.keys[key] = !optional[prop]
					}
				}
			} else {
				for _, prop := range order {
					model.keys[prop] = !optional[prop]
				}
			}
			if len(model.keys) > 0 {
				models[name] = model
			}
		}
	}
	return models
}

// fillValue sets every field of v to a non-zero value so that no omitempty
// key is missing from the encoding: the DTO's full key set is what is
// compared, not one particular row's.
func fillValue(v reflect.Value) {
	fillValueOnPath(v, map[reflect.Type]bool{})
}

// fillValueOnPath is fillValue carrying the struct types being filled on the
// way down. A DTO that points at its own type — QuotedWorkDTO.Nested, the next
// level of a quote chain — would otherwise be filled forever. A pointer whose
// target type is already on the path is set to an empty value and not entered:
// its key is still emitted, which is all the comparison needs, and the filling
// ends the way the real chain does, one level in.
func fillValueOnPath(v reflect.Value, path map[reflect.Type]bool) {
	switch v.Kind() {
	case reflect.Ptr:
		v.Set(reflect.New(v.Type().Elem()))
		if path[v.Type().Elem()] {
			return
		}
		fillValueOnPath(v.Elem(), path)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)))
			return
		}
		path[v.Type()] = true
		defer delete(path, v.Type())
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fillValueOnPath(v.Field(i), path)
			}
		}
	case reflect.Slice:
		el := reflect.New(v.Type().Elem()).Elem()
		fillValueOnPath(el, path)
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), el))
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64, reflect.Int32:
		v.SetInt(1)
	case reflect.Float64, reflect.Float32:
		v.SetFloat(1.5)
	}
}

func jsonKeys(t *testing.T, v interface{}) map[string]bool {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%T does not encode as an object: %v", v, err)
	}
	keys := map[string]bool{}
	for k := range m {
		keys[k] = true
	}
	return keys
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestAPIV1_SwiftModelKeys is the contract itself. For each Swift model the
// clients decode, the Go DTO it is decoded from must emit every key the
// model declares and nothing the model does not.
func TestAPIV1_SwiftModelKeys(t *testing.T) {
	models := loadSwiftModels(t)

	// Keys the Go side emits that the Swift model does not declare: allowed
	// only when named here, with the reason.
	extra := map[string]map[string]bool{
		// The PIAL the device signs works as. Read by the signing layer
		// (Malkuth), not by the CurrentUser model.
		"CurrentUser": {"pial_id": true},
	}

	pairs := []struct {
		swift string
		dto   interface{}
	}{
		{"User", &UserDTO{}},
		{"CurrentUser", &MeDTO{}},
		{"Session", &SessionDTO{}},
		{"WorkPage", &WorkPageDTO{}},
		{"WorkThread", &WorkThreadDTO{}},
		{"Profile", &ProfileDTO{}},
		{"Work", &WorkDTO{}},
		{"WorkAuthor", &WorkAuthorDTO{}},
		{"WorkVideo", &VideoDTO{}},
		{"WorkVoice", &VoiceDTO{}},
		{"Poll", &PollDTO{}},
		{"PollResult", &PollResultDTO{}},
		{"QuotedWork", &QuotedWorkDTO{}},
		{"LinkPreview", &LinkPreviewDTO{}},
		{"WorkProvenance", &ProvenanceDTO{}},
		{"NotificationItem", &NotificationDTO{}},
		{"NotificationPage", &NotificationPageDTO{}},
		{"SearchResults", &SearchDTO{}},
		{"WalletSnapshot", &WalletDTO{}},
		{"WalletEntry", &WalletEntryDTO{}},
		{"TagCount", &TagDTO{}},
		{"UserPage", &UserPageDTO{}},
		{"SessionInfo", &SessionInfoDTO{}},
		{"VisionArtboard", &VisionArtboardDTO{}},
		{"Vision", &VisionDTO{}},
		{"VisionRing", &VisionRingDTO{}},
		{"VisionTray", &VisionTrayDTO{}},
		{"LiveStream", &LiveStreamDTO{}},
		{"Tipper", &TipperDTO{}},
		{"LiveChatMessage", &LiveChatDTO{}},
		{"LiveRoom", &LiveRoomDTO{}},
		{"LiveList", &LiveListDTO{}},
		{"LiveSummary", &LiveSummaryDTO{}},
		// Frequencies — the live audio rooms. The Swift side is
		// Models/Frequency.swift; FrequencyDraft is a request struct with no
		// DTO behind it and is deliberately absent from this list.
		{"Frequency", &FrequencySummaryDTO{}},
		{"FrequencyParticipant", &FrequencyParticipantDTO{}},
		{"FrequencyRequest", &FrequencyRequestDTO{}},
		{"FrequencyViewer", &FrequencyViewerDTO{}},
		{"FrequencyRoom", &FrequencyRoomDTO{}},
		{"FrequencyList", &FrequencyListDTO{}},
		{"Conversation", &ConversationDTO{}},
		{"ChatMessage", &MessageDTO{}},
		{"ConversationMember", &ThreadMemberDTO{}},
		{"ConversationPage", &ConversationPageDTO{}},
		{"MessageThread", &ThreadDTO{}},
		{"ContactNumber", &NumberDTO{}},
		{"NumbersPage", &NumbersDTO{}},
		{"ContactRequest", &ContactRequestDTO{}},
		{"ContactRequestPage", &ContactRequestsDTO{}},
		{"CashtagQuote", &CashtagQuoteDTO{}},
		{"CashtagSuggestion", &CashtagSuggestionDTO{}},
		// The upload lane (api_v1_media.go): the app's composer decodes these
		// for video, voice and GIF attachments.
		{"VideoJob", &VideoJobDTO{}},
		{"VoiceUpload", &VoiceUploadDTO{}},
		{"GifSearchPage", &GifSearchDTO{}},
		{"GifResult", &GifResultDTO{}},
		// The settings surface (api_v1_account.go).
		{"BlockList", &BlockListDTO{}},
		{"NotificationPrefs", &NotificationPrefsDTO{}},
		{"TwoFactorSetup", &TwoFactorSetupDTO{}},
		{"SportsTeam", &SportsTeamDTO{}},
		{"SportsGame", &SportsGameDTO{}},
		{"SportsLeague", &SportsLeagueDTO{}},
		{"SportsBoard", &SportsBoardDTO{}},
	}
	// A model whose init(from:) decodes another model from the same flat
	// object reads that model's keys too: MeDTO embeds UserDTO.
	flattens := map[string]string{"CurrentUser": "User"}

	for _, p := range pairs {
		model, ok := models[p.swift]
		if !ok {
			t.Errorf("Swift model %s not found under %s (known: %v)", p.swift, swiftModelsDir, sortedKeys(namesOf(models)))
			continue
		}
		declared := map[string]bool{}
		for k, required := range model.keys {
			declared[k] = required
		}
		if base, ok := flattens[p.swift]; ok {
			for k, required := range models[base].keys {
				declared[k] = required
			}
		}
		fillValue(reflect.ValueOf(p.dto).Elem())
		got := jsonKeys(t, p.dto)
		for key, required := range declared {
			if !got[key] {
				kind := "optional"
				if required {
					kind = "REQUIRED"
				}
				t.Errorf("%s: Swift model reads %s key %q; %T never emits it", p.swift, kind, key, p.dto)
			}
		}
		for key := range got {
			if _, declaredKey := declared[key]; !declaredKey && !extra[p.swift][key] {
				t.Errorf("%s: %T emits %q, which the Swift model does not declare", p.swift, p.dto, key)
			}
		}
	}
}

func namesOf(m map[string]swiftModel) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

// TestAPIV1_EnvelopeKeys pins the answers that wrap a list or a receipt in
// one object. The phone decodes these through one-line private structs inside
// APIClient+Reads.swift and the stores, not through Models/, so their keys
// are named here. A Go key the phone does not read is allowed only when
// listed under extra; a key the phone reads that Go does not emit fails.
func TestAPIV1_EnvelopeKeys(t *testing.T) {
	cases := []struct {
		dto   interface{}
		reads []string // what the Swift decoder declares
		extra []string // emitted, not yet read on the phone
	}{
		{&MediaUploadDTO{}, []string{"url"}, []string{"bytes"}},       // APIClient+Reads.swift `Uploaded`
		{&TrendingTagsDTO{}, []string{"tags"}, nil},                   // `Page{tags}`
		{&SessionListDTO{}, []string{"sessions"}, nil},                // `Page{sessions}`
		{&CashtagSearchDTO{}, []string{"results"}, nil},               // `Page{results}`
		{&VisionViewersDTO{}, []string{"viewers"}, []string{"count"}}, // VisionViewerView `{viewers}`
		{&FrequencyEndedDTO{}, []string{"reason"}, nil},               // frequency_ended frame (no client yet)
		{&FrequencyHeartbeatDTO{}, []string{"state", "present", "role", "muted", "counts"}, nil},
		{&FrequencyCountsDTO{}, []string{"listeners", "speakers", "participants"}, nil},
	}
	for _, c := range cases {
		fillValue(reflect.ValueOf(c.dto).Elem())
		got := jsonKeys(t, c.dto)
		allowed := map[string]bool{}
		for _, k := range c.reads {
			if !got[k] {
				t.Errorf("%T: the phone reads %q; it is never emitted", c.dto, k)
			}
			allowed[k] = true
		}
		for _, k := range c.extra {
			allowed[k] = true
		}
		for k := range got {
			if !allowed[k] {
				t.Errorf("%T emits %q, which no client reads and no allowance names", c.dto, k)
			}
		}
	}
}

// TestAPIV1_SignupRequestKeys is the request half of the auth contract: every
// key the phone's SignupRequest encodes is one apiV1Signup reads, so nothing
// a person types is silently dropped. The other direction — that the phone
// sends what createAccount requires (terms_accepted, date_of_birth) — is a
// decode of the server's refusal on the phone and is checked by the Swift
// tests on the Mac, where the form lives.
func TestAPIV1_SignupRequestKeys(t *testing.T) {
	models := loadSwiftModelsFrom(t, swiftNetworkDir)
	model, ok := models["SignupRequest"]
	if !ok {
		t.Fatalf("SignupRequest not found under %s (known: %v)", swiftNetworkDir, sortedKeys(namesOf(models)))
	}
	accepted := map[string]bool{}
	rt := reflect.TypeOf(apiSignupRequest{})
	for i := 0; i < rt.NumField(); i++ {
		tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			accepted[tag] = true
		}
	}
	for key := range model.keys {
		if !accepted[key] {
			t.Errorf("SignupRequest encodes %q; apiV1Signup never reads it", key)
		}
	}
	for _, required := range []string{"handle", "password", "terms_accepted", "date_of_birth"} {
		if !accepted[required] {
			t.Errorf("apiSignupRequest lost %q, which createAccount requires", required)
		}
	}
}

// TestAPIV1_GoldenFixtures checks the DTOs against the JSON the Swift tests
// decode: every fixture key is a DTO key, and a fixture survives a decode
// through the DTO and back with every value intact, which is what proves the
// types agree and not only the names.
func TestAPIV1_GoldenFixtures(t *testing.T) {
	cases := []struct {
		fixture string
		dto     interface{}
	}{
		{"user.json", &UserDTO{}},
		{"me.json", &MeDTO{}},
		{"session.json", &SessionDTO{}},
		{"work_reaction.json", &WorkDTO{}},
		// The two Frequencies goldens the Swift tests decode. A room and a
		// lane, each carrying a null in every nullable position, so the
		// fixture proves the types agree and not only the names.
		{"frequency_room.json", &FrequencyRoomDTO{}},
		{"frequency_list.json", &FrequencyListDTO{}},
		// The settings reads.
		{"block_list.json", &BlockListDTO{}},
		{"notification_prefs.json", &NotificationPrefsDTO{}},
		{"two_factor_setup.json", &TwoFactorSetupDTO{}},
	}
	for _, c := range cases {
		raw, err := os.ReadFile(filepath.Join(swiftFixtureDir, c.fixture))
		if err != nil {
			t.Fatalf("fixture: %v", err)
		}
		var want map[string]json.RawMessage
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatalf("%s: %v", c.fixture, err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(c.dto); err != nil {
			t.Errorf("%s does not decode into %T: %v", c.fixture, c.dto, err)
			continue
		}
		got := jsonKeys(t, c.dto)
		for k := range want {
			if !got[k] {
				t.Errorf("%s: key %q is not emitted by %T", c.fixture, k, c.dto)
			}
		}
		// Value round trip, compared as canonical JSON per key.
		re, _ := json.Marshal(c.dto)
		var back map[string]json.RawMessage
		json.Unmarshal(re, &back)
		for k, w := range want {
			if !sameJSON(w, back[k]) {
				t.Errorf("%s: key %q changed through %T: fixture %s, emitted %s", c.fixture, k, c.dto, w, back[k])
			}
		}
	}

	// The error envelope.
	raw, err := os.ReadFile(filepath.Join(swiftFixtureDir, "error.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	apiError(rec, http.StatusUnauthorized, "invalid_credentials", "Invalid handle or password.")
	if !sameJSON(raw, rec.Body.Bytes()) {
		t.Errorf("error envelope: fixture %s, emitted %s", raw, rec.Body.String())
	}
}

func sameJSON(a, b []byte) bool {
	var x, y interface{}
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// TestAPIV1_BearerSession: the same token the cookie carries is accepted from
// the Authorization header, the header outranks a leftover cookie, and the
// legacy handle sentinel is never accepted as a bearer.
func TestAPIV1_BearerSession(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer tok-header")
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "tok-cookie"})
	if got := GetSessionToken(r); got != "tok-header" {
		t.Errorf("header should outrank cookie, got %q", got)
	}
	r = httptest.NewRequest("GET", "/api/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "tok-cookie"})
	if got := GetSessionToken(r); got != "tok-cookie" {
		t.Errorf("cookie alone, got %q", got)
	}
	for _, bad := range []string{"Basic abc", "Bearer", "Bearer ", "Bearer __handle__dev", "bearer " + strings.Repeat("x", 201)} {
		r = httptest.NewRequest("GET", "/api/v1/me", nil)
		r.Header.Set("Authorization", bad)
		if got := GetSessionToken(r); got != "" {
			t.Errorf("%q should not yield a token, got %q", bad, got)
		}
	}
	r = httptest.NewRequest("GET", "/api/v1/me", nil)
	r.Header.Set("Authorization", "bearer lower-case-scheme")
	if got := GetSessionToken(r); got != "lower-case-scheme" {
		t.Errorf("scheme is case-insensitive, got %q", got)
	}
}

// TestAPIV1_EventFields: one event case reads the same parameter from a form
// body or a JSON body, by any of its accepted names, and a JSON number is a
// value too.
func TestAPIV1_EventFields(t *testing.T) {
	form := httptest.NewRequest("POST", "/events", strings.NewReader("event_type=poll_vote&post_id=abc&option_idx=2"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := eventField(form, nil, "work_id", "post_id"); got != "abc" {
		t.Errorf("form fallback name: %q", got)
	}
	if n, ok := eventIntField(form, nil, "option_idx"); !ok || n != 2 {
		t.Errorf("form int: %d %v", n, ok)
	}

	var raw map[string]json.RawMessage
	json.Unmarshal([]byte(`{"event_type":"poll_vote","work_id":"w1","option_idx":3,"reason":" spam "}`), &raw)
	js := httptest.NewRequest("POST", "/events", strings.NewReader("{}"))
	js.Header.Set("Content-Type", "application/json")
	if got := eventField(js, raw, "post_id", "work_id"); got != "w1" {
		t.Errorf("json second name: %q", got)
	}
	if n, ok := eventIntField(js, raw, "option_idx"); !ok || n != 3 {
		t.Errorf("json number: %d %v", n, ok)
	}
	if got := eventField(js, raw, "reason"); got != "spam" {
		t.Errorf("trimmed: %q", got)
	}
	if _, ok := eventIntField(js, raw, "missing"); ok {
		t.Error("missing field reported present")
	}
}

func TestAPIV1_ImageExtension(t *testing.T) {
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBP"), make([]byte, 4)...)
	cases := map[string][]byte{
		".jpg":  append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 12)...),
		".png":  append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, make([]byte, 8)...),
		".gif":  append([]byte("GIF89a"), make([]byte, 8)...),
		".webp": webp,
		"":      []byte("<!doctype html><html>"),
	}
	for want, data := range cases {
		if got := imageExtensionFor(data); got != want {
			t.Errorf("%q: got %q", want, got)
		}
	}
	if got := imageExtensionFor([]byte{0xFF, 0xD8}); got != "" {
		t.Errorf("short input: %q", got)
	}
}
