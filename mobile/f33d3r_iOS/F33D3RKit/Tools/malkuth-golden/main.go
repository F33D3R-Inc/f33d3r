// Golden-vector generator for the Malkuth work canonical payload.
//
// workCanonicalPayload and verifyCID below are COPIED VERBATIM from
// feed-engine/internal/handler/work_event.go. Nothing here is re-derived: if the
// Go there changes, this file must be updated from it and the fixtures
// regenerated.
//
// For every case this program:
//  1. builds the payload in its natural Go types,
//  2. runs the exact verifyCID normalise+marshal path to get canonical bytes,
//  3. unmarshals those bytes back into workCanonicalPayload — which is literally
//     what the server does with what the client sends — and re-runs the path,
//     asserting the result is byte-identical. That fixed-point check is what
//     proves a client that emits these bytes will survive the server's
//     unmarshal/re-marshal round trip (interface{} numbers become float64,
//     poll_options becomes []interface{}, and so on).
//  4. emits the input description, canonical bytes and CID as a fixture.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// ─── VERBATIM from internal/handler/work_event.go ────────────────────────────

type workCanonicalPayload struct {
	AuthorPIAL        string      `json:"author_pial"`
	Body              string      `json:"body"`
	CommentGating     string      `json:"comment_gating"`
	IsRepost          bool        `json:"is_repost"`
	Kind              string      `json:"kind"`
	MediaURLs         []string    `json:"media_urls"`
	ParentCID         interface{} `json:"parent_cid"`
	PollEndsAt        interface{} `json:"poll_ends_at"`
	PollOptions       interface{} `json:"poll_options"`
	RepostSourceID    interface{} `json:"repost_source_id"`
	ScheduledAt       interface{} `json:"scheduled_at"`
	SubscriberOnly    bool        `json:"subscriber_only"`
	Tags              []string    `json:"tags"`
	TimestampMS       int64       `json:"timestamp_ms"`
	VideoDurationSecs interface{} `json:"video_duration_secs"`
	VideoMasterURL    interface{} `json:"video_master_url"`
	VideoPosterURL    interface{} `json:"video_poster_url"`
	VoiceDurationSecs interface{} `json:"voice_duration_secs"`
	VoiceURL          interface{} `json:"voice_url"`
}

