package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/f33d3r/feed-engine/internal/config"
)

// The JSON twin of the cashtag card. What is held here is what the app cannot
// work out for itself: which symbols count as a cashtag, what the numbers are
// called when they are printed, and the difference between a deployment with
// no quotes provider and a symbol nobody lists.

// ── The regex is the contract ────────────────────────────────────────────────

// The web's linkifier, the composer's highlighter, the work's stored embed and
// the app's body parser must all agree on what a cashtag is. Three of those
// are in this repository's Go; the fourth is Swift, and it is read as text for
// the same reason api_v1_contract_test.go reads the models as text — a looser
// rule on the phone lights up words the server does not consider tickers and
// then asks for a quote nobody can give.
func TestCashtagPatternMatchesTheApp(t *testing.T) {
	want := strings.TrimPrefix(cashtagRe.String(), "(?m)")

	path := filepath.Join("..", "..", "..", "mobile", "f33d3r_iOS", "F33D3RKit",
		"Sources", "F33D3RKit", "Models", "CashtagQuote.swift")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Swift cashtag model not found at %s: %v", path, err)
	}
	m := regexp.MustCompile(`static let pattern = #"([^"]+)"#`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("no `static let pattern` in %s", path)
	}
	if m[1] != want {
		t.Fatalf("the app matches cashtags with %q; this server matches %q", m[1], want)
	}
}

// The shape the JSON lane accepts in a path is the shape the body linkifier
// produces, and nothing else: a lower-case word or a six-letter one is not a
// ticker that could exist.
func TestCashtagTickerShape(t *testing.T) {
	for _, ok := range []string{"A", "AAPL", "GOOGL"} {
		if !safeTickerRe.MatchString(ok) {
			t.Errorf("%q is a ticker the body linkifier makes and this refused it", ok)
		}
	}
	for _, bad := range []string{"", "aapl", "ABCDEF", "AA-PL", "AA PL", "1234"} {
		if safeTickerRe.MatchString(bad) {
			t.Errorf("%q is not a ticker and this accepted it", bad)
		}
	}
}

// ── The labels ───────────────────────────────────────────────────────────────

// Both surfaces print what these return. A phone that formatted a price itself
// would be a second opinion about how a number reads.
func TestCashtagLabelsAreTheServersWord(t *testing.T) {
	if got := cashtagPriceLabel(227.5); got != "227.50" {
		t.Errorf("price label %q", got)
	}
	cases := map[float64]string{
		-0.8412: "-0.84%",
		0:       "+0.00%",
		1.5:     "+1.50%",
	}
	for pct, want := range cases {
		if got := cashtagChangeLabel(pct); got != want {
			t.Errorf("change label for %v: %q, want %q", pct, got, want)
		}
	}
}

// The Facet's card and the JSON twin read one snapshot, so they cannot print
// different prices for the same ticker.
func TestCashtagFacetAndJSONAgree(t *testing.T) {
	snap := &cashtagSnapshot{
		ticker:  "AAPL",
		quote:   &tiingoQuote{Ticker: "aapl", Last: 227.52, PrevClose: 229.45},
		closes:  []float64{226.1, 227.9, 228.4, 227.5},
		company: "Apple Inc.",
		asOf:    time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
	}
	snap.sparkline = buildSparkline(snap.closes)

	card := snap.cardData()
	dto := cashtagQuoteDTO(snap)

	if card.Ticker != dto.Display {
		t.Errorf("card says %q, JSON says %q", card.Ticker, dto.Display)
	}
	if card.PriceStr != dto.PriceLabel || card.ChangeStr != dto.ChangeLabel {
		t.Errorf("card prints %s %s, JSON prints %s %s",
			card.PriceStr, card.ChangeStr, dto.PriceLabel, dto.ChangeLabel)
	}
	if card.IsPositive != dto.IsPositive {
		t.Error("the two surfaces disagree about which way the price moved")
	}
	if dto.IsPositive {
		t.Error("227.52 against a 229.45 close is a fall")
	}
	// The polyline the browser draws and the numbers the phone draws are the
	// same series; neither is fetched separately.
	if snap.sparkline == "" || len(dto.Sparkline) != len(snap.closes) {
		t.Errorf("polyline %q, series %v", snap.sparkline, dto.Sparkline)
	}
}

