// validator checks endpoint health: status codes, JSON fields, latency thresholds.
package validator

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Result struct {
	Label     string
	URL       string
	Method    string
	Identity  string
	Status    int
	Latency   time.Duration
	Pass      bool
	Warn      bool
	Error     string
	BodySnip  string
}

func (r Result) Symbol() string {
	if r.Pass { return "✅" }
	if r.Warn { return "⚠️ " }
	return "❌"
}

type Check struct {
	Label       string
	Method      string
	URL         string
	Body        string
	BodyType    string         // "json" (default) or "form"
	Cookie      string         // f33d3r_handle value to inject
	WantStatus  []int          // any of these is OK (empty = 200 only)
	WantField   string         // JSON field that must exist in response
	WantNoErr   bool           // body must not contain "error"
	MaxLatency  time.Duration  // 0 = no limit
	Identity    string
}

func Run(client *http.Client, c Check) Result {
	r := Result{
		Label:    c.Label,
		URL:      c.URL,
		Method:   c.Method,
		Identity: c.Identity,
	}
	if r.Method == "" { r.Method = http.MethodGet }

	var bodyReader io.Reader
	if c.Body != "" {
		bodyReader = strings.NewReader(c.Body)
	}

	req, err := http.NewRequest(r.Method, c.URL, bodyReader)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	if c.Body != "" {
		if c.BodyType == "form" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	req.Header.Set("User-Agent", "AethyrWalker/1.0")
	if c.Cookie != "" {
		req.AddCookie(&http.Cookie{Name: "f33d3r_handle", Value: c.Cookie})
	}

	start := time.Now()
	resp, err := client.Do(req)
	r.Latency = time.Since(start)
	if err != nil {
		r.Error = "request failed: " + err.Error()
		return r
	}
	defer resp.Body.Close()

	r.Status = resp.StatusCode
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if len(raw) > 120 {
		r.BodySnip = string(raw[:120]) + "…"
	} else {
		r.BodySnip = string(raw)
	}

	// Status check
	wantCodes := c.WantStatus
	if len(wantCodes) == 0 { wantCodes = []int{200} }
	statusOK := false
	for _, code := range wantCodes {
		if r.Status == code { statusOK = true; break }
	}

	// JSON field check
	fieldOK := true
	if c.WantField != "" {
		var obj interface{}
		if json.Unmarshal(raw, &obj) == nil {
			fieldOK = strings.Contains(string(raw), `"`+c.WantField+`"`)
		} else {
			fieldOK = false
		}
	}

	// Error check
	noErrOK := true
	if c.WantNoErr && strings.Contains(string(raw), `"error"`) {
		noErrOK = false
	}

	// Latency check
	latOK := c.MaxLatency == 0 || r.Latency <= c.MaxLatency

	if !latOK {
		r.Warn = true
		r.Error = fmt.Sprintf("slow: %s > %s", r.Latency.Round(time.Millisecond), c.MaxLatency)
		r.Pass = statusOK && fieldOK && noErrOK
		return r
	}

	if statusOK && fieldOK && noErrOK {
		r.Pass = true
	} else if !statusOK {
		r.Error = fmt.Sprintf("got %d, want one of %v", r.Status, wantCodes)
	} else if !fieldOK {
		r.Error = fmt.Sprintf("field %q missing in: %s", c.WantField, r.BodySnip)
	} else {
		r.Error = "body contains error: " + r.BodySnip
	}

	return r
}