// canonicalBytes is verifyCID with the comparison replaced by a return of the
// bytes it hashes. Every line before the hash is verbatim.
func canonicalBytes(p workCanonicalPayload) []byte {
	if p.MediaURLs == nil {
		p.MediaURLs = []string{}
	}
	sort.Strings(p.MediaURLs)
	if p.Tags == nil {
		p.Tags = []string{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		panic(err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func cidOf(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return fmt.Sprintf("sha256:%x", sum)
}

// ─── Fixture shapes ──────────────────────────────────────────────────────────

// input mirrors the payload in natural types. Swift decodes this and must
// rebuild the canonical bytes from it. media_urls is deliberately given
// unsorted in some cases: sorting is part of what the client has to do.
type input struct {
	AuthorPIAL        string   `json:"author_pial"`
	Body              string   `json:"body"`
	CommentGating     string   `json:"comment_gating"`
	IsRepost          bool     `json:"is_repost"`
	Kind              string   `json:"kind"`
	MediaURLs         []string `json:"media_urls"`
	ParentCID         *string  `json:"parent_cid"`
	PollEndsAt        *string  `json:"poll_ends_at"`
	PollOptions       []string `json:"poll_options"`
	RepostSourceID    *string  `json:"repost_source_id"`
	ScheduledAt       *string  `json:"scheduled_at"`
	SubscriberOnly    bool     `json:"subscriber_only"`
	Tags              []string `json:"tags"`
	TimestampMS       int64    `json:"timestamp_ms"`
	VideoDurationSecs *int     `json:"video_duration_secs"`
	VideoMasterURL    *string  `json:"video_master_url"`
	VideoPosterURL    *string  `json:"video_poster_url"`
	VoiceDurationSecs *int     `json:"voice_duration_secs"`
	VoiceURL          *string  `json:"voice_url"`
}

type fixture struct {
	Name         string `json:"name"`
	Note         string `json:"note"`
	Input        input  `json:"input"`
	Canonical    string `json:"canonical"`
	CanonicalB64 string `json:"canonical_b64"`
	CID          string `json:"cid"`
}

func s(v string) *string { return &v }
func i(v int) *int       { return &v }

// toPayload turns the fixture input into the Go struct the server holds.
func (in input) toPayload() workCanonicalPayload {
	p := workCanonicalPayload{
		AuthorPIAL:     in.AuthorPIAL,
		Body:           in.Body,
		CommentGating:  in.CommentGating,
		IsRepost:       in.IsRepost,
		Kind:           in.Kind,
		MediaURLs:      append([]string(nil), in.MediaURLs...),
		SubscriberOnly: in.SubscriberOnly,
		Tags:           append([]string(nil), in.Tags...),
		TimestampMS:    in.TimestampMS,
	}
	if in.ParentCID != nil {
		p.ParentCID = *in.ParentCID
	}
	if in.PollEndsAt != nil {
		p.PollEndsAt = *in.PollEndsAt
	}
	if in.PollOptions != nil {
		p.PollOptions = in.PollOptions
	}
	if in.RepostSourceID != nil {
		p.RepostSourceID = *in.RepostSourceID
	}
	if in.ScheduledAt != nil {
		p.ScheduledAt = *in.ScheduledAt
	}
	if in.VideoDurationSecs != nil {
		p.VideoDurationSecs = *in.VideoDurationSecs
	}
	if in.VideoMasterURL != nil {
		p.VideoMasterURL = *in.VideoMasterURL
	}
	if in.VideoPosterURL != nil {
		p.VideoPosterURL = *in.VideoPosterURL
	}
	if in.VoiceDurationSecs != nil {
		p.VoiceDurationSecs = *in.VoiceDurationSecs
	}
	if in.VoiceURL != nil {
		p.VoiceURL = *in.VoiceURL
	}
	return p
}

const pial = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"

func cases() []struct {
	name string
	note string
	in   input
} {
	base := func() input {
		return input{
			AuthorPIAL:    pial,
			Body:          "",
			CommentGating: "everyone",
			Kind:          "post",
			MediaURLs:     []string{},
			Tags:          []string{},
			TimestampMS:   1767225600000,
		}
	}
	type c = struct {
		name string
		note string
		in   input
	}
	out := []c{}

	add := func(name, note string, mut func(*input)) {
		in := base()
		mut(&in)
		out = append(out, c{name, note, in})
	}

	add("empty_body_post", "the minimum work: nothing but an author, a kind and a clock", func(in *input) {})

	add("plain_post", "one line of ASCII", func(in *input) {
		in.Body = "hello f33d3r"
	})

	add("html_chars_body", "& < > — the SetEscapeHTML(false) trap. These must appear raw.", func(in *input) {
		in.Body = "a & b < c > d </script> \"quoted\" back\\slash"
	})

	add("unicode_body", "non-ASCII stays raw UTF-8, never \\uXXXX", func(in *input) {
		in.Body = "café — naïve — Ω — 日本語 — Здравствуйте — العربية"
	})

	add("emoji_body", "astral-plane code points: surrogate pairs in UTF-16, 4 bytes in UTF-8", func(in *input) {
		in.Body = "🜃 malkuth 🔥 ship it 👩‍💻 family: 👨‍👩‍👧‍👦 flag: 🏴󠁧󠁢󠁳󠁣󠁴󠁿"
	})

	add("control_chars_body", "newline/tab/CR use short escapes; other C0 use \\u00xx; DEL is NOT escaped", func(in *input) {
		in.Body = "line1\nline2\ttabbed\r\nbell:\a null-ish:\x01 vertical:\v formfeed:\f del:\x7f"
	})

	add("backspace_and_formfeed_body", "\\b and \\f are short escapes in Go 1.22+ (and in JSON.stringify); older Go emitted \\u0008/\\u000c", func(in *input) {
		in.Body = "back\bspace form\ffeed unit\u001fsep nul\u0000byte"
	})

	add("line_separator_body", "U+2028/U+2029 — Go escapes these only when escapeHTML is on", func(in *input) {
		in.Body = "before after end"
	})

	add("no_media", "explicit empty array, never null", func(in *input) {
		in.Body = "no attachments"
	})

	add("one_media", "single image", func(in *input) {
		in.Body = "one image"
		in.MediaURLs = []string{"/media/2026/09/a1b2c3.webp"}
	})

	add("several_media_unsorted", "input order is NOT the hashed order — the server sorts before hashing", func(in *input) {
		in.Body = "four images"
		in.MediaURLs = []string{
			"/media/2026/09/zeta.webp",
			"/media/2026/09/alpha.webp",
			"/media/2026/09/Beta.webp",
			"/media/2026/09/_underscore.webp",
		}
	})

	add("media_sort_is_bytewise", "uppercase sorts before lowercase; a non-ASCII name sorts by UTF-8 bytes", func(in *input) {
		in.Body = "sort order proof"
		in.MediaURLs = []string{"/media/é.webp", "/media/z.webp", "/media/Z.webp", "/media/a.webp", "/media/A.webp"}
	})

	add("reply", "kind=reply carries parent_cid", func(in *input) {
		in.Kind = "reply"
		in.Body = "replying to you"
		in.ParentCID = s("sha256:9f2c1f7bcb1e0a4a4c2f0b6f0e1d8a3b5c7d9e0f1a2b3c4d5e6f708192a3b4c5")
	})

	add("quote", "kind=quote — quoted_work_id rides outside the signature", func(in *input) {
		in.Kind = "quote"
		in.Body = "adding to this"
	})

	add("poll", "poll options and an RFC3339 end time", func(in *input) {
		in.Kind = "poll"
		in.Body = "which one?"
		in.PollOptions = []string{"Deep Space", "Flow State", "Soft Power", "Sharp Edge"}
		in.PollEndsAt = s("2026-09-12T18:30:00Z")
	})

	add("poll_empty_options_array", "an empty poll_options array is [] not null — a real distinction on the wire", func(in *input) {
		in.Kind = "poll"
		in.Body = "options still being written"
		in.PollOptions = []string{}
	})

	add("repost", "is_repost with a source UUID", func(in *input) {
		in.IsRepost = true
		in.RepostSourceID = s("7c9e6679-7425-40de-944b-e07fc1f90ae7")
		in.Body = ""
	})

	add("scheduled", "scheduled_at set; Go silently ignores a non-RFC3339 value, so the client must validate", func(in *input) {
		in.Body = "goes out later"
		in.ScheduledAt = s("2026-12-25T09:00:00Z")
	})

	add("scheduled_with_offset", "RFC3339 with a numeric zone offset, not Z", func(in *input) {
		in.Body = "timezone offset form"
		in.ScheduledAt = s("2026-12-25T09:00:00+02:00")
	})

	add("voice", "voice_url and an integer duration", func(in *input) {
		in.Kind = "voice"
		in.Body = "listen"
		in.VoiceURL = s("/media/voice/2026/09/note.m4a")
		in.VoiceDurationSecs = i(47)
	})

	add("voice_zero_duration", "a 0-second duration is 0, not null — the JS `|| null` coercion loses this", func(in *input) {
		in.Kind = "voice"
		in.VoiceURL = s("/media/voice/2026/09/blip.m4a")
		in.VoiceDurationSecs = i(0)
	})

	add("video", "HLS master, poster and duration", func(in *input) {
		in.Kind = "video"
		in.Body = "watch this"
		in.VideoMasterURL = s("/media/video/2026/09/abc/master.m3u8")
		in.VideoPosterURL = s("/media/video/2026/09/abc/poster.webp")
		in.VideoDurationSecs = i(184)
	})

	add("react_video", "react_video kind with a layout that rides outside the signature", func(in *input) {
		in.Kind = "react_video"
		in.Body = "reacting"
		in.VideoMasterURL = s("/media/video/2026/09/def/master.m3u8")
		in.VideoDurationSecs = i(612)
	})

	add("thread_post", "thread_post kind", func(in *input) {
		in.Kind = "thread_post"
		in.Body = "1/ a thread"
	})

	add("empty_tags", "tags [] when nothing was tagged", func(in *input) {
		in.Body = "untagged"
	})

	add("populated_tags", "tags keep input order — unlike media_urls they are NOT sorted", func(in *input) {
		in.Body = "tagged #zeta #alpha"
		in.Tags = []string{"zeta", "alpha", "Mixed-Case", "日本語"}
	})

	add("gating_followers", "comment_gating other than the default", func(in *input) {
		in.Body = "followers may reply"
		in.CommentGating = "followers"
	})

	add("gating_none", "replies closed", func(in *input) {
		in.Body = "no replies"
		in.CommentGating = "none"
	})

	add("gating_circle", "circle gating", func(in *input) {
		in.Body = "circle only"
		in.CommentGating = "circle"
	})

	add("subscriber_only", "subscriber_only true", func(in *input) {
		in.Body = "for subscribers"
		in.SubscriberOnly = true
	})

	add("everything_at_once", "every optional field populated simultaneously", func(in *input) {
		in.Body = "everything <at> once & then some — 🜃"
		in.CommentGating = "circle"
		in.IsRepost = true
		in.Kind = "video"
		in.MediaURLs = []string{"/media/b.webp", "/media/a.webp"}
		in.ParentCID = s("sha256:0000000000000000000000000000000000000000000000000000000000000001")
		in.PollEndsAt = s("2027-01-01T00:00:00Z")
		in.PollOptions = []string{"yes", "no"}
		in.RepostSourceID = s("7c9e6679-7425-40de-944b-e07fc1f90ae7")
		in.ScheduledAt = s("2027-01-01T00:00:00Z")
		in.SubscriberOnly = true
		in.Tags = []string{"one", "two"}
		in.TimestampMS = 1893456000123
		in.VideoDurationSecs = i(3600)
		in.VideoMasterURL = s("/media/video/master.m3u8")
		in.VideoPosterURL = s("/media/video/poster.webp")
		in.VoiceDurationSecs = i(12)
		in.VoiceURL = s("/media/voice/clip.m4a")
	})

	add("timestamp_boundaries", "int64 timestamp far past JS Number-safe formatting concerns", func(in *input) {
		in.Body = "clock"
		in.TimestampMS = 9007199254740993
	})

	add("solidus_and_quotes", "forward slash is never escaped; quote and backslash always are", func(in *input) {
		in.Body = `path/to/thing "quoted" \ backslash \" both`
	})

	return out
}

func main() {
	outPath := "golden_canonical.json"
	if len(os.Args) > 1 {
		outPath = os.Args[1]
	}

	fixtures := []fixture{}
	seen := map[string]string{}

	for _, c := range cases() {
		p := c.in.toPayload()
		canonical := canonicalBytes(p)

		// Fixed-point check: what the server does with these exact bytes.
		var round workCanonicalPayload
		if err := json.Unmarshal(canonical, &round); err != nil {
			panic(fmt.Sprintf("%s: server-side unmarshal failed: %v", c.name, err))
		}
		again := canonicalBytes(round)
		if !bytes.Equal(canonical, again) {
			panic(fmt.Sprintf(
				"%s: NOT a fixed point under the server's unmarshal/re-marshal\n  first: %s\n  again: %s",
				c.name, canonical, again))
		}

		cid := cidOf(canonical)
		if prev, dup := seen[cid]; dup {
			panic(fmt.Sprintf("%s collides with %s on cid %s", c.name, prev, cid))
		}
		seen[cid] = c.name

		fixtures = append(fixtures, fixture{
			Name:         c.name,
			Note:         c.note,
			Input:        c.in,
			Canonical:    string(canonical),
			CanonicalB64: base64.StdEncoding.EncodeToString(canonical),
			CID:          cid,
		})
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]interface{}{
		"generated_by": "feed-engine/internal/handler/work_event.go workCanonicalPayload + verifyCID, " +
			"copied verbatim into /tmp/malkuth-golden/main.go. Regenerate after any change to that struct.",
		"cases": fixtures,
	}); err != nil {
		panic(err)
	}
	if err := os.WriteFile(outPath, buf.Bytes(), 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %d cases to %s\n", len(fixtures), outPath)
}
