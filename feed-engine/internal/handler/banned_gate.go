package handler

// banned_gate.go — the upload path's banned-content gate.
//
// Every upload passes through bannedContentGate before a byte is stored. The
// gate has two layers and one rule: an unanswered gate is a closed gate.
//
//  1. Exact bytes (sha256), queried locally against banned_content_hashes.
//     Always runs. A database error refuses the upload — a discarded error here
//     is an admission, and on this registry the failure direction is refusal.
//
//  2. Perceptual (phash, audio_fp), by asking content-scan. content-scan owns
//     every perceptual hash on this platform: it produced the values the
//     registry holds, and such a hash matches only when the same algorithm
//     produced both sides. feed-engine asks; it never computes. Both layers are
//     compared by radius there, in two bands each — inside the tight band the
//     media is refused, in the outer band it is admitted and recorded for
//     review. Measured on this pipeline, a JPEG re-encode or an 80% resize
//     moves a phash by 2 bits of 64, and rewrapping the same AAC stream from
//     m4a to mka changes its chromaprint fingerprint outright. Both defeat the
//     sha256 layer above and are caught by this one.
//
// Layer 2 depends on a service that is optional in this deployment and can be
// restarting. It is never skipped silently. When it cannot answer, the upload
// is admitted with a csam_scan_log row at result='pending_scan' — the same
// shape internal/live/scan.go records for a frame no classifier judged, and the
// query that enumerates the re-check backlog:
//
//	SELECT * FROM csam_scan_log WHERE result = 'pending_scan' ORDER BY scanned_at;
//
// Refusing every upload while content-scan warms would take the site down;
// recording nothing would leave a gate that reads as enforced and is not.
// Admitted-and-recorded is the defensible middle: the exact-bytes layer still
// blocks, and nothing is ever written down as clean that was not judged.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/scanclient"
)

// errMediaBanned means the offered media matched the banned registry.
var errMediaBanned = errors.New("media matches a banned content hash")

// errBanGateUnavailable means the gate could not reach a verdict. The upload is
// refused on it, because admitting on an unknown verdict is the defect.
var errBanGateUnavailable = errors.New("banned-hash registry could not be consulted")

// gateInput is one thing offered for upload. Exactly one of Data or Path
// carries the payload; both empty means the caller holds only a digest.
type gateInput struct {
	Kind     string // scanclient.KindImage | KindVideo | KindAudio
	Filename string // recorded as csam_scan_log.media_url
	Data     []byte // the bytes offered, when they are already in memory
	Path     string // the bytes on disk, for media too large to hold in memory
	SHA256   string // hex digest of the payload
	PIALID   string
	Label    string // short call-site tag carried into every log line
}

