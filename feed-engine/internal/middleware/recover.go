package middleware

import (
	"log"
	"net/http"
	"runtime/debug"
)

// recoverWriter tracks whether anything has been written to the response so the
// Recover middleware knows if it can still emit a clean 500. It proxies
// http.Flusher so streaming/SSE handlers keep working.
type recoverWriter struct {
	http.ResponseWriter
	wrote bool
}

func (rw *recoverWriter) WriteHeader(code int) {
	rw.wrote = true
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *recoverWriter) Write(b []byte) (int, error) {
	rw.wrote = true
	return rw.ResponseWriter.Write(b)
}

func (rw *recoverWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Recover is the outermost middleware: it converts any panic in a downstream
// handler into a clean 500 (when nothing has been written yet) instead of a
// dropped connection, and logs the stack trace for forensics. Buffered
// rendering (Handler.render / Handler.renderPartial) already makes template
// execution failures atomic; this is the final backstop for unexpected panics
// anywhere else in the request path, so one bad request can never take the
// whole connection — or an adjacent surface like the right rail — down with it.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &recoverWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				// http.ErrAbortHandler is the stdlib's intentional "abort this
				// request silently" sentinel — re-panic so net/http handles it.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				log.Printf("[panic] %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				if !rw.wrote {
					http.Error(rw, "internal server error", http.StatusInternalServerError)
				}
			}
		}()
		next.ServeHTTP(rw, r)
	})
}
