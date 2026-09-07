package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"f33d3r.com/ios/devserver/internal/store"
)

// harness is a seeded in-memory server.
type harness struct {
	t     *testing.T
	srv   *httptest.Server
	store *store.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedIfEmpty(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedFrequencies(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, t.TempDir()).Handler())
	t.Cleanup(func() { srv.Close(); st.Close() })
	return &harness{t: t, srv: srv, store: st}
}

func (h *harness) do(method, path, token string, body any, contentType string) (int, []byte) {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
		if contentType == "" {
			contentType = "application/json"
		}
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func (h *harness) login(handle string) (token string, me map[string]any) {
	h.t.Helper()
	status, body := h.do("POST", "/api/v1/auth/login", "", map[string]string{
		"handle": handle, "password": store.DevPassword, "device_name": "test",
	}, "")
	if status != 200 {
		h.t.Fatalf("login %s: %d %s", handle, status, body)
	}
	var session struct {
		Token string         `json:"token"`
		User  map[string]any `json:"user"`
	}
	json.Unmarshal(body, &session)
	return session.Token, session.User
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestFixtureKeySets: every key the golden fixtures carry must be present in
// the live response, so a Swift `decode` that the fixture proves works also
// works against this server.
func TestFixtureKeySets(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login("dev")

	check := func(name string, live map[string]any, allowedExtra ...string) {
		t.Helper()
		var fixture map[string]any
		readFixture(t, name+".json", &fixture)
		// Optional strings are omitted when empty; the Swift side decodes them
		// with decodeIfPresent, so absence is within the contract.
		for _, k := range []string{"avatar_url", "header_url", "bio", "pronouns", "location", "website", "official_type", "theme_id", "accent_hex"} {
			if _, ok := live[k]; !ok {
				delete(fixture, k)
			}
		}
		for k := range fixture {
			if _, ok := live[k]; !ok {
				t.Errorf("%s: live response lacks key %q", name, k)
			}
		}
		extra := map[string]bool{}
		for _, k := range allowedExtra {
			extra[k] = true
		}
		for k := range live {
			if _, ok := fixture[k]; !ok && !extra[k] {
				t.Errorf("%s: live response has key %q the fixture does not", name, k)
			}
		}
	}

	status, body := h.do("GET", "/api/v1/me", token, nil, "")
	if status != 200 {
		t.Fatalf("me: %d %s", status, body)
	}
	var me map[string]any
	json.Unmarshal(body, &me)
	check("me", me, "pial_id")

	status, body = h.do("POST", "/api/v1/auth/login", "", map[string]string{"handle": "dev", "password": store.DevPassword}, "")
	var session map[string]any
	json.Unmarshal(body, &session)
	check("session", session)
	check("me", session["user"].(map[string]any), "pial_id")

	status, body = h.do("GET", "/api/v1/users/dev", token, nil, "")
	if status != 200 {
		t.Fatalf("profile: %d %s", status, body)
	}
	var profile map[string]any
	json.Unmarshal(body, &profile)
	check("user", profile["user"].(map[string]any))

	status, body = h.do("POST", "/api/v1/auth/login", "", map[string]string{"handle": "dev", "password": "wrong"}, "")
	if status != 401 {
		t.Fatalf("bad login: %d", status)
	}
	var errBody map[string]any
	json.Unmarshal(body, &errBody)
	check("error", errBody)
}

var uuidRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// TestNoIdentityLeaks: no response other than the owner's own /me may carry a
// PIAL, and nothing may carry the ranking vocabulary.
func TestNoIdentityLeaks(t *testing.T) {
	h := newHarness(t)
	token, me := h.login("dev")
	ownPIAL, _ := me["pial_id"].(string)
	if ownPIAL == "" {
		t.Fatal("/me must carry pial_id for signing")
	}

	rows, err := h.store.DB().Query(`SELECT pial_id FROM pial_roots`)
	if err != nil {
		t.Fatal(err)
	}
	var pials []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		pials = append(pials, p)
	}
	rows.Close()

	for _, path := range []string{
		"/api/v1/feed?surface=following", "/api/v1/feed?surface=foryou", "/api/v1/feed?surface=trending",
		"/api/v1/feed?surface=music", "/api/v1/feed?surface=visions", "/api/v1/feed?surface=live",
		"/api/v1/users/miiyazuko", "/api/v1/users/miiyazuko/works", "/api/v1/notifications", "/api/v1/wallet",
		"/api/v1/search?q=roll",
		"/api/v1/frequencies?lane=live", "/api/v1/frequencies?lane=scheduled", "/api/v1/frequencies?lane=mine",
	} {
		status, body := h.do("GET", path, token, nil, "")
		if status != 200 {
			t.Errorf("%s: %d %s", path, status, body)
			continue
		}
		lower := strings.ToLower(string(body))
		for _, forbidden := range []string{"jung", "archetype", "aesq", "shadow", "pial"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("%s: response contains %q", path, forbidden)
			}
		}
		for _, p := range pials {
			if strings.Contains(lower, p) {
				t.Errorf("%s: response contains a PIAL", path)
			}
		}
	}
}

