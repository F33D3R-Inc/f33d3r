package observ

import (
	"context"
	"net/http"
	"time"
)

// WrapClient returns an *http.Client whose transport injects the request id
// (and PIAL when present) from the request context onto every outbound
// request. Use this for ALL inter-brain HTTP calls.
//
// Example:
//
//	c := observ.WrapClient(&http.Client{Timeout: 5 * time.Second})
//	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
//	resp, err := c.Do(req)
//
// The wrapped client also records outbound latency to Prometheus.
func WrapClient(base *http.Client) *http.Client {
	if base == nil {
		base = &http.Client{Timeout: 10 * time.Second}
	}
	t := base.Transport
	if t == nil {
		t = http.DefaultTransport
	}
	clone := *base
	clone.Transport = &correlatingTransport{next: t}
	return &clone
}

type correlatingTransport struct {
	next http.RoundTripper
}

func (c *correlatingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx := r.Context()

	// Inject correlation headers if missing on the outbound request.
	if r.Header.Get(HeaderRequestID) == "" {
		if id := RequestID(ctx); id != "" {
			r.Header.Set(HeaderRequestID, id)
		}
	}
	if r.Header.Get(HeaderPialIdentity) == "" {
		if pial := PIAL(ctx); pial != "" {
			r.Header.Set(HeaderPialIdentity, pial)
		}
	}

	start := time.Now()
	resp, err := c.next.RoundTrip(r)
	dur := time.Since(start).Seconds()

	// Outbound metrics — bucketed by destination host for capacity planning.
	dest := r.URL.Host
	status := "error"
	if resp != nil {
		status = http.StatusText(resp.StatusCode)
		if status == "" {
			status = "unknown"
		}
	}
	outboundRequests.WithLabelValues(Brain(), dest, status).Inc()
	outboundDuration.WithLabelValues(Brain(), dest).Observe(dur)

	return resp, err
}

// PropagateToOutbound is a convenience for places that build their own
// http.Request without a context, e.g. cron jobs. It manually adds the
// correlation headers from the supplied ctx onto req.
func PropagateToOutbound(ctx context.Context, req *http.Request) {
	if req == nil {
		return
	}
	if id := RequestID(ctx); id != "" && req.Header.Get(HeaderRequestID) == "" {
		req.Header.Set(HeaderRequestID, id)
	}
	if pial := PIAL(ctx); pial != "" && req.Header.Get(HeaderPialIdentity) == "" {
		req.Header.Set(HeaderPialIdentity, pial)
	}
}
