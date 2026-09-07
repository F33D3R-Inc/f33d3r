package api

import (
	"errors"
	"net/http"

	"f33d3r.com/ios/devserver/internal/store"
)

// workPurchaseEvent — {event_type:"work_purchase", work_id}. Buys a priced
// work outright: the buyer's ledger is debited by the work's price_uaet and
// the author's credited, in one transaction, and the entitlement is recorded
// so every later read of the work carries work_purchased_by_viewer. 204.
//
// The client never states a price. It names a work; the price is the row's,
// read inside the same transaction that moves the money, so a price change
// between the card being drawn and the tap cannot charge the old figure.
//
// Idempotent: a buyer who already owns the work is answered 204 and charged
// nothing, so a retry after a lost response is safe. 402 when the settled
// balance cannot cover it, 400 for a work with no price or the buyer's own,
// 404 for a work that does not exist.
func (s *Server) workPurchaseEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	workID := body.get("work_id")
	if workID == "" {
		plainError(w, http.StatusBadRequest, "work_id required")
		return
	}
	ctx := r.Context()
	authorID, _, err := s.store.WorkOwner(ctx, workID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			plainError(w, http.StatusNotFound, "work not found")
			return
		}
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	price, err := s.store.Purchase(ctx, u.ID, u.PIALID, workID)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrAlreadyPurchased):
		w.WriteHeader(http.StatusNoContent)
		return
	case errors.Is(err, store.ErrNotForSale):
		plainError(w, http.StatusBadRequest, "work is not for sale")
		return
	case errors.Is(err, store.ErrInsufficientBalance):
		plainError(w, http.StatusPaymentRequired, "insufficient balance")
		return
	case errors.Is(err, store.ErrNotFound):
		plainError(w, http.StatusNotFound, "work not found")
		return
	case errors.Is(err, store.ErrOwnWork):
		plainError(w, http.StatusBadRequest, "cannot buy your own work")
		return
	default:
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	author, err := s.store.GetUserByID(ctx, authorID)
	if err == nil {
		s.store.Notify(ctx, author.ID, "purchase", u.ID, workID, "work", map[string]any{"amount_uaet": price, "preview": s.previewOf(r, workID)})
		s.signalNotify(r, author.ID)
		s.signalBalance(r, author)
	}
	s.signalBalance(r, u)
	w.WriteHeader(http.StatusNoContent)
}
