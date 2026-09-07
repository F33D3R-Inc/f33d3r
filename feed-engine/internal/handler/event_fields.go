package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// The POST /events lane takes a body as a form or as JSON. A browser button
// posts a form; the native clients post JSON. The two helpers here read one
// field either way, so an event case reads its parameters once and does not
// know which client sent them. The first non-empty name wins, so a case can
// accept the web's name and the clients' name for the same parameter.

// eventField returns the first of names present as a string.
func eventField(r *http.Request, rawBody map[string]json.RawMessage, names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(r.FormValue(name)); v != "" {
			return v
		}
		if rawBody == nil {
			continue
		}
		raw, ok := rawBody[name]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
			continue
		}
		// A JSON number where a string was expected is still the value:
		// {"option_idx": 2} and {"option_idx": "2"} name the same option.
		var n json.Number
		if err := json.Unmarshal(raw, &n); err == nil && n != "" {
			return n.String()
		}
	}
	return ""
}

// eventIntField reads an integer field. ok is false when the field is absent
// or is not an integer.
func eventIntField(r *http.Request, rawBody map[string]json.RawMessage, names ...string) (int, bool) {
	v := eventField(r, rawBody, names...)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}
