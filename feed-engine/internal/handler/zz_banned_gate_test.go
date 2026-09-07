package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/f33d3r/feed-engine/internal/scanclient"
)

// The gate must never admit on an unanswered exact-bytes lookup. With no
// database handle there is no registry to clear the content against, so the
// only correct answer is a refusal.
func TestBannedGateRefusesWhenRegistryCannotBeConsulted(t *testing.T) {
	h := &Handler{}
	err := h.bannedContentGate(context.Background(), gateInput{
		Kind:     scanclient.KindImage,
		Filename: "photo.jpg",
		Data:     []byte("bytes"),
		SHA256:   strings.Repeat("a", 64),
		PIALID:   "pial-1",
		Label:    "unit-test",
	})
	if err == nil {
		t.Fatal("gate admitted an upload it could not check")
	}
	if !errors.Is(err, errBanGateUnavailable) {
		t.Fatalf("want errBanGateUnavailable, got %v", err)
	}
	if errors.Is(err, errMediaBanned) {
		t.Fatal("an unanswered lookup was reported as a registry match")
	}
}

// The two refusal reasons must reach the browser as different statuses: a match
// is the user's content being rejected, an unavailable gate is ours.
func TestBanGateHTTPErrorStatuses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		body string
	}{
		{
			name: "registry match",
			err:  errMediaBanned,
			want: http.StatusBadRequest,
			body: "This content cannot be uploaded",
		},
		{
			name: "wrapped registry match",
			err:  fmt.Errorf("image-upload: %w", errMediaBanned),
			want: http.StatusBadRequest,
			body: "This content cannot be uploaded",
		},
		{
			name: "gate could not answer",
			err:  fmt.Errorf("%w: dial tcp: refused", errBanGateUnavailable),
			want: http.StatusServiceUnavailable,
			body: "content safety check could not run",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			banGateHTTPError(rec, tc.err)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if !strings.Contains(rec.Body.String(), tc.body) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tc.body)
			}
		})
	}
}

// A refusal must never leak which hash matched or what category it carries.
func TestBanGateResponseLeaksNothingAboutTheMatch(t *testing.T) {
	rec := httptest.NewRecorder()
	banGateHTTPError(rec, errMediaBanned)
	body := strings.ToLower(rec.Body.String())
	for _, leak := range []string{"csam", "sha256", "phash", "hash", "registry"} {
		if strings.Contains(body, leak) {
			t.Errorf("refusal body mentions %q: %q", leak, rec.Body.String())
		}
	}
}

func TestHashPrefixHandlesShortDigests(t *testing.T) {
	cases := map[string]string{
		"":                      "",
		"abcd":                  "abcd",
		strings.Repeat("f", 64): strings.Repeat("f", 16),
	}
	for in, want := range cases {
		if got := hashPrefix(in); got != want {
			t.Errorf("hashPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// The registry must refuse entries that could not do their job — malformed ones
// that match nothing, and degenerate perceptual hashes that match far too much.
func TestValidateBannedHashValue(t *testing.T) {
	cases := []struct {
		name     string
		hashType string
		value    string
		wantErr  bool
	}{
		{"good sha256", "sha256", strings.Repeat("a1", 32), false},
		{"short sha256", "sha256", "abc123", true},
		{"uppercase sha256", "sha256", strings.Repeat("A1", 32), true},
		{"non-hex sha256", "sha256", strings.Repeat("z", 64), true},
		{"good phash", "phash", "d3d96c37305a8f05", false},
		{"phash wrong length", "phash", "d3d96c37305a8f", true},
		{"phash of a uniform frame, all zero bits", "phash", "0000000000000000", true},
		{"phash of a uniform frame, all one bits", "phash", "ffffffffffffffff", true},
		{"phash with too few bits set", "phash", "0000000000000107", true},
		{"good audio fingerprint", "audio_fp", "AQAAG0mUaEkSZSoAAAAAAAAAAAAA", false},
		{"truncated audio fingerprint", "audio_fp", "AQAA", true},
		{"unknown type", "md5", strings.Repeat("a", 32), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBannedHashValue(tc.hashType, tc.value)
			if tc.wantErr && err == nil {
				t.Errorf("%s %q was accepted", tc.hashType, tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("%s %q was refused: %v", tc.hashType, tc.value, err)
			}
		})
	}
}
