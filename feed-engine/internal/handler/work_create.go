package handler

// work_create.go — the outcome of creating a work, and the one way it is told.
//
// Every create ends in exactly one WorkCreateState. Each state has one HTTP
// status and one rendering: the compose status Facet
// facet:f33d3r:compose:<account>:status, delivered to every connection the
// posting account holds over FA Live. The HTTP answer carries the native apps' contract — the
// JSON receipt {"work_id","cid"} on 201, the reason as text otherwise — and the
// state's name in X-Work-Create-State.

import (
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// WorkCreateState is the closed vocabulary of create outcomes.
type WorkCreateState string

const (
	WorkStateOk                WorkCreateState = "ok"
	WorkStateDuplicate         WorkCreateState = "duplicate"
	WorkStateTextTooLong       WorkCreateState = "text_too_long"
	WorkStateTextBlank         WorkCreateState = "text_blank"
	WorkStateInvalidField      WorkCreateState = "invalid_field"
	WorkStateCitedNotFound     WorkCreateState = "cited_not_found"
	WorkStateRepliesRestricted WorkCreateState = "replies_restricted"
	WorkStatePostingRestricted WorkCreateState = "posting_restricted"
	WorkStateRateLimited       WorkCreateState = "rate_limited"
	WorkStateServerError       WorkCreateState = "server_error"
	// WorkStateAdultRecordRequired refuses an 18+ work whose author has no
	// passing creator verification at Verity: 18 U.S.C. § 2257 requires the
	// record before the work exists. 403, so a client that does not know the
	// state reads it as posting_restricted and shows the server's reason.
	WorkStateAdultRecordRequired WorkCreateState = "adult_record_required"
	// WorkStateComplianceUnavailable refuses an 18+ work when Verity could not
	// be asked. Fail closed: nothing 18+ is published unchecked.
	WorkStateComplianceUnavailable WorkCreateState = "compliance_unavailable"
)

// Status is the one HTTP status of a state.
func (s WorkCreateState) Status() int {
	switch s {
	case WorkStateOk:
		return http.StatusCreated
	case WorkStateDuplicate:
		return http.StatusConflict
	case WorkStateTextTooLong, WorkStateTextBlank:
		return http.StatusUnprocessableEntity
	case WorkStateInvalidField:
		return http.StatusBadRequest
	case WorkStateCitedNotFound:
		return http.StatusNotFound
	case WorkStateRepliesRestricted, WorkStatePostingRestricted, WorkStateAdultRecordRequired:
		return http.StatusForbidden
	case WorkStateComplianceUnavailable:
		return http.StatusServiceUnavailable
	case WorkStateRateLimited:
		return http.StatusTooManyRequests
	}
	return http.StatusInternalServerError
}

// workCreateAnswer is one decided outcome. Field and Detail say what an
// InvalidField is about; WorkID and CID name the work an Ok stands for.
type workCreateAnswer struct {
	State       WorkCreateState
	Field       string
	Detail      string
	WorkID      string
	CID         string
	Replayed    bool
	ScheduledAt *time.Time
	Over        int // weighted units over the limit, for TextTooLong
}

func refuse(state WorkCreateState) *workCreateAnswer { return &workCreateAnswer{State: state} }

func invalidField(field, detail string) *workCreateAnswer {
	return &workCreateAnswer{State: WorkStateInvalidField, Field: field, Detail: detail}
}

// message is what the poster reads.
func (a *workCreateAnswer) message() string {
	switch a.State {
	case WorkStateOk:
		switch {
		case a.Replayed:
			return "Already posted."
		case a.ScheduledAt != nil:
			return "Scheduled for " + a.ScheduledAt.UTC().Format("Jan 2, 2006 15:04 UTC") + "."
		}
		return "Posted."
	case WorkStateDuplicate:
		return "You already posted this."
	case WorkStateTextTooLong:
		return fmt.Sprintf("Your post is %d over the limit.", a.Over)
	case WorkStateTextBlank:
		return "Write something, or add media, a poll or a voice note."
	case WorkStateInvalidField:
		return "This post could not be sent: " + a.Field + " — " + a.Detail + "."
	case WorkStateCitedNotFound:
		return "That post is no longer available."
	case WorkStateRepliesRestricted:
		return "The author has limited who can reply."
	case WorkStatePostingRestricted:
		return "Posting is restricted on this account."
	case WorkStateRateLimited:
		return "You have reached the posting limit. Try again later."
	case WorkStateAdultRecordRequired:
		return "18+ posts need creator verification first. Complete it at /kyc?mode=creator, then post again."
	case WorkStateComplianceUnavailable:
		return "We could not confirm your creator verification right now, so this 18+ post was not sent. Try again shortly."
	}
	return "Something went wrong. Your post was not sent."
}

// composeStatusFacetID is the compose status Facet of one posting account.
func composeStatusFacetID(account string) string {
	return "facet:f33d3r:compose:" + account + ":status"
}

// renderComposeStatus renders the compose status Fragment for an answer.
func (h *Handler) renderComposeStatus(account string, a *workCreateAnswer) string {
	return h.renderFacet("compose_status", map[string]interface{}{
		"Account": account,
		"State":   string(a.State),
		"Message": a.message(),
		"WorkID":  a.WorkID,
	})
}

// answerWorkCreate delivers an outcome: the Fragment to the poster's stream,
// then the HTTP answer.
func (h *Handler) answerWorkCreate(w http.ResponseWriter, user *model.User, a *workCreateAnswer) {
	if a.State == WorkStateServerError || a.State == WorkStateInvalidField {
		log.Printf("[work] %s for %s: %s %s", a.State, user.PIALID, a.Field, a.Detail)
	}
	if fragment := h.renderComposeStatus(user.ID, a); fragment != "" {
		PublishFacetToAccount(user.ID, FacetMutation{
			FacetID:  composeStatusFacetID(user.ID),
			Fragment: fragment,
			Priority: FacetPriorityHigh,
		})
	}
	if a.State == WorkStateOk {
		// The compose surface closes on success, so the outcome is also the
		// poster's toast — rendered here, like every other toast.
		publishToast(user.PIALID, "work_created_"+a.WorkID, html.EscapeString(a.message()))
	}
	w.Header().Set("X-Work-Create-State", string(a.State))
	if a.State != WorkStateOk {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(a.State.Status())
		_, _ = w.Write([]byte(a.message()))
		return
	}
	body, err := json.Marshal(map[string]string{"work_id": a.WorkID, "cid": a.CID})
	if err != nil {
		log.Printf("[work] encoding receipt of %s: %v", a.WorkID, err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

// commentGatingOption is one choice of the compose_gating Facet.
type commentGatingOption struct {
	Value string
	Label string
}

// commentGatingLabels names each value of dbpkg.CommentGatings for a reader.
var commentGatingLabels = map[string]string{
	"everyone":  "Everyone",
	"followers": "Your followers",
	"circle":    "Your circle (you follow each other)",
	"verified":  "Verified accounts",
	"none":      "Only you",
}

// commentGatingOptions is the compose_gating Facet's list, in vocabulary order.
func commentGatingOptions() []commentGatingOption {
	out := make([]commentGatingOption, 0, len(dbpkg.CommentGatings))
	for _, g := range dbpkg.CommentGatings {
		out = append(out, commentGatingOption{Value: g, Label: commentGatingLabels[g]})
	}
	return out
}
