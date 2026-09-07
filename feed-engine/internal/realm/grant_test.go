package realm

import (
	"testing"

	"github.com/f33d3r/feed-engine/internal/model"
)

// The grant model in one file. Realm has two sources — earned XP and a floor —
// and Effective is the only place they meet. These tests pin the four
// properties the whole design rests on:
//
//  1. a grant raises standing that XP has not earned,
//  2. earned standing above a grant is never pulled down to it,
//  3. no XP award can erase a grant, at any XP total,
//  4. the founder role carries the top of the scale by itself.

// TestAGrantIsAFloorNotAnOverride is the central claim. A granted account
// stands at its grant until its XP carries it higher, and then it stands at
// what it earned.
func TestAGrantIsAFloorNotAnOverride(t *testing.T) {
	cases := []struct {
		name  string
		xp    int64
		grant int
		role  string
		want  int
	}{
		{"no grant, no xp", 0, GrantNone, model.RoleUser, 1},
		{"no grant, earned R3", 2000, GrantNone, model.RoleUser, 3},

		// The bug this fixes: standing placed by hand, XP nowhere near it.
		{"granted R5 on 125 xp", 125, 5, model.RoleUser, 5},
		{"granted R3 on 195 xp", 195, 3, model.RoleUser, 3},

		// A grant is a floor, so earning past it carries the account past it.
		{"granted R2, earned R4", 7500, 2, model.RoleUser, 4},
		{"granted R2, earned R5", 20000, 2, model.RoleUser, 5},

		// And a grant below what was earned changes nothing at all — it can
		// never demote.
		{"granted R1 on earned R3", 2000, 1, model.RoleUser, 3},

		// Clearing a grant returns the account to exactly what it earned.
		{"grant cleared", 125, GrantNone, model.RoleUser, 1},

		// An out-of-scale grant degrades through Clamp like every other
		// untrusted realm value: it becomes no grant, never a wrong one.
		{"grant above the scale", 125, 9, model.RoleUser, 1},
		{"negative grant", 125, -3, model.RoleUser, 1},
	}
	for _, c := range cases {
		if got := Effective(c.xp, c.grant, c.role); got != c.want {
			t.Errorf("%s: Effective(%d, %d, %q) = R%d, want R%d",
				c.name, c.xp, c.grant, c.role, got, c.want)
		}
	}
}

// TestNoXPTotalCanErodeAGrant is the property AwardXP relies on. AwardXP
// recomputes standing on every award; if any XP total could produce an answer
// below the grant, an award would erase it. None can.
func TestNoXPTotalCanErodeAGrant(t *testing.T) {
	for grant := Min; grant <= Max; grant++ {
		for _, xp := range []int64{-1, 0, 1, 125, 195, 499, 500, 1999, 2000, 7499, 7500, 19999, 20000, 1 << 40} {
			got := Effective(xp, grant, model.RoleUser)
			if got < grant {
				t.Errorf("grant R%d at %d XP fell to R%d — an award erased a grant", grant, xp, got)
			}
			// And it never overrides what was earned either.
			if earned := ComputeRealm(xp); got < earned {
				t.Errorf("grant R%d at %d XP pulled earned R%d down to R%d", grant, xp, earned, got)
			}
		}
	}
}

// TestFounderWearsTheTopOfTheScaleWithoutAGrant proves the founder floor comes
// from the role itself: no grant to remember, nothing for an award to erase,
// and no sixth realm invented to express it.
func TestFounderWearsTheTopOfTheScaleWithoutAGrant(t *testing.T) {
	if FounderFloor != Max {
		t.Fatalf("FounderFloor = %d, want the top of the existing scale (%d) — "+
			"founder is a role with a floor, not a realm of its own", FounderFloor, Max)
	}
	for _, xp := range []int64{0, 195, 20000} {
		if got := Effective(xp, GrantNone, model.RoleFounder); got != Max {
			t.Errorf("founder on %d XP wears R%d, want R%d", xp, got, Max)
		}
	}
	// Losing the role loses the floor: standing falls back to what was earned,
	// which is the whole point of deriving it from the role rather than
	// stamping a grant that would outlive it.
	if got := Effective(195, GrantNone, model.RoleUser); got != 1 {
		t.Errorf("after founder is removed, 195 XP wears R%d, want R1", got)
	}
	// A grant on top of the founder floor cannot lower it.
	if got := Effective(0, 2, model.RoleFounder); got != Max {
		t.Errorf("founder with an R2 grant wears R%d, want R%d", got, Max)
	}
}

// TestIsGrantedNamesTheSourceOfStanding pins what the admin surface reads to
// tell "earned" from "placed".
func TestIsGrantedNamesTheSourceOfStanding(t *testing.T) {
	if !IsGranted(125, 5, model.RoleUser) {
		t.Error("R5 on 125 XP is held by its grant, not earned")
	}
	if IsGranted(20000, 5, model.RoleUser) {
		t.Error("R5 on 20000 XP is earned — clearing the grant would not move it")
	}
	if !IsGranted(0, GrantNone, model.RoleFounder) {
		t.Error("the founder floor is standing the account has not earned")
	}
	if IsGranted(0, GrantNone, model.RoleUser) {
		t.Error("an account with no floor at all is standing on what it earned")
	}
}
