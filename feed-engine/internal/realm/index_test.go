package realm

import (
	"context"
	"testing"
)

// TestClampDefinesTheDegradeInOnePlace proves that everything outside the scale
// lands on the same defined value, so no renderer has to decide for itself.
func TestClampDefinesTheDegradeInOnePlace(t *testing.T) {
	for _, in := range []int{-7, -1, 0, 6, 99} {
		if got := Clamp(in); got != Default {
			t.Errorf("Clamp(%d) = %d, want the default %d", in, got, Default)
		}
	}
	for in := Min; in <= Max; in++ {
		if got := Clamp(in); got != in {
			t.Errorf("Clamp(%d) = %d, want it unchanged", in, got)
		}
	}
}

// TestUnknownHandleWearsTheDefault proves the read path is total: an empty
// index, an empty handle and a handle nobody has heard of all answer, and all
// answer the same thing.
func TestUnknownHandleWearsTheDefault(t *testing.T) {
	ix := Start(context.Background(), nil, 0)
	for _, h := range []string{"", "   ", "nobody", "@nobody"} {
		if got := ix.Of(h); got != Default {
			t.Errorf("Of(%q) = %d, want %d", h, got, Default)
		}
	}
	if got := RingClass("nobody"); got != "" {
		t.Errorf("an account on the floor draws no ring, got %q", got)
	}
}

// TestNoteCarriesStandingToTheRing proves the XP write path's update is what a
// render reads, and that dropping back to the floor removes the ring rather
// than leaving a stale one behind.
func TestNoteCarriesStandingToTheRing(t *testing.T) {
	ix := Start(context.Background(), nil, 0)

	ix.Note("Miiyazuko", 4)
	if got := Of("miiyazuko"); got != 4 {
		t.Fatalf("Of after Note = %d, want 4", got)
	}
	// Handles are case-insensitive and are sometimes rendered with their @.
	if got := Of("@MIIYAZUKO"); got != 4 {
		t.Errorf("Of(@MIIYAZUKO) = %d, want 4", got)
	}
	if got := RingClass("miiyazuko"); got != " realm-ring realm-ring--r4" {
		t.Errorf("RingClass = %q", got)
	}
	if ix.Size() != 1 {
		t.Errorf("Size = %d, want 1 account above the floor", ix.Size())
	}

	// An out-of-scale write degrades rather than being stored.
	ix.Note("miiyazuko", 9)
	if got := Of("miiyazuko"); got != Default {
		t.Errorf("out-of-scale Note left %d behind, want the default %d", got, Default)
	}
	if ix.Size() != 0 {
		t.Errorf("Size = %d, want the floor account dropped from the index", ix.Size())
	}
}

// TestComputeRealmMatchesTheThresholds pins the one place XP becomes a realm.
func TestComputeRealmMatchesTheThresholds(t *testing.T) {
	cases := []struct {
		xp   int64
		want int
	}{
		{-100, 1}, {0, 1}, {499, 1},
		{500, 2}, {1999, 2},
		{2000, 3}, {7499, 3},
		{7500, 4}, {19999, 4},
		{20000, 5}, {1 << 40, 5},
	}
	for _, c := range cases {
		if got := ComputeRealm(c.xp); got != c.want {
			t.Errorf("ComputeRealm(%d) = %d, want %d", c.xp, got, c.want)
		}
	}
}

// TestProgressNeverLeavesTheBar proves the XP bar cannot be handed a percentage
// it can draw wrong, including for a realm the column should never hold.
func TestProgressNeverLeavesTheBar(t *testing.T) {
	for _, r := range []int{-1, 0, 1, 2, 3, 4, 5, 6, 40} {
		for _, xp := range []int64{-500, 0, 250, 20000, 1 << 30} {
			got := Progress(xp, r)
			if got < 0 || got > 100 {
				t.Errorf("Progress(%d, %d) = %d, outside 0–100", xp, r, got)
			}
		}
	}
	if got := Progress(20000, Max); got != 100 {
		t.Errorf("Progress at the top realm = %d, want 100", got)
	}
}
