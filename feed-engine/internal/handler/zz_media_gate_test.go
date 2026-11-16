package handler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/f33d3r/feed-engine/internal/caeor"
	"github.com/f33d3r/feed-engine/internal/model"
)

func adultViewer() *model.User {
	return &model.User{ID: "11111111-1111-1111-1111-111111111111", ContentSetting: "adult_enabled", IsAdult: true}
}

func minorViewer() *model.User {
	return &model.User{ID: "22222222-2222-2222-2222-222222222222", ContentSetting: "default", IsMinor: true}
}

// TestMediaWallClosesCapturedURLs pins the read-path wall: every one of these
// requests reached MinIO unauthorized before the gate existed.
func TestMediaWallClosesCapturedURLs(t *testing.T) {
	const author = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	cases := []struct {
		name   string
		owner  mediaOwner
		viewer *model.User
		access authorAccess
		want   int
	}{
		{
			name:   "live public work is served",
			owner:  mediaOwner{AuthorID: author},
			viewer: adultViewer(),
			want:   0,
		},
		{
			name:   "captured url replayed after the ephemeral work expired",
			owner:  mediaOwner{AuthorID: author, Expired: true},
			viewer: adultViewer(),
			want:   http.StatusNotFound,
		},
		{
			name:   "media of a deleted work",
			owner:  mediaOwner{AuthorID: author, Deleted: true},
			viewer: adultViewer(),
			want:   http.StatusNotFound,
		},
		{
			name:   "media of a work taken down by moderation",
			owner:  mediaOwner{AuthorID: author, Removed: true},
			viewer: adultViewer(),
			want:   http.StatusNotFound,
		},
		{
			name:   "expired work is still closed to its own author",
			owner:  mediaOwner{AuthorID: author, Expired: true},
			viewer: adultViewer(),
			access: authorAccess{IsOwner: true},
			want:   http.StatusNotFound,
		},
		{
			name:   "non-follower asking a private account directly",
			owner:  mediaOwner{AuthorID: author, AuthorIsPrivate: true},
			viewer: adultViewer(),
			want:   http.StatusNotFound,
		},
		{
			name:   "follower of a private account",
			owner:  mediaOwner{AuthorID: author, AuthorIsPrivate: true},
			viewer: adultViewer(),
			access: authorAccess{IsFollowing: true},
			want:   0,
		},
		{
			name:   "private account owner reads their own media",
			owner:  mediaOwner{AuthorID: author, AuthorIsPrivate: true},
			viewer: adultViewer(),
			access: authorAccess{IsOwner: true},
			want:   0,
		},
		{
			name:   "blocked viewer",
			owner:  mediaOwner{AuthorID: author},
			viewer: adultViewer(),
			access: authorAccess{IsBlocked: true},
			want:   http.StatusNotFound,
		},
		{
			name:   "block outranks a follow that predates it",
			owner:  mediaOwner{AuthorID: author, AuthorIsPrivate: true},
			viewer: adultViewer(),
			access: authorAccess{IsFollowing: true, IsBlocked: true},
			want:   http.StatusNotFound,
		},
		{
			name:   "paid media without a subscription",
			owner:  mediaOwner{AuthorID: author, SubscriberOnly: true},
			viewer: adultViewer(),
			want:   http.StatusForbidden,
		},
		{
			name:   "paid media with an active subscription",
			owner:  mediaOwner{AuthorID: author, SubscriberOnly: true},
			viewer: adultViewer(),
			access: authorAccess{IsSubscribed: true},
			want:   0,
		},
		{
			name:   "adult media requested anonymously",
			owner:  mediaOwner{AuthorID: author, IsNSFW: true},
			viewer: nil,
			want:   http.StatusForbidden,
		},
		{
			name:   "adult media requested by a minor",
			owner:  mediaOwner{AuthorID: author, IsNSFW: true},
			viewer: minorViewer(),
			want:   http.StatusForbidden,
		},
		{
			name:   "gore media requested by a safe-mode account",
			owner:  mediaOwner{AuthorID: author, IsGore: true},
			viewer: &model.User{ID: "33333333-3333-3333-3333-333333333333", ContentSetting: "safe_mode"},
			want:   http.StatusForbidden,
		},
		{
			name:   "adult creator media requested anonymously",
			owner:  mediaOwner{AuthorID: author, AuthorIsAdultCreator: true},
			viewer: nil,
			want:   http.StatusForbidden,
		},
		{
			name:   "adult media requested by an adult account",
			owner:  mediaOwner{AuthorID: author, IsNSFW: true},
			viewer: adultViewer(),
			want:   0,
		},
		{
			name:   "author always reads their own adult media",
			owner:  mediaOwner{AuthorID: author, IsNSFW: true},
			viewer: minorViewer(),
			access: authorAccess{IsOwner: true},
			want:   0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideMediaAccess(tc.owner, tc.viewer, tc.access); got != tc.want {
				t.Fatalf("decideMediaAccess = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCanonicalMediaPath pins the one key signing and gating share. A path that
// canonicalized differently on the two sides would be a wall with a seam in it.
func TestCanonicalMediaPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/static/media/posts/abc-feed.webp", "/static/media/posts/abc-feed.webp"},
		{"/static/media/posts/abc-feed.webp?tok=xyz", "/static/media/posts/abc-feed.webp"},
		{"/static/media/posts/abc-feed.webp#frag", "/static/media/posts/abc-feed.webp"},
		{"/static/media//posts/../posts/abc.webp", "/static/media/posts/abc.webp"},
		{"static/media/posts/abc.webp", "/static/media/posts/abc.webp"},
		{"/static/media/a/6f1c2d3e-4a5b-6c7d-8e9f-0a1b2c3d4e5f/hls/master.m3u8", "/static/media/a/6f1c2d3e-4a5b-6c7d-8e9f-0a1b2c3d4e5f/hls/master.m3u8"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := canonicalMediaPath(tc.in); got != tc.want {
			t.Fatalf("canonicalMediaPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestStaticMediaHandlerServesAndRefuses covers the wiring the gate sits on:
// real bytes still reach the browser, and nothing outside the media root does.
func TestStaticMediaHandlerServesAndRefuses(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "avatars"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "avatars", "a.webp"), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := mediaRoot
	mediaRoot = dir
	t.Cleanup(func() { mediaRoot = old })

	// No database and no Caeor: the wall has nothing to authorize against, so it
	// fails closed.
	// Existence is still resolved first, so what does not exist is still a 404.
	h := &Handler{}
	srv := h.staticMediaHandler()

	cases := []struct {
		path string
		want int
	}{
		{"/static/media/avatars/a.webp", http.StatusServiceUnavailable},
		{"/static/media/avatars/missing.webp", http.StatusNotFound},
		{"/static/media/avatars", http.StatusNotFound},
		{"/static/media/../../etc/passwd", http.StatusNotFound},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "http://f33d3r.com"+tc.path, nil)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("GET %s = %d, want %d", tc.path, w.Code, tc.want)
		}
		if tc.want == http.StatusOK && w.Body.String() != "bytes" {
			t.Fatalf("GET %s body = %q", tc.path, w.Body.String())
		}
	}
}

// TestMediaServable pins what the gate serves before any wall: only what Caeor
// calls servable, on a ready asset that is on the platform.
func TestMediaServable(t *testing.T) {
	ready := caeor.Resolved{AssetID: "a", Kind: caeor.KindImage, Status: caeor.StatusReady, Servable: true, Watermarked: true}
	cases := []struct {
		name string
		obj  *caeor.Resolved
		want bool
	}{
		{"unknown path", nil, false},
		{"ready, servable", &ready, true},
		{"caeor says not servable (download file, private source, awaiting re-mark)", func() *caeor.Resolved { o := ready; o.Servable = false; return &o }(), false},
		{"withdrawn", func() *caeor.Resolved { o := ready; o.Withdrawn = true; return &o }(), false},
		{"still processing", func() *caeor.Resolved { o := ready; o.Status = caeor.StatusProcessing; return &o }(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (&mediaGovernance{Object: tc.obj}).servable(); got != tc.want {
				t.Fatalf("servable = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDecideGovernedAccess pins the uploader rule: only the uploader's works
// govern their media, and media none of them carries is the uploader's alone.
func TestDecideGovernedAccess(t *testing.T) {
	const uploader = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const uploaderPIAL = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	obj := &caeor.Resolved{AssetID: "a", UploaderPIAL: uploaderPIAL, Kind: caeor.KindImage,
		Status: caeor.StatusReady, Servable: true, Watermarked: true}
	withdrawn := *obj
	withdrawn.Withdrawn = true

	uploaderViewer := adultViewer()
	uploaderViewer.ID = uploader
	uploaderViewer.PIALID = uploaderPIAL

	owner := map[string]authorAccess{uploader: {IsOwner: true}}
	stranger := map[string]authorAccess{uploader: {}}
	subscriber := map[string]authorAccess{uploader: {IsSubscribed: true}}

	cases := []struct {
		name   string
		gov    *mediaGovernance
		viewer *model.User
		walls  map[string]authorAccess
		want   int
	}{
		{"unknown to caeor", &mediaGovernance{}, adultViewer(), stranger, http.StatusNotFound},
		{"withdrawn asset, even to its uploader",
			&mediaGovernance{Object: &withdrawn, Owners: []mediaOwner{{AuthorID: uploader}}}, uploaderViewer, owner, http.StatusNotFound},
		{"unpublished, uploader previews", &mediaGovernance{Object: obj}, uploaderViewer, owner, 0},
		{"unpublished, anybody else", &mediaGovernance{Object: obj}, adultViewer(), subscriber, http.StatusNotFound},
		{"subscriber-only work, no subscription",
			&mediaGovernance{Object: obj, Owners: []mediaOwner{{AuthorID: uploader, SubscriberOnly: true}}}, adultViewer(), stranger, http.StatusForbidden},
		{"one public work of the uploader admits",
			&mediaGovernance{Object: obj, Owners: []mediaOwner{{AuthorID: uploader, SubscriberOnly: true}, {AuthorID: uploader}}}, adultViewer(), stranger, 0},
		{"blocked by the scanner",
			&mediaGovernance{Object: obj, Owners: []mediaOwner{{AuthorID: uploader, Removed: true}}}, adultViewer(), stranger, http.StatusNotFound},
		{"scheduled work, before release",
			&mediaGovernance{Object: obj, Owners: []mediaOwner{{AuthorID: uploader, Scheduled: true}}}, adultViewer(), stranger, http.StatusNotFound},
		{"scheduled work, its author",
			&mediaGovernance{Object: obj, Owners: []mediaOwner{{AuthorID: uploader, Scheduled: true}}}, uploaderViewer, owner, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideGovernedAccess(tc.gov, tc.viewer, tc.walls); got != tc.want {
				t.Fatalf("decideGovernedAccess = %d, want %d", got, tc.want)
			}
		})
	}
}
