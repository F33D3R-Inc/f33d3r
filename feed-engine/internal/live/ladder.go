package live

// ladder.go — the ABR ladder.
//
// The ladder is derived from what the source actually delivered, never from
// what the broadcaster asked for. A rung above the source height is an upscale:
// it costs encoder time, adds bitrate, and returns no detail, so it is never
// emitted. The phone is the primary capture device and modern phones deliver
// 2160p, so the top rung is real rather than aspirational.
//
// Audio is not laddered. The brief calls out microphone quality explicitly, so
// every rung carries the same AAC-LC 192 kbit/s 48 kHz stereo track and the
// bitrate ladder only moves video.

import (
	"fmt"
	"strconv"
	"strings"
)

// AudioKbps is the one audio bitrate, shared by every rung. It does not degrade
// with the video rung: a viewer on a poor connection gets a smaller picture,
// not a worse microphone.
const AudioKbps = 192

// AudioSampleRate is the capture rate carried end to end.
const AudioSampleRate = 48000

// Rung is one rendition of the ABR ladder.
type Rung struct {
	Height    int // vertical resolution; the name is fmt "%dp"
	VideoKbps int // target video bitrate
	MaxKbps   int // encoder ceiling (VBV maxrate)
	BufKbps   int // VBV buffer size
	// Bandwidth is the value advertised in the master playlist's
	// EXT-X-STREAM-INF: video ceiling + audio + muxing overhead.
	Bandwidth int
}

// Name is the rung's directory and playlist segment, e.g. "1080p".
func (r Rung) Name() string { return fmt.Sprintf("%dp", r.Height) }

// ladder is the full set of rungs, ascending. Bitrates follow the brief:
// 2160p 15-25M, 1440p 8-12M, 1080p 5-8M, 720p 2.5-4M, 480p 1-2M. Each rung
// targets the middle of its band and lets the VBV ceiling reach the top of it,
// so a static talking head costs the floor and a moving 4K scene gets the room
// it needs.
var ladder = []Rung{
	{Height: 480, VideoKbps: 1500, MaxKbps: 2000, BufKbps: 3000, Bandwidth: 2_300_000},
	{Height: 720, VideoKbps: 3200, MaxKbps: 4000, BufKbps: 6000, Bandwidth: 4_400_000},
	{Height: 1080, VideoKbps: 6500, MaxKbps: 8000, BufKbps: 12000, Bandwidth: 8_500_000},
	{Height: 1440, VideoKbps: 10000, MaxKbps: 12000, BufKbps: 18000, Bandwidth: 12_600_000},
	{Height: 2160, VideoKbps: 20000, MaxKbps: 25000, BufKbps: 37500, Bandwidth: 25_800_000},
}

// minLadderHeight is the smallest source we will package. Below this there is
// no ladder to build and the source is already a thumbnail.
const minLadderHeight = 240

