package scanclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// serveVerdict answers one /v1/hashes/check-media call with a fixed JSON body.
func serveVerdict(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("writing verdict: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

// An audio near match must survive the wire with its score, and must render as
// a score rather than as a distance it does not carry.
func TestAudioNearMatchCarriesItsScore(t *testing.T) {
	c := serveVerdict(t, `{"banned":false,"kind":"video","audio_fp_present":true,
		"checked":["sha256","phash","audio_fp"],"degraded":[],
		"near_match":{"hash_type":"audio_fp","score":0.8532,"category":"test_vector"},
		"near_matches":[{"hash_type":"audio_fp","score":0.8532,"category":"test_vector"}]}`)

	verdict, err := c.CheckMedia(context.Background(), KindVideo, "clip.mp4", []byte("bytes"))
	if err != nil {
		t.Fatalf("CheckMedia: %v", err)
	}
	nears := verdict.Nears()
	if len(nears) != 1 {
		t.Fatalf("want 1 near match, got %d", len(nears))
	}
	if nears[0].Score != 0.8532 {
		t.Fatalf("score lost in transit: %v", nears[0].Score)
	}
	if got := nears[0].Detail(); got != "s0.8532" {
		t.Fatalf("audio near match rendered as %q, want s0.8532", got)
	}
	if got := nears[0].Describe(); got != "audio at similarity 0.8532 of banned test_vector content" {
		t.Fatalf("audio near match described as %q", got)
	}
}

// The phash vocabulary must not change: the signal an existing evidence row
// carries is banned_phash_near:<category>:d<n>.
func TestPHashNearMatchStillRendersADistance(t *testing.T) {
	n := NearMatch{HashType: HashTypePHash, Distance: 9, Category: "csam"}
	if got := n.Detail(); got != "d9" {
		t.Fatalf("phash near match rendered as %q, want d9", got)
	}
	if got := n.Describe(); got != "phash within distance 9 of banned csam content" {
		t.Fatalf("phash near match described as %q", got)
	}
}

// A hash type this build does not know must carry both measurements rather than
// be silently rendered as a distance it may not have.
func TestUnknownHashTypeKeepsBothMeasurements(t *testing.T) {
	n := NearMatch{HashType: "video_fp", Distance: 3, Score: 0.77, Category: "x"}
	if got := n.Detail(); got != "d3:s0.7700" {
		t.Fatalf("unknown near match rendered as %q", got)
	}
}

// A video can be near banned content on its frames and on its audio at once.
// Neither finding may be dropped.
func TestBothLayersNearMatchAreKept(t *testing.T) {
	c := serveVerdict(t, `{"banned":false,"kind":"video",
		"near_match":{"hash_type":"audio_fp","score":0.81,"category":"a"},
		"near_matches":[{"hash_type":"audio_fp","score":0.81,"category":"a"},
		                {"hash_type":"phash","distance":11,"category":"b"}]}`)

	verdict, err := c.CheckMedia(context.Background(), KindVideo, "clip.mp4", []byte("bytes"))
	if err != nil {
		t.Fatalf("CheckMedia: %v", err)
	}
	nears := verdict.Nears()
	if len(nears) != 2 {
		t.Fatalf("want both near matches, got %d", len(nears))
	}
	if nears[0].Detail() != "s0.8100" || nears[1].Detail() != "d11" {
		t.Fatalf("near matches rendered as %q and %q", nears[0].Detail(), nears[1].Detail())
	}
}

// A verdict from a content-scan that reports only the single near_match field
// must still be seen by every caller that reads the list.
func TestNearsFallsBackToTheSingleField(t *testing.T) {
	v := &MediaVerdict{NearMatch: &NearMatch{HashType: HashTypePHash, Distance: 7, Category: "c"}}
	nears := v.Nears()
	if len(nears) != 1 || nears[0].Distance != 7 {
		t.Fatalf("single near_match lost: %#v", nears)
	}
	if (&MediaVerdict{}).Nears() != nil {
		t.Fatal("a verdict with no near match reported one")
	}
}

// A banned verdict is a refusal whichever layer produced it: the audio radius
// must not need a phash to be acted on.
func TestBannedOnAudioRadiusIsABannedVerdict(t *testing.T) {
	c := serveVerdict(t, `{"banned":true,"hash_type":"audio_fp","category":"test_vector",
		"kind":"audio","checked":["sha256","audio_fp"],
		"near_match":{"hash_type":"audio_fp","score":0.9968,"category":"test_vector"}}`)

	verdict, err := c.CheckMedia(context.Background(), KindAudio, "t.mka", []byte("bytes"))
	if err != nil {
		t.Fatalf("CheckMedia: %v", err)
	}
	if !verdict.Banned || verdict.HashType != HashTypeAudioFP {
		t.Fatalf("audio radius block not carried: %#v", verdict)
	}
	if err := json.Unmarshal([]byte(`{}`), &MediaVerdict{}); err != nil {
		t.Fatalf("empty verdict must decode: %v", err)
	}
}
