package web

import (
	"strings"
	"testing"
)

func TestRenderAppendsCSSVersion(t *testing.T) {
	r, err := LoadTemplates("../../templates")
	if err != nil {
		t.Fatalf("templates: %v", err)
	}

	var b strings.Builder
	if err := r.Render(&b, "error", map[string]any{"Message": "x"}); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(b.String(), `href="/static/style.css"`) {
		t.Error("unset version should leave the stylesheet URL bare")
	}

	r.SetCSSVersion("abc12345")
	b.Reset()
	if err := r.Render(&b, "error", map[string]any{"Message": "x"}); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(b.String(), `href="/static/style.css?v=abc12345"`) {
		t.Error("stylesheet URL should carry the cache-busting version")
	}
}
