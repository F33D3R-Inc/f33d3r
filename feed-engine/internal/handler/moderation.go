package handler

// moderation.go — the admin moderation surfaces, rendered from Abraxas.
//
// Abraxas is the integrity authority. It holds the review queue (ML review
// first, then admin review) and every report a person files; this brain renders
// them as server-side Facets and carries a moderator's decision back to it.
// Nothing here changes a work's scan state: a decision is recorded in Abraxas,
// which publishes review_decided on safety.events, and the safety consumer
// (safety_events.go) applies it to the content and re-renders the cards on
// screen. One authority, one path for a verdict to reach content.
//
// Every card is the Facet facet:f33d3r:moderation:<id>:card, where <id> is the
// Abraxas review or report id. A decision answers with that same Facet in its
// decided state.
//
// PIALs are Abraxas's keys and never appear in a page: every person a card
// names is resolved to a handle through the identity projection first.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/bits"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/f33d3r/feed-engine/internal/abraxas"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// moderationListLimit bounds each section of the panel.
const moderationListLimit = 50

// moderationCallTimeout bounds one round trip to Abraxas from an admin page.
const moderationCallTimeout = 10 * time.Second

// moderationScore is one classifier score on a card.
type moderationScore struct {
	Name string
	Pct  int
	Warn bool
}

// moderationCard is everything one moderation Facet shows. It carries handles,
// never PIALs.
type moderationCard struct {
	ID      string
	FacetID string
	// Source is review | report.
	Source string
	// Stage is ml_review | admin_review (review items and escalated reports).
	Stage      string
	StageLabel string
	// Kind is the report kind: content | ownership_claim.
	Kind string
	// ItemKind is the review item's kind; it decides which decisions are offered.
	ItemKind string
	// Decisions are the decisions this item takes (abraxas.DecisionsFor).
	Decisions []moderationDecision
	// Category is a banned_near item's banned category.
	Category string
	// CurrentVerdict is what Abraxas says of the subject now.
	CurrentVerdict string
	// MLOutcome is what the ML stage concluded, in words.
	MLOutcome string
	// Evidence is what the item rests on, as lines a person can read. Built from
	// named fields only, with every person resolved to a handle.
	Evidence []string
	// CanBan offers "resolve with ban" on a report whose subject belongs to
	// somebody this brain can name.
	CanBan      bool
	SubjectKind string
	SubjectLink string
	// AuthorHandle is who made the subject (a work's author, an asset's uploader).
	AuthorHandle string
	Body         string
	ThumbURL     string
	Reason       string
	Detail       string
	Scores       []moderationScore
	Signals      []string
	// CreditHandle names the original owner a credit decision would credit.
	CreditHandle string
	CreditKind   string
	// Reporters are the handles that filed the report.
	Reporters []string
	// MatchedOwners are the handles whose own uploads Abraxas matched to the
	// reported content (an ownership claim's evidence).
	MatchedOwners []string
	ReportCount   int
	CreatedAt     time.Time
	// Decided is set once a moderator has acted: the card then shows what was
	// done instead of its actions.
	Decided string
	// Problem is set when the card could not be acted on.
	Problem string
}

func moderationFacetID(id string) string { return "facet:f33d3r:moderation:" + id + ":card" }

// moderationDecision is one decision button.
type moderationDecision struct {
	Value   string
	Label   string
	Style   string // ok | warn | danger | plain
	Confirm string
}

// decisionButtons lists the buttons for the decisions an item takes.
func decisionButtons(itemKind, creditHandle string) []moderationDecision {
	var out []moderationDecision
	for _, d := range abraxas.DecisionsFor(itemKind) {
		switch d {
		case abraxas.DecisionApprove:
			out = append(out, moderationDecision{Value: d, Label: "✓ Approve", Style: "ok"})
		case abraxas.DecisionAgeGate:
			out = append(out, moderationDecision{Value: d, Label: "18+ Age-gate", Style: "warn"})
		case abraxas.DecisionBlock:
			out = append(out, moderationDecision{Value: d, Label: "✕ Block", Style: "danger",
				Confirm: "Block this? It is removed from every surface."})
		case abraxas.DecisionCredit:
			label := "Credit the original owner"
			if creditHandle != "" {
				label = "Credit @" + creditHandle
			}
			out = append(out, moderationDecision{Value: d, Label: label, Style: "warn"})
		case abraxas.DecisionReject:
			out = append(out, moderationDecision{Value: d, Label: "Reject", Style: "plain"})
		}
	}
	return out
}

