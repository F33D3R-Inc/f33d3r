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

type cashtagCacheEntry struct {
	data      *tiingoQuote
	sparkline string // pre-rendered SVG polyline points
	exp       time.Time
}

var (
	cashtagCache   sync.Map // ticker (upper) → cashtagCacheEntry
	cashtagCacheTTL = 15 * time.Minute
)

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
		return fmt.Errorf("tiingo %d: %s", resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ── Sparkline SVG generator ───────────────────────────────────────────────────
// Returns SVG polyline points string for a 120×40 viewBox.
// Points are normalised so the full price range fills the height.

func buildSparkline(points []tiingoPricePoint, positive bool) string {
	if len(points) < 2 {
		return ""
	}
	var closes []float64
	for _, p := range points {
		if p.Close > 0 {
			closes = append(closes, p.Close)
		}
	}
	if len(closes) < 2 {
		return ""
	}
	minV, maxV := closes[0], closes[0]
	for _, v := range closes {
		if v < minV { minV = v }
		if v > maxV { maxV = v }
	}
	rng := maxV - minV
	if rng == 0 { rng = 1 }

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

// ── fetchQuoteWithSparkline fetches price + 5-day intraday for sparkline ─────

func (h *Handler) fetchQuoteWithSparkline(ticker string) (*tiingoQuote, string, error) {
	ticker = strings.ToUpper(strings.TrimSpace(ticker))

	// Check cache
	if v, ok := cashtagCache.Load(ticker); ok {
		e := v.(cashtagCacheEntry)
		if time.Now().Before(e.exp) {
			return e.data, e.sparkline, nil
		}
		cashtagCache.Delete(ticker)
	}

	// Fetch real-time quote
	var quotes []tiingoQuote
	if err := h.tiingoGet("/iex/"+url.PathEscape(ticker), &quotes); err != nil {
		return nil, "", err
	}
	if len(quotes) == 0 {
		return nil, "", fmt.Errorf("no quote data for %s", ticker)
	}
	quote := quotes[0]

	// Fetch 5-day intraday for sparkline (30-min bars)
	start := time.Now().AddDate(0, 0, -5).Format("2006-01-02")
	var prices []tiingoPricePoint
	_ = h.tiingoGet(fmt.Sprintf("/iex/%s/prices?startDate=%s&resampleFreq=30min",
		url.PathEscape(ticker), start), &prices)

	positive := quote.bestPrice() >= quote.PrevClose
	sparkline := buildSparkline(prices, positive)

	entry := cashtagCacheEntry{data: &quote, sparkline: sparkline, exp: time.Now().Add(cashtagCacheTTL)}
	cashtagCache.Store(ticker, entry)

	return &quote, sparkline, nil
}

// ── Handler: GET /facets/cashtag/search?q=TSLA ───────────────────────────────

func (h *Handler) facetCashtagSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || h.cfg.TiingoAPIKey == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return
	}

	var results []tiingoSearchResult
	if err := h.tiingoGet("/tiingo/utilities/search?query="+url.QueryEscape(q)+"&limit=8", &results); err != nil {
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

	quote, sparkline, err := h.fetchQuoteWithSparkline(ticker)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	last := quote.bestPrice()
	changePct := quote.changePct()

	sign := "+"
	if changePct < 0 { sign = "" }

	// Fetch company name from search if not available
	companyName := ticker
	var searchRes []tiingoSearchResult
	if err2 := h.tiingoGet("/tiingo/utilities/search?query="+url.QueryEscape(ticker)+"&limit=1", &searchRes); err2 == nil && len(searchRes) > 0 {
		companyName = searchRes[0].Name
	}

	data := CashtagCardData{
		Ticker:      "$" + ticker,
		CompanyName: companyName,
		Price:       last,
		PriceStr:    fmt.Sprintf("%.2f", last),
		ChangePct:   changePct,
		ChangeStr:   fmt.Sprintf("%s%.2f%%", sign, changePct),
		IsPositive:  changePct >= 0,
		Sparkline:   sparkline,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=300")
	h.renderPartial(w, "cashtag_card", data)
}

// renderCashtagCardHTML fetches the current price and renders the cashtag_card
// Facet to an HTML string. Used by the watch endpoint to push a live price_update
// SSE event the moment a user's viewport shows a cashtag card.
func (h *Handler) renderCashtagCardHTML(ticker string) (string, error) {
	if h.cfg.TiingoAPIKey == "" {
		return "", fmt.Errorf("tiingo not configured")
	}
	quote, sparkline, err := h.fetchQuoteWithSparkline(ticker)
	if err != nil {
		return "", err
	}
	last := quote.bestPrice()
	changePct := quote.changePct()
	sign := "+"
	if changePct < 0 { sign = "" }
	companyName := ticker
	var searchRes []tiingoSearchResult
	if err2 := h.tiingoGet("/tiingo/utilities/search?query="+url.QueryEscape(ticker)+"&limit=1", &searchRes); err2 == nil && len(searchRes) > 0 {
		companyName = searchRes[0].Name
	}
	data := CashtagCardData{
		Ticker:      "$" + ticker,
		CompanyName: companyName,
		Price:       last,
		PriceStr:    fmt.Sprintf("%.2f", last),
		ChangePct:   changePct,
		ChangeStr:   fmt.Sprintf("%s%.2f%%", sign, changePct),
		IsPositive:  changePct >= 0,
		Sparkline:   sparkline,
	}
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, "cashtag_card", data); err != nil {
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
	rail := h.railData(user, "search")
	h.render(w, "stocks.html", map[string]interface{}{
		"User":              user,
		"Ticker":            ticker,
		"Works":             works,
		"Title":             "$" + ticker + " · F33D3R",
		"SessionID":         uuid.New().String(),
		"ShowScores":        h.cfg.ShowScores,
		"CurrentUserHandle": user.Handle,
		"Themes":            ThemesWithActive(user.ThemeID),
		"TrendingTags":      rail["TrendingTags"],
		"SuggestedUsers":    rail["SuggestedUsers"],
		"RailContext":       rail["RailContext"],
	})
}

// ── DB helpers (called from post creation) ────────────────────────────────────

// StoreCashtagForPost fetches the current price and writes to post_cashtags.
// Called asynchronously from createPost after insert.
func (h *Handler) storeCashtagForPost(postID, ticker string) {
	if h.db == nil || ticker == "" || h.cfg.TiingoAPIKey == "" {
		return
	}
	ticker = strings.ToUpper(strings.TrimSpace(ticker))

	quote, _, err := h.fetchQuoteWithSparkline(ticker)
	if err != nil {
		return
	}

	last := quote.bestPrice()
	changePct := quote.changePct()

	companyName := ticker
	var searchRes []tiingoSearchResult
	if err2 := h.tiingoGet("/tiingo/utilities/search?query="+url.QueryEscape(ticker)+"&limit=1", &searchRes); err2 == nil && len(searchRes) > 0 {
		companyName = searchRes[0].Name
	}

	h.db.Exec(`
		INSERT INTO post_cashtags (post_id, ticker, company_name, price_at_post, change_pct)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING`,
		postID, ticker, companyName, last, changePct,
	)
}