// The series crosses as prices. Sending the Facet's points would send a
// coordinate system the phone did not choose.
func TestCashtagSparklineIsPricesNotPoints(t *testing.T) {
	snap := &cashtagSnapshot{ticker: "AAPL", quote: &tiingoQuote{Last: 10, PrevClose: 10}}
	raw, err := json.Marshal(cashtagQuoteDTO(snap))
	if err != nil {
		t.Fatal(err)
	}
	// A ticker with no intraday bars still carries a series, empty rather than
	// null: a client that draws what it is given must be given something.
	if !strings.Contains(string(raw), `"sparkline":[]`) {
		t.Errorf("empty series encoded as %s", raw)
	}

	snap.closes = closeSeries([]tiingoPricePoint{{Close: 226.1}, {Close: 0}, {Close: 227.9}})
	dto := cashtagQuoteDTO(snap)
	if len(dto.Sparkline) != 2 || dto.Sparkline[0] != 226.1 || dto.Sparkline[1] != 227.9 {
		t.Errorf("series %v; a bar with no close is not a price of zero", dto.Sparkline)
	}
}

// ── Not configured is not "no such ticker" ───────────────────────────────────

// The Facet answers 204 to both, which is all a browser needs and not enough
// for a client deciding whether the feature exists at all.
func TestCashtagUnconfiguredIsNot204(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/cashtag/AAPL", nil)
	r.SetPathValue("ticker", "AAPL")

	h.apiV1Cashtag(rec, r, nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "cashtag_unavailable" {
		t.Fatalf("code %q", got)
	}
}

// A word shaped like a ticker that no provider lists is a different answer,
// and it is reached without asking anybody.
func TestCashtagUnknownSymbolIs404(t *testing.T) {
	h := &Handler{cfg: &config.Config{TiingoAPIKey: "test-key"}}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/cashtag/aapl", nil)
	r.SetPathValue("ticker", "TOOLONG")

	h.apiV1Cashtag(rec, r, nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "cashtag_not_found" {
		t.Fatalf("code %q", got)
	}
}

// Which upstream failures mean "no such company" and which mean "the provider
// is having a bad day".
func TestCashtagUnknownTickerDetection(t *testing.T) {
	if !isUnknownTicker(errUnknownTicker) {
		t.Error("an empty quote array is a symbol nobody lists")
	}
	if !isUnknownTicker(&tiingoStatusError{status: http.StatusNotFound}) {
		t.Error("an upstream 404 is a symbol nobody lists")
	}
	for _, err := range []error{
		&tiingoStatusError{status: http.StatusInternalServerError},
		&tiingoStatusError{status: http.StatusUnauthorized},
	} {
		if isUnknownTicker(err) {
			t.Errorf("%v is the provider failing, not a missing company", err)
		}
	}
}

// ── The bytes, against a provider that answers ───────────────────────────────

// tiingoStub answers the three calls a snapshot makes, so the whole path —
// quote, intraday bars, company name — can be read as the app reads it.
type tiingoStub func(path string) (int, string)

func (f tiingoStub) RoundTrip(r *http.Request) (*http.Response, error) {
	status, body := f(r.URL.Path + "?" + r.URL.RawQuery)
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    r,
	}, nil
}

func stubbedHandler(answer tiingoStub) *Handler {
	return &Handler{
		cfg:        &config.Config{TiingoAPIKey: "test-key"},
		httpClient: &http.Client{Transport: answer},
	}
}

// The payload the app decodes, in full.
func TestCashtagQuoteJSON(t *testing.T) {
	cashtagCache.Delete("AAPL")
	defer cashtagCache.Delete("AAPL")

	h := stubbedHandler(func(path string) (int, string) {
		switch {
		case strings.HasPrefix(path, "/iex/AAPL/prices"):
			return 200, `[{"close":226.1},{"close":227.9},{"close":228.4},{"close":227.5}]`
		case strings.HasPrefix(path, "/iex/AAPL"):
			return 200, `[{"ticker":"AAPL","last":227.52,"prevClose":229.45,"timestamp":"2026-09-06T12:00:00Z"}]`
		default:
			return 200, `[{"ticker":"AAPL","name":"Apple Inc.","exchangeCode":"NASDAQ"}]`
		}
	})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/cashtag/AAPL", nil)
	r.SetPathValue("ticker", "AAPL")
	h.apiV1Cashtag(rec, r, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %s", rec.Body)
	}
	want := map[string]interface{}{
		"ticker":       "AAPL",
		"display":      "$AAPL",
		"company_name": "Apple Inc.",
		"exchange":     "NASDAQ",
		"price":        227.52,
		"price_label":  "227.52",
		"change_label": "-0.84%",
		"is_positive":  false,
		"as_of":        "2026-09-06T12:00:00Z",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %v, want %v", k, got[k], w)
		}
	}
	series, _ := got["sparkline"].([]interface{})
	if len(series) != 4 || series[0] != 226.1 {
		t.Errorf("sparkline %v; the app draws its own line from prices", got["sparkline"])
	}
	if pct, _ := got["change_pct"].(float64); pct > -0.83 || pct < -0.85 {
		t.Errorf("change_pct %v", got["change_pct"])
	}
}

