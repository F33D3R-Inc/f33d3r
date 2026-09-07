package scanclient

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An unconfigured client must say so rather than behave like a clean verdict.
func TestUnconfiguredClientRefusesToAnswer(t *testing.T) {
	c := New("")
	if c.Configured() {
		t.Fatal("client with empty base reports itself configured")
	}
	if _, err := c.CheckMedia(context.Background(), KindImage, "x.jpg", []byte("bytes")); err != ErrNotConfigured {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

// The request must carry the kind, the exact payload, and a known length —
// content-scan runs behind gunicorn and the body must not go out chunked.
func TestCheckMediaSendsKindAndPayload(t *testing.T) {
	payload := strings.Repeat("banned-bytes", 4096)

	var gotKind, gotFilename, gotBody string
	var gotContentLength int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentLength = r.ContentLength

		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("parsing content type: %v", err)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("reading part: %v", err)
				return
			}
			body, _ := io.ReadAll(part)
			switch part.FormName() {
			case "kind":
				gotKind = string(body)
			case "file":
				gotFilename = part.FileName()
				gotBody = string(body)
			}
		}
		json.NewEncoder(w).Encode(MediaVerdict{
			Banned:   true,
			HashType: "phash",
			Category: "csam",
			Checked:  []string{"sha256", "phash"},
		})
	}))
	defer srv.Close()

	verdict, err := New(srv.URL).CheckMedia(context.Background(), KindVideo, "clip.mp4", []byte(payload))
	if err != nil {
		t.Fatalf("CheckMedia: %v", err)
	}
	if gotContentLength <= 0 {
		t.Errorf("request went out without a Content-Length (chunked); got %d", gotContentLength)
	}
	if gotKind != KindVideo {
		t.Errorf("kind = %q, want %q", gotKind, KindVideo)
	}
	if gotFilename != "clip.mp4" {
		t.Errorf("filename = %q, want clip.mp4", gotFilename)
	}
	if gotBody != payload {
		t.Errorf("payload was altered in transit: %d bytes received, %d sent", len(gotBody), len(payload))
	}
	if !verdict.Banned || verdict.HashType != "phash" || verdict.Category != "csam" {
		t.Errorf("verdict not decoded: %+v", verdict)
	}
}

// A refusal from content-scan is an error, never an empty verdict the caller
// could mistake for "nothing matched".
func TestCheckMediaRefusalIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"registry_unavailable"}`))
	}))
	defer srv.Close()

	verdict, err := New(srv.URL).CheckMedia(context.Background(), KindImage, "a.png", []byte("x"))
	if err == nil {
		t.Fatalf("503 from content-scan produced no error (verdict %+v)", verdict)
	}
	if verdict != nil {
		t.Errorf("a refused check returned a verdict: %+v", verdict)
	}
	if !strings.Contains(err.Error(), "registry_unavailable") {
		t.Errorf("error dropped the reason content-scan gave: %v", err)
	}
}

// A body that is not a verdict must not decode into a zero-valued one.
func TestCheckMediaRejectsUndecodableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	if _, err := New(srv.URL).CheckMedia(context.Background(), KindImage, "a.png", []byte("x")); err == nil {
		t.Fatal("undecodable body accepted as a verdict")
	}
}

func TestCheckMediaRejectsUnknownKind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached the server for an unknown media kind")
	}))
	defer srv.Close()

	if _, err := New(srv.URL).CheckMedia(context.Background(), "hologram", "a.bin", []byte("x")); err == nil {
		t.Fatal("unknown media kind accepted")
	}
}

func TestCheckMediaRejectsEmptyPayload(t *testing.T) {
	if _, err := New("http://example.invalid").CheckMedia(context.Background(), KindImage, "a.png", nil); err == nil {
		t.Fatal("empty payload accepted")
	}
}

// A video is gigabytes and is asked about from disk. The bytes on the wire must
// still be exactly the bytes on disk, with a known length.
func TestCheckMediaFileStreamsFromDisk(t *testing.T) {
	payload := strings.Repeat("frame-data", 20000)
	path := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotBody string
	var gotContentLength int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentLength = r.ContentLength
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			t.Errorf("parsing form: %v", err)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("no file part: %v", err)
			return
		}
		defer f.Close()
		body, _ := io.ReadAll(f)
		gotBody = string(body)
		json.NewEncoder(w).Encode(MediaVerdict{Checked: []string{"sha256", "phash"}})
	}))
	defer srv.Close()

	if _, err := New(srv.URL).CheckMediaFile(context.Background(), KindVideo, "clip.mp4", path); err != nil {
		t.Fatalf("CheckMediaFile: %v", err)
	}
	if gotContentLength <= int64(len(payload)) {
		t.Errorf("Content-Length %d does not cover a %d byte payload", gotContentLength, len(payload))
	}
	if gotBody != payload {
		t.Errorf("file content altered in transit: %d bytes received, %d on disk", len(gotBody), len(payload))
	}
}

func TestCheckMediaFileRejectsMissingAndEmptyFiles(t *testing.T) {
	c := New("http://example.invalid")
	if _, err := c.CheckMediaFile(context.Background(), KindVideo, "gone.mp4", filepath.Join(t.TempDir(), "gone.mp4")); err == nil {
		t.Error("missing file accepted")
	}
	empty := filepath.Join(t.TempDir(), "empty.mp4")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CheckMediaFile(context.Background(), KindVideo, "empty.mp4", empty); err == nil {
		t.Error("empty file accepted")
	}
}
