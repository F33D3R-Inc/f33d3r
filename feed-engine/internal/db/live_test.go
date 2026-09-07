package db

import (
	"database/sql"
	"errors"
	"testing"

	_ "github.com/lib/pq"
)

// TestStreamIDGate pins the rule that keeps a malformed identifier out of the
// database. live_streams.id is a uuid column: an identifier that cannot name a
// row is a not-found, decided here, and never a query Postgres has to reject.
//
// This is the gate every live route taking {id} passes through — watch,
// broadcast, heartbeat, chat, end, encoder reveal and the WHIP publish proxy all
// reach their stream through this package.
func TestStreamIDGate(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want bool // true when the id is allowed through to the database
	}{
		{"canonical uuid", "6a52b472-fce5-4fb4-b263-0421c3777fec", true},
		{"upper case uuid", "6A52B472-FCE5-4FB4-B263-0421C3777FEC", true},
		{"a word", "new", false},
		{"empty", "", false},
		{"path traversal", "../../etc/passwd", false},
		{"sql fragment", "1' OR '1'='1", false},
		{"uuid with trailing text", "6a52b472-fce5-4fb4-b263-0421c3777fec/x", false},
		{"unhyphenated 32 hex", "6a52b472fce54fb4b2630421c3777fec", false},
		{"braced form", "{6a52b472-fce5-4fb4-b263-0421c3777fec}", false},
		// Postgres rejects the URN form outright, so accepting it here would put
		// the 500 straight back.
		{"urn form", "urn:uuid:6a52b472-fce5-4fb4-b263-0421c3777fec", false},
	}
	for _, c := range cases {
		err := checkStreamID(c.id)
		if c.want && err != nil {
			t.Errorf("%s: %q was refused: %v", c.name, c.id, err)
		}
		if !c.want {
			if err == nil {
				t.Errorf("%s: %q reached the database", c.name, c.id)
			} else if !errors.Is(err, ErrLiveStreamNotFound) {
				t.Errorf("%s: %q gave %v, want ErrLiveStreamNotFound", c.name, c.id, err)
			}
		}
	}
}

// TestStreamReadsRefuseMalformedID proves the gate is reached before a query is
// built, so a bad path segment can never become a driver error and therefore
// never a 500.
//
// sql.Open does not connect — it hands back a lazy handle — so any of these
// calls that did reach Postgres would fail on the unreachable address instead of
// answering ErrLiveStreamNotFound, and the test would say so.
func TestStreamReadsRefuseMalformedID(t *testing.T) {
	handle, err := sql.Open("postgres", "postgres://nobody@127.0.0.1:1/nothing?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("open lazy handle: %v", err)
	}
	defer handle.Close()

	const bad = "new"

	if _, err := GetLiveStreamByID(handle, bad); !errors.Is(err, ErrLiveStreamNotFound) {
		t.Errorf("GetLiveStreamByID: %v, want ErrLiveStreamNotFound", err)
	}
	if err := EndStream(handle, bad); !errors.Is(err, ErrLiveStreamNotFound) {
		t.Errorf("EndStream: %v, want ErrLiveStreamNotFound", err)
	}
	if err := StartStream(handle, bad, 1920, 1080, "webrtc"); !errors.Is(err, ErrLiveStreamNotFound) {
		t.Errorf("StartStream: %v, want ErrLiveStreamNotFound", err)
	}
	if err := UpdateViewerCount(handle, bad, 3); !errors.Is(err, ErrLiveStreamNotFound) {
		t.Errorf("UpdateViewerCount: %v, want ErrLiveStreamNotFound", err)
	}
	if _, err := RotateIngestKey(handle, bad); !errors.Is(err, ErrLiveStreamNotFound) {
		t.Errorf("RotateIngestKey: %v, want ErrLiveStreamNotFound", err)
	}
	if err := SetStreamMeta(handle, bad, "t", "d"); !errors.Is(err, ErrLiveStreamNotFound) {
		t.Errorf("SetStreamMeta: %v, want ErrLiveStreamNotFound", err)
	}
	if _, err := AuthorizeIngest(handle, bad, "somekey"); !errors.Is(err, ErrLiveStreamNotFound) {
		t.Errorf("AuthorizeIngest: %v, want ErrLiveStreamNotFound", err)
	}
	if err := SetStreamScanState(handle, bad, "blocked", "r", "a"); !errors.Is(err, ErrLiveStreamNotFound) {
		t.Errorf("SetStreamScanState: %v, want ErrLiveStreamNotFound", err)
	}
	if err := RecordScanSample(handle, LiveScanSample{StreamID: bad}); !errors.Is(err, ErrLiveStreamNotFound) {
		t.Errorf("RecordScanSample: %v, want ErrLiveStreamNotFound", err)
	}
	if _, err := GetStreamForAuthor(handle, "demo_user"); !errors.Is(err, ErrLiveStreamNotFound) {
		t.Errorf("GetStreamForAuthor: %v, want ErrLiveStreamNotFound", err)
	}
}