// signer is a test device key.
type signer struct{ key *ecdsa.PrivateKey }

func newSigner(t *testing.T) *signer {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{key: k}
}

func (s *signer) spkiB64() string {
	der, _ := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
	return base64.StdEncoding.EncodeToString(der)
}

func (s *signer) sign(cid string) string {
	digest := sha256.Sum256([]byte(cid))
	r, sg, _ := ecdsa.Sign(rand.Reader, s.key, digest[:])
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	sg.FillBytes(out[32:])
	return base64.RawURLEncoding.EncodeToString(out)
}

// envelope builds the POST /events body the Swift WorkEnvelope produces.
func envelope(t *testing.T, s *signer, p workCanonicalPayload, extra map[string]any) []byte {
	canonical, err := canonicalBytes(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	cid := fmt.Sprintf("sha256:%x", sum)
	outer := map[string]any{"event_type": p.Kind, "cid": cid, "signature": s.sign(cid), "is_nsfw": false}
	for k, v := range extra {
		outer[k] = v
	}
	raw, _ := json.Marshal(outer)
	// Splice the canonical bytes in verbatim, as the client does.
	raw = raw[:len(raw)-1]
	raw = append(raw, []byte(`,"payload":`)...)
	raw = append(raw, canonical...)
	raw = append(raw, '}')
	return raw
}

// TestWriteLane exercises the whole write path the app uses: register a key,
// post, see it in the feed, like it, reply, tip, and read the notifications
// and wallet on the other side.
func TestWriteLane(t *testing.T) {
	h := newHarness(t)
	devToken, devMe := h.login("dev")
	devPIAL := devMe["pial_id"].(string)
	miiToken, miiMe := h.login("miiyazuko")
	miiPIAL := miiMe["pial_id"].(string)

	dev := newSigner(t)
	payload := workCanonicalPayload{
		AuthorPIAL: devPIAL, Body: "hello from the test #hello", CommentGating: "everyone", Kind: "post",
		MediaURLs: []string{}, Tags: []string{}, TimestampMS: time.Now().UnixMilli(),
	}

	// Unregistered key: the signature cannot verify.
	status, body := h.do("POST", "/events", devToken, envelope(t, dev, payload, nil), "application/json")
	if status != 403 || !strings.Contains(string(body), "signature invalid") {
		t.Fatalf("unregistered key: %d %s", status, body)
	}

	status, body = h.do("POST", "/api/pial/signing-key/register", devToken,
		map[string]string{"public_key_b64": dev.spkiB64(), "algorithm": "ECDSA-P256"}, "")
	if status != 200 {
		t.Fatalf("register: %d %s", status, body)
	}

	status, body = h.do("POST", "/events", devToken, envelope(t, dev, payload, nil), "application/json")
	if status != 201 {
		t.Fatalf("post: %d %s", status, body)
	}
	var accepted workAccepted
	json.Unmarshal(body, &accepted)
	if accepted.WorkID == "" {
		t.Fatal("no work_id")
	}

	// Same bytes again: not a second work.
	status, body = h.do("POST", "/events", devToken, envelope(t, dev, payload, nil), "application/json")
	if status != 200 {
		t.Fatalf("duplicate post: %d %s", status, body)
	}
	var again workAccepted
	json.Unmarshal(body, &again)
	if again.WorkID != accepted.WorkID {
		t.Fatalf("duplicate produced a different work")
	}

	// Tampered body: cid mismatch.
	tampered := bytes.Replace(envelope(t, dev, payload, nil), []byte("hello from"), []byte("HELLO from"), 1)
	status, body = h.do("POST", "/events", devToken, tampered, "application/json")
	if status != 400 || !strings.Contains(string(body), "cid mismatch") {
		t.Fatalf("tampered: %d %s", status, body)
	}

	// Mii follows dev (seeded), so the work is on her Following lane, with the
	// hashtag the server derived.
	status, body = h.do("GET", "/api/v1/feed?surface=following", miiToken, nil, "")
	if status != 200 {
		t.Fatalf("feed: %d %s", status, body)
	}
	var page WorkPageDTO
	json.Unmarshal(body, &page)
	var found *WorkDTO
	for i := range page.Works {
		if page.Works[i].ID == accepted.WorkID {
			found = &page.Works[i]
		}
	}
	if found == nil {
		t.Fatalf("posted work not in following feed: %s", body)
	}
	if len(found.Tags) != 1 || found.Tags[0] != "hello" {
		t.Errorf("tags = %v", found.Tags)
	}
	if found.Provenance == nil || found.Provenance.Kind != "you_follow" {
		t.Errorf("provenance = %+v", found.Provenance)
	}

	// Like as a form post, like the web; then as JSON.
	status, _ = h.do("POST", "/events", miiToken, "event_type=work_like&work_id="+accepted.WorkID, "application/x-www-form-urlencoded")
	if status != 204 {
		t.Fatalf("like: %d", status)
	}
	status, body = h.do("GET", "/api/v1/works/"+accepted.WorkID, miiToken, nil, "")
	var thread WorkThreadDTO
	json.Unmarshal(body, &thread)
	if !thread.Work.LikedByViewer || thread.Work.LikeCount != 1 {
		t.Errorf("after like: liked=%v count=%d", thread.Work.LikedByViewer, thread.Work.LikeCount)
	}

	// Reply from Mii.
	mii := newSigner(t)
	h.do("POST", "/api/pial/signing-key/register", miiToken, map[string]string{"public_key_b64": mii.spkiB64()}, "")
	reply := workCanonicalPayload{
		AuthorPIAL: miiPIAL, Body: "welcome @dev", CommentGating: "everyone", Kind: "reply",
		ParentCID: accepted.CID, MediaURLs: []string{}, Tags: []string{}, TimestampMS: time.Now().UnixMilli(),
	}
	status, body = h.do("POST", "/events", miiToken, envelope(t, mii, reply, nil), "application/json")
	if status != 201 {
		t.Fatalf("reply: %d %s", status, body)
	}
	status, body = h.do("GET", "/api/v1/works/"+accepted.WorkID, devToken, nil, "")
	json.Unmarshal(body, &thread)
	if len(thread.Replies) != 1 || thread.Work.ReplyCount != 1 {
		t.Errorf("replies = %d count = %d", len(thread.Replies), thread.Work.ReplyCount)
	}
	if thread.Replies[0].ParentCID == nil || *thread.Replies[0].ParentCID != accepted.CID {
		t.Errorf("reply parent_cid = %v", thread.Replies[0].ParentCID)
	}

	// Tip 1.25 AET from Mii to dev on the work.
	status, body = h.do("POST", "/events", miiToken,
		"event_type=tip&target_handle=dev&amount_aet=125&work_id="+accepted.WorkID, "application/x-www-form-urlencoded")
	if status != 204 {
		t.Fatalf("tip: %d %s", status, body)
	}
	status, body = h.do("GET", "/api/v1/works/"+accepted.WorkID, devToken, nil, "")
	json.Unmarshal(body, &thread)
	if thread.Work.TipTotalUAET == nil || *thread.Work.TipTotalUAET != 1_250_000 {
		t.Errorf("tip_total_uaet = %v", thread.Work.TipTotalUAET)
	}

	// Dev's inbox has the like, the reply and the tip; dev's wallet has the credit.
	status, body = h.do("GET", "/api/v1/notifications", devToken, nil, "")
	var inbox NotificationPageDTO
	json.Unmarshal(body, &inbox)
	kinds := map[string]bool{}
	for _, n := range inbox.Notifications {
		kinds[n.Kind] = true
	}
	for _, want := range []string{"like", "reply", "tip"} {
		if !kinds[want] {
			t.Errorf("inbox lacks %s: %s", want, body)
		}
	}
	// The reply mentioned @dev, who is also the parent's author: one
	// notification for the reply, never a second for the mention.
	if kinds["mention"] {
		t.Errorf("parent author was double-notified: %s", body)
	}
	// A mention of someone not otherwise involved does notify them.
	mention := workCanonicalPayload{
		AuthorPIAL: devPIAL, Body: "hey @guest look at this", CommentGating: "everyone", Kind: "post",
		MediaURLs: []string{}, Tags: []string{}, TimestampMS: time.Now().UnixMilli() + 1,
	}
	if status, body := h.do("POST", "/events", devToken, envelope(t, dev, mention, nil), "application/json"); status != 201 {
		t.Fatalf("mention post: %d %s", status, body)
	}
	guestToken, _ := h.login("guest")
	_, body = h.do("GET", "/api/v1/notifications", guestToken, nil, "")
	if !strings.Contains(string(body), `"kind":"mention"`) {
		t.Errorf("guest inbox lacks the mention: %s", body)
	}
	status, body = h.do("GET", "/api/v1/wallet", devToken, nil, "")
	var wallet WalletDTO
	json.Unmarshal(body, &wallet)
	// Seeded 50 AET, minus the seeded 2.5 tip to Mii, plus this 1.25.
	if wallet.BalanceUAET != 50*store.UAETPerAET-2_500_000+1_250_000 {
		t.Errorf("balance = %d", wallet.BalanceUAET)
	}

	// Mark the like read; unread count falls.
	before := inbox.UnreadCount
	status, _ = h.do("POST", "/events", devToken, map[string]string{"event_type": "notif_read", "notif_id": inbox.Notifications[0].ID}, "")
	if status != 204 {
		t.Fatalf("notif_read: %d", status)
	}
	_, body = h.do("GET", "/api/v1/notifications", devToken, nil, "")
	json.Unmarshal(body, &inbox)
	if inbox.UnreadCount >= before {
		t.Errorf("unread did not fall: %d → %d", before, inbox.UnreadCount)
	}

	// Follow/unfollow by handle, form-encoded.
	status, _ = h.do("POST", "/events", devToken, "event_type=follow&target_handle=guest", "application/x-www-form-urlencoded")
	if status != 204 {
		t.Fatalf("follow: %d", status)
	}
	_, body = h.do("GET", "/api/v1/users/guest", devToken, nil, "")
	var profile ProfileDTO
	json.Unmarshal(body, &profile)
	if !profile.ViewerFollows {
		t.Error("follow did not stick")
	}

	// Delete own work, then it is gone from the feed.
	status, _ = h.do("POST", "/events", devToken, map[string]string{"event_type": "work_delete", "work_id": accepted.WorkID}, "")
	if status != 200 {
		t.Fatalf("delete: %d", status)
	}
	status, _ = h.do("GET", "/api/v1/works/"+accepted.WorkID, devToken, nil, "")
	if status != 404 {
		t.Errorf("deleted work still readable: %d", status)
	}

	// Someone else's likes are private.
	status, body = h.do("GET", "/api/v1/users/miiyazuko/works?tab=likes", devToken, nil, "")
	if status != 403 || !strings.Contains(string(body), "likes_private") {
		t.Errorf("likes: %d %s", status, body)
	}

	// Unknown surface.
	status, body = h.do("GET", "/api/v1/feed?surface=nope", devToken, nil, "")
	if status != 400 || !strings.Contains(string(body), "unknown_surface") {
		t.Errorf("unknown surface: %d %s", status, body)
	}
}

func TestSeededLanesHaveContent(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login("dev")
	for _, surface := range []string{"following", "foryou", "trending", "music", "visions"} {
		status, body := h.do("GET", "/api/v1/feed?surface="+surface, token, nil, "")
		if status != 200 {
			t.Fatalf("%s: %d %s", surface, status, body)
		}
		var page WorkPageDTO
		json.Unmarshal(body, &page)
		if len(page.Works) == 0 {
			t.Errorf("%s: empty on a seeded database", surface)
		}
		for _, w := range page.Works {
			if w.Author.Handle == "" || w.CreatedAt.IsZero() || !strings.HasPrefix(w.CID, "sha256:") {
				t.Errorf("%s: malformed work %+v", surface, w)
			}
		}
	}
	// The poll came through projected.
	status, body := h.do("GET", "/api/v1/users/admin/works", token, nil, "")
	if status != 200 {
		t.Fatal(status)
	}
	var page WorkPageDTO
	json.Unmarshal(body, &page)
	var poll *PollDTO
	for _, w := range page.Works {
		if w.Poll != nil {
			poll = w.Poll
		}
	}
	if poll == nil || poll.TotalVotes != 3 || len(poll.Results) != 3 || poll.TimeLeft == "" {
		t.Errorf("poll = %+v", poll)
	}
}

// TestFrequencyFixtureKeySets: the Frequencies DTOs, pinned against the Kit's
// golden fixtures the same way `me`, `session`, `user` and `error` are.
//
// The fixtures are what the Swift `Models/Frequency.swift` decoders were
// written and tested against. If a key here drifts, the app stops rendering a
// room, so the drift is caught in Go rather than in the simulator.
func TestFrequencyFixtureKeySets(t *testing.T) {
	h := newHarness(t)
	// The co-host sees every part of the room: the stage, the co-host list,
	// the microphone queue and a viewer block.
	token, _ := h.login("tehanibentley")

	// Author strips omit the optional strings when the account has none, and
	// the Swift side decodes those with decodeIfPresent.
	optionalAuthorKeys := []string{"avatar_url", "official_type"}

	compare := func(what string, fixture, live map[string]any, optional ...string) {
		t.Helper()
		for _, k := range optional {
			if _, ok := live[k]; !ok {
				delete(fixture, k)
			}
		}
		for k := range fixture {
			if _, ok := live[k]; !ok {
				t.Errorf("%s: live response lacks key %q", what, k)
			}
		}
		for k := range live {
			if _, ok := fixture[k]; !ok {
				t.Errorf("%s: live response has key %q the fixture does not", what, k)
			}
		}
	}
	obj := func(v any) map[string]any {
		t.Helper()
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("expected an object, got %T", v)
		}
		return m
	}
	first := func(what string, v any) map[string]any {
		t.Helper()
		arr, ok := v.([]any)
		if !ok || len(arr) == 0 {
			t.Fatalf("%s: expected a non-empty array, got %v", what, v)
		}
		return obj(arr[0])
	}

	var listFixture, roomFixture map[string]any
	readFixture(t, "frequency_list.json", &listFixture)
	readFixture(t, "frequency_room.json", &roomFixture)

	status, body := h.do("GET", "/api/v1/frequencies?lane=live", token, nil, "")
	if status != 200 {
		t.Fatalf("frequencies: %d %s", status, body)
	}
	var liveList map[string]any
	json.Unmarshal(body, &liveList)
	compare("frequency_list", listFixture, liveList)

	fixtureSummary := first("fixture frequencies", listFixture["frequencies"])
	liveSummary := first("live frequencies", liveList["frequencies"])
	compare("frequency", fixtureSummary, liveSummary)
	compare("frequency.host", obj(fixtureSummary["host"]), obj(liveSummary["host"]), optionalAuthorKeys...)

	status, body = h.do("GET", "/api/v1/frequencies/"+liveSummary["id"].(string), token, nil, "")
	if status != 200 {
		t.Fatalf("room: %d %s", status, body)
	}
	var liveRoom map[string]any
	json.Unmarshal(body, &liveRoom)
	compare("frequency_room", roomFixture, liveRoom)
	compare("frequency_room.frequency", obj(roomFixture["frequency"]), obj(liveRoom["frequency"]))

	fixtureSpeaker := first("fixture speakers", roomFixture["speakers"])
	liveSpeaker := first("live speakers", liveRoom["speakers"])
	compare("frequency_participant", fixtureSpeaker, liveSpeaker)
	compare("frequency_participant.author", obj(fixtureSpeaker["author"]), obj(liveSpeaker["author"]), optionalAuthorKeys...)

	compare("frequency_room.cohosts[0]",
		first("fixture cohosts", roomFixture["cohosts"]),
		first("live cohosts", liveRoom["cohosts"]), optionalAuthorKeys...)

	fixtureRequest := first("fixture requests", roomFixture["requests"])
	liveRequest := first("live requests", liveRoom["requests"])
	compare("frequency_request", fixtureRequest, liveRequest)
	compare("frequency_request.author", obj(fixtureRequest["author"]), obj(liveRequest["author"]), optionalAuthorKeys...)

	compare("frequency_viewer", obj(roomFixture["viewer"]), obj(liveRoom["viewer"]))
}
