package main

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"time"
)

//go:embed templates/*.html
var templatesFS embed.FS

var pageFuncs = template.FuncMap{
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[fmt.Sprint(kv[i])] = kv[i+1]
		}
		return m
	},
	"ago": func(v any) string {
		t, ok := v.(time.Time)
		if !ok || t.IsZero() {
			return "never"
		}
		return time.Since(t).Round(time.Second).String() + " ago"
	},
}

// Pages renders the built-in templates, optionally overridden by name from a directory.
// An override replaces whole {{define}} blocks, so a theme can change only "theme" (colours,
// fonts), "header" or "art" and keep the built-in pages.
type Pages struct {
	t   *template.Template
	cfg PagesConfig
}

func loadPages(cfg PagesConfig) (*Pages, error) {
	t, err := template.New("").Funcs(pageFuncs).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	if cfg.TemplatesDir != "" {
		files, err := filepath.Glob(filepath.Join(cfg.TemplatesDir, "*.html"))
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("pages.templates_dir %s has no .html files", cfg.TemplatesDir)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			if _, err := t.New(filepath.Base(f)).Parse(string(b)); err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
		}
	}
	return &Pages{t: t, cfg: cfg}, nil
}

// Render executes a page with the shared fields every page may use.
func (p *Pages) Render(w io.Writer, name string, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	data["Site"] = map[string]string{"Name": p.cfg.SiteName, "URL": p.cfg.SiteURL}
	data["Style"] = p.cfg.StyleURL
	return p.t.ExecuteTemplate(w, name, data)
}
