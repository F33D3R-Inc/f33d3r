package handler

// cashtag.go — D-070 read lane: /facets/cashtag/*
//
// Serves two Facet fragments for the cashtag (stock ticker) feature:
//   GET /facets/cashtag/search?q=TS  — autocomplete rows (Tiingo search API)
//   GET /facets/cashtag/card?ticker=TSLA — price card with sparkline (Tiingo IEX)
//
// Prices are cached in-memory for 15 minutes to avoid hammering the API.
// If TIINGO_API_KEY is not set both endpoints return a graceful empty state.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// ── In-memory price cache ─────────────────────────────────────────────────────

// cashtagSnapshot is one ticker's state as both surfaces need it: the quote,
// the raw close series, the polyline the Facet draws from that series, and the
// company's own name. Cached whole, so a browser and a phone looking at the
// same ticker are looking at the same numbers, and Tiingo is asked once for
// both rather than once per surface.
//
// The close series is kept alongside the polyline because the polyline cannot
// be turned back into it: its points are normalised into a 120×38 viewBox and
// inverted for SVG's downward y. The JSON lane needs the prices themselves.
type cashtagSnapshot struct {
	ticker    string // as asked for, upper case
	quote     *tiingoQuote
	closes    []float64 // oldest first, as Tiingo returned them
	sparkline string    // the same series as SVG polyline points, or ""
	company   string    // the company's name, or the ticker when unknown
	exchange  string
	asOf      time.Time
	exp       time.Time
}

var (
	cashtagCache    sync.Map // ticker (upper) → *cashtagSnapshot
	cashtagCacheTTL = 15 * time.Minute
)

// errUnknownTicker is a ticker the provider has no quote for — a different
// thing from a provider that is down, and the JSON lane answers them
// differently (404 against 503).
var errUnknownTicker = errors.New("cashtag: no quote for ticker")

// tiingoStatusError keeps the upstream status, so a caller can tell a ticker
// that does not exist from a provider that is refusing everything.
type tiingoStatusError struct {
	status int
	body   string
}

func (e *tiingoStatusError) Error() string {
	return fmt.Sprintf("tiingo %d: %s", e.status, e.body)
}

// ── Tiingo API types ──────────────────────────────────────────────────────────

