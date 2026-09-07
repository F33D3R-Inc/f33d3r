package jung

import "strings"

// The twelve archetypes a person may declare on their profile
// (user_profiles.jung_archetype), each expressed as a leaning on the eight
// axes. These are priors, not verdicts: a declared archetype is what a person
// says about themselves, and it seeds the interest vector only until their
// engagement says something more specific. They are weighted 30/70 against
// observed engagement in the handler's interest computation and are the whole
// vector only for a person who has engaged with nothing yet.
//
// Order of coordinates: persona, shadow, agency, integration, attachment,
// disruption, tension, release.
var archetypePriors = map[string]Vector{
	// Safety, simplicity, wanting things to be right and whole.
	"innocent": {0.40, 0.10, 0.30, 0.80, 0.70, 0.10, 0.10, 0.60},
	// Belonging, connection, being one of the crowd.
	"everyman": {0.30, 0.30, 0.40, 0.60, 0.80, 0.20, 0.30, 0.50},
	// Mastery through effort, proving worth, the fight and the win.
	"hero": {0.60, 0.30, 0.90, 0.30, 0.30, 0.30, 0.50, 0.50},
	// Protecting and providing for others.
	"caregiver": {0.30, 0.30, 0.40, 0.60, 0.90, 0.10, 0.20, 0.40},
	// Freedom, the road, finding out what is over the next hill.
	"explorer": {0.40, 0.30, 0.70, 0.30, 0.20, 0.70, 0.30, 0.50},
	// Breaking what does not work, the outlaw, revolution.
	"rebel": {0.30, 0.70, 0.60, 0.10, 0.20, 0.90, 0.70, 0.40},
	// Intimacy, passion, being close.
	"lover": {0.50, 0.40, 0.30, 0.40, 0.90, 0.20, 0.30, 0.60},
	// Making things of lasting value, craft and vision.
	"creator": {0.80, 0.30, 0.70, 0.50, 0.30, 0.40, 0.30, 0.40},
	// Joy, play, living in the moment, the trickster.
	"jester": {0.50, 0.20, 0.40, 0.20, 0.40, 0.80, 0.20, 0.80},
	// Understanding, truth, wisdom, the long view.
	"sage": {0.40, 0.40, 0.30, 0.90, 0.30, 0.20, 0.30, 0.30},
	// Transformation, making the vision real, the hidden law.
	"magician": {0.50, 0.60, 0.50, 0.70, 0.30, 0.60, 0.40, 0.50},
	// Control, order, prosperity, responsibility.
	"ruler": {0.80, 0.20, 0.80, 0.60, 0.30, 0.10, 0.40, 0.30},
}

// ArchetypePrior returns the normalised leaning for a declared archetype.
// The name is matched case-insensitively with surrounding space ignored; an
// empty or unknown archetype yields Neutral(), which is the honest prior for
// a person who declared nothing.
func ArchetypePrior(archetype string) Vector {
	key := strings.ToLower(strings.TrimSpace(archetype))
	if v, ok := archetypePriors[key]; ok {
		return v.Normalise()
	}
	return Neutral()
}

// Archetypes lists the recognised archetype names, for anything that needs
// to validate or enumerate the declared vocabulary.
func Archetypes() []string {
	out := make([]string, 0, len(archetypePriors))
	for k := range archetypePriors {
		out = append(out, k)
	}
	return out
}
