package handler

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/model"
)

// ── /api/v1/cashtag/{ticker} — one quote, as JSON ────────────────────────────
//
// The web draws a $TICKER as a Facet: /facets/cashtag/card renders the price,
// the change and an SVG polyline, and the browser swaps the fragment under the
// work. A phone has no fragment to swap into, so it reads the same snapshot as
// data and draws its own card.
//
// Two things here are deliberately not what the Facet carries.
//
// The sparkline crosses as numbers. The Facet's polyline points are normalised
// into a 120×38 viewBox this server chose and inverted for SVG's downward y;
// handing them to a phone would be handing it a coordinate system it did not
// pick, and it would have to undo the normalisation before it could draw at
// its own size. The prices themselves survive any frame.
//
// Every label stays the server's, exactly as the scoreboard's do. price_label
// and change_label are written here, so the app never forms a second opinion
// about how a price reads.
//
// On the price_update SSE event: it has no JSON twin, and that is a decision
// rather than an omission. Exactly one thing publishes it — the browser's
// IntersectionObserver POSTing to /api/events/watch when a card scrolls into
// view, which echoes the snapshot straight back — and nothing else ever calls
// PublishToTickerWatchers, so no price change is pushed on its own. A twin
// would buy the app a duplicate of the fetch it makes on appear, at the price
// of first calling an HTML-shaped watch endpoint to ask for it. When something
// starts pushing price changes unprompted, the twin is worth adding and the
// event goes into apiUserStreamKinds with it; today it would be ceremony.
//
// Shape is F33D3RKit/Sources/F33D3RKit/Models/CashtagQuote.swift.

type CashtagQuoteDTO struct {
	// Ticker is bare; Display is how it is written in a body and on a card.
	Ticker  string `json:"ticker"`
	Display string `json:"display"`
	// CompanyName falls back to the ticker when the provider will not name it,
	// so a card always has something to print under the symbol.
	CompanyName string `json:"company_name"`
	Exchange    string `json:"exchange"`

	Price      float64 `json:"price"`
	PriceLabel string  `json:"price_label"`
	// ChangePct is against the previous close, and is 0 rather than -100 when
	// the market is shut and there is no live price to compare.
	ChangePct   float64 `json:"change_pct"`
	ChangeLabel string  `json:"change_label"`
	IsPositive  bool    `json:"is_positive"`

	// Sparkline is the close of every bar over the last five days, oldest
	// first, in the currency of the price — not the Facet's polyline points.
	Sparkline []float64 `json:"sparkline"`
	// AsOf is when the provider priced this, so a card can say how old it is
	// rather than implying it is live.
	AsOf time.Time `json:"as_of"`
}

func cashtagQuoteDTO(s *cashtagSnapshot) CashtagQuoteDTO {
	price := s.quote.bestPrice()
	change := s.quote.changePct()
	sparkline := s.closes
	if sparkline == nil {
		// An empty run, never a null: a client that draws a line from what it
		// is given should get a series with nothing in it.
		sparkline = []float64{}
	}
	return CashtagQuoteDTO{
		Ticker:      s.ticker,
		Display:     "$" + s.ticker,
		CompanyName: s.company,
		Exchange:    s.exchange,
		Price:       price,
		PriceLabel:  cashtagPriceLabel(price),
		ChangePct:   change,
		ChangeLabel: cashtagChangeLabel(change),
		IsPositive:  change >= 0,
		Sparkline:   sparkline,
		AsOf:        s.asOf,
	}
}

// ── /api/v1/cashtag/search?q= — the composer's ticker dropdown ──────────────
//
// The Facet at /facets/cashtag/search answers the web's dropdown as rendered
// rows; this answers the same lookup as data. Both go through searchTickers,
// so there is one path to the provider's symbol search.
//
// A query shorter than cashtagSearchMinQuery is an empty list and a 200, not
// an error: a composer calls this on nearly every keystroke, and one letter
// matches half the exchange — a dropdown of arbitrary companies is noise, and
// answering it with a refusal would make the composer draw an error for
// somebody who has simply not finished typing.

type CashtagSuggestionDTO struct {
	Ticker      string `json:"ticker"`
	Display     string `json:"display"`
	CompanyName string `json:"company_name"`
	Exchange    string `json:"exchange"`
}