var stageLabels = map[string]string{
	abraxas.StageMLReview:    "ML review",
	abraxas.StageAdminReview: "Admin review",
}

// handleOfPIAL resolves a PIAL to the handle it currently answers to. An
// identity with no account (erased) has no handle; that is not an error.
func (h *Handler) handleOfPIAL(pial string) (string, error) {
	if pial == "" {
		return "", nil
	}
	handle, _, err := dbpkg.UploaderCredit(h.db, pial)
	return handle, err
}

// handlesOfPIALs resolves several PIALs, keeping order and dropping the ones
// with no account.
func (h *Handler) handlesOfPIALs(pials []string) ([]string, error) {
	out := make([]string, 0, len(pials))
	seen := map[string]bool{}
	for _, p := range pials {
		handle, err := h.handleOfPIAL(p)
		if err != nil {
			return nil, err
		}
		if handle != "" && !seen[handle] {
			seen[handle] = true
			out = append(out, handle)
		}
	}
	return out, nil
}

// fillSubject loads what a card shows of its subject from this brain's own
// records. A subject this brain does not hold (an asset, a stream) shows its
// kind and id only.
func (h *Handler) fillSubject(c *moderationCard, subject abraxas.Subject, subjectPIAL string) error {
	c.SubjectKind = subject.Kind
	if subject.Kind == "work" {
		ws, err := dbpkg.ModerationWorkSubject(h.db, subject.ID)
		if err != nil {
			return err
		}
		if ws != nil {
			c.SubjectLink = "/work/" + subject.ID
			c.AuthorHandle = ws.AuthorHandle
			c.Body = ws.Body
			c.ThumbURL = ws.ThumbURL
			return nil
		}
	}
	if subject.Kind == "account" {
		// An account subject's id is the holder's PIAL: resolved here, never shown.
		handle, err := h.handleOfPIAL(subject.ID)
		if err != nil {
			return err
		}
		if handle != "" {
			c.SubjectLink = "/" + handle
			c.AuthorHandle = handle
		}
		return nil
	}
	if subject.Kind == "stream" {
		c.SubjectLink = "/live/" + subject.ID
	}
	handle, err := h.handleOfPIAL(subjectPIAL)
	if err != nil {
		return err
	}
	c.AuthorHandle = handle
	return nil
}

// reviewCard builds the card of one review item.
func (h *Handler) reviewCard(item abraxas.ReviewItem) (moderationCard, error) {
	c := moderationCard{
		ID: item.ReviewID, FacetID: moderationFacetID(item.ReviewID), Source: "review",
		Stage: item.Stage, StageLabel: stageLabels[item.Stage], Reason: item.Reason,
		Signals: item.Signals, CreatedAt: item.CreatedAt,
		ItemKind: item.ItemKind, Category: item.Category, CurrentVerdict: item.CurrentVerdict,
	}
	if err := h.fillSubject(&c, item.Subject, item.PIAL); err != nil {
		return c, err
	}
	names := make([]string, 0, len(item.Scores))
	for name := range item.Scores {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := item.Scores[name]
		c.Scores = append(c.Scores, moderationScore{Name: name, Pct: int(v*100 + 0.5), Warn: v > 0.5})
	}
	if item.Ownership != nil {
		handle, err := h.handleOfPIAL(item.Ownership.OriginalPIAL)
		if err != nil {
			return c, err
		}
		c.CreditHandle, c.CreditKind = handle, item.Ownership.Kind
	}
	c.Decisions = decisionButtons(item.ItemKind, c.CreditHandle)
	c.MLOutcome = mlOutcomeText(item.MLOutcome)
	evidence, err := h.evidenceLines(item.Evidence)
	if err != nil {
		return c, err
	}
	c.Evidence = evidence
	return c, nil
}

