package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
)

// The browser Shell (base.html) is created once per session and lives for the
// whole session. Navigation never loads a document again: the Shell asks for
// the Playground of the next page and swaps only #playground. These helpers
// name that caller and answer the cases a Playground cannot serve.

// playgroundSwap is the swap the Shell applies to #playground: replace its
// contents and put the viewport back at the top, the way a page load would.
const playgroundSwap = "innerHTML show:window:top"

// playgroundRequest reports whether the caller is the browser Shell asking for
// the Playground of a page: a boosted anchor or form, an explicit htmx request
// targeting #playground, or a history restore of #playground. A native Shell
// (Android) also says HX-Request but none of these, and keeps the body-only
// facet answer.
func playgroundRequest(r *http.Request) bool {
	if r == nil || r.Header.Get("HX-Request") != "true" {
		return false
	}
	return r.Header.Get("HX-Boosted") == "true" ||
		r.Header.Get("HX-Target") == "playground" ||
		r.Header.Get("HX-History-Restore-Request") == "true"
}

// hxRedirect answers an htmx caller with a full document navigation. It is the
// one honest reply when the destination is a different Shell (login, onboard,
// deactivated) and the current Shell cannot be kept.
func hxRedirect(w http.ResponseWriter, target string) {
	w.Header().Set("HX-Redirect", target)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// hxAwareRedirect sends the caller to a Shell-changing destination in the way
// each caller can follow: a document redirect for a page load, HX-Redirect for
// an htmx request (which would otherwise follow the 3xx inside its XHR and try
// to swap a different Shell into this one).
func hxAwareRedirect(w http.ResponseWriter, r *http.Request, target string) {
	if facetRequest(r) {
		hxRedirect(w, target)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// hxCurrentPath returns the path and query of the page the Shell is showing,
// taken from the HX-Current-URL header htmx sends with every request.
func hxCurrentPath(r *http.Request) string {
	cur := r.Header.Get("HX-Current-URL")
	if cur == "" {
		return "/"
	}
	u, err := url.Parse(cur)
	if err != nil || u.Path == "" {
		return "/"
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

// hxLocationHeader writes HX-Location so the Shell navigates its Playground to
// path — a server-directed navigation with no document load. push controls
// whether the URL is pushed onto history or replaces the current entry.
func hxLocationHeader(w http.ResponseWriter, path string, push bool) {
	spec := map[string]interface{}{
		"path":   path,
		"target": "#playground",
		"swap":   playgroundSwap,
	}
	if push {
		spec["push"] = "true"
	} else {
		spec["push"] = false
		spec["replace"] = "true"
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return
	}
	w.Header().Set("HX-Location", string(b))
	w.Header().Set("Cache-Control", "no-store")
}

// overlaySlotClear is the out-of-band fragment a mutation appends to its answer
// when the overlay it was submitted from is done: the Shell empties
// #overlay-slot alongside the main swap, and a native Shell does the same from
// the same markup.
const overlaySlotClear = `<div id="overlay-slot" hx-swap-oob="innerHTML"></div>`

// writeOverlaySlotClear appends overlaySlotClear to a response already carrying
// its main fragment.
func writeOverlaySlotClear(w http.ResponseWriter) {
	_, _ = io.WriteString(w, overlaySlotClear)
}

// facetOverlayClose — GET /facets/overlay/close
// The close control of every overlay Facet asks for this: an empty fragment
// swapped into #overlay-slot. Closing an overlay is a request like opening one,
// so it works identically in the browser Shell and a native Shell — no page
// script is involved.
func (h *Handler) facetOverlayClose(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// hxRefreshPlayground tells the Shell to ask the server for the page it is
// showing again and swap the fresh Playground in place. This is how a mutation
// that changes a whole page (creator mode enabled, shop opened) is reflected:
// the server renders the truth, the browser only places it. The history entry
// is replaced, not duplicated.
func hxRefreshPlayground(w http.ResponseWriter, r *http.Request) {
	hxLocationHeader(w, hxCurrentPath(r), false)
}
