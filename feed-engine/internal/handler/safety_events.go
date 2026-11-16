package handler

// safety_events.go — carrying out Abraxas's answers.
//
// The safety consumer (internal/sitra/safety.go) hands every safety.events
// record here, and commits past it only once this returns nil:
//
//  1. Triggering event: Abraxas publishes a verdict, an ownership finding, a
//     banned match, a review opening or a review decision.
//  2. Server-side handler: ApplySafetyEvent applies it — scan_state on works
//     and Visions (db/safety.go), the "Media by" credit, the admins' notice that
//     an item waits in the review queue, or the CSAM report and account action
//     (csam_report.go) — inside the subject's seq ledger.
//  3. Facet renderer: the affected work card Facets are re-rendered.
//  4. Produced fragment: facet:f33d3r:work:<id>:card (the card, or its removed
//     state) and facet:f33d3r:work:<id>:media_credit (the credit chip).
//  5. Stream mutation: pushed over FA Live — a removed card to everyone who has
//     it on screen, a released card to its author, a credit chip to watchers.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/f33d3r/feed-engine/internal/caeor"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/sitra"
)

// ApplySafetyEvent carries out one safety.events record.
func (h *Handler) ApplySafetyEvent(ctx context.Context, ev sitra.SafetyEvent) error {
	if h.db == nil {
		return errors.New("safety event: no database")
	}
	if ev.Type == sitra.SafetyTypeBannedMatch {
		return h.applyBannedMatch(ctx, ev)
	}

	// Admins are told when Abraxas says an item now waits for a person
	// (review_opened: created at admin_review, or promoted there from the ML
	// stage) — once per event, never on the verdict that may still be settled
	// by the ML stage without anyone.
	apply := dbpkg.SafetyApply{
		EventID:      ev.EventID,
		Seq:          ev.Seq,
		Type:         ev.Type,
		SubjectKind:  ev.Subject.Kind,
		SubjectID:    ev.Subject.ID,
		AssetID:      ev.AssetID,
		Verdict:      ev.Verdict,
		NotifyAdmins: ev.Type == sitra.SafetyTypeReviewOpened,
	}
	// A credit: a standalone ownership event (automatic, at check time) or a
	// review_decided carrying ownership (an ML or admin credit). A rejected
	// finding carries no original owner and credits nobody.
	if ev.Ownership.Credited() {
		handle, err := h.handleOfPIAL(ev.Ownership.OriginalPIAL)
		if err != nil {
			return err
		}
		apply.OriginalPIAL, apply.OriginalHandle = ev.Ownership.OriginalPIAL, handle
	}
	applied, changes, err := dbpkg.ApplySafetyEvent(ctx, h.db, apply)
	if err != nil {
		if errors.Is(err, dbpkg.ErrSafetyPermanent) {
			return fmt.Errorf("%w: %v", sitra.ErrPermanent, err)
		}
		return err
	}
	if !applied {
		return nil
	}

	// A decision about a held upload: Caeor fetches the answer from Abraxas on
	// its own schedule; this only brings that forward. Caeor's pull is the
	// durable mechanism, so a failure here costs latency, not the release.
	if ev.Subject.Kind == "asset" && ev.Verdict != "" && ev.Verdict != "pending" {
		assetID := ev.Subject.ID
		if err := h.caeor.Recheck(ctx, assetID); err != nil && !errors.Is(err, caeor.ErrNotFound) {
			log.Printf("[safety] asking caeor to recheck asset %s: %v — its own schedule will", assetID, err)
		}
	}

	if ev.Subject.Kind == "stream" {
		if err := h.applyStreamDecision(ctx, ev); err != nil {
			// The event is recorded applied; the stream decision is a
			// person's, and it is said loudly rather than retried into a
			// ledger that has moved past it.
			log.Printf("[safety] INCIDENT: review decision %s on stream %s not carried out: %v", ev.EventID, ev.Subject.ID, err)
		}
	}

	for _, c := range changes {
		h.redrawSafetyChange(c)
	}
	return nil
}

// streamStates maps a person's decision on a live stream to its scan_state.
var streamStates = map[string]string{"approve": "clean", "age_gate": "age_gated", "human_review": "human_review"}

// applyStreamDecision carries a person's decision on a live stream to the live
// lane. An automated frame verdict needs nothing here: the live lane applied it
// the moment Abraxas answered the frame check.
func (h *Handler) applyStreamDecision(ctx context.Context, ev sitra.SafetyEvent) error {
	if ev.Type != sitra.SafetyTypeReviewDecided {
		return nil
	}
	reason := "abraxas review decision: " + ev.Verdict
	if ev.Verdict == "block" {
		if h.BlockLiveStream == nil {
			return errors.New("no live lane is wired to block the stream")
		}
		return h.BlockLiveStream(ctx, ev.Subject.ID, reason)
	}
	state, ok := streamStates[ev.Verdict]
	if !ok {
		return nil
	}
	if err := dbpkg.SetStreamScanState(h.db, ev.Subject.ID, state, reason, "abraxas"); err != nil {
		return err
	}
	h.PublishStreamState(ev.Subject.ID)
	return nil
}

