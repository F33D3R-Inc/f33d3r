package handler

// wallet_facets.go — the wallet's history and governance, rendered on the server.
//
// These surfaces used to be drawn in the browser from Ain Soph's JSON, reached
// through a blanket reverse proxy at /ainsoph/. That JSON is keyed on PIALs: the
// balance and history routes take one in the path, every history row names the
// other party's PIAL as `counterparty`, and every proposal names its author's
// PIAL as `creator_id`. A PIAL is a person's root identity and is never handed to
// a client, because a client that held one could link that person's accounts.
// So the proxy is gone, the browser asks for finished Fragments, and the server
// talks to Ain Soph with the session's PIAL and renders only what a page may show.
//
// In Facet Architecture order:
//
//	triggering event  page load / fa:resync / fa:wallet-moved / fa:governance-changed
//	                  (GET), and POST /events governance_vote | governance_propose
//	server handler    facetWalletTx, facetGovernanceProposals, facetGovernanceProposal,
//	                  governanceVoteEvent, governanceProposeEvent
//	Facet renderer    wallet_tx_rows, governance_proposals, governance_proposal_detail,
//	                  governance_propose_result
//	fragment          finished HTML, carrying data-facet-id; no PIAL in any of it
//	stream mutation   the answer to the request; a value move also reaches the page
//	                  as the `balance` SSE event, which raises fa:wallet-moved

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// walletTxRow is one history row as the page shows it. The other party is not
// named: the ledger records PIALs, which never reach a page, and records no
// account, so there is no persona the row could truthfully name.
type walletTxRow struct {
	Direction string // in | out | self
	Label     string
	Amount    string
	TimeAgo   string
}

// facetWalletTx — GET /facets/wallet/tx: the session's recent transactions.
func (h *Handler) facetWalletTx(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	data := map[string]interface{}{"AccountID": user.ID}
	walletID := h.walletIdentity(user)
	if walletID == "" {
		data["Notice"] = "Wallet unavailable — no identity for this account."
		h.renderPartial(w, "wallet_tx_rows", data)
		return
	}
	var history struct {
		Entries []struct {
			TxType    string    `json:"tx_type"`
			Direction string    `json:"direction"`
			NetAet    string    `json:"net_aet"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"entries"`
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := h.ainSophGet(ctx, "/v1/transactions/"+url.PathEscape(walletID)+"?limit=20", &history); err != nil {
		log.Printf("[wallet] transactions for account %s: %v", user.ID, err)
		data["Notice"] = "Could not load transactions."
		h.renderPartial(w, "wallet_tx_rows", data)
		return
	}
	rows := make([]walletTxRow, 0, len(history.Entries))
	for _, e := range history.Entries {
		label := e.TxType
		if label != "" {
			label = strings.ToUpper(label[:1]) + label[1:]
		}
		rows = append(rows, walletTxRow{
			Direction: e.Direction,
			Label:     label,
			Amount:    e.NetAet,
			TimeAgo:   TimeAgo(e.CreatedAt),
		})
	}
	data["Rows"] = rows
	h.renderPartial(w, "wallet_tx_rows", data)
}

// governanceProposal is a proposal as the page shows it. Ain Soph's creator_id
// (a PIAL) is deliberately not decoded.
type governanceProposal struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	WeightModel string    `json:"weight_model"`
	Options     []string  `json:"options"`
	EndsAt      time.Time `json:"ends_at"`
	Status      string    `json:"status"`
	TotalVotes  int64     `json:"total_votes"`
}

// governanceResult is one option's tally.
type governanceResult struct {
	Option    string  `json:"option"`
	VoteCount int64   `json:"vote_count"`
	Pct       float64 `json:"pct"`
}

// governanceDetail is Ain Soph's proposal detail, minus every identity.
type governanceDetail struct {
	Proposal governanceProposal `json:"proposal"`
	Results  []governanceResult `json:"results"`
	Leading  *string            `json:"leading"`
}

// governanceWeightModels is Ain Soph's closed weight vocabulary, in form order.
var governanceWeightModels = []struct{ Value, Label string }{
	{"identity", "One identity, one vote"},
	{"equal", "Equal"},
	{"token", "AET-weighted"},
}

func governanceWeightModelKnown(m string) bool {
	for _, wm := range governanceWeightModels {
		if wm.Value == m {
			return true
		}
	}
	return false
}

// facetGovernanceProposals — GET /facets/wallet/proposals: the proposal list.
func (h *Handler) facetGovernanceProposals(w http.ResponseWriter, r *http.Request) {
	h.renderGovernanceProposals(w, r)
}

func (h *Handler) renderGovernanceProposals(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{}
	var proposals []governanceProposal
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := h.ainSophGet(ctx, "/v1/proposals", &proposals); err != nil {
		log.Printf("[governance] listing proposals: %v", err)
		data["Notice"] = "Could not load proposals."
	}
	data["Proposals"] = proposals
	h.renderPartial(w, "governance_proposals", data)
}

// facetGovernanceProposal — GET /facets/wallet/proposals/{id}: one proposal and
// its tally.
func (h *Handler) facetGovernanceProposal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, "unknown proposal", http.StatusNotFound)
		return
	}
	h.renderGovernanceProposal(w, r, id, "")
}

