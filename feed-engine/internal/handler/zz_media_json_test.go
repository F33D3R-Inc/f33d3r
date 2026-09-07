package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/f33d3r/feed-engine/internal/config"
	"github.com/f33d3r/feed-engine/internal/model"
)

// The upload lane's JSON twins. What is held here is what the app cannot work
// out for itself: that a bearer is required on every route, that the bytes
// and not the filename decide what a file is, the one shape a video job is
// reported in, and the difference between a server with no GIF provider and
// a search that found nothing.
//
// Admission past the sniff needs the banned-content registry, which is a
// database; this package's tests have none, so the gate's own refusal (503
// scan_unavailable) is where an upload stops here — and reaching it is the
// proof that the sniff let the file through, since a refused file never gets
// that far.

const mediaTestPIAL = "7b2c8a2e-1c7d-4d5e-9a0b-0a1b2c3d4e5f"

// mediaHandler resolves one bearer to one account without dialling a database
// (authedHandler's arrangement), with the outbound client swapped for a stub
// where a test needs a provider to answer.
func mediaHandler(t *testing.T, cfg *config.Config, rt http.RoundTripper) *Handler {
	t.Helper()
	h := authedHandler(t, "", &model.User{
		ID:     "2f6c1d0e-9b3a-4c8d-8e7f-6a5b4c3d2e1f",
		Handle: "dev",
		PIALID: mediaTestPIAL,
	})
	h.cfg = cfg
	if rt != nil {
		h.httpClient = &http.Client{Transport: rt}
	}
	return h
}

func withBearer(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer "+injectionSessionToken)
	return r
}

