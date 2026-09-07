package middleware

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// gzipPool reuses gzip.Writer allocations across requests so the compression
// layer adds no per-request allocation pressure on the hot path.
var gzipPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// compressibleType reports whether a Content-Type benefits from gzip. Images,
// video, audio, fonts (woff2) and wasm are already compressed or stream-decoded,
// so gzipping them burns CPU for no size win. SSE (event-stream) must never be
// buffered. Everything text-like — HTML, CSS, JS, JSON, SVG, XML — compresses well.
func compressibleType(ct string) bool {
	ct = strings.ToLower(ct)
	if strings.Contains(ct, "event-stream") {
		return false
	}
	switch {
	case strings.HasPrefix(ct, "text/"),
		strings.HasPrefix(ct, "application/json"),
		strings.Contains(ct, "+json"),
		strings.HasPrefix(ct, "application/javascript"),
		strings.HasPrefix(ct, "application/xml"),
		strings.Contains(ct, "+xml"),
		strings.HasPrefix(ct, "application/manifest"),
		strings.HasPrefix(ct, "image/svg"):
		return true
	}
	return false
}

// gzipResponseWriter switches to gzip lazily on the first write, but only once
// the handler's Content-Type is known and compressible. Until that decision it
// passes through untouched, so binary media is never buffered or re-encoded.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz       *gzip.Writer
	decided  bool
	compress bool
}

func (g *gzipResponseWriter) decide() {
	if g.decided {
		return
	}
	g.decided = true
	h := g.ResponseWriter.Header()
	// Never double-encode if a handler already set its own Content-Encoding.
	if h.Get("Content-Encoding") != "" || !compressibleType(h.Get("Content-Type")) {
		return
	}
	g.compress = true
	h.Del("Content-Length") // length changes after compression — must be chunked
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	gz := gzipPool.Get().(*gzip.Writer)
	gz.Reset(g.ResponseWriter)
	g.gz = gz
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	g.decide()
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.decided {
		// Handler wrote without an explicit WriteHeader. Pin a Content-Type first
		// so the compress/no-compress decision is stable for this response.
		if g.ResponseWriter.Header().Get("Content-Type") == "" {
			g.ResponseWriter.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.compress {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

// Flush proxies through gzip then to the underlying writer so streaming handlers
// keep working. (SSE is excluded upstream, but other flushing handlers rely on this.)
func (g *gzipResponseWriter) Flush() {
	if g.compress && g.gz != nil {
		g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack proxies http.Hijacker so any future connection-upgrade handler behind
// this layer keeps working. Upgrade requests are also skipped before wrapping.
func (g *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := g.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (g *gzipResponseWriter) close() {
	if g.gz != nil {
		g.gz.Close()
		gzipPool.Put(g.gz)
		g.gz = nil
	}
}

// Compress gzip-encodes compressible responses when the client advertises gzip
// support. It is a no-op for clients without gzip, for connection upgrades, for
// Range requests (which require byte-exact bodies), and for the SSE channel. The
// compress decision is made lazily from the response Content-Type, so binary
// media flows through with zero buffering.
func Compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			r.Header.Get("Range") != "" ||
			strings.EqualFold(r.Header.Get("Connection"), "Upgrade") ||
			r.Header.Get("Upgrade") != "" ||
			r.URL.Path == "/api/events" {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}
