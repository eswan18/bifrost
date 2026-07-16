package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"time"
)

type Renderer struct {
	tmpls      map[string]*template.Template
	cssVersion string
}

// LoadTemplates parses every template under dir as a child of base.html.
// Each non-base file becomes its own named template, e.g. overview.html →
// "overview".
func LoadTemplates(dir string) (*Renderer, error) {
	base := filepath.Join(dir, "base.html")
	matches, err := filepath.Glob(filepath.Join(dir, "*.html"))
	if err != nil {
		return nil, err
	}
	r := &Renderer{tmpls: map[string]*template.Template{}}
	// Late-bound so SetCSSVersion can be called after parsing.
	funcs := template.FuncMap{
		"cssVersion": func() string { return r.cssVersion },
		// reltime renders against the request-time clock, so the relative
		// label stays fresh across the page's background re-renders.
		"reltime": func(t time.Time) string { return relativeTime(t, time.Now()) },
		"abstime": absTime,
		// dict builds a map inline so a shared block (e.g. the rollback modal
		// row) can be handed several named values at once.
		"dict": func(pairs ...any) (map[string]any, error) {
			if len(pairs)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i < len(pairs); i += 2 {
				key, ok := pairs[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %d is not a string", i)
				}
				m[key] = pairs[i+1]
			}
			return m, nil
		},
	}
	for _, m := range matches {
		if m == base {
			continue
		}
		t, err := template.New(filepath.Base(base)).Funcs(funcs).ParseFiles(base, m)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", m, err)
		}
		name := filepath.Base(m)
		name = name[:len(name)-len(filepath.Ext(name))]
		r.tmpls[name] = t
	}
	return r, nil
}

// SetCSSVersion sets the value the cssVersion template function returns;
// base.html appends it to the stylesheet URL as a cache-busting query
// param. Empty means no param (e.g. during tests).
func (r *Renderer) SetCSSVersion(v string) { r.cssVersion = v }

// CSSVersion content-hashes the compiled stylesheet. Each CSS change yields
// a new URL, so no cache layer (browser heuristics, Cloudflare edge or
// Browser Cache TTL) can ever serve a stale stylesheet against new HTML.
func CSSVersion(staticDir string) string {
	b, err := os.ReadFile(filepath.Join(staticDir, "style.css"))
	if err != nil {
		return "" // no busting, but the app still works
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

func (r *Renderer) Render(w io.Writer, name string, data any) error {
	return r.RenderNamed(w, name, "base", data)
}

// RenderNamed renders a specific block (e.g. "rows") from the page template
// set, rather than the whole "base" document. Used to serve HTML fragments
// the browser swaps in without a full-page reload.
func (r *Renderer) RenderNamed(w io.Writer, page, block string, data any) error {
	t, ok := r.tmpls[page]
	if !ok {
		return fmt.Errorf("template %q not found", page)
	}
	return t.ExecuteTemplate(w, block, data)
}