// mlOutcomeText reads the ML stage's conclusion from ml_outcome: its outcome
// word and its reason, when it gave them.
func mlOutcomeText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var o struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return ""
	}
	switch {
	case o.Outcome != "" && o.Reason != "":
		return strings.ReplaceAll(o.Outcome, "_", " ") + " — " + o.Reason
	case o.Outcome != "":
		return strings.ReplaceAll(o.Outcome, "_", " ")
	}
	return o.Reason
}

// evidenceLines reads the parts of an item's evidence a moderator needs, by
// name: a banned match's layer, category and score; how many reports and
// matching uploads there are; who claims it, by handle. Evidence also names
// PIALs (uploader, claimants, escalator), and those are only ever resolved.
func (h *Handler) evidenceLines(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var e struct {
		Match *struct {
			Layer    string  `json:"layer"`
			Category string  `json:"category"`
			Match    string  `json:"match"`
			Score    float64 `json:"score"`
		} `json:"match"`
		Claimants     []string          `json:"claimants"`
		MatchedAssets []json.RawMessage `json:"matched_assets"`
		ReportCount   int               `json:"report_count"`
		Note          string            `json:"note"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return nil, nil
	}
	var out []string
	if m := e.Match; m != nil && m.Layer != "" {
		line := m.Match + " match on " + m.Layer
		if m.Category != "" {
			line += " (" + m.Category + ")"
		}
		if m.Score > 0 {
			line += fmt.Sprintf(", score %.2f", m.Score)
		}
		out = append(out, strings.TrimSpace(line))
	}
	if len(e.Claimants) > 0 {
		handles, err := h.handlesOfPIALs(e.Claimants)
		if err != nil {
			return nil, err
		}
		if len(handles) > 0 {
			out = append(out, "Claimed by @"+strings.Join(handles, ", @"))
		}
	}
	if n := len(e.MatchedAssets); n > 0 {
		out = append(out, fmt.Sprintf("%d of the claimant's own uploads match", n))
	}
	if e.ReportCount > 0 {
		out = append(out, fmt.Sprintf("%d report(s) filed", e.ReportCount))
	}
	if e.Note != "" {
		out = append(out, "Note: "+e.Note)
	}
	return out, nil
}

// reportCard builds the card of one report.
func (h *Handler) reportCard(r abraxas.Report) (moderationCard, error) {
	c := moderationCard{
		ID: r.ReportID, FacetID: moderationFacetID(r.ReportID), Source: "report",
		Stage: r.Stage, StageLabel: stageLabels[r.Stage], Kind: r.Kind,
		Reason: r.Reason, Detail: r.Detail, ReportCount: r.ReportCount, CreatedAt: r.CreatedAt,
	}
	if err := h.fillSubject(&c, r.Subject, ""); err != nil {
		return c, err
	}
	reporters, err := h.handlesOfPIALs(r.ReporterPIALs)
	if err != nil {
		return c, err
	}
	c.Reporters = reporters
	owners := make([]string, 0, len(r.MatchedAssets))
	for _, m := range r.MatchedAssets {
		owners = append(owners, m.PIAL)
	}
	if c.MatchedOwners, err = h.handlesOfPIALs(owners); err != nil {
		return c, err
	}
	c.CanBan = r.Kind != abraxas.ReportKindOwnershipClaim
	return c, nil
}

// moderationPanel is what the moderation tab renders.
type moderationPanel struct {
	MLReview    []moderationCard
	AdminReview []moderationCard
	Reports     []moderationCard
	// Problem names a section Abraxas could not answer for. The panel says so
	// rather than showing an empty queue that reads as "nothing to do".
	Problem string
}

func (h *Handler) loadModerationPanel(ctx context.Context) moderationPanel {
	var p moderationPanel
	var problems []string
	fail := func(what string, err error) {
		log.Printf("[moderation] %s: %v", what, err)
		problems = append(problems, what)
	}
	for _, stage := range []string{abraxas.StageMLReview, abraxas.StageAdminReview} {
		items, err := h.abraxas.ListReview(ctx, stage, moderationListLimit)
		if err != nil {
			fail(stageLabels[stage]+" queue", err)
			continue
		}
		for _, item := range items {
			c, err := h.reviewCard(item)
			if err != nil {
				fail("review item "+item.ReviewID, err)
				continue
			}
			if stage == abraxas.StageMLReview {
				p.MLReview = append(p.MLReview, c)
			} else {
				p.AdminReview = append(p.AdminReview, c)
			}
		}
	}
	reports, err := h.abraxas.ListReports(ctx, "open", moderationListLimit)
	if err != nil {
		fail("reports", err)
	}
	for _, r := range reports {
		c, err := h.reportCard(r)
		if err != nil {
			fail("report "+r.ReportID, err)
			continue
		}
		p.Reports = append(p.Reports, c)
	}
	if len(problems) > 0 {
		p.Problem = "Abraxas could not be read for: " + strings.Join(problems, ", ") + ". These sections may be incomplete."
	}
	return p
}

// openReportCount is how many reports Abraxas holds open, for the admin
// badges. A count Abraxas could not give is logged and shown as zero; the
// moderation tab itself says when Abraxas is unreadable.
func (h *Handler) openReportCount(ctx context.Context) int {
	cctx, cancel := context.WithTimeout(ctx, moderationCallTimeout)
	defer cancel()
	reports, err := h.abraxas.ListReports(cctx, "open", 200)
	if err != nil {
		log.Printf("[moderation] counting open reports: %v", err)
		return 0
	}
	return len(reports)
}

// renderModerationPanel renders the admin_panel_moderation Facet.
func (h *Handler) renderModerationPanel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*moderationCallTimeout)
	defer cancel()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_panel_moderation", h.loadModerationPanel(ctx))
}

// renderModerationCard answers with one card Facet.
func (h *Handler) renderModerationCard(w http.ResponseWriter, c moderationCard) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "moderation_card", c)
}

// moderationActor is the admin acting, or nil after answering 403.
func (h *Handler) moderationActor(w http.ResponseWriter, r *http.Request) *model.User {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() || user.PIALID == "" {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return nil
	}
	return user
}

// moderationItemID reads and checks the {id} path value.
func moderationItemID(r *http.Request) (string, bool) {
	parsed, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return "", false
	}
	return parsed.String(), true
}

var decisionLabels = map[string]string{
	abraxas.DecisionApprove: "Approved",
	abraxas.DecisionAgeGate: "Age-gated",
	abraxas.DecisionBlock:   "Blocked",
	abraxas.DecisionCredit:  "Credited to the original owner",
	abraxas.DecisionReject:  "Claim rejected",
}

// moderationReviewDecide — POST /admin/moderation/review/{id}/decide
// Form: decision (approve | age_gate | block | credit | reject), note, and
// credit_handle so the decided card can say whom it credited. Abraxas refuses
// a decision the item's kind does not take.
func (h *Handler) moderationReviewDecide(w http.ResponseWriter, r *http.Request) {
	admin := h.moderationActor(w, r)
	if admin == nil {
		return
	}
	id, ok := moderationItemID(r)
	if !ok {
		http.Error(w, "no such review item", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	decision := r.FormValue("decision")
	label, known := decisionLabels[decision]
	if !known {
		http.Error(w, "unknown decision", http.StatusBadRequest)
		return
	}
	note := truncate(strings.TrimSpace(r.FormValue("note")), 500)
	card := moderationCard{
		ID: id, FacetID: moderationFacetID(id), Source: "review",
		CreditHandle: r.FormValue("credit_handle"),
	}
	ctx, cancel := context.WithTimeout(r.Context(), moderationCallTimeout)
	defer cancel()
	err := h.abraxas.Decide(ctx, id, abraxas.Decision{Decision: decision, ActorPIAL: admin.PIALID, Note: note})
	switch {
	case err == nil:
		card.Decided = label + " by @" + admin.Handle
		if decision == abraxas.DecisionCredit && card.CreditHandle != "" {
			card.Decided = "Credited to @" + card.CreditHandle + " by @" + admin.Handle
		}
	case errors.Is(err, abraxas.ErrConflict):
		card.Decided = "Already decided"
	case errors.Is(err, abraxas.ErrNotFound):
		card.Decided = "No longer in the queue"
	default:
		log.Printf("[moderation] %s deciding %s on review %s: %v", admin.Handle, decision, id, err)
		card.Problem = "Abraxas did not take the decision. Try again."
		w.WriteHeader(http.StatusBadGateway)
		h.renderModerationCard(w, card)
		return
	}
	go LogAdminAction(h.db, admin.ID, "moderation_"+decision, "abraxas_review", id, requestIP(r), map[string]interface{}{"note": note})
	log.Printf("[moderation] %s decided %s on review %s", admin.Handle, decision, id)
	h.renderModerationCard(w, card)
}

// moderationReportAction — POST /admin/moderation/report/{id}/{action}
// action: resolve | ban | dismiss | escalate. "ban" resolves the report and
// bans the reported subject's owner, whose PIAL is found here, server-side.
func (h *Handler) moderationReportAction(w http.ResponseWriter, r *http.Request) {
	admin := h.moderationActor(w, r)
	if admin == nil {
		return
	}
	id, ok := moderationItemID(r)
	if !ok {
		http.Error(w, "no such report", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	note := truncate(strings.TrimSpace(r.FormValue("note")), 500)
	card := moderationCard{ID: id, FacetID: moderationFacetID(id), Source: "report"}
	ctx, cancel := context.WithTimeout(r.Context(), moderationCallTimeout)
	defer cancel()
	action := r.PathValue("action")
	var err error
	switch action {
	case "resolve":
		err = h.abraxas.ResolveReport(ctx, id, abraxas.ReportAction{ActorPIAL: admin.PIALID, Status: "resolved", Note: note})
		card.Decided = "Resolved by @" + admin.Handle
	case "ban":
		var target string
		target, err = h.reportBanTarget(ctx, id)
		if err == nil {
			err = h.abraxas.ResolveReport(ctx, id, abraxas.ReportAction{
				ActorPIAL: admin.PIALID, Status: "resolved", Note: note,
				Action: abraxas.ReportActionBan, BanPIAL: target,
			})
		}
		card.Decided = "Resolved with a ban by @" + admin.Handle
	case "dismiss":
		err = h.abraxas.ResolveReport(ctx, id, abraxas.ReportAction{ActorPIAL: admin.PIALID, Status: "dismissed", Note: note})
		card.Decided = "Dismissed by @" + admin.Handle
	case "escalate":
		err = h.abraxas.EscalateReport(ctx, id, abraxas.ReportAction{ActorPIAL: admin.PIALID, Note: note})
		card.Decided = "Escalated to admin review by @" + admin.Handle
	default:
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}
	switch {
	case err == nil:
	case errors.Is(err, abraxas.ErrConflict):
		card.Decided = "Already closed"
	case errors.Is(err, abraxas.ErrNotFound):
		card.Decided = "No longer open"
	case errors.Is(err, errNoBanTarget):
		card.Decided = ""
		card.Problem = "Nobody to ban: the reported content has no owner this platform can name. Resolve it without a ban."
		w.WriteHeader(http.StatusConflict)
		h.renderModerationCard(w, card)
		return
	default:
		log.Printf("[moderation] %s %s report %s: %v", admin.Handle, action, id, err)
		card.Decided = ""
		card.Problem = "Abraxas did not take that. Try again."
		w.WriteHeader(http.StatusBadGateway)
		h.renderModerationCard(w, card)
		return
	}
	go LogAdminAction(h.db, admin.ID, "report_"+action, "abraxas_report", id, requestIP(r), map[string]interface{}{"note": note})
	log.Printf("[moderation] %s %s report %s", admin.Handle, action, id)
	h.renderModerationCard(w, card)
}

// errNoBanTarget: the reported subject belongs to nobody this brain can name.
var errNoBanTarget = errors.New("the reported subject has no owner to ban")

// reportBanTarget is the PIAL a "resolve with ban" falls on: an account
// subject's own id (its PIAL), a work's author, a Vision's lane owner, or the
// owner Abraxas recorded for anything else.
func (h *Handler) reportBanTarget(ctx context.Context, reportID string) (string, error) {
	r, err := h.abraxas.GetReport(ctx, reportID)
	if err != nil {
		return "", err
	}
	switch r.Subject.Kind {
	case "account":
		return r.Subject.ID, nil
	case "work":
		pial, err := dbpkg.AuthorPIALOfWork(h.db, r.Subject.ID)
		if err != nil {
			return "", err
		}
		if pial != "" {
			return pial, nil
		}
	case "vision":
		if pial, _, ok := strings.Cut(r.Subject.ID, "."); ok && pial != "" {
			return pial, nil
		}
	}
	if r.ReportedPIAL != "" {
		return r.ReportedPIAL, nil
	}
	return "", errNoBanTarget
}

// hashPrefix shortens a hex digest for a log line without panicking on a short
// or empty one.
func hashPrefix(hexDigest string) string {
	if len(hexDigest) <= 16 {
		return hexDigest
	}
	return hexDigest[:16]
}

// bannedCategories are the categories an admin may register.
var bannedCategories = map[string]bool{"csam": true, "gore": true, "hate": true, "spam": true}

// moderationAdmin reads the admin and a JSON body, answering the failure.
func (h *Handler) moderationAdmin(w http.ResponseWriter, r *http.Request, into any) *model.User {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() || user.PIALID == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(into); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return nil
	}
	return user
}

// answerRegistry answers a registry call: Abraxas's refusal of a value is the
// admin's to correct (400), anything else is Abraxas being unreachable (502).
func answerRegistry(w http.ResponseWriter, err error, what string) {
	if err == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
		return
	}
	log.Printf("[admin] %s: %v", what, err)
	switch {
	case errors.Is(err, abraxas.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case strings.Contains(err.Error(), "HTTP 400"):
		http.Error(w, "Abraxas refused the value", http.StatusBadRequest)
	default:
		http.Error(w, "the banned registry could not be reached", http.StatusBadGateway)
	}
}

// ModerationHashBan registers a value in the banned registry, which is
// Abraxas's: it matches every upload and live frame against it. The admin is
// recorded by PIAL, server to server, and never shown.
// POST /admin/moderation/hash-ban  {hash_type, hash_value, category, note}
func (h *Handler) ModerationHashBan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		HashType  string `json:"hash_type"`
		HashValue string `json:"hash_value"`
		Category  string `json:"category"`
		Note      string `json:"note"`
	}
	user := h.moderationAdmin(w, r, &req)
	if user == nil {
		return
	}
	if !bannedCategories[req.Category] {
		http.Error(w, "invalid category", http.StatusBadRequest)
		return
	}
	hashValue := strings.TrimSpace(req.HashValue)
	if err := validateBannedHashValue(req.HashType, hashValue); err != nil {
		// A malformed entry does not fail loudly later — it simply never matches
		// anything. It is refused here, before it reaches the registry.
		log.Printf("[admin] %s offered an unusable %s value: %v", user.Handle, req.HashType, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), moderationCallTimeout)
	defer cancel()
	err := h.abraxas.AddBanned(ctx, abraxas.BannedHash{
		HashType: req.HashType, HashValue: hashValue, Category: req.Category,
		Note: truncate(req.Note, 500), AddedBy: user.PIALID,
	})
	if err == nil {
		log.Printf("[admin] %s registered banned %s %s (%s)", user.Handle, req.HashType, hashPrefix(hashValue), req.Category)
	}
	answerRegistry(w, err, user.Handle+" registering a banned "+req.HashType)
}

// ModerationHashRevoke takes a value out of the banned registry.
// POST /admin/moderation/hash-revoke  {hash_type, hash_value, reason}
func (h *Handler) ModerationHashRevoke(w http.ResponseWriter, r *http.Request) {
	var req struct {
		HashType  string `json:"hash_type"`
		HashValue string `json:"hash_value"`
		Reason    string `json:"reason"`
	}
	user := h.moderationAdmin(w, r, &req)
	if user == nil {
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		http.Error(w, "say why the value is revoked", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), moderationCallTimeout)
	defer cancel()
	err := h.abraxas.RevokeBanned(ctx, req.HashType, strings.TrimSpace(req.HashValue), user.PIALID, truncate(reason, 500))
	if err == nil {
		log.Printf("[admin] %s revoked banned %s %s", user.Handle, req.HashType, hashPrefix(strings.TrimSpace(req.HashValue)))
	}
	answerRegistry(w, err, user.Handle+" revoking a banned "+req.HashType)
}

// ModerationAssetBan bans an existing asset's material on every layer its
// fingerprint has (exact bytes, perceptual hashes, index entries) and blocks
// the asset.
// POST /admin/moderation/asset-ban  {asset_id, category, note}
func (h *Handler) ModerationAssetBan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID  string `json:"asset_id"`
		Category string `json:"category"`
		Note     string `json:"note"`
	}
	user := h.moderationAdmin(w, r, &req)
	if user == nil {
		return
	}
	assetID, err := uuid.Parse(strings.TrimSpace(req.AssetID))
	if err != nil {
		http.Error(w, "asset_id is not an asset id", http.StatusBadRequest)
		return
	}
	if !bannedCategories[req.Category] {
		http.Error(w, "invalid category", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), moderationCallTimeout)
	defer cancel()
	err = h.abraxas.BanFromAsset(ctx, assetID.String(), req.Category, truncate(req.Note, 500), user.PIALID)
	if err == nil {
		log.Printf("[admin] %s banned the material of asset %s (%s)", user.Handle, assetID, req.Category)
	}
	answerRegistry(w, err, user.Handle+" banning asset "+assetID.String())
}

// validateBannedHashValue refuses a registry entry that could not do its job.
//
// The two failure modes are opposite and both silent:
//
//   - A malformed value (wrong length, not hex) matches nothing. The gate then
//     reads as enforced while catching nothing, which is how this registry sat
//     empty and unnoticed in the first place.
//   - A degenerate perceptual hash matches far too much. content-scan compares
//     a phash by Hamming distance, so the hash of a near-uniform frame — nearly
//     all zero or nearly all one bits — sits inside the block radius of a large
//     share of ordinary dark or blank media. Registering one would refuse
//     innocent uploads across the platform.
//
// An operator who cannot register a frame's phash can still register its
// sha256, which carries no such risk.
func validateBannedHashValue(hashType, value string) error {
	switch hashType {
	case "sha256":
		if len(value) != 64 || !isLowerHex(value) {
			return errors.New("sha256 must be 64 lowercase hex characters")
		}
	case "phash":
		if len(value) != 16 || !isLowerHex(value) {
			return errors.New("phash must be 16 lowercase hex characters (a 64-bit imagehash phash)")
		}
		value64, err := strconv.ParseUint(value, 16, 64)
		if err != nil {
			return errors.New("phash is not a 64-bit hex value")
		}
		ones := bits.OnesCount64(value64)
		if ones < 12 || ones > 52 {
			return fmt.Errorf("phash %s has %d of 64 bits set — that is the hash of a near-uniform "+
				"image, and matching it by distance would block ordinary blank or dark media; "+
				"register the sha256 instead", value, ones)
		}
	case "audio_fp", "chromaprint":
		// A chromaprint fingerprint is base64 of compressed data; its length
		// varies with duration. Only an obviously truncated value is refused.
		if len(value) < 16 {
			return errors.New("audio_fp is too short to be a chromaprint fingerprint")
		}
	default:
		return fmt.Errorf("unknown hash type %q", hashType)
	}
	return nil
}

// isLowerHex reports whether every character is a lowercase hex digit. Case
// matters because the registry is compared with string equality, so an
// upper-case duplicate would be a second row that never matches.
func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
