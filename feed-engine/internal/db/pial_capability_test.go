package db

import (
	"errors"
	"testing"

	"github.com/f33d3r/feed-engine/internal/model"
)

// TestCapabilityFailsClosed pins the direction of failure. Every path that
// cannot produce a stored answer must deny: an identity with no PIAL root and an
// unreachable capability table both used to resolve to "allowed".
func TestCapabilityFailsClosed(t *testing.T) {
	t.Run("no pial root denies and says why", func(t *testing.T) {
		ok, err := CheckCapability(nil, "", model.CapPosting)
		if ok {
			t.Fatal("CheckCapability granted a capability to an identity with no PIAL root")
		}
		if !errors.Is(err, ErrNoPIAL) {
			t.Fatalf("err = %v, want ErrNoPIAL", err)
		}
		if HasCapability(nil, "", model.CapPosting) {
			t.Fatal("HasCapability granted a capability to an identity with no PIAL root")
		}
	})

	t.Run("unreadable capability store denies", func(t *testing.T) {
		ok, err := CheckCapability(nil, "00000000-0000-0000-0000-0000000000aa", model.CapPosting)
		if ok {
			t.Fatal("CheckCapability granted a capability it could not read")
		}
		if err == nil {
			t.Fatal("CheckCapability swallowed the lookup failure")
		}
		if HasCapability(nil, "00000000-0000-0000-0000-0000000000aa", model.CapPosting) {
			t.Fatal("HasCapability granted a capability it could not read")
		}
	})
}

// TestCapabilityMapStates pins the grant semantics the check relies on: only an
// explicit, unexpired grant is a yes.
func TestCapabilityMapStates(t *testing.T) {
	m := model.PIALCapabilityMap{
		model.CapPosting:   &model.PIALCapability{Capability: model.CapPosting, State: model.CapStateGranted},
		model.CapMessaging: &model.PIALCapability{Capability: model.CapMessaging, State: model.CapStateRevoked},
	}
	if !m.Can(model.CapPosting) {
		t.Fatal("granted capability read as denied")
	}
	if m.Can(model.CapMessaging) {
		t.Fatal("revoked capability read as granted")
	}
	if m.Can(model.CapMonetization) {
		t.Fatal("absent capability read as granted")
	}
}
