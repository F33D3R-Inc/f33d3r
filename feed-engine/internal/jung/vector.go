// Package jung is the psychological coordinate system shared by every brain
// that reasons about what a person is drawn to.
//
// A work and a person are both described by one vector on eight axes. The
// axes, their order and their meaning are defined once, in Zior
// (zior-engine/src/jung/axes.rs), and this package mirrors that definition
// exactly so a vector produced here, a vector Zior produces from audio, and a
// vector AethyrRank scores are all the same object. Nothing in this package
// hashes anything: every coordinate is derived from a real property of the
// thing it describes.
package jung

import "math"

// Dim is the dimensionality of the psychological space. It is a contract with
// Zior and AethyrRank, not a tunable: a vector of any other length is rejected
// by the ranker.
const Dim = 8

// Axis indexes one coordinate of a Vector.
//
// The order is canonical and MUST match zior-engine/src/jung/axes.rs
// AXIS_NAMES exactly — Zior writes topic vectors for audio works in this
// order and AethyrRank's Jung-aware scoring reads specific indexes (tension,
// release) by position.
type Axis int

const (
	// Persona — social performance, craft visibility, production polish.
	Persona Axis = iota
	// Shadow — raw depth, darkness, unresolved tension, emotional truth.
	Shadow
	// Agency — energy, drive, momentum, the hero arc.
	Agency
	// Integration — resolution, harmony, coherence, having made peace.
	Integration
	// Attachment — intimacy, vulnerability, warmth, anima/animus.
	Attachment
	// Disruption — chaos, surprise, genre-fluidity, trickster energy.
	Disruption
	// Tension — dissonance, contrast, the unresolved and the contested.
	Tension
	// Release — catharsis, payoff, euphoria, the thing finally landing.
	Release
)

// AxisNames are the canonical names, in canonical order.
var AxisNames = [Dim]string{
	"persona", "shadow", "agency", "integration",
	"attachment", "disruption", "tension", "release",
}

func (a Axis) String() string {
	if a < 0 || int(a) >= Dim {
		return "unknown"
	}
	return AxisNames[a]
}

// Vector is one point in the psychological space. Coordinates are in [0, 1];
// a vector that has been through Normalise is additionally unit length, so
// its coordinates are the proportions of the whole rather than absolute
// intensities. Both works and people are stored normalised.
type Vector [Dim]float32

// floor is the smallest coordinate any mapped vector may hold. A coordinate
// of exactly zero makes cosine degenerate for people who have no evidence on
// that axis yet, and a person with no evidence on an axis is not a person
// who is repelled by it.
const floor = 0.05

// FromSlice adopts a slice as a Vector. The second result is false when the
// slice is not exactly Dim long or holds a non-finite value, which is how a
// column written by an older code path is told apart from a real vector.
func FromSlice(s []float32) (Vector, bool) {
	var v Vector
	if len(s) != Dim {
		return v, false
	}
	for i, x := range s {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return v, false
		}
		v[i] = x
	}
	return v, true
}

// Slice returns the vector as a fresh slice, the shape the wire types and the
// REAL[] column want.
func (v Vector) Slice() []float32 {
	out := make([]float32, Dim)
	copy(out, v[:])
	return out
}

// Clamp bounds every coordinate to [0, 1].
func (v Vector) Clamp() Vector {
	for i := range v {
		if v[i] < 0 {
			v[i] = 0
		} else if v[i] > 1 {
			v[i] = 1
		}
	}
	return v
}

// Normalise clamps to [0, 1], raises every coordinate to at least floor, and
// scales to unit length. The result is what gets stored and what gets
// scored. A vector that is all zeros comes back as Neutral().
func (v Vector) Normalise() Vector {
	v = v.Clamp()
	var sum float64
	for i := range v {
		if v[i] < floor {
			v[i] = floor
		}
		sum += float64(v[i]) * float64(v[i])
	}
	if sum <= 0 {
		return Neutral()
	}
	n := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= n
	}
	return v
}

// Neutral is the point with no leaning on any axis, normalised. It is the
// prior for a person about whom nothing is known.
func Neutral() Vector {
	var v Vector
	for i := range v {
		v[i] = 0.5
	}
	return v.Normalise()
}

// Blend moves a toward b by weight t in [0, 1] and normalises the result.
// t = 0 returns a unchanged; t = 1 returns b.
func Blend(a, b Vector, t float32) Vector {
	if t <= 0 {
		return a.Normalise()
	}
	if t >= 1 {
		return b.Normalise()
	}
	var out Vector
	for i := range out {
		out[i] = a[i] + t*(b[i]-a[i])
	}
	return out.Normalise()
}

// Repel moves a away from b by weight t in [0, 1] and normalises the result.
// It is the negative-feedback step: a dislike moves a person's interest
// vector in the opposite direction of the work's vector, not merely less far
// toward it.
func Repel(a, b Vector, t float32) Vector {
	if t <= 0 {
		return a.Normalise()
	}
	var out Vector
	for i := range out {
		out[i] = a[i] - t*(b[i]-a[i])
	}
	return out.Normalise()
}

// Cosine is the cosine similarity in [-1, 1]. For two normalised vectors it
// is in [0, 1] because no coordinate is negative.
func Cosine(a, b Vector) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Distance is the Euclidean distance between two vectors.
func Distance(a, b Vector) float64 {
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return math.Sqrt(sum)
}

// Dominant is the axis with the largest coordinate — what a work is mostly
// about, or what a person mostly leans toward.
func (v Vector) Dominant() Axis {
	best := 0
	for i := 1; i < Dim; i++ {
		if v[i] > v[best] {
			best = i
		}
	}
	return Axis(best)
}

// Mean averages a set of vectors and normalises the result. An empty set
// yields Neutral().
func Mean(vs []Vector) Vector {
	if len(vs) == 0 {
		return Neutral()
	}
	var sum [Dim]float64
	for _, v := range vs {
		for i := range v {
			sum[i] += float64(v[i])
		}
	}
	var out Vector
	n := float64(len(vs))
	for i := range out {
		out[i] = float32(sum[i] / n)
	}
	return out.Normalise()
}