// applyBannedMatch brings a banned match to the CSAM flow. The report and the
// account action run in their own transaction (RecordCSAMMatch), which is
// idempotent while the report is open; the seq ledger records the event once
// they have.
func (h *Handler) applyBannedMatch(ctx context.Context, ev sitra.SafetyEvent) error {
	done, err := dbpkg.SafetyEventApplied(ctx, h.db, ev.Subject.Kind, ev.Subject.ID, ev.Seq)
	if err != nil || done {
		return err
	}
	// Only csam entries report; the registries also hold gore, hate and spam,
	// whose refusal is the block itself.
	if ev.BannedMatch.Category == "csam" {
		m := dbpkg.CSAMMatch{
			PIALID:   ev.PIAL,
			HashType: ev.BannedMatch.Layer,
			SHA256:   ev.BannedMatch.SHA256,
			// An exact match — the very bytes the platform received — ends
			// the account; a near match holds it for a person to judge.
			Exact: ev.BannedMatch.Match == "exact",
		}
		switch ev.Subject.Kind {
		case "stream":
			m.Surface, m.MediaRef = "live", "stream "+ev.Subject.ID
		case "asset", "work":
			m.Surface, m.MediaRef = "upload", ev.Subject.Kind+" "+ev.Subject.ID
		default:
			// Other subject ids (an account's is its PIAL) are never written
			// where an admin page shows them.
			m.Surface, m.MediaRef = "upload", ev.Subject.Kind
		}
		if err := h.recordCSAMMatch(m); err != nil {
			return err
		}
	}
	return dbpkg.MarkSafetyEventApplied(ctx, h.db, ev.Subject.Kind, ev.Subject.ID, ev.Seq, ev.EventID)
}

// SafetyDeadLetter keeps a safety.events record that can never be applied.
func (h *Handler) SafetyDeadLetter(ctx context.Context, topic string, partition int32, offset int64, record []byte, reason string) error {
	return dbpkg.RecordSafetyDeadLetter(ctx, h.db, topic, partition, offset, record, reason)
}

// workCardFacetID is the Facet ID of a work's card.
func workCardFacetID(workID string) string { return "facet:f33d3r:work:" + workID + ":card" }

// redrawSafetyChange pushes what one applied event changed on screen. Every
// render failure is logged with the work it concerns; the change itself is
// already committed, and the next read of the surface shows it.
func (h *Handler) redrawSafetyChange(c dbpkg.SafetyChange) {
	if c.FromState != c.ToState {
		switch {
		case c.ToState == "blocked":
			h.publishRemovedWorkCard(c.WorkID, c.AuthorPIAL)
		default:
			// Released from pending, or a person's decision lifted a hold:
			// the author has the card on screen in its old state.
			h.publishWorkCardToAuthor(c.WorkID, c.AuthorPIAL)
		}
	}
	if c.Credited && c.ToState != "blocked" {
		h.publishMediaCredit(c.WorkID)
	}
}

// publishRemovedWorkCard replaces a blocked work's card, wherever it is on
// screen, with its removed state. The removed state shows nothing of the work,
// so one render serves every viewer.
func (h *Handler) publishRemovedWorkCard(workID, authorPIAL string) {
	if h.partial == nil {
		return
	}
	var buf bytes.Buffer
	data := map[string]interface{}{"W": &model.Work{ID: workID, ScanState: "blocked"}, "Ctx": h.workCardCtx(nil, "")}
	if err := h.partial.ExecuteTemplate(&buf, "work_card", data); err != nil {
		log.Printf("[safety] rendering the removed card of work %s: %v", workID, err)
		return
	}
	m := FacetMutation{FacetID: workCardFacetID(workID), Fragment: buf.String(), Priority: FacetPriorityHigh}
	PublishFacetToPostWatchers(workID, m)
	PublishFacetToUser(authorPIAL, m)
}

// publishWorkCardToAuthor re-renders a work's card for its author and pushes
// it to the author's connections.
func (h *Handler) publishWorkCardToAuthor(workID, authorPIAL string) {
	author, err := dbpkg.AuthorOfWork(h.db, workID)
	if err != nil {
		log.Printf("[safety] author of work %s: %v", workID, err)
		return
	}
	if author == nil {
		return
	}
	work, err := dbpkg.GetWorkByID(h.db, workID, author.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return // gone (deleted or expired) before it could be redrawn
	}
	if err != nil {
		log.Printf("[safety] reloading work %s for its author: %v", workID, err)
		return
	}
	cards := h.renderWorkCardSet([]*model.Work{work}, author, h.workCardCtx(author, "feed"))
	if len(cards) == 0 {
		return
	}
	PublishFacetToUser(authorPIAL, FacetMutation{
		FacetID: workCardFacetID(workID), Fragment: string(cards[0].Fragment), Priority: FacetPriorityHigh,
	})
}

// publishMediaCredit pushes a work's "Media by" chip to everyone watching it.
// The chip names the original owner by handle and is the same for every viewer.
func (h *Handler) publishMediaCredit(workID string) {
	if h.partial == nil {
		return
	}
	work, err := dbpkg.GetWorkByID(h.db, workID, "")
	if errors.Is(err, sql.ErrNoRows) {
		return // not visible to anyone but its author yet; its release redraws it
	}
	if err != nil {
		log.Printf("[safety] reloading work %s for its credit: %v", workID, err)
		return
	}
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, "work_media_credit", work); err != nil {
		log.Printf("[safety] rendering the credit of work %s: %v", workID, err)
		return
	}
	PublishFacetToPostWatchers(workID, FacetMutation{
		FacetID: "facet:f33d3r:work:" + workID + ":media_credit", Fragment: buf.String(),
	})
}
