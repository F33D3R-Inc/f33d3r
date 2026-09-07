package store

// The public progression ladder — five realms with the XP floors CLAUDE.md
// states. `realm.XPThreshold()` in feed-engine is the same table.
//
// A founder wears the top of the scale regardless of XP, which is
// feed-engine's rule as well (realm/grant_test.go: "founder wears the top of
// the scale without a grant").

var realmNames = [...]string{"Wanderer", "Initiate", "Seeker", "Adept", "Guardian"}
var realmFloors = [...]int64{0, 500, 2000, 7500, 20000}

// Realm resolves XP and role to the 1–5 level and its public name.
func Realm(xp int64, role string) (int, string) {
	if role == "founder" {
		return 5, realmNames[4]
	}
	level := 1
	for i, floor := range realmFloors {
		if xp >= floor {
			level = i + 1
		}
	}
	return level, realmNames[level-1]
}

// XP awards, mirroring the sizes feed-engine's realm package hands out.
const (
	XPPost   = 10
	XPReply  = 5
	XPLike   = 1
	XPFollow = 2
)