// A symbol the provider lists nothing for is a 404, not an empty card and not
// a 503: this $WORD was never a company.
func TestCashtagProviderKnowsNothingIs404(t *testing.T) {
	cashtagCache.Delete("ZZZZ")
	defer cashtagCache.Delete("ZZZZ")

	h := stubbedHandler(func(string) (int, string) { return 200, `[]` })
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/cashtag/ZZZZ", nil)
	r.SetPathValue("ticker", "ZZZZ")
	h.apiV1Cashtag(rec, r, nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", rec.Code, rec.Body)
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "cashtag_not_found" {
		t.Fatalf("code %q", got)
	}
}

// ── The composer's dropdown ─────────────────────────────────────────────────

// Half a word is an empty list and a 200. A composer asks on every keystroke,
// and an error for somebody who has not finished typing would draw as a
// failure under their cursor.
func TestCashtagSearchShortQueryIsEmptyNotAnError(t *testing.T) {
	h := stubbedHandler(func(string) (int, string) {
		t.Error("the provider was asked about a single letter")
		return 200, `[]`
	})
	for _, q := range []string{"", " ", "$", "a", "$a"} {
		rec := httptest.NewRecorder()
		h.apiV1CashtagSearch(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cashtag/search?q="+url.QueryEscape(q), nil), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%q: status %d", q, rec.Code)
		}
		if strings.TrimSpace(rec.Body.String()) != `{"results":[]}` {
			t.Fatalf("%q answered %s", q, rec.Body)
		}
	}
}

// Only symbols a body could carry are offered. The provider lists instruments
// whose tickers the linkifier would never turn into a cashtag, and suggesting
// one would put a $TICKER in a work that renders as plain text.
func TestCashtagSearchOffersOnlyLinkifiableTickers(t *testing.T) {
	h := stubbedHandler(func(path string) (int, string) {
		if !strings.Contains(path, "limit=8") {
			t.Errorf("dropdown asked for %s", path)
		}
		return 200, `[{"ticker":"AAPL","name":"Apple Inc.","exchangeCode":"NASDAQ"},
		              {"ticker":"BRK-B","name":"Berkshire Hathaway"},
		              {"ticker":"TOOLONG","name":"Six letters"},
		              {"ticker":"msft","name":"Microsoft","exchangeCode":"NASDAQ"}]`
	})
	rec := httptest.NewRecorder()
	h.apiV1CashtagSearch(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cashtag/search?q=appl", nil), nil)

	var out CashtagSearchDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON: %s", rec.Body)
	}
	if len(out.Results) != 2 {
		t.Fatalf("offered %+v", out.Results)
	}
	if out.Results[0].Display != "$AAPL" || out.Results[0].CompanyName != "Apple Inc." || out.Results[0].Exchange != "NASDAQ" {
		t.Errorf("first row %+v", out.Results[0])
	}
	// A provider writing a ticker in lower case still means the symbol.
	if out.Results[1].Ticker != "MSFT" || out.Results[1].Display != "$MSFT" {
		t.Errorf("second row %+v", out.Results[1])
	}
}

// A deployment with no provider says so once, in the code the app branches on,
// on both routes.
func TestCashtagSearchUnconfigured(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	h.apiV1CashtagSearch(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cashtag/search?q=apple", nil), nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "cashtag_unavailable" {
		t.Fatalf("code %q", got)
	}
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Error apiErrorBody `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("not the error envelope: %s", body)
	}
	return envelope.Error.Code
}