// LadderFor returns the rungs to encode for a source of the given height,
// ascending. Rungs above the source are dropped — the ladder never upscales.
//
// A source shorter than the lowest rung still gets exactly one rendition, cut
// at its own height, so a 360p phone in a bad signal area still packages.
func LadderFor(sourceHeight int) []Rung {
	if sourceHeight < minLadderHeight {
		return nil
	}
	out := make([]Rung, 0, len(ladder))
	for _, r := range ladder {
		if r.Height <= sourceHeight {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		// Between minLadderHeight and the lowest rung: encode at source height
		// with the lowest rung's bitrate envelope rather than upscaling to 480p.
		low := ladder[0]
		low.Height = sourceHeight
		out = append(out, low)
	}
	return out
}

// LadderNames returns the rung names for a source height, ascending.
func LadderNames(sourceHeight int) []string {
	rungs := LadderFor(sourceHeight)
	names := make([]string, 0, len(rungs))
	for _, r := range rungs {
		names = append(names, r.Name())
	}
	return names
}

// LadderSummary renders the ladder as one line for a log or a report.
func LadderSummary(sourceHeight int) string {
	rungs := LadderFor(sourceHeight)
	parts := make([]string, 0, len(rungs))
	for _, r := range rungs {
		parts = append(parts, fmt.Sprintf("%s@%dk", r.Name(), r.VideoKbps))
	}
	return strings.Join(parts, " ")
}

// ── the encode plan ──────────────────────────────────────────────────────────
//
// The rung table above used to be written out a second time, by hand, in
// infra/live/scripts/live_ladder.sh. Two copies of one contract is a contract
// that drifts: a bitrate corrected here and not there produces a master
// playlist that advertises a bandwidth the segments do not carry, and nothing
// fails loudly enough to notice. The script no longer holds a rung table. It
// reports what the source is and executes the plan this package hands back, so
// there is exactly one ladder definition and it is this one.
//
// The plan is also where capacity is spent. What the box can afford is decided
// here, on the server, with knowledge of every other broadcast in flight — the
// ladder process knows only about itself and could never make that call.

// EncodeMode is how one rung is produced.
type EncodeMode string

const (
	// ModeEncode scales and re-encodes the source into this rung.
	ModeEncode EncodeMode = "encode"
	// ModeCopy remuxes the source's own H.264 elementary stream into this rung
	// without touching a pixel. It is only ever chosen for a rung whose height
	// is exactly the source height, so the label on the rung is the truth.
	ModeCopy EncodeMode = "copy"
)

// EncoderX264 is the software encoder: always available, paid for in cores.
const EncoderX264 = "libx264"

// EncoderNVENC is the GPU encoder: paid for in a fixed, small number of
// hardware sessions rather than in cores.
const EncoderNVENC = "h264_nvenc"

// PlannedRung is one rung of the ladder as the server decided to produce it.
type PlannedRung struct {
	Height    int        `json:"height"`
	VideoKbps int        `json:"video_kbps"`
	MaxKbps   int        `json:"max_kbps"`
	BufKbps   int        `json:"buf_kbps"`
	Name      string     `json:"name"`
	Mode      EncodeMode `json:"mode"`
	// Threads is the per-rung ceiling on encoder parallelism. It is the
	// isolation mechanism: an encode that may only occupy this many threads
	// cannot take the cores another broadcast was admitted on the promise of.
	// Zero means the encoder chooses, and is never emitted under contention.
	Threads int `json:"threads"`
}

// EncodePlan is the complete instruction one ladder process executes. It is
// produced by the server, for one broadcast, at the moment the source geometry
// becomes known, and it is the only thing that decides what gets encoded.
type EncodePlan struct {
	Rungs   []PlannedRung `json:"rungs"`
	Encoder string        `json:"encoder"`
	// SegmentSeconds is the HLS target duration and, identically, the forced
	// keyframe cadence of every encoded rung. When a rung is copied it is
	// derived from the publisher's own GOP, because a copied rung's segments
	// break where the publisher put its keyframes and nowhere else; making the
	// encoded rungs break there too is what keeps the ladder switchable.
	SegmentSeconds int `json:"segment_seconds"`
	// Degraded reports that this is less than the full ladder for this source.
	// It exists so the row and the surfaces can say so rather than implying a
	// ladder that is not being produced.
	Degraded bool   `json:"degraded"`
	Reason   string `json:"reason"`
}

// Summary renders the plan as one line for a log, a row or a report. A copied
// rung is marked, because "1080p" produced by remux and "1080p" produced by an
// encoder are not the same thing to anyone debugging a playback fault.
func (p EncodePlan) Summary() string {
	if len(p.Rungs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(p.Rungs))
	for _, r := range p.Rungs {
		if r.Mode == ModeCopy {
			parts = append(parts, r.Name+"@copy")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s@%dk", r.Name, r.VideoKbps))
	}
	return strings.Join(parts, " ") + " [" + p.Encoder + "]"
}

// Names returns the rung names in the plan, ascending. These are the directory
// names on disk and the variant names in the master playlist.
func (p EncodePlan) Names() []string {
	names := make([]string, 0, len(p.Rungs))
	for _, r := range p.Rungs {
		names = append(names, r.Name)
	}
	return names
}

// plannedFrom turns ladder rungs into planned rungs, all encoded, no thread
// ceiling. Capacity and passthrough are applied to this by the governor.
func plannedFrom(rungs []Rung) []PlannedRung {
	out := make([]PlannedRung, 0, len(rungs))
	for _, r := range rungs {
		out = append(out, PlannedRung{
			Height:    r.Height,
			VideoKbps: r.VideoKbps,
			MaxKbps:   r.MaxKbps,
			BufKbps:   r.BufKbps,
			Name:      r.Name(),
			Mode:      ModeEncode,
		})
	}
	return out
}

// RungHeightsFromSummary reads the rung heights back out of a plan summary, in
// the order the summary carries them.
//
// The summary is what the row records, and the row is the only place a surface
// can learn which rungs are actually being written. Under load the ladder a
// source could support and the ladder it is getting are different things, and a
// quality selector built from the first offers a viewer a rung that nothing is
// producing — a selection that resolves to a 404 against the edge.
//
// An empty or unparseable summary yields nothing, and callers fall back to the
// theoretical ladder: before the first plan is issued there is no truth to tell
// yet, and the full ladder is the right thing to offer until there is.
func RungHeightsFromSummary(summary string) []int {
	var out []int
	for _, field := range strings.Fields(summary) {
		name, _, ok := strings.Cut(field, "@")
		if !ok {
			continue // the trailing "[encoder]" and anything else unrecognised
		}
		height, err := strconv.Atoi(strings.TrimSuffix(name, "p"))
		if err != nil || height <= 0 {
			continue
		}
		out = append(out, height)
	}
	return out
}
