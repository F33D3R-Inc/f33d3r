package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestWorkPurchase: the Music surface lists the seeded priced tracks with
// their price and no entitlement; buying one moves exactly the price from the
// buyer to the author, marks the work owned for the buyer only, is idempotent
// on a retry, and refuses an author's own work and an unpriced work.
func TestWorkPurchase(t *testing.T) {
	h := newHarness(t)
	devToken, _ := h.login("dev")
	miiToken, _ := h.login("miiyazuko")

	status, body := h.do("GET", "/api/v1/feed?surface=music", devToken, nil, "")
	if status != 200 {
		t.Fatalf("music feed: %d %s", status, body)
	}
	var page WorkPageDTO
	json.Unmarshal(body, &page)
	var priced, free *WorkDTO
	for i := range page.Works {
		w := &page.Works[i]
		if w.Voice == nil {
			t.Errorf("music surface carries a work with no audio: %s", w.ID)
		}
		if w.PriceUAET != nil && *w.PriceUAET == 2_000_000 && w.Author.Handle == "miiyazuko" {
			priced = w
		}
		if w.PriceUAET == nil && w.Author.Handle == "miiyazuko" {
			free = w
		}
	}
	if priced == nil || free == nil {
		t.Fatalf("expected a 2 AET track and a free track from @miiyazuko: %s", body)
	}
	if priced.WorkPurchasedByViewer {
		t.Errorf("dev owns the track before buying it")
	}

	balance := func(token string) int64 {
		_, b := h.do("GET", "/api/v1/wallet", token, nil, "")
		var w WalletDTO
		json.Unmarshal(b, &w)
		return w.BalanceUAET
	}
	devBefore, miiBefore := balance(devToken), balance(miiToken)

	status, body = h.do("POST", "/events", devToken, map[string]string{"event_type": "work_purchase", "work_id": priced.ID}, "")
	if status != 204 {
		t.Fatalf("work_purchase: %d %s", status, body)
	}
	if got := balance(devToken); got != devBefore-2_000_000 {
		t.Errorf("buyer balance %d, want %d", got, devBefore-2_000_000)
	}
	if got := balance(miiToken); got != miiBefore+2_000_000 {
		t.Errorf("author balance %d, want %d", got, miiBefore+2_000_000)
	}

	// The work now reads as owned — for the buyer, and only for the buyer.
	_, body = h.do("GET", "/api/v1/works/"+priced.ID, devToken, nil, "")
	var thread WorkThreadDTO
	json.Unmarshal(body, &thread)
	if !thread.Work.WorkPurchasedByViewer {
		t.Errorf("buyer does not see the work as owned: %s", body)
	}
	if thread.Work.PriceUAET == nil || *thread.Work.PriceUAET != 2_000_000 {
		t.Errorf("price should still be reported on an owned work: %+v", thread.Work.PriceUAET)
	}
	guestToken, _ := h.login("guest")
	_, body = h.do("GET", "/api/v1/works/"+priced.ID, guestToken, nil, "")
	json.Unmarshal(body, &thread)
	if thread.Work.WorkPurchasedByViewer {
		t.Errorf("a stranger sees the work as owned")
	}

	// A retry is a no-op: same answer, no second charge.
	status, _ = h.do("POST", "/events", devToken, map[string]string{"event_type": "work_purchase", "work_id": priced.ID}, "")
	if status != 204 {
		t.Errorf("repeat purchase: %d", status)
	}
	if got := balance(devToken); got != devBefore-2_000_000 {
		t.Errorf("buyer charged twice: %d", got)
	}

	// The author's ledger and inbox carry the sale.
	_, body = h.do("GET", "/api/v1/wallet", miiToken, nil, "")
	if !strings.Contains(string(body), `"kind":"sale"`) {
		t.Errorf("author ledger lacks the sale: %s", body)
	}
	_, body = h.do("GET", "/api/v1/wallet", devToken, nil, "")
	if !strings.Contains(string(body), `"kind":"unlock"`) {
		t.Errorf("buyer ledger lacks the unlock: %s", body)
	}
	_, body = h.do("GET", "/api/v1/notifications", miiToken, nil, "")
	if !strings.Contains(string(body), `"kind":"purchase"`) {
		t.Errorf("author inbox lacks the purchase: %s", body)
	}

	// Refusals.
	if status, _ = h.do("POST", "/events", miiToken, map[string]string{"event_type": "work_purchase", "work_id": priced.ID}, ""); status != 400 {
		t.Errorf("buying own work: %d, want 400", status)
	}
	if status, _ = h.do("POST", "/events", devToken, map[string]string{"event_type": "work_purchase", "work_id": free.ID}, ""); status != 400 {
		t.Errorf("buying an unpriced work: %d, want 400", status)
	}
	if status, _ = h.do("POST", "/events", devToken, map[string]string{"event_type": "work_purchase", "work_id": "00000000-0000-0000-0000-000000000000"}, ""); status != 404 {
		t.Errorf("buying a missing work: %d, want 404", status)
	}
	if status, _ = h.do("POST", "/events", devToken, map[string]string{"event_type": "work_purchase"}, ""); status != 400 {
		t.Errorf("no work_id: %d, want 400", status)
	}

	// Not enough money: guest has 49 AET after the seeded tip, so nine 5 AET
	// tips leave 4 AET and the 5 AET track is out of reach.
	for i := 0; i < 9; i++ {
		if status, body = h.do("POST", "/events", guestToken, "event_type=tip&target_handle=admin&amount_aet=500", "application/x-www-form-urlencoded"); status != 204 {
			t.Fatalf("drain tip: %d %s", status, body)
		}
	}
	var fiveAET string
	for _, w := range page.Works {
		if w.PriceUAET != nil && *w.PriceUAET == 5_000_000 {
			fiveAET = w.ID
		}
	}
	if fiveAET == "" {
		t.Fatalf("no 5 AET track seeded")
	}
	if status, body = h.do("POST", "/events", guestToken, map[string]string{"event_type": "work_purchase", "work_id": fiveAET}, ""); status != 402 {
		t.Errorf("insufficient balance: %d %s, want 402", status, body)
	}
}
