package handler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
		{"/media/media-derived/posts/id/master.m3u8", "/media/media-derived/posts/id/master.m3u8"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := canonicalMediaPath(tc.in); got != tc.want {
			t.Fatalf("canonicalMediaPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMediaAssetStem pins variant matching: the work stores only the primary
// derivative, so the full-resolution variant must resolve to the same asset.
func TestMediaAssetStem(t *testing.T) {
	const uuid = "6f1c2d3e-4a5b-6c7d-8e9f-0a1b2c3d4e5f"
	dir := "/static/media/posts/"
	for _, variant := range []string{"-thumb.webp", "-feed.webp", "-full.webp", ".webp"} {
		if got := mediaAssetStem(dir + uuid + variant); got != dir+uuid {
			t.Fatalf("mediaAssetStem(%q) = %q, want %q", dir+uuid+variant, got, dir+uuid)
		}
	}
	// Too short to be an asset id — no stem, so no over-broad match.
	if got := mediaAssetStem("/static/media/posts/x/clean/720p/index.m3u8"); got != "" {
		t.Fatalf("short stem = %q, want empty", got)
	}
}

// TestMediaNeedsWorkLookup pins which objects cost a database lookup: only work
// media does. Avatars and headers carry no work-level rules.
func TestMediaNeedsWorkLookup(t *testing.T) {
	cases := map[string]bool{
		"/static/media/posts/abc.webp":              true,
		"/static/media/posts/id/clean/master.m3u8":  true,
		"/media/media-derived/posts/id/master.m3u8": true,
		"/static/media/avatars/abc.webp":            false,
		"/static/media/headers/abc.webp":            false,
		"/static/uploads/covers/abc.webp":           false,
	}
	for path, want := range cases {
		if got := mediaNeedsWorkLookup(path); got != want {
			t.Fatalf("mediaNeedsWorkLookup(%q) = %v, want %v", path, got, want)
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

	h := &Handler{} // no db: the wall has nothing to authorize against
	srv := h.staticMediaHandler()

	cases := []struct {
		path string
		want int
	}{
		{"/static/media/avatars/a.webp", http.StatusOK},
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

// TestMediaCandidates pins the derived lookup keys: the image derivatives of one
// asset, and the HLS master that owns a stream object from any depth beneath it.
func TestMediaCandidates(t *testing.T) {
	const asset = "77c274c2-7026-440a-b8bf-88c1e0642000"

	derivatives, _ := mediaCandidates("/static/media/posts/2944d0c7-5ef7-4334-aa9f-89ac6b21fd11-full.webp")
	want := "/static/media/posts/2944d0c7-5ef7-4334-aa9f-89ac6b21fd11-feed.webp"
	if !contains(derivatives, want) {
		t.Fatalf("derivatives %v missing the stored primary %q", derivatives, want)
	}

	_, masters := mediaCandidates("/static/media/posts/" + asset + "/clean/720p/index.m3u8")
	wantMaster := "/static/media/posts/" + asset + "/clean/master.m3u8"
	if !contains(masters, wantMaster) {
		t.Fatalf("masters %v missing %q", masters, wantMaster)
	}

	_, posterMasters := mediaCandidates("/static/media/posts/" + asset + "/clean/poster.jpg")
	if !contains(posterMasters, wantMaster) {
		t.Fatalf("poster masters %v missing %q", posterMasters, wantMaster)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
