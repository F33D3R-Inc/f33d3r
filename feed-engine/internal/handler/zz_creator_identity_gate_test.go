package handler

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/f33d3r/feed-engine/internal/model"
)

// TestDenyCreatorPanelRefusesAgeAttestation exercises the single admission check
// every /create/partials/* surface runs. Before this existed each of the eight
// panels carried its own membership check and none carried a verification check,
// so /create rendered the verification page while /create/partials/earnings
// served the earnings surface to the same unidentified account.
func TestDenyCreatorPanelRefusesAgeAttestation(t *testing.T) {
	h := &Handler{}

	cases := []struct {
		name       string
		user       *model.User
		wantDenied bool
	}{
		{
			name: "creator at basic tier with age_verified is refused",
			user: &model.User{
				Role: model.RoleUser, IsCreator: true,
				KYCTier: model.KYCTierBasic, IsVerified: true, IsAgeVerified: true,
			},
			wantDenied: true,
		},
		{
			name: "creator at soft tier is refused — document read, face never checked",
			user: &model.User{
				Role: model.RoleUser, IsCreator: true,
				KYCTier: model.KYCTierSoft, IsVerified: true, IsAgeVerified: true,
			},
			wantDenied: true,
		},
		{
			name: "creator at full tier is admitted",
			user: &model.User{
				Role: model.RoleUser, IsCreator: true,
				KYCTier: model.KYCTierFull, IsVerified: true, IsAgeVerified: true,
			},
			wantDenied: false,
		},
		{
			name: "identity-verified non-creator is still refused a creator surface",
			user: &model.User{
				Role: model.RoleUser, IsCreator: false, KYCTier: model.KYCTierFull,
			},
			wantDenied: true,
		},
		{
			name:       "admin bypass is preserved",
			user:       &model.User{Role: model.RoleAdmin, KYCTier: model.KYCTierBasic},
			wantDenied: false,
		},
	}

	for _, c := range cases {
		w := httptest.NewRecorder()
		got := h.denyCreatorPanel(w, c.user)
		if got != c.wantDenied {
			t.Errorf("%s: denyCreatorPanel = %v, want %v", c.name, got, c.wantDenied)
		}
		if got && w.Code != 403 {
			t.Errorf("%s: denied with status %d, want 403", c.name, w.Code)
		}
	}
}

// identityPredicateNames are the only ways a call site is allowed to ask the
// identity question. Anything else — a bare kyc_tier string comparison — is the
// drift the predicate exists to prevent.
var identityPredicateNames = []string{"CanMonetize(", "IsIdentityVerified(", "KYCTierIsIdentity("}

// uploadSurfaceFiles hold the handlers behind uploading, posting and going live.
// The owner's requirement draws the line here explicitly: uploading needs no ID,
// only being a creator does. If the identity predicate ever appears in one of
// these files, uploading has been gated and that is a regression, not a feature.
var uploadSurfaceFiles = []string{
	"media.go",       // /upload/post-media, /upload/avatar, /upload/header, /upload/voice
	"tus.go",         // resumable large-file upload
	"music.go",       // /upload/track
	"react_video.go", // /upload/react-video
	"live.go",        // /golive, /live/start, WHIP publish
	"post.go",        // work composition
	"work_event.go",  // the work write path
}

func TestUploadPostAndLiveAreNotIdentityGated(t *testing.T) {
	for _, name := range uploadSurfaceFiles {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v — the file this invariant is pinned against must exist", name, err)
		}
		for _, pred := range identityPredicateNames {
			if strings.Contains(string(src), pred) {
				t.Errorf("%s calls %s — uploading, posting and going live must remain reachable "+
					"without identity verification. Only creator status and payout are gated.", name, pred)
			}
		}
	}
}

// creatorAndPayoutSurfaces are the files that must ask the identity question.
// This is the other half of the pin: if a gate is deleted or quietly reverted to
// IsVerified, this fails.
var creatorAndPayoutSurfaces = []string{
	"creator_dashboard.go",  // /create and every /create/partials/* panel
	"creator_onboarding.go", // /create/setup and creator.setup.complete
	"shop.go",               // /api/creator/enable, /api/subscribe
	"marketplace_pages.go",  // /create/marketplace seller dashboard
	"marketplace.go",        // marketplace.shop.open / listing.create
	"wallet.go",             // /ainsoph/v1/withdraw
	"kyc.go",                // adult creator activation
}

func TestCreatorAndPayoutSurfacesAskTheIdentityPredicate(t *testing.T) {
	for _, name := range creatorAndPayoutSurfaces {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := false
		for _, pred := range identityPredicateNames {
			if strings.Contains(string(src), pred) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s asks no identity question — a creator or payout surface must call one of %v",
				name, identityPredicateNames)
		}
	}
}

// TestCreatorVerificationGateHasADoor pins the walkable path. A gate that blocks
// a user without telling them where to get verified is worse than no gate, so
// the page the creator gate renders must link to the real eKYC flow.
func TestCreatorVerificationGateHasADoor(t *testing.T) {
	path := filepath.Join("..", "..", "web", "templates", "creator_verification_required.html")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the page the creator gate renders is missing: %v", err)
	}
	if !strings.Contains(string(src), `href="/kyc"`) {
		t.Error("creator_verification_required.html does not link to /kyc — the gate has no door")
	}
}
