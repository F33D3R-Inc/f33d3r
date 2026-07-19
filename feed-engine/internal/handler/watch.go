package handler

import (
	"net/http"
	"strings"
)

// watchHandler handles POST /api/events/watch.
// Called by the client's IntersectionObserver when a post card or cashtag card
// enters or exits the viewport. Updates the SSE session registry so the server
// knows which posts/tickers to push live events for.
//
// Form fields:
//
//	type:   "post" | "ticker"
//	id:     post UUID or ticker symbol (e.g. "AAPL")
//	action: "watch" | "unwatch"
func (h *Handler) watchHandler(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	watchType := strings.TrimSpace(r.FormValue("type"))
	id := strings.TrimSpace(r.FormValue("id"))
	action := strings.TrimSpace(r.FormValue("action"))

	if watchType == "" || id == "" || (action != "watch" && action != "unwatch") {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch watchType {
	case "post":
		if action == "watch" {
			AddPostWatcher(user.PIALID, id)
		} else {
			RemovePostWatcher(user.PIALID, id)
		}

	case "ticker":
		ticker := strings.ToUpper(id)
		if action == "watch" {
			AddTickerWatcher(user.PIALID, ticker)
			// Immediately push the current price to this user — no polling,
			// the event fires the moment they start watching.
			go func() {
				html, err := h.renderCashtagCardHTML(ticker)
				if err == nil && html != "" {
					PublishToUser(user.PIALID, SSEEvent{Type: "price_update", Data: html})
				}
			}()
		} else {
			RemoveTickerWatcher(user.PIALID, ticker)
		}
	}

	w.WriteHeader(http.StatusOK)
}