func (h *Handler) renderGovernanceProposal(w http.ResponseWriter, r *http.Request, id, notice string) {
	var d governanceDetail
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := h.ainSophGet(ctx, "/v1/proposals/"+url.PathEscape(id), &d); err != nil {
		log.Printf("[governance] proposal %s: %v", id, err)
		h.renderPartial(w, "governance_proposal_detail", map[string]interface{}{
			"ID": id, "Unavailable": true, "Notice": "Could not load this proposal.",
		})
		return
	}
	leading := ""
	if d.Leading != nil {
		leading = *d.Leading
	}
	h.renderPartial(w, "governance_proposal_detail", map[string]interface{}{
		"ID":       id,
		"Proposal": d.Proposal,
		"Results":  d.Results,
		"Leading":  leading,
		"Notice":   notice,
	})
}

// governanceVoteEvent — POST /events {event_type: governance_vote, proposal_id,
// option}. The voter is the session's identity: Ain Soph derives it from the
// X-Pial-Identity header this server sets, and the body names no voter.
func (h *Handler) governanceVoteEvent(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	walletID := h.walletIdentity(user)
	if walletID == "" {
		htmxError(w, r, "not authenticated", http.StatusUnauthorized)
		return
	}
	id := strings.TrimSpace(r.FormValue("proposal_id"))
	option := strings.TrimSpace(r.FormValue("option"))
	if _, err := uuid.Parse(id); err != nil || option == "" {
		htmxError(w, r, "proposal_id and option required", http.StatusBadRequest)
		return
	}
	status, msg, err := h.ainSophPost(r.Context(), walletID, "/v1/proposals/"+url.PathEscape(id)+"/vote",
		map[string]interface{}{"option": option})
	if err != nil {
		log.Printf("[governance] vote on %s by account %s: %v", id, user.ID, err)
		h.renderGovernanceProposal(w, r, id, "The vote could not be sent. Try again.")
		return
	}
	notice := ""
	if status != http.StatusOK && status != http.StatusCreated {
		notice = msg
	} else {
		w.Header().Set("HX-Trigger", "fa:governance-changed")
	}
	h.renderGovernanceProposal(w, r, id, notice)
}

// governanceProposeEvent — POST /events {event_type: governance_propose, title,
// description, weight_model, ends_at}. ends_at is the form's datetime-local
// value, read as UTC (the form says so). The author is the session's identity.
func (h *Handler) governanceProposeEvent(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	walletID := h.walletIdentity(user)
	if walletID == "" {
		htmxError(w, r, "not authenticated", http.StatusUnauthorized)
		return
	}
	answer := func(ok bool, msg string) {
		if ok {
			w.Header().Set("HX-Trigger", "fa:governance-changed")
		}
		h.renderPartial(w, "governance_propose_result", map[string]interface{}{"OK": ok, "Message": msg})
	}
	title := strings.TrimSpace(r.FormValue("title"))
	description := strings.TrimSpace(r.FormValue("description"))
	weight := strings.TrimSpace(r.FormValue("weight_model"))
	if title == "" || len([]rune(title)) > 120 {
		answer(false, "A title of up to 120 characters is required.")
		return
	}
	if !governanceWeightModelKnown(weight) {
		answer(false, "Choose a weight model.")
		return
	}
	endsAt, err := time.ParseInLocation("2006-01-02T15:04", strings.TrimSpace(r.FormValue("ends_at")), time.UTC)
	if err != nil {
		answer(false, "Choose when voting ends.")
		return
	}
	if !endsAt.After(time.Now()) {
		answer(false, "Voting has to end in the future.")
		return
	}
	status, msg, err := h.ainSophPost(r.Context(), walletID, "/v1/proposals", map[string]interface{}{
		"title":        title,
		"description":  description,
		"weight_model": weight,
		"ends_at":      endsAt.Format(time.RFC3339),
	})
	if err != nil {
		log.Printf("[governance] proposal by account %s: %v", user.ID, err)
		answer(false, "The proposal could not be sent. Try again.")
		return
	}
	if status != http.StatusOK && status != http.StatusCreated {
		answer(false, msg)
		return
	}
	answer(true, "Proposal created.")
}

// ainSophPost sends one authenticated write to Ain Soph as the given identity.
// A transport failure is an error; an answer Ain Soph gave is its status and
// its message, so the caller can show the refusal it was given.
func (h *Handler) ainSophPost(ctx context.Context, pialID, path string, body interface{}) (int, string, error) {
	if h.cfg.AinSophURL == "" {
		return 0, "", errors.New("AIN_SOPH_URL is not set")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, "", fmt.Errorf("encoding %s body: %w", path, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.AinSophURL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	// Ain Soph answers nothing without the service key; the identity rides
	// alongside it and is honoured only with it.
	req.Header.Set("X-Internal-Key", h.cfg.InternalAPIKey)
	req.Header.Set("X-Pial-Identity", pialID)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return 0, "", fmt.Errorf("reading %s answer: %w", path, err)
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return resp.StatusCode, "", nil
	}
	var refusal struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(answer, &refusal) == nil {
		msg = refusal.Message
		if msg == "" {
			msg = refusal.Error
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("Ain Soph refused (%d).", resp.StatusCode)
	}
	return resp.StatusCode, msg, nil
}