type tiingoSearchResult struct {
	Ticker      string `json:"ticker"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Exchange    string `json:"exchangeCode"`
}

type tiingoQuote struct {
	Ticker    string  `json:"ticker"`
	Last      float64 `json:"last"`
	Open      float64 `json:"open"`
	High      float64 `json:"high"`
	Low       float64 `json:"low"`
	PrevClose float64 `json:"prevClose"`
	Timestamp string  `json:"timestamp"`
	Mid       float64 `json:"mid"`
	// TngoLast is Tiingo's computed last price — it stays populated when the US
	// market is CLOSED (when `last` and `mid` come back null/0). Without it the card
	// rendered $0.00 / -100.00% out of hours.
	TngoLast float64 `json:"tngoLast"`
}

// bestPrice returns the most reliable current price: live last → mid → Tiingo's
// computed last (market-closed) → previous close. Zero only if Tiingo gave nothing.
func (q tiingoQuote) bestPrice() float64 {
	for _, v := range []float64{q.Last, q.Mid, q.TngoLast, q.PrevClose} {
		if v != 0 {
			return v
		}
	}
	return 0
}

// changePct returns the % change of the best price vs the previous close, guarding
// the "-100%" artifact that appeared when the live price was 0 out of market hours.
func (q tiingoQuote) changePct() float64 {
	p := q.bestPrice()
	if q.PrevClose == 0 || p == 0 {
		return 0
	}
	return ((p - q.PrevClose) / q.PrevClose) * 100
}

type tiingoPricePoint struct {
	Date  string  `json:"date"`
	Close float64 `json:"close"`
	Open  float64 `json:"open"`
	High  float64 `json:"high"`
	Low   float64 `json:"low"`
}

// ── Tiingo HTTP helpers ───────────────────────────────────────────────────────

func (h *Handler) tiingoGet(path string, out interface{}) error {
	key := h.cfg.TiingoAPIKey
	if key == "" {
		return fmt.Errorf("TIINGO_API_KEY not configured")
	}
	req, err := http.NewRequest(http.MethodGet, "https://api.tiingo.com"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &tiingoStatusError{status: resp.StatusCode, body: string(body)}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ── Sparkline ─────────────────────────────────────────────────────────────────

// closeSeries is the close of every bar that has one, oldest first. It is the
// price series itself: buildSparkline projects it into a viewBox, and the JSON
// lane sends it as numbers so a phone can project it into its own.
func closeSeries(points []tiingoPricePoint) []float64 {
	closes := make([]float64, 0, len(points))
	for _, p := range points {
		if p.Close > 0 {
			closes = append(closes, p.Close)
		}
	}
	return closes
}

// buildSparkline returns SVG polyline points for a 120×40 viewBox. Points are
// normalised so the full price range fills the height.
func buildSparkline(closes []float64) string {
	if len(closes) < 2 {
		return ""
	}
	minV, maxV := closes[0], closes[0]
	for _, v := range closes {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	rng := maxV - minV
	if rng == 0 {
		rng = 1
	}

	w := 120.0
	h := 38.0
	n := len(closes)
	var sb strings.Builder
	for i, v := range closes {
		x := (float64(i) / float64(n-1)) * w
		y := h - ((v-minV)/rng)*h
		y = math.Max(1, math.Min(h, y))
		if i == 0 {
			fmt.Fprintf(&sb, "%.1f,%.1f", x, y)
		} else {
			fmt.Fprintf(&sb, " %.1f,%.1f", x, y)
		}
	}
	return sb.String()
}

// ── fetchSnapshot: price + 5-day intraday + the company's name ───────────────

// fetchSnapshot returns the cached snapshot for ticker, fetching it when the
// cache has none or the one it has has expired. Every surface reads through
// here — the Facet, the SSE push and the JSON twin — so there is one path to
// Tiingo and one set of numbers behind all three.
func (h *Handler) fetchSnapshot(ticker string) (*cashtagSnapshot, error) {
	ticker = strings.ToUpper(strings.TrimSpace(ticker))

	if v, ok := cashtagCache.Load(ticker); ok {
		s := v.(*cashtagSnapshot)
		if time.Now().Before(s.exp) {
			return s, nil
		}
		cashtagCache.Delete(ticker)
	}

	// Fetch real-time quote
	var quotes []tiingoQuote
	if err := h.tiingoGet("/iex/"+url.PathEscape(ticker), &quotes); err != nil {
		return nil, err
	}
	if len(quotes) == 0 {
		// Tiingo answers 200 with an empty array for a symbol it does not
		// list, so this — not the HTTP status — is what "no such ticker"
		// usually looks like.
		return nil, errUnknownTicker
	}
	quote := quotes[0]

	// Fetch 5-day intraday for sparkline (30-min bars)
	start := time.Now().AddDate(0, 0, -5).Format("2006-01-02")
	var prices []tiingoPricePoint
	_ = h.tiingoGet(fmt.Sprintf("/iex/%s/prices?startDate=%s&resampleFreq=30min",
		url.PathEscape(ticker), start), &prices)
	closes := closeSeries(prices)

	snap := &cashtagSnapshot{
		ticker:    ticker,
		quote:     &quote,
		closes:    closes,
		sparkline: buildSparkline(closes),
		company:   ticker,
		asOf:      quoteTime(quote.Timestamp),
		exp:       time.Now().Add(cashtagCacheTTL),
	}

	// The company's own name and its exchange, cached with the price rather
	// than looked up on every render: it was an uncached call to the same
	// search endpoint per card, and a company does not rename itself inside
	// fifteen minutes. A search that fails leaves the ticker standing as its
	// own name, which is what the card showed before.
	if searchRes, err := h.searchTickers(ticker, 1); err == nil && len(searchRes) > 0 {
		if searchRes[0].Name != "" {
			snap.company = searchRes[0].Name
		}
		snap.exchange = searchRes[0].Exchange
	}

	cashtagCache.Store(ticker, snap)
	return snap, nil
}

// quoteTime is the instant Tiingo priced the quote at, or now when it said
// nothing readable. A card that cannot say when it was priced is worse than
// one that says "just now" and is a few minutes out.
func quoteTime(raw string) time.Time {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw)); err == nil {
		return t.UTC()
	}
	return time.Now().UTC()
}

// cashtagPriceLabel and cashtagChangeLabel are the only two places a price is
// written into words. Both surfaces print what they return, so the phone and
// the browser cannot disagree about how a number reads.
func cashtagPriceLabel(price float64) string {
	return fmt.Sprintf("%.2f", price)
}

func cashtagChangeLabel(pct float64) string {
	if pct < 0 {
		return fmt.Sprintf("%.2f%%", pct)
	}
	return fmt.Sprintf("+%.2f%%", pct)
}

// cardData is the snapshot as the cashtag_card Facet takes it.
func (s *cashtagSnapshot) cardData() CashtagCardData {
	price := s.quote.bestPrice()
	change := s.quote.changePct()
	return CashtagCardData{
		Ticker:      "$" + s.ticker,
		CompanyName: s.company,
		Exchange:    s.exchange,
		Price:       price,
		PriceStr:    cashtagPriceLabel(price),
		ChangePct:   change,
		ChangeStr:   cashtagChangeLabel(change),
		IsPositive:  change >= 0,
		Sparkline:   s.sparkline,
	}
}

// ── Handler: GET /facets/cashtag/search?q=TSLA ───────────────────────────────

// cashtagSearchLimit is how many rows a ticker dropdown gets. Eight is what
// the web's autocomplete has always asked for and what fits above a composer
// without covering what is being written.
const cashtagSearchLimit = 8

// searchTickers is the one path to Tiingo's symbol search. Both the Facet's
// dropdown and the JSON twin the composer reads go through here, so neither
// can drift into asking for something the other does not.
func (h *Handler) searchTickers(q string, limit int) ([]tiingoSearchResult, error) {
	var results []tiingoSearchResult
	err := h.tiingoGet(fmt.Sprintf("/tiingo/utilities/search?query=%s&limit=%d",
		url.QueryEscape(q), limit), &results)
	return results, err
}

func (h *Handler) facetCashtagSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || h.cfg.TiingoAPIKey == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return
	}

	results, err := h.searchTickers(q, cashtagSearchLimit)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=60")
	for _, res := range results {
		h.renderPartial(w, "cashtag_autocomplete_row", res)
	}
}

// ── Handler: GET /facets/cashtag/card?ticker=TSLA ────────────────────────────

// CashtagCardData is passed to the cashtag_card Facet template.
type CashtagCardData struct {
	Ticker      string
	CompanyName string
	Exchange    string
	Price       float64
	PriceStr    string
	ChangePct   float64
	ChangeStr   string
	IsPositive  bool
	Sparkline   string // SVG polyline points or ""
}

func (h *Handler) facetCashtagCard(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("ticker")))
	if ticker == "" || h.cfg.TiingoAPIKey == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	snap, err := h.fetchSnapshot(ticker)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=300")
	h.renderPartial(w, "cashtag_card", snap.cardData())
}

// renderCashtagCardHTML fetches the current price and renders the cashtag_card
// Facet to an HTML string. Used by the watch endpoint to push a live price_update
// SSE event the moment a user's viewport shows a cashtag card.
func (h *Handler) renderCashtagCardHTML(ticker string) (string, error) {
	if h.cfg.TiingoAPIKey == "" {
		return "", fmt.Errorf("tiingo not configured")
	}
	snap, err := h.fetchSnapshot(ticker)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, "cashtag_card", snap.cardData()); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ── Handler: GET /stocks/{ticker} ────────────────────────────────────────────

var safeTickerRe = regexp.MustCompile(`^[A-Z]{1,5}$`)

func (h *Handler) stocksPage(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(strings.TrimSpace(r.PathValue("ticker")))
	if !safeTickerRe.MatchString(ticker) {
		http.NotFound(w, r)
		return
	}
	user := h.userFromRequest(w, r)
	var works interface{}
	if h.db != nil {
		// SearchWorks enriches with reactions when viewerID is non-empty.
		wks, sErr := dbpkg.SearchWorks(h.db, ticker, 40, user.ID)
		if sErr != nil {
			log.Printf("[cashtag] search works %q: %v", ticker, sErr)
		}
		works = filterAdultForViewer(user, wks)
	}
	h.render(w, r, "stocks.html", h.withRail(map[string]interface{}{
		"User":              user,
		"Ticker":            ticker,
		"Works":             works,
		"Title":             "$" + ticker + " · F33D3R",
		"SessionID":         uuid.New().String(),
		"ShowScores":        h.cfg.ShowScores,
		"CurrentUserHandle": user.Handle,
		"Themes":            ThemesWithActive(user.ThemeID),
	}, user, "search"))
}

// NOTE: storeCashtagForPost (INSERT INTO post_cashtags) was removed with the
// posts lane. It froze a price alongside a post row at creation time; a work
// carries no such row. The ticker is extracted from the body at render time
// (db.extractCashtagTicker) and the cashtag_card facet lazy-loads the live quote,
// so the card shows the price now rather than the price when someone posted.
