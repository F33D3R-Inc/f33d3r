package model

import "testing"

// TestKYCTierIsIdentity pins the bar. The whole point of the predicate is that
// the bar is written down in exactly one place, so this test is what makes
// moving it a deliberate act rather than a side effect.
func TestKYCTierIsIdentity(t *testing.T) {
	cases := []struct {
		tier string
		want bool
		why  string
	}{
		{KYCTierNone, false, "nothing recorded"},
		{KYCTierBasic, false, "self-attested birthday typed into the signup form — every account reaches this"},
		{KYCTierSoft, false, "document read, but no face match and no liveness: a scraped passport image satisfies it"},
		{KYCTierFull, true, "document + face match + liveness — the only tier that names the human operating the account"},
		{"", false, "unset"},
		{"FULL", false, "case must not be coerced"},
		{"platinum", false, "a tier this build has never heard of must never satisfy the gate"},
	}
	for _, c := range cases {
		if got := KYCTierIsIdentity(c.tier); got != c.want {
			t.Errorf("KYCTierIsIdentity(%q) = %v, want %v — %s", c.tier, got, c.want, c.why)
		}
	}
}

// TestCanMonetizeIsNotAgeAttestation is the regression this whole change exists
// for. The creator gate used to read IsVerified, which is PIAL age_verified, and
// SetPIALBirthday sets age_verified TRUE from a birthday the account holder
// types into the signup form. So an account that had verified nothing walked
// through the creator gate. CanMonetize must not be satisfiable that way.
func TestCanMonetizeIsNotAgeAttestation(t *testing.T) {
	freshSignup := &User{
		Role:          RoleUser,
		KYCTier:       KYCTierBasic,
		IsVerified:    true, // PIAL age_verified — set by the typed-in birthday
		IsAgeVerified: true,
		IsAdult:       true,
		IsCreator:     true,
	}
	if freshSignup.CanMonetize() {
		t.Fatal("a basic-tier account with age_verified=TRUE was admitted to creator status — " +
			"the gate is reading age attestation, not identity")
	}
	if freshSignup.IsIdentityVerified() {
		t.Fatal("basic tier reported as identity-verified")
	}
}

func TestCanMonetize(t *testing.T) {
	cases := []struct {
		name string
		user *User
		want bool
	}{
		{"nil user", nil, false},
		{"no tier", &User{Role: RoleUser}, false},
		{"none", &User{Role: RoleUser, KYCTier: KYCTierNone}, false},
		{"basic", &User{Role: RoleUser, KYCTier: KYCTierBasic}, false},
		{"soft — document but no liveness", &User{Role: RoleUser, KYCTier: KYCTierSoft}, false},
		{"full", &User{Role: RoleUser, KYCTier: KYCTierFull}, true},
		{"admin bypass at basic", &User{Role: RoleAdmin, KYCTier: KYCTierBasic}, true},
		{"founder bypass at none", &User{Role: RoleFounder, KYCTier: KYCTierNone}, true},
		{"is_creator does not substitute for identity", &User{Role: RoleUser, KYCTier: KYCTierBasic, IsCreator: true}, false},
	}
	for _, c := range cases {
		if got := c.user.CanMonetize(); got != c.want {
			t.Errorf("%s: CanMonetize() = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestKYCTierRankOrdering pins the ladder order itself, so the tiers cannot be
// silently reordered underneath the predicate.
func TestKYCTierRankOrdering(t *testing.T) {
	ordered := []string{KYCTierNone, KYCTierBasic, KYCTierSoft, KYCTierFull}
	for i := 1; i < len(ordered); i++ {
		if kycTierRank(ordered[i]) <= kycTierRank(ordered[i-1]) {
			t.Fatalf("tier %q does not rank above %q", ordered[i], ordered[i-1])
		}
	}
	if kycTierRank("nonsense") >= kycTierRank(KYCTierNone) {
		t.Fatal("an unrecognised tier must rank below every known tier")
	}
}