type CashtagSearchDTO struct {
	Results []CashtagSuggestionDTO `json:"results"`
}

// cashtagSearchMinQuery is the shortest query worth asking the provider about.
const cashtagSearchMinQuery = 2

// apiV1CashtagSearch — GET /api/v1/cashtag/search?q=appl
func (h *Handler) apiV1CashtagSearch(w http.ResponseWriter, r *http.Request, _ *model.User) {
	if h.cfg == nil || h.cfg.TiingoAPIKey == "" {
		apiError(w, http.StatusServiceUnavailable, "cashtag_unavailable",
			"Quotes are not available on this deployment.")
		return
	}
	out := CashtagSearchDTO{Results: []CashtagSuggestionDTO{}}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	// A leading $ is what the composer has on screen; it is not part of the
	// query, and the provider finds nothing with it attached.
	q = strings.TrimPrefix(q, "$")
	if len([]rune(q)) < cashtagSearchMinQuery {
		apiJSON(w, http.StatusOK, out)
		return
	}

	results, err := h.searchTickers(q, cashtagSearchLimit)
	if err != nil {
		log.Printf("[api/v1] cashtag search %q: %v", q, err)
		apiError(w, http.StatusServiceUnavailable, "cashtag_unavailable",
			"Quotes could not be read right now.")
		return
	}
	for _, res := range results {
		ticker := strings.ToUpper(strings.TrimSpace(res.Ticker))
		// Only symbols a body could carry. The provider lists instruments
		// whose tickers cashtagRe would never linkify, and offering one would
		// put a $TICKER in a work that renders as plain text.
		if !safeTickerRe.MatchString(ticker) {
			continue
		}
		name := res.Name
		if name == "" {
			name = ticker
		}
		out.Results = append(out.Results, CashtagSuggestionDTO{
			Ticker:      ticker,
			Display:     "$" + ticker,
			CompanyName: name,
			Exchange:    res.Exchange,
		})
	}
	apiJSON(w, http.StatusOK, out)
}

// apiV1Cashtag — GET /api/v1/cashtag/{ticker}
//
// `503 cashtag_unavailable` where no quotes provider is configured, which the
// app must be able to tell from `404 cashtag_not_found` for a symbol nobody
// lists: the first means show nothing at all, the second means this particular
// $WORD was never a company. The Facet answers 204 to both, which is enough
// for a browser that would swap an empty fragment either way and not enough
// for a client deciding whether the feature exists.
func (h *Handler) apiV1Cashtag(w http.ResponseWriter, r *http.Request, _ *model.User) {
	ticker := strings.ToUpper(strings.TrimSpace(r.PathValue("ticker")))
	if !safeTickerRe.MatchString(ticker) {
		// The same shape cashtagRe accepts in a body. Anything else is not a
		// ticker that could exist, so it is answered without asking anybody.
		apiError(w, http.StatusNotFound, "cashtag_not_found", "No quote for that symbol.")
		return
	}
	if h.cfg == nil || h.cfg.TiingoAPIKey == "" {
		apiError(w, http.StatusServiceUnavailable, "cashtag_unavailable",
			"Quotes are not available on this deployment.")
		return
	}

	snap, err := h.fetchSnapshot(ticker)
	if err != nil {
		if isUnknownTicker(err) {
			apiError(w, http.StatusNotFound, "cashtag_not_found", "No quote for that symbol.")
			return
		}
		log.Printf("[api/v1] cashtag %s: %v", ticker, err)
		apiError(w, http.StatusServiceUnavailable, "cashtag_unavailable",
			"Quotes could not be read right now.")
		return
	}
	apiJSON(w, http.StatusOK, cashtagQuoteDTO(snap))
}

// isUnknownTicker separates "there is no such company" from "the provider is
// having a bad day". Tiingo says the first as an empty array and, for some
// symbols, as a 404; everything else is the second.
func isUnknownTicker(err error) bool {
	if errors.Is(err, errUnknownTicker) {
		return true
	}
	var status *tiingoStatusError
	return errors.As(err, &status) && status.status == http.StatusNotFound
}