// bannedContentGate runs both layers. It returns nil to admit, errMediaBanned
// to refuse a match, and an errBanGateUnavailable-wrapped error to refuse an
// upload the gate could not judge on the layer that must never be skipped.
func (h *Handler) bannedContentGate(ctx context.Context, in gateInput) error {
	if in.Filename == "" {
		in.Filename = "(unnamed)"
	}

	if h.db == nil {
		// No registry to consult, so nothing can be cleared by it.
		log.Printf("[csam-GATE] %s: no database handle — upload refused, pial=%s", in.Label, in.PIALID)
		return fmt.Errorf("%w: no database handle", errBanGateUnavailable)
	}

	// ── Layer 1: exact bytes ──────────────────────────────────────────────────
	banned, category, err := dbpkg.IsBannedHash(h.db, "sha256", in.SHA256)
	if err != nil {
		log.Printf("[csam-GATE] %s: banned sha256 lookup failed for pial=%s: %v — upload refused",
			in.Label, in.PIALID, err)
		return fmt.Errorf("%w: %v", errBanGateUnavailable, err)
	}
	if banned {
		h.recordCSAMScan(in, "flagged", 1.0)
		log.Printf("[csam-BLOCK] %s: banned sha256 %s (%s) — upload blocked, pial=%s",
			in.Label, hashPrefix(in.SHA256), category, in.PIALID)
		return errMediaBanned
	}

	// ── Layer 2: perceptual ───────────────────────────────────────────────────
	scan := scanclient.Shared()
	if !scan.Configured() {
		// Announced once at first use. The perceptual layer is absent by
		// configuration, not broken, so there is no per-upload backlog to build.
		return nil
	}
	if len(in.Data) == 0 && in.Path == "" {
		// A digest-only pre-flight probe: the client hashed a file it has not
		// sent yet. Nothing is stored on this path, and the same bytes go
		// through the whole gate when they actually arrive.
		return nil
	}

	var verdict *scanclient.MediaVerdict
	if in.Path != "" {
		verdict, err = scan.CheckMediaFile(ctx, in.Kind, in.Filename, in.Path)
	} else {
		verdict, err = scan.CheckMedia(ctx, in.Kind, in.Filename, in.Data)
	}
	if err != nil {
		// No perceptual verdict. Admitted on the exact-bytes layer alone and
		// written down as unjudged; scanclient logs the outage itself.
		h.recordCSAMScan(in, "pending_scan", 0)
		log.Printf("[csam-PENDING] %s: perceptual layer did not run for pial=%s (%v) — "+
			"admitted on sha256 only, recorded pending_scan", in.Label, in.PIALID, err)
		return nil
	}

	if verdict.Banned {
		h.recordCSAMScan(in, "flagged", 1.0)
		log.Printf("[csam-BLOCK] %s: banned %s (%s) — upload blocked, pial=%s sha256=%s phash=%s",
			in.Label, verdict.HashType, verdict.Category, in.PIALID,
			hashPrefix(verdict.SHA256), verdict.PHash)
		return errMediaBanned
	}

	// A partial verdict is logged whatever else the gate found, so a layer that
	// did not run is never hidden by a finding from a layer that did.
	if len(verdict.Degraded) > 0 {
		log.Printf("[csam-PENDING] %s: content-scan could not compute %v for pial=%s — "+
			"admitted on the layers that ran (%v)",
			in.Label, verdict.Degraded, in.PIALID, verdict.Checked)
	}

	nears := verdict.Nears()
	switch {
	case len(nears) > 0:
		// Near banned content, but outside the radius content-scan treats as the
		// same content. Perceptual hashes of low-detail media cluster and audio
		// below the block cut is derived rather than identical, so the outer
		// band puts a human on it instead of refusing a creator. Every layer
		// that found something is logged: a video can be near banned content on
		// its frames and on its audio, and both are evidence.
		h.recordCSAMScan(in, "review", 0)
		for _, near := range nears {
			log.Printf("[csam-REVIEW] %s: %s — admitted, recorded for review, pial=%s phash=%s",
				in.Label, near.Describe(), in.PIALID, verdict.PHash)
		}

	case len(verdict.Degraded) > 0:
		// content-scan answered, but a layer that applied to this media could
		// not be computed. That is a partial verdict, not a clean one.
		h.recordCSAMScan(in, "pending_scan", 0)
	}

	return nil
}

// recordCSAMScan writes one audit row. A failure to write it is logged, never
// swallowed: the row is the only record that an upload went by unjudged.
func (h *Handler) recordCSAMScan(in gateInput, result string, score float64) {
	if h.db == nil {
		return
	}
	kind := in.Kind
	if kind == "" {
		kind = "image"
	}
	if _, err := h.db.Exec(
		`INSERT INTO csam_scan_log (media_url, pial_id, content_type, result, score)
		 VALUES ($1, $2, $3, $4, $5)`,
		in.Filename, in.PIALID, kind, result, score,
	); err != nil {
		log.Printf("[csam-GATE] %s: csam_scan_log %s row for pial=%s: %v",
			in.Label, result, in.PIALID, err)
	}
}

// banGateHTTPError renders a gate refusal. The banned wording matches what the
// Vision lane already returns, so one message covers every surface.
func banGateHTTPError(w http.ResponseWriter, err error) {
	if errors.Is(err, errMediaBanned) {
		http.Error(w, "This content cannot be uploaded", http.StatusBadRequest)
		return
	}
	http.Error(w, "Upload refused — the content safety check could not run. Please try again.",
		http.StatusServiceUnavailable)
}

// hashPrefix shortens a hex digest for a log line without panicking on a short
// or empty one.
func hashPrefix(hexDigest string) string {
	if len(hexDigest) <= 16 {
		return hexDigest
	}
	return hexDigest[:16]
}