// multipartRequest builds a POST to path with one file part.
func multipartRequest(t *testing.T, path, field, filename string, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, path, &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

// The leading bytes of the containers the routes see. Each is padded past the
// twelve bytes readMagic looks at.
var (
	pngHead  = pad([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
	zipHead  = pad([]byte{'P', 'K', 0x03, 0x04})
	m4aHead  = pad([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'M', '4', 'A', ' '})
	isomHead = pad([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'})
	ebmlHead = pad([]byte{0x1A, 0x45, 0xDF, 0xA3})
	oggHead  = pad([]byte("OggS"))
	id3Head  = pad([]byte("ID3"))
	adtsHead = pad([]byte{0xFF, 0xF1})
)

func pad(head []byte) []byte {
	return append(head, make([]byte, 16)...)
}

// ── Every route wants a bearer ───────────────────────────────────────────────

func TestMediaRoutesRequireBearer(t *testing.T) {
	h := mediaHandler(t, &config.Config{}, nil)
	routes := []struct {
		name string
		req  *http.Request
		fn   func(http.ResponseWriter, *http.Request, *model.User)
	}{
		{"image", multipartRequest(t, "/api/v1/media", "file", "a.png", pngHead), h.apiV1UploadMedia},
		{"video", multipartRequest(t, "/api/v1/media/video", "file", "a.mp4", isomHead), h.apiV1UploadVideo},
		{"video status", httptest.NewRequest(http.MethodGet, "/api/v1/media/video/x", nil), h.apiV1VideoJob},
		{"voice", multipartRequest(t, "/api/v1/media/voice", "audio", "a.m4a", m4aHead), h.apiV1UploadVoice},
		{"gif", httptest.NewRequest(http.MethodGet, "/api/v1/gif/search?q=cats", nil), h.apiV1GifSearch},
	}
	for _, rt := range routes {
		rec := httptest.NewRecorder()
		h.requireAPIUser(rt.fn)(rec, rt.req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a bearer: status %d, want 401", rt.name, rec.Code)
			continue
		}
		if got := errorCode(t, rec.Body.Bytes()); got != "unauthenticated" {
			t.Errorf("%s without a bearer: code %q", rt.name, got)
		}
	}
}

// ── The bytes decide ─────────────────────────────────────────────────────────

// The image twin is unchanged by the routes added beside it: a PNG under
// either field name goes through to admission, and a ZIP named .png does not.
func TestImageUploadStillAdmitsImagesByTheirBytes(t *testing.T) {
	h := mediaHandler(t, &config.Config{}, nil)
	for _, field := range []string{"file", "media"} {
		rec := httptest.NewRecorder()
		h.requireAPIUser(h.apiV1UploadMedia)(rec, withBearer(multipartRequest(t, "/api/v1/media", field, "photo.png", pngHead)))
		if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec.Body.Bytes()) != "scan_unavailable" {
			t.Errorf("PNG under %q: %d %s — expected to reach the gate", field, rec.Code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	h.requireAPIUser(h.apiV1UploadMedia)(rec, withBearer(multipartRequest(t, "/api/v1/media", "file", "photo.png", zipHead)))
	if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec.Body.Bytes()) != "unsupported_media" {
		t.Errorf("ZIP named .png: %d %s", rec.Code, rec.Body)
	}
}

func TestVideoUploadRefusesWhatIsNotVideo(t *testing.T) {
	h := mediaHandler(t, &config.Config{}, nil)
	for _, c := range []struct {
		name string
		data []byte
	}{{"clip.mp4", zipHead}, {"clip.mov", pngHead}, {"clip.webm", []byte{0x1A, 0x45}}} {
		rec := httptest.NewRecorder()
		h.requireAPIUser(h.apiV1UploadVideo)(rec, withBearer(multipartRequest(t, "/api/v1/media/video", "file", c.name, c.data)))
		if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec.Body.Bytes()) != "unsupported_media" {
			t.Errorf("%s with non-video bytes: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	// A phone that names its recording without an extension is still sending
	// video; it goes through to admission, and admission stops at the gate.
	rec := httptest.NewRecorder()
	h.requireAPIUser(h.apiV1UploadVideo)(rec, withBearer(multipartRequest(t, "/api/v1/media/video", "file", "IMG_0412", isomHead)))
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec.Body.Bytes()) != "scan_unavailable" {
		t.Errorf("ISO BMFF without an extension: %d %s — expected to reach the gate", rec.Code, rec.Body)
	}
}

// The name a video reaches the transcoder under: the sender's when it agrees
// with the bytes, the container's own otherwise.
func TestVideoFilenameFollowsTheBytes(t *testing.T) {
	cases := []struct {
		declared string
		head     []byte
		want     string
		ok       bool
	}{
		{"clip.MOV", isomHead, "clip.MOV", true},
		{"clip.m4v", isomHead, "clip.m4v", true},
		{"IMG_0412", isomHead, "IMG_0412.mp4", true},
		{"clip.webm", isomHead, "clip.mp4", true},
		{"clip.mkv", ebmlHead, "clip.mkv", true},
		{"clip.mp4", ebmlHead, "clip.webm", true},
		{"", ebmlHead, "upload.webm", true},
		{"clip.mp4", zipHead, "", false},
	}
	for _, c := range cases {
		got, ok := videoFilenameFor(c.declared, c.head)
		if ok != c.ok || got != c.want {
			t.Errorf("videoFilenameFor(%q, %x) = %q, %v; want %q, %v", c.declared, c.head[:8], got, ok, c.want, c.ok)
		}
	}
}

// Every recorder the clients use is recognised by what it writes, and the
// stored extension comes from that rather than from the filename. The ISO
// BMFF cases are the ones the app depends on: AVAudioRecorder's M4A brand,
// and the isom/mp42 brands Android and Safari write.
func TestVoiceSniffAcceptsRecordersAndRefusesTheRest(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"M4A brand", m4aHead, ".m4a"},
		{"isom brand", isomHead, ".m4a"},
		{"WebM/Opus", ebmlHead, ".webm"},
		{"Ogg/Opus", oggHead, ".ogg"},
		{"MP3 with ID3", id3Head, ".mp3"},
		{"MP3 sync word", pad([]byte{0xFF, 0xFB}), ".mp3"},
		{"ADTS AAC", adtsHead, ".aac"},
		{"ZIP", zipHead, ""},
		{"PNG", pngHead, ""},
		{"three bytes", []byte{0xFF, 0xFB, 0}, ""},
		{"ftyp too short", []byte{0, 0, 0, 0x18, 'f', 't', 'y'}, ""},
	}
	for _, c := range cases {
		if got := audioExtensionFor(c.data); got != c.want {
			t.Errorf("%s: audioExtensionFor = %q, want %q", c.name, got, c.want)
		}
	}

	h := mediaHandler(t, &config.Config{}, nil)
	rec := httptest.NewRecorder()
	h.requireAPIUser(h.apiV1UploadVoice)(rec, withBearer(multipartRequest(t, "/api/v1/media/voice", "audio", "voice.m4a", zipHead)))
	if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec.Body.Bytes()) != "unsupported_media" {
		t.Errorf("ZIP as voice: %d %s", rec.Code, rec.Body)
	}
	// AVAudioRecorder output, under the generic field name, goes through to
	// admission and stops at the gate.
	rec = httptest.NewRecorder()
	h.requireAPIUser(h.apiV1UploadVoice)(rec, withBearer(multipartRequest(t, "/api/v1/media/voice", "file", "recording.m4a", m4aHead)))
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec.Body.Bytes()) != "scan_unavailable" {
		t.Errorf("M4A as voice: %d %s — expected to reach the gate", rec.Code, rec.Body)
	}
}

// ── GIF search ───────────────────────────────────────────────────────────────

// klipyStub answers as the provider, remembering what it was asked.
type klipyStub struct {
	asked  []string
	status int
	body   string
	err    error
}

func (s *klipyStub) RoundTrip(r *http.Request) (*http.Response, error) {
	s.asked = append(s.asked, r.URL.String())
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: s.status,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    r,
	}, nil
}

const klipyAnswer = `{"result":true,"data":{"data":[
	{"id":12345,"title":"cat typing","file":{
		"hd":{"gif":{"url":"https://cdn.klipy.test/hd.gif","width":960,"height":540}},
		"md":{"gif":{"url":"https://cdn.klipy.test/md.gif","width":480,"height":270}},
		"sm":{"gif":{"url":"https://cdn.klipy.test/sm.gif","width":240,"height":135}}}},
	{"id":"no-gif","title":"mp4 only","file":{"md":{"mp4":{"url":"https://cdn.klipy.test/x.mp4"}}}}
]}}`

// No provider is a 503 with a code, not an empty list: the picker's button
// should not be offered on a server that cannot answer it. The web keeps its
// 200-with-a-note, which its picker already draws as an empty grid.
func TestGifSearchUnconfiguredIs503(t *testing.T) {
	h := mediaHandler(t, &config.Config{}, nil)
	rec := httptest.NewRecorder()
	h.requireAPIUser(h.apiV1GifSearch)(rec, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/gif/search?q=cats", nil)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", rec.Code, rec.Body)
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "gif_search_unavailable" {
		t.Fatalf("code %q", got)
	}

	web := httptest.NewRecorder()
	h.gifSearch(web, httptest.NewRequest(http.MethodGet, "/api/gif/search?q=cats", nil))
	if web.Code != http.StatusOK || strings.TrimSpace(web.Body.String()) != `{"results":[],"error":"gif_search_not_configured"}` {
		t.Fatalf("web unconfigured answer changed: %d %s", web.Code, web.Body)
	}
}

// The provider's shape stays on the server. Both lanes read one answer from
// it and project it in their own keys; an item with no gif rendition is not
// offered by either.
func TestGifSearchNormalisesTheProvider(t *testing.T) {
	stub := &klipyStub{status: 200, body: klipyAnswer}
	h := mediaHandler(t, &config.Config{KlipyAPIKey: "k3y"}, stub)

	rec := httptest.NewRecorder()
	h.requireAPIUser(h.apiV1GifSearch)(rec, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/gif/search?q=cats", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out GifSearchDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON: %s", rec.Body)
	}
	if len(out.Results) != 1 {
		t.Fatalf("results %+v; the mp4-only item has nothing to attach", out.Results)
	}
	want := GifResultDTO{ID: "12345", URL: "https://cdn.klipy.test/md.gif", PreviewURL: "https://cdn.klipy.test/sm.gif", Width: 480, Height: 270, Title: "cat typing"}
	if out.Results[0] != want {
		t.Errorf("result %+v, want %+v", out.Results[0], want)
	}
	if len(stub.asked) != 1 || !strings.Contains(stub.asked[0], "/k3y/gifs/search?") || !strings.Contains(stub.asked[0], "q=cats") {
		t.Errorf("provider asked %v", stub.asked)
	}
	// The keys are the app's, not the web picker's.
	if strings.Contains(rec.Body.String(), "thumb_url") {
		t.Errorf("web key leaked into the app's contract: %s", rec.Body)
	}

	// Empty q is trending, on both lanes.
	stub.asked = nil
	rec = httptest.NewRecorder()
	h.requireAPIUser(h.apiV1GifSearch)(rec, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/gif/search", nil)))
	if len(stub.asked) != 1 || !strings.Contains(stub.asked[0], "/gifs/trending?") {
		t.Errorf("empty q asked %v", stub.asked)
	}

	// The web picker reads the same answer in the keys it always has.
	web := httptest.NewRecorder()
	h.gifSearch(web, httptest.NewRequest(http.MethodGet, "/api/gif/search?q=cats", nil))
	var webOut struct {
		Results []struct {
			ID       string `json:"id"`
			URL      string `json:"url"`
			ThumbURL string `json:"thumb_url"`
		} `json:"results"`
	}
	if err := json.Unmarshal(web.Body.Bytes(), &webOut); err != nil || len(webOut.Results) != 1 {
		t.Fatalf("web answer %d %s", web.Code, web.Body)
	}
	if webOut.Results[0].URL != want.URL || webOut.Results[0].ThumbURL != want.PreviewURL {
		t.Errorf("web row %+v disagrees with the app's %+v", webOut.Results[0], want)
	}
}

// A provider that cannot be reached is neither "not configured" nor "no GIFs".
func TestGifSearchProviderDownIs502(t *testing.T) {
	h := mediaHandler(t, &config.Config{KlipyAPIKey: "k3y"}, &klipyStub{err: errors.New("dial tcp: connection refused")})
	rec := httptest.NewRecorder()
	h.requireAPIUser(h.apiV1GifSearch)(rec, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/gif/search?q=cats", nil)))
	if rec.Code != http.StatusBadGateway || errorCode(t, rec.Body.Bytes()) != "gif_search_failed" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	web := httptest.NewRecorder()
	h.gifSearch(web, httptest.NewRequest(http.MethodGet, "/api/gif/search?q=cats", nil))
	if web.Code != http.StatusBadGateway || strings.TrimSpace(web.Body.String()) != "gif search failed" {
		t.Fatalf("web provider-down answer changed: %d %s", web.Code, web.Body)
	}
}

// ── Video job state ──────────────────────────────────────────────────────────

// seedUpload puts one upload in the store for the test and takes it out after.
func seedUpload(t *testing.T, u *tusUpload) {
	t.Helper()
	globalTusStore.mu.Lock()
	globalTusStore.uploads[u.ID] = u
	globalTusStore.mu.Unlock()
	t.Cleanup(func() {
		globalTusStore.mu.Lock()
		delete(globalTusStore.uploads, u.ID)
		globalTusStore.mu.Unlock()
	})
}

func videoJob(t *testing.T, h *Handler, id string) (int, VideoJobDTO, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	r := withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/media/video/"+id, nil))
	r.SetPathValue("id", id)
	h.requireAPIUser(h.apiV1VideoJob)(rec, r)
	var dto VideoJobDTO
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			t.Fatalf("not a VideoJobDTO: %s", rec.Body)
		}
	}
	return rec.Code, dto, rec.Body.Bytes()
}

func webJob(t *testing.T, h *Handler, id string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/upload/tus/status/"+id, nil)
	r.SetPathValue("id", id)
	h.tusJobStatus(rec, r)
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestVideoJobUnknownIs404(t *testing.T) {
	h := mediaHandler(t, &config.Config{}, nil)
	code, _, body := videoJob(t, h, "no-such-upload")
	if code != http.StatusNotFound || errorCode(t, body) != "video_job_not_found" {
		t.Errorf("app: %d %s", code, body)
	}
	if code, body := webJob(t, h, "no-such-upload"); code != http.StatusNotFound || body != "upload not found" {
		t.Errorf("web: %d %q", code, body)
	}
}

// One upload, read by both lanes: the same state, each in its own keys. The
// web's answer for a job the transcoder has not taken is the exact bytes its
// compose box has always polled for.
func TestVideoJobOneStateTwoProjections(t *testing.T) {
	h := mediaHandler(t, &config.Config{}, nil)

	seedUpload(t, &tusUpload{ID: "upload-queued", UploaderPIAL: mediaTestPIAL})
	code, dto, _ := videoJob(t, h, "upload-queued")
	if code != http.StatusOK || dto.Status != "queued" || dto.UploadID != "upload-queued" {
		t.Errorf("queued upload: %d %+v", code, dto)
	}
	if dto.MasterURL != "" || dto.Width != nil {
		t.Errorf("a queued job has no renditions: %+v", dto)
	}
	if code, body := webJob(t, h, "upload-queued"); code != http.StatusOK || body != `{"status":"queued"}` {
		t.Errorf("web queued: %d %s", code, body)
	}

	// A duplicate caught before transcoding. The app is pointed at the work;
	// the web keeps its shape; the creator's PIAL reaches neither.
	seedUpload(t, &tusUpload{ID: "upload-dup", UploaderPIAL: mediaTestPIAL, DupInfo: &duplicateVideoInfo{
		OriginalPIAL:      "0000dead-beef-4000-8000-000000000001",
		OriginalHandle:    "miiyazuko",
		CreatorName:       "Mii",
		WorkID:            "w-1",
		ExistingMasterURL: "/static/media/posts/a1/master.m3u8",
		ExistingPosterURL: "/static/media/posts/a1/poster.jpg",
		DurationSecs:      12.5,
		Width:             1920,
		Height:            1080,
	}})
	code, dto, body := videoJob(t, h, "upload-dup")
	if code != http.StatusOK || dto.Status != "duplicate" || dto.DuplicateOfWorkID != "w-1" || dto.DuplicateOfHandle != "miiyazuko" {
		t.Errorf("duplicate: %d %+v", code, dto)
	}
	if dto.MasterURL != "/static/media/posts/a1/master.m3u8" || dto.Width == nil || *dto.Width != 1920 || dto.DurationSecs == nil || *dto.DurationSecs != 12.5 {
		t.Errorf("duplicate renditions: %+v", dto)
	}
	if bytes.Contains(body, []byte("dead-beef")) {
		t.Errorf("a PIAL crossed the JSON lane: %s", body)
	}
	webCode, webBody := webJob(t, h, "upload-dup")
	if webCode != http.StatusOK || !strings.Contains(webBody, `"status":"duplicate"`) || !strings.Contains(webBody, `"work_id":"w-1"`) || !strings.Contains(webBody, `"creator_handle":"miiyazuko"`) {
		t.Errorf("web duplicate: %d %s", webCode, webBody)
	}
	if strings.Contains(webBody, "dead-beef") || strings.Contains(webBody, "creator_pial") {
		t.Errorf("a PIAL reached the browser: %s", webBody)
	}

	// Somebody else's upload is nobody's business.
	seedUpload(t, &tusUpload{ID: "upload-foreign", UploaderPIAL: "0000dead-beef-4000-8000-000000000002"})
	if code, _, body := videoJob(t, h, "upload-foreign"); code != http.StatusNotFound || errorCode(t, body) != "video_job_not_found" {
		t.Errorf("foreign upload: %d %s", code, body)
	}
}

// Once the transcoder has the job, its words are mapped onto the DTO's
// states, its output onto the renditions, and its loss of the job onto a
// failure the client can act on. The web relays its answer as it arrived.
func TestVideoJobFollowsTheTranscoder(t *testing.T) {
	stub := &klipyStub{status: 200}
	h := mediaHandler(t, &config.Config{}, stub)
	h.transcodingURL = "http://transcoder.test"
	seedUpload(t, &tusUpload{ID: "upload-job", UploaderPIAL: mediaTestPIAL, JobID: "job-1"})

	stub.body = `{"job_id":"job-1","status":"transcoding","error":null,"output":null}`
	if _, dto, _ := videoJob(t, h, "upload-job"); dto.Status != "processing" {
		t.Errorf("transcoding → %q, want processing", dto.Status)
	}
	if len(stub.asked) != 1 || stub.asked[0] != "http://transcoder.test/v1/video/job/job-1" {
		t.Errorf("transcoder asked %v", stub.asked)
	}

	stub.body = `{"job_id":"job-1","status":"ready","error":null,"output":{
		"asset_id":"a1","master_url":"/static/media/posts/a1/master.m3u8",
		"watermarked_mp4_url":"/static/media/posts/a1/watermarked.mp4",
		"poster_url":"/static/media/posts/a1/poster.jpg","variants":[],
		"source_width":1280,"source_height":720,"duration_seconds":42.25}}`
	_, dto, _ := videoJob(t, h, "upload-job")
	if dto.Status != "ready" || dto.MasterURL != "/static/media/posts/a1/master.m3u8" || dto.WatermarkedURL != "/static/media/posts/a1/watermarked.mp4" || dto.PosterURL != "/static/media/posts/a1/poster.jpg" {
		t.Errorf("ready: %+v", dto)
	}
	if dto.DurationSecs == nil || *dto.DurationSecs != 42.25 || dto.Width == nil || *dto.Width != 1280 || dto.Height == nil || *dto.Height != 720 {
		t.Errorf("ready renditions: %+v", dto)
	}
	if code, body := webJob(t, h, "upload-job"); code != http.StatusOK || body != strings.TrimSpace(stub.body) {
		t.Errorf("web did not relay the transcoder's answer: %d %s", code, body)
	}

	stub.body = `{"job_id":"job-1","status":"failed","error":"ffmpeg exited with 1","output":null}`
	if _, dto, _ := videoJob(t, h, "upload-job"); dto.Status != "failed" || dto.Error != "ffmpeg exited with 1" {
		t.Errorf("failed: %+v", dto)
	}

	stub.status, stub.body = 404, `{"error":"job not found"}`
	if _, dto, _ := videoJob(t, h, "upload-job"); dto.Status != "failed" || dto.Error == "" {
		t.Errorf("a job the transcoder lost: %+v", dto)
	}

	stub.status, stub.body = 502, `bad gateway`
	if _, dto, _ := videoJob(t, h, "upload-job"); dto.Status != "processing" {
		t.Errorf("a transcoder having a bad moment: %+v", dto)
	}
}

// An absolute URL from a media backend would name an internal host; the
// contract carries paths.
func TestVideoJobURLsStayRelative(t *testing.T) {
	if got := relativeMediaURL("http://minio:9000/media/posts/a1/master.m3u8?x=1"); got != "/media/posts/a1/master.m3u8?x=1" {
		t.Errorf("absolute → %q", got)
	}
	if got := relativeMediaURL("/static/media/posts/a1/master.m3u8"); got != "/static/media/posts/a1/master.m3u8" {
		t.Errorf("relative changed: %q", got)
	}
}
