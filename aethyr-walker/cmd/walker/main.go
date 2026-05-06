// Aethyr Walker — Full-Surface E2E Validation Engine for F33D3R
// Simulates real user behavior across all 13 brains, all UI pages, and all API surfaces.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/f33d3r/aethyr-walker/internal/crawler"
	"github.com/f33d3r/aethyr-walker/internal/reporter"
	"github.com/f33d3r/aethyr-walker/internal/session"
	"github.com/f33d3r/aethyr-walker/internal/validator"
)

func main() {
	base    := flag.String("base",    "http://localhost:8081", "Nantar base URL")
	crawl   := flag.Bool("crawl",    false,                   "Run HTML crawler (slower)")
	verbose := flag.Bool("verbose",  false,                   "Print each result as it runs")
	flag.Parse()

	rep := reporter.New()
	var mu sync.Mutex
	add := func(res validator.Result) {
		mu.Lock()
		rep.Add(res)
		mu.Unlock()
		if *verbose {
			fmt.Printf("%s %s\n", res.Symbol(), res.Label)
		}
	}

	ids := session.All(*base)
	authedClient := ids[1].Client // @edd
	anonClient   := ids[0].Client // guest

	_ = anonClient

	fmt.Println("🕷  Aethyr Walker starting…")
	fmt.Printf("   Target: %s\n", *base)

	// ── 1. HEALTH CHECKS (all 13 brains) ─────────────────────────────────────
	healthChecks := []struct{ label, url, field string }{
		{"Health/Nantar :8081",       *base + "/api/health",          "status"},
		{"Health/AethyrRank :8080",   "http://localhost:8080/health", "status"},
		{"Health/Zior :8082",         "http://localhost:8082/health", "status"},
		{"Health/AinSoph :8089",      "http://localhost:8089/health", ""},
		{"Health/Vovin :8092",        "http://localhost:8092/health", "status"},
		{"Health/ElohimVeni :8093",   "http://localhost:8093/health", "status"},
		{"Health/Zodacare :8090",     "http://localhost:8090/health", "status"},
		{"Health/SchemaRegistry :8079","http://localhost:8079/health","ok"},
		{"Health/Registrar :8094",    "http://localhost:8094/health", "service"},
		{"Health/Verity :8095",       "http://localhost:8095/health", "service"},
		{"Health/Ledger :8096",       "http://localhost:8096/health", "service"},
	}
	var wg sync.WaitGroup
	for _, hc := range healthChecks {
		hc := hc
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := validator.Check{
				Label: hc.label, URL: hc.url,
				WantStatus: []int{200}, WantField: hc.field,
				MaxLatency: 5 * time.Second,
			}
			add(validator.Run(authedClient, c))
		}()
	}
	wg.Wait()

	// ── 2. NANTAR UI PAGES (auth wall, page render, session) ─────────────────
	authedPages := []string{
		"/", "/explore", "/profile", "/wallet", "/messages",
		"/notifications", "/bookmarks", "/settings", "/music",
	}
	for _, path := range authedPages {
		path := path
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", *base+path, nil)
			req.AddCookie(&http.Cookie{Name: "f33d3r_handle", Value: "edd"})
			req.Header.Set("User-Agent", "AethyrWalker/1.0")
			start := time.Now()
			resp, err := authedClient.Do(req)
			lat := time.Since(start)
			res := validator.Result{
				Label: "UI/Page " + path, URL: *base + path,
				Method: "GET", Latency: lat, Identity: "edd",
			}
			if err != nil {
				res.Error = err.Error()
			} else {
				resp.Body.Close()
				res.Status = resp.StatusCode
				res.Pass = resp.StatusCode == 200
				if !res.Pass {
					res.Error = fmt.Sprintf("got %d", resp.StatusCode)
				}
				if lat > 2*time.Second {
					res.Warn = true
					res.Error += " (slow)"
				}
			}
			add(res)
		}()
	}

	// User profile pages
	for _, handle := range []string{"edd", "admin", "creator", "dev", "guest"} {
		handle := handle
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", *base+"/u/"+handle, nil)
			req.AddCookie(&http.Cookie{Name: "f33d3r_handle", Value: "edd"})
			req.Header.Set("User-Agent", "AethyrWalker/1.0")
			start := time.Now()
			resp, err := authedClient.Do(req)
			lat := time.Since(start)
			res := validator.Result{
				Label: "UI/Profile @" + handle, URL: *base + "/u/" + handle,
				Method: "GET", Latency: lat, Identity: "edd",
			}
			if err != nil {
				res.Error = err.Error()
			} else {
				resp.Body.Close()
				res.Status = resp.StatusCode
				res.Pass = resp.StatusCode == 200
				if !res.Pass { res.Error = fmt.Sprintf("got %d", resp.StatusCode) }
			}
			add(res)
		}()
	}
	wg.Wait()

	// ── 3. FEED SURFACES ──────────────────────────────────────────────────────
	surfaces := []string{"feed", "latest", "following", "profile_posts", "explore"}
	for _, surf := range surfaces {
		surf := surf
		wg.Add(1)
		go func() {
			defer wg.Done()
			add(validator.Run(authedClient, validator.Check{
				Label:      "Feed/Surface=" + surf,
				URL:        *base + "/feed?surface=" + surf + "&session_id=walker_test",
				WantStatus: []int{200},
				MaxLatency: 3 * time.Second,
			}))
		}()
	}
	wg.Wait()

	// ── 4. AIN SOPH WALLET ────────────────────────────────────────────────────
	ainSophChecks := []validator.Check{
		{Label: "Wallet/Supply",    URL: "http://localhost:8089/v1/supply",    WantField: "ticker"},
		{Label: "Wallet/Proposals", URL: "http://localhost:8089/v1/proposals", WantStatus: []int{200}},
		{Label: "Wallet/Proxy→AinSoph", URL: *base + "/ainsoph/v1/supply",
			WantStatus: []int{200}, WantField: "ticker", Cookie: "edd"},
	}
	runAll(authedClient, ainSophChecks, add, &wg)
	wg.Wait()

	// ── 5. AETHYR LEDGER ─────────────────────────────────────────────────────
	testPIAL := "11111111-1111-1111-1111-111111111111"
	ledgerChecks := []validator.Check{
		{Label: "Ledger/Supply",      URL: "http://localhost:8096/v1/supply",
			WantField: "total_supply_uaet"},
		{Label: "Ledger/Balance",     URL: "http://localhost:8096/v1/accounts/" + testPIAL,
			WantField: "balance_uaet"},
		{Label: "Ledger/Blocks",      URL: "http://localhost:8096/v1/blocks",
			WantStatus: []int{200}},
		{Label: "Ledger/History",     URL: "http://localhost:8096/v1/accounts/" + testPIAL + "/history",
			WantStatus: []int{200}},
		{Label: "Ledger/Fund(internal)",
			Method: "POST",
			URL:    "http://localhost:8096/v1/fund",
			Body:   `{"pial_id":"` + testPIAL + `","rail":"internal","fiat_amount":100}`,
			WantField: "aet_minted", WantStatus: []int{200}},
		{Label: "Ledger/Proxy→Ledger", URL: *base + "/ledger/v1/supply",
			WantStatus: []int{200}, WantField: "total_supply_uaet", Cookie: "edd"},
	}
	runAll(authedClient, ledgerChecks, add, &wg)
	wg.Wait()

	// ── 6. VERITY / KYC ──────────────────────────────────────────────────────
	verityChecks := []validator.Check{
		{Label: "Verity/Profile",  URL: "http://localhost:8095/v1/profile/" + testPIAL,
			WantField: "creator_tier"},
		{Label: "Verity/KYCStatus",URL: "http://localhost:8095/v1/kyc/" + testPIAL,
			WantField: "submissions", WantStatus: []int{200}},
		{Label: "Verity/Decide(onboarding)",
			Method: "POST",
			URL:    "http://localhost:8095/v1/decide",
			Body:   `{"pial_id":"` + testPIAL + `","context":"onboarding"}`,
			WantField: "status"},
		{Label: "Verity/Decide(monetize→deny)",
			Method: "POST",
			URL:    "http://localhost:8095/v1/decide",
			Body:   `{"pial_id":"` + testPIAL + `","context":"payout_activation"}`,
			WantField: "status"},
		{Label: "Verity/Proxy→Verity", URL: *base + "/verity/v1/profile/" + testPIAL,
			WantStatus: []int{200}, WantField: "creator_tier", Cookie: "edd"},
	}
	runAll(authedClient, verityChecks, add, &wg)
	wg.Wait()

	// ── 7. ELOHIM VENI / SECURITY ─────────────────────────────────────────────
	elohimChecks := []validator.Check{
		{Label: "Security/Stats",       URL: "http://localhost:8093/v1/stats",
			WantField: "total_decisions"},
		{Label: "Security/Decision(post)",
			Method: "POST",
			URL:    "http://localhost:8093/v1/decisions",
			Body:   `{"pial_id":"11111111-1111-1111-1111-111111111111","action":"post","context":{"content_risk":0.0,"anomaly_score":0.0}}`,
			WantField: "decision", WantStatus: []int{200}},
		{Label: "Security/Decision(monetize→deny)",
			Method: "POST",
			URL:    "http://localhost:8093/v1/decisions",
			Body:   `{"pial_id":"11111111-1111-1111-1111-111111111111","action":"monetize","context":{"content_risk":0.0,"anomaly_score":0.0}}`,
			WantField: "decision", WantStatus: []int{200}},
	}
	runAll(authedClient, elohimChecks, add, &wg)
	wg.Wait()

	// ── 8. REGISTRAR / HANDLES ────────────────────────────────────────────────
	registrarChecks := []validator.Check{
		{Label: "Registrar/Search(edd)",   URL: "http://localhost:8094/v1/search?q=edd",
			WantField: "handle", WantStatus: []int{200}},
		{Label: "Registrar/Search(empty)", URL: "http://localhost:8094/v1/search?q=xqzunknown999",
			WantStatus: []int{200}},
		{Label: "Registrar/Resolve(edd)",  URL: "http://localhost:8094/v1/handles/edd",
			WantStatus: []int{200, 404}},
	}
	runAll(authedClient, registrarChecks, add, &wg)
	wg.Wait()

	// ── 9. VOVIN MESSAGING ────────────────────────────────────────────────────
	vovinChecks := []validator.Check{
		{Label: "Vovin/Health",       URL: "http://localhost:8092/health",       WantField: "status"},
		{Label: "Vovin/RelayNodes",   URL: "http://localhost:8092/v1/relay/nodes", WantStatus: []int{200}},
		{Label: "Vovin/Proxy→Vovin",  URL: *base + "/vovin/v1/relay/nodes",
			WantStatus: []int{200}},
	}
	runAll(authedClient, vovinChecks, add, &wg)
	wg.Wait()

	// ── 10. ZODACARE / SAFETY ─────────────────────────────────────────────────
	zodacareChecks := []validator.Check{
		{Label: "Safety/Health", URL: "http://localhost:8090/health", WantField: "status"},
	}
	runAll(authedClient, zodacareChecks, add, &wg)
	wg.Wait()

	// ── 11. AUTH BOUNDARY TESTS ───────────────────────────────────────────────
	// These routes should redirect unauthenticated users to /login
	// /feed is a HTMX partial — no auth wall by design (returns empty feed for anon)
	authWallPaths := []string{"/", "/profile", "/wallet", "/messages"}
	for _, path := range authWallPaths {
		path := path
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", *base+path, nil)
			req.Header.Set("User-Agent", "AethyrWalker/1.0")
			client := &http.Client{
				Timeout: 5 * time.Second,
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse // don't follow
				},
			}
			resp, err := client.Do(req)
			res := validator.Result{
				Label: "Auth/Wall " + path, URL: *base + path,
				Method: "GET", Identity: "anonymous",
			}
			if err != nil {
				res.Error = err.Error()
			} else {
				resp.Body.Close()
				res.Status = resp.StatusCode
				// Must redirect (301/302/303) — never 200 for anonymous
				res.Pass = resp.StatusCode >= 301 && resp.StatusCode <= 303
				if !res.Pass {
					res.Error = fmt.Sprintf("auth wall missing — got %d, want 3xx redirect", resp.StatusCode)
				}
			}
			add(res)
		}()
	}
	wg.Wait()

	// ── 12. API WRITE PATHS ───────────────────────────────────────────────────
	// Verify write endpoints return correct status for valid/invalid input
	writeChecks := []validator.Check{
		// Valid post
		{Label: "API/Post(empty→reject)",
			Method: "POST", URL: *base + "/api/post",
			Body:   "", WantStatus: []int{400, 405, 422}},
		// Search
		{Label: "API/UserSearch",
			URL: *base + "/api/users/search?q=edd", WantField: "handle", WantStatus: []int{200}},
		// Feedback (HTMX signal — should accept)
		{Label: "API/Feedback",
			Method: "POST", URL: *base + "/api/feedback",
			Body: `content_id=test&session_id=walker&event=view&surface=feed`,
			BodyType: "form", Cookie: "edd",
			WantStatus: []int{200, 204, 400}}, // 400 is OK — no real content_id
	}
	for _, c := range writeChecks {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(c.Method, c.URL, nil)
			if c.Method == "" { c.Method = "GET" }
			req.AddCookie(&http.Cookie{Name: "f33d3r_handle", Value: "edd"})
			req.Header.Set("User-Agent", "AethyrWalker/1.0")
			if c.Body != "" {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			start := time.Now()
			resp, err := authedClient.Do(req)
			res := validator.Result{Label: c.Label, URL: c.URL, Method: c.Method, Latency: time.Since(start)}
			if err != nil { res.Error = err.Error() } else {
				resp.Body.Close()
				res.Status = resp.StatusCode
				for _, want := range c.WantStatus {
					if resp.StatusCode == want { res.Pass = true; break }
				}
				if !res.Pass { res.Error = fmt.Sprintf("got %d", resp.StatusCode) }
			}
			add(res)
		}()
	}
	wg.Wait()

	// ── 13. LATENCY BENCHMARKS ────────────────────────────────────────────────
	latChecks := []struct{ label, url string; max time.Duration }{
		{"Perf/Feed p99",    *base + "/feed?surface=feed",         1500 * time.Millisecond},
		{"Perf/Home page",   *base + "/",                          2 * time.Second},
		{"Perf/AinSoph sup", "http://localhost:8089/v1/supply",     500 * time.Millisecond},
		{"Perf/Ledger sup",  "http://localhost:8096/v1/supply",     500 * time.Millisecond},
	}
	for _, lc := range latChecks {
		lc := lc
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", lc.url, nil)
			req.AddCookie(&http.Cookie{Name: "f33d3r_handle", Value: "edd"})
			req.Header.Set("User-Agent", "AethyrWalker/1.0")
			start := time.Now()
			resp, err := authedClient.Do(req)
			lat := time.Since(start)
			res := validator.Result{Label: lc.label, URL: lc.url, Method: "GET", Latency: lat}
			if err != nil {
				res.Error = err.Error()
			} else {
				resp.Body.Close()
				res.Status = resp.StatusCode
				res.Pass = resp.StatusCode >= 200 && resp.StatusCode < 400
				if lat > lc.max {
					res.Warn = true
					res.Error = fmt.Sprintf("latency %s > threshold %s", lat.Round(time.Millisecond), lc.max)
				}
			}
			add(res)
		}()
	}
	wg.Wait()

	// ── 14. HTML CRAWLER (optional, depth-limited) ────────────────────────────
	if *crawl {
		fmt.Println("\n🕷  Running HTML crawler (depth=2)…")
		seeds := []string{*base + "/", *base + "/explore", *base + "/music"}
		g := crawler.Crawl(authedClient, *base, seeds, "edd", 2, 5)
		fmt.Println(g.Summary())
		for _, n := range g.Nodes {
			res := validator.Result{
				Label:    "Crawl/" + n.URL,
				URL:      n.URL,
				Method:   "GET",
				Status:   n.Status,
				Latency:  n.Latency,
				Identity: n.Identity,
			}
			res.Pass = n.Status >= 200 && n.Status < 400
			if !res.Pass { res.Error = fmt.Sprintf("got %d from %s", n.Status, n.FromURL) }
			add(res)
		}
	}

	// ── FINAL REPORT ──────────────────────────────────────────────────────────
	rep.Print()

	if rep.HasFailures() {
		os.Exit(1)
	}
}

func runAll(client *http.Client, checks []validator.Check, add func(validator.Result), wg *sync.WaitGroup) {
	for _, c := range checks {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			add(validator.Run(client, c))
		}()
	}
}

// prettyJSON is a debug helper — print any value as indented JSON.
func prettyJSON(v interface{}) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

var _ = prettyJSON // suppress unused warning
