package handler

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/f33d3r/feed-engine/internal/model"
)

// Proves settings_section_privacy executes end-to-end against a real model.User
// (no phantom fields) and reaches the Celebrations toggle. Regression guard for
// the template-abort bug where .User.DefaultReplyRestriction halted the render.
func TestPrivacySectionRendersCelebrations(t *testing.T) {
	tmpl, err := template.New("_settings_section_privacy.html").
		Funcs(template.FuncMap{"currentCelebration": func() string { return "Pride" }}).
		ParseFiles(filepath.Join("..", "..", "web", "templates", "partials", "_settings_section_privacy.html"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	u := &model.User{
		ContentSetting:      "default",
		ShowSensitive:       false,
		CelebrationsEnabled: true,
		IsAgeVerified:       true,
		IsVerified:          true,
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "settings_section_privacy", map[string]interface{}{"User": u}); err != nil {
		t.Fatalf("execute aborted (the bug): %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Celebration themes", "settings.privacy.celebrations", "Right now we're celebrating", "Show sensitive content"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered output missing %q — section truncated", want)
		}
	}
}
