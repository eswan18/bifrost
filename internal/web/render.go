package web

import (
	"fmt"
	"html/template"
	"io"
	"path/filepath"
)

type Renderer struct {
	tmpls map[string]*template.Template
}

// LoadTemplates parses every template under dir as a child of base.html.
// Each non-base file becomes its own named template, e.g. status.html →
// "status".
func LoadTemplates(dir string) (*Renderer, error) {
	base := filepath.Join(dir, "base.html")
	matches, err := filepath.Glob(filepath.Join(dir, "*.html"))
	if err != nil {
		return nil, err
	}
	r := &Renderer{tmpls: map[string]*template.Template{}}
	for _, m := range matches {
		if m == base {
			continue
		}
		t, err := template.ParseFiles(base, m)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", m, err)
		}
		name := filepath.Base(m)
		name = name[:len(name)-len(filepath.Ext(name))]
		r.tmpls[name] = t
	}
	return r, nil
}

func (r *Renderer) Render(w io.Writer, name string, data any) error {
	t, ok := r.tmpls[name]
	if !ok {
		return fmt.Errorf("template %q not found", name)
	}
	return t.ExecuteTemplate(w, "base", data)
}
