package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"f33d3r.com/ios/devserver/internal/store"
)

// multipart builds the body the app's MultipartPart produces: one file part
// under `field`.
func multipartBody(t *testing.T, field, filename, mime string, data []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="` + field + `"; filename="` + filename + `"`}
	h["Content-Type"] = []string{mime}
	part, err := mw.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	part.Write(data)
	mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

// box frames an ISO BMFF box.
func box(kind string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out[:4], uint32(8+len(body)))
	copy(out[4:8], kind)
	return append(out, body...)
}

func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }

// syntheticMP4 is the smallest file the probe reads as a movie: ftyp, a moov
// with an mvhd running `secs` seconds and one video track of w×h, rotated
// when `rotated`, then an empty mdat. Enough for the sniffer and the probe;
// nothing here decodes.
func syntheticMP4(secs float64, w, h int, rotated bool) []byte {
	ftyp := box("ftyp", []byte("isom"), u32(0x200), []byte("isomiso2mp41"))
	timescale := uint32(600)
	mvhd := box("mvhd", u32(0), u32(0), u32(0), u32(timescale), u32(uint32(secs*float64(timescale))),
		u32(0x00010000), u32(0), u32(0), u32(0), make([]byte, 36), make([]byte, 24), u32(2))
	matrix := []byte{}
	if rotated {
		// 90°: a=0 b=1 c=-1 d=0.
		matrix = bytes.Join([][]byte{u32(0), u32(0x00010000), u32(0), u32(0xFFFF0000), u32(0), u32(0), u32(0), u32(0), u32(0x40000000)}, nil)
	} else {
		matrix = bytes.Join([][]byte{u32(0x00010000), u32(0), u32(0), u32(0), u32(0x00010000), u32(0), u32(0), u32(0), u32(0x40000000)}, nil)
	}
	tkhd := box("tkhd", u32(0), u32(0), u32(0), u32(1), u32(0), u32(uint32(secs*float64(timescale))),
		make([]byte, 8), u32(0), u32(0), matrix, u32(uint32(w)<<16), u32(uint32(h)<<16))
	trak := box("trak", tkhd)
	moov := box("moov", mvhd, trak)
	mdat := box("mdat", make([]byte, 64))
	return bytes.Join([][]byte{ftyp, moov, mdat}, nil)
}

// syntheticM4A is what an AVAudioRecorder file opens with: the M4A brand.
func syntheticM4A() []byte {
	return append(box("ftyp", []byte("M4A "), u32(0), []byte("M4A mp42isom")), box("mdat", make([]byte, 32))...)
}

func TestProbeMP4(t *testing.T) {
	p, err := probeMP4(bytes.NewReader(syntheticMP4(42.5, 1280, 720, false)))
	if err != nil {
		t.Fatal(err)
	}
	if p.DurationSecs != 42.5 || p.Width != 1280 || p.Height != 720 {
		t.Fatalf("probe = %+v", p)
	}
	p, _ = probeMP4(bytes.NewReader(syntheticMP4(3, 1920, 1080, true)))
	if p.Width != 1080 || p.Height != 1920 {
		t.Fatalf("rotated probe = %+v; a phone's portrait clip is stored landscape with a 90° matrix", p)
	}
	if _, err := probeMP4(bytes.NewReader([]byte("not a movie at all"))); err != errNotMP4 {
		t.Fatalf("garbage: %v", err)
	}
}

// TestVoiceLane: a recording goes up under `audio`, comes back as a path,
// and a signed voice work carrying that path is served with the recorder's
// duration — the whole path the composer takes.
func TestVoiceLane(t *testing.T) {
	h := newHarness(t)
	token, me := h.login("dev")
	pial := me["pial_id"].(string)

	body, ct := multipartBody(t, "audio", "voice.m4a", "audio/mp4", syntheticM4A())
	status, resp := h.do("POST", "/api/v1/media/voice", token, body, ct)
	if status != 201 {
		t.Fatalf("voice upload: %d %s", status, resp)
	}
	var uploaded VoiceUploadDTO
	json.Unmarshal(resp, &uploaded)
	if !strings.HasPrefix(uploaded.URL, "/media/voice/") || !strings.HasSuffix(uploaded.URL, ".m4a") {
		t.Fatalf("url = %q; the bytes name the file, and they are M4A", uploaded.URL)
	}
	if _, ok := resp2map(resp)["duration_secs"]; ok {
		t.Fatal("the server must not claim a duration it did not measure")
	}
	if st, _ := h.do("GET", uploaded.URL, "", nil, ""); st != 200 {
		t.Fatalf("stored file not served: %d", st)
	}

	// The web's field name is served too, and the bytes decide the type.
	body, ct = multipartBody(t, "file", "note.webm", "audio/webm", []byte{0x1A, 0x45, 0xDF, 0xA3, 0, 0, 0, 0})
	if status, resp = h.do("POST", "/api/v1/media/voice", token, body, ct); status != 201 || !strings.HasSuffix(resp2map(resp)["url"].(string), ".webm") {
		t.Fatalf("webm under file: %d %s", status, resp)
	}
	body, ct = multipartBody(t, "audio", "voice.m4a", "audio/mp4", []byte("#!/bin/sh\nrm -rf /\n"))
	if status, resp = h.do("POST", "/api/v1/media/voice", token, body, ct); status != 415 || !strings.Contains(string(resp), "unsupported_media") {
		t.Fatalf("a script named voice.m4a must be refused: %d %s", status, resp)
	}

	// Post the voice work as the composer does: kind voice, the path and the
	// recorder's whole seconds in the signed payload.
	dev := newSigner(t)
	h.do("POST", "/api/pial/signing-key/register", token, map[string]string{"public_key_b64": dev.spkiB64()}, "")
	payload := workCanonicalPayload{
		AuthorPIAL: pial, Body: "said out loud", CommentGating: "everyone", Kind: "voice",
		MediaURLs: []string{}, Tags: []string{}, TimestampMS: time.Now().UnixMilli(),
		VoiceURL: uploaded.URL, VoiceDurationSecs: float64(7),
	}
	status, resp = h.do("POST", "/events", token, envelope(t, dev, payload, nil), "application/json")
	if status != 201 {
		t.Fatalf("post voice: %d %s", status, resp)
	}
	var accepted workAccepted
	json.Unmarshal(resp, &accepted)
	status, resp = h.do("GET", "/api/v1/works/"+accepted.WorkID, token, nil, "")
	var thread WorkThreadDTO
	json.Unmarshal(resp, &thread)
	if thread.Work.Kind != "voice" || thread.Work.Voice == nil || thread.Work.Voice.URL != uploaded.URL || thread.Work.Voice.DurationSecs != 7 {
		t.Fatalf("served voice work = kind %q voice %+v", thread.Work.Kind, thread.Work.Voice)
	}
}

// TestVideoLane: upload, poll to ready with the probe's numbers, post, and
// see the video on the work; the same bytes again are a duplicate naming the
// original's author; another account cannot see the job.
func TestVideoLane(t *testing.T) {
	h := newHarness(t)
	devToken, devMe := h.login("dev")
	miiToken, _ := h.login("miiyazuko")
	clip := syntheticMP4(42, 1280, 720, false)

	body, ct := multipartBody(t, "file", "clip.mov", "video/quicktime", clip)
	status, resp := h.do("POST", "/api/v1/media/video", devToken, body, ct)
	if status != 202 {
		t.Fatalf("video upload: %d %s", status, resp)
	}
	var job VideoJobDTO
	json.Unmarshal(resp, &job)
	if job.Status != videoJobQueued || job.UploadID == "" {
		t.Fatalf("admission = %+v", job)
	}

	// Another account asking about it is told there is no such job.
	if st, _ := h.do("GET", "/api/v1/media/video/"+job.UploadID, miiToken, nil, ""); st != 404 {
		t.Fatalf("other account's poll: %d", st)
	}

	deadline := time.Now().Add(5 * time.Second)
	for job.Status != videoJobReady && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		status, resp = h.do("GET", "/api/v1/media/video/"+job.UploadID, devToken, nil, "")
		if status != 200 {
			t.Fatalf("poll: %d %s", status, resp)
		}
		json.Unmarshal(resp, &job)
	}
	if job.Status != videoJobReady {
		t.Fatalf("never ready: %+v", job)
	}
	if !strings.HasPrefix(job.MasterURL, "/media/video/") || !strings.HasSuffix(job.MasterURL, ".mp4") || job.WatermarkedURL != job.MasterURL {
		t.Fatalf("renditions = %+v", job)
	}
	if job.DurationSecs == nil || *job.DurationSecs != 42 || job.Width == nil || *job.Width != 1280 || job.Height == nil || *job.Height != 720 {
		t.Fatalf("probe numbers missing: %+v", job)
	}
	if st, _ := h.do("GET", job.MasterURL, "", nil, ""); st != 200 {
		t.Fatalf("stored video not served: %d", st)
	}

	dev := newSigner(t)
	h.do("POST", "/api/pial/signing-key/register", devToken, map[string]string{"public_key_b64": dev.spkiB64()}, "")
	payload := workCanonicalPayload{
		AuthorPIAL: devMe["pial_id"].(string), Body: "a clip", CommentGating: "everyone", Kind: "video",
		MediaURLs: []string{}, Tags: []string{}, TimestampMS: time.Now().UnixMilli(),
		VideoMasterURL: job.MasterURL, VideoDurationSecs: float64(42),
	}
	extra := map[string]any{"video_watermarked_url": job.WatermarkedURL, "video_width": 1280, "video_height": 720}
	status, resp = h.do("POST", "/events", devToken, envelope(t, dev, payload, extra), "application/json")
	if status != 201 {
		t.Fatalf("post video: %d %s", status, resp)
	}
	var accepted workAccepted
	json.Unmarshal(resp, &accepted)
	_, resp = h.do("GET", "/api/v1/works/"+accepted.WorkID, miiToken, nil, "")
	var thread WorkThreadDTO
	json.Unmarshal(resp, &thread)
	if thread.Work.Kind != "video" || thread.Work.Video == nil || thread.Work.Video.MasterURL != job.MasterURL ||
		thread.Work.Video.Width != 1280 || thread.Work.Video.Height != 720 || thread.Work.Video.DurationSecs != 42 {
		t.Fatalf("served video work = kind %q video %+v", thread.Work.Kind, thread.Work.Video)
	}

	// Mii uploads the same bytes: a duplicate, credited to dev, with dev's
	// renditions to reference.
	body, ct = multipartBody(t, "file", "copy.mp4", "video/mp4", clip)
	status, resp = h.do("POST", "/api/v1/media/video", miiToken, body, ct)
	if status != 200 {
		t.Fatalf("duplicate upload: %d %s", status, resp)
	}
	var dup VideoJobDTO
	json.Unmarshal(resp, &dup)
	if dup.Status != videoJobDuplicate || dup.DuplicateOfHandle != "dev" || dup.DuplicateOfWorkID != accepted.WorkID || dup.MasterURL != job.MasterURL || dup.UploadID != "" {
		t.Fatalf("duplicate = %+v", dup)
	}

	// Not a video at all.
	body, ct = multipartBody(t, "file", "clip.mp4", "video/mp4", []byte("GIF89a not a movie"))
	if status, resp = h.do("POST", "/api/v1/media/video", devToken, body, ct); status != 415 {
		t.Fatalf("garbage video: %d %s", status, resp)
	}
}

// TestGifSearch: the local library answers trending and a word search with
// served paths and real sizes; a server with no library says so in the code
// the picker branches on.
func TestGifSearch(t *testing.T) {
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	st.SeedIfEmpty(context.Background())
	srv := httptest.NewServer(New(st, t.TempDir()).WithSeedMedia("../../seedmedia").Handler())
	t.Cleanup(func() { srv.Close(); st.Close() })
	h := &harness{t: t, srv: srv, store: st}
	token, _ := h.login("dev")

	status, resp := h.do("GET", "/api/v1/gif/search", token, nil, "")
	if status != 200 {
		t.Fatalf("trending: %d %s", status, resp)
	}
	var page GifSearchDTO
	json.Unmarshal(resp, &page)
	if len(page.Results) < 5 {
		t.Fatalf("trending has %d results", len(page.Results))
	}
	for _, g := range page.Results {
		if g.ID == "" || g.Title == "" || g.Width == 0 || g.Height == 0 || !strings.HasPrefix(g.URL, "/media/seed/gifs/") || g.PreviewURL == "" {
			t.Fatalf("result = %+v", g)
		}
		if st, _ := h.do("GET", g.URL, "", nil, ""); st != 200 {
			t.Fatalf("gif %s not served: %d", g.URL, st)
		}
	}

	status, resp = h.do("GET", "/api/v1/gif/search?q=WAVE", token, nil, "")
	json.Unmarshal(resp, &page)
	if status != 200 || len(page.Results) != 1 || page.Results[0].Title != "ocean wave" {
		t.Fatalf("search wave: %d %s", status, resp)
	}
	status, resp = h.do("GET", "/api/v1/gif/search?q=zebra", token, nil, "")
	json.Unmarshal(resp, &page)
	if status != 200 || len(page.Results) != 0 {
		t.Fatalf("search zebra: %d %s", status, resp)
	}

	bare := httptest.NewServer(New(st, t.TempDir()).WithSeedMedia(t.TempDir()).Handler())
	defer bare.Close()
	req, _ := http.NewRequest("GET", bare.URL+"/api/v1/gif/search", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var errBody map[string]errorBody
	json.NewDecoder(res.Body).Decode(&errBody)
	if res.StatusCode != 503 || errBody["error"].Code != "gif_search_unavailable" {
		t.Fatalf("no library: %d %+v", res.StatusCode, errBody)
	}
}

func resp2map(b []byte) map[string]any {
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}
