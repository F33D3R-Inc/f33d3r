package api

import (
	"net/http"

	"f33d3r.com/ios/devserver/internal/store"
)

// notifications — GET /api/v1/notifications?limit=&cursor=
//
// Grouped server-side (like/repost/follow within an hour on one target), with
// the unread total alongside so the badge and the list come from one answer.
func (s *Server) notifications(w http.ResponseWriter, r *http.Request, u *store.User) {
	groups, next, err := s.store.ListNotifications(r.Context(), u.ID, limitParam(r, defaultPageSize, maxPageSize), r.URL.Query().Get("cursor"))
	if err != nil {
		serverError(w, err)
		return
	}
	unread, err := s.store.UnreadCount(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]NotificationDTO, 0, len(groups))
	for _, g := range groups {
		out = append(out, notificationDTO(g))
	}
	writeJSON(w, http.StatusOK, NotificationPageDTO{Notifications: out, NextCursor: next, UnreadCount: unread})
}

// wallet — GET /api/v1/wallet: settled and pending balances, never summed, and
// the recent ledger.
func (s *Server) wallet(w http.ResponseWriter, r *http.Request, u *store.User) {
	bal, err := s.store.Balance(r.Context(), u.PIALID)
	if err != nil {
		serverError(w, err)
		return
	}
	entries, err := s.store.Entries(r.Context(), u.PIALID, 50)
	if err != nil {
		serverError(w, err)
		return
	}
	out := WalletDTO{BalanceUAET: bal.SettledUAET, PendingUAET: bal.PendingUAET, Entries: make([]WalletEntryDTO, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, WalletEntryDTO{
			ID:                 e.ID,
			Kind:               e.Kind,
			AmountUAET:         e.AmountUAET,
			CounterpartyHandle: strPtr(e.CounterpartyHandle),
			CreatedAt:          e.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
