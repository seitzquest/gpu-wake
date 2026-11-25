package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPagesRender(t *testing.T) {
	for _, site := range []PagesConfig{{}, {SiteName: "example.org", SiteURL: "https://example.org/",
		StyleURL: "https://example.org/theme.css"}} {
		p, err := loadPages(site)
		if err != nil {
			t.Fatal(err)
		}
		for _, page := range []string{"busy.html", "waking.html", "offline.html", "status.html"} {
			var body bytes.Buffer
			err := p.Render(&body, page, map[string]any{"Title": "Voice <Edit>", "state": "asleep", "Timeout": 120})
			if err != nil {
				t.Fatalf("%s: %v", page, err)
			}
			html := body.String()
			if strings.Contains(html, "Voice <Edit>") {
				t.Errorf("%s: unescaped title", page)
			}
			if page != "status.html" && !strings.Contains(html, "Voice &lt;Edit&gt;") {
				t.Errorf("%s: missing title", page)
			}
			if strings.Contains(html, "<no value>") {
				t.Errorf("%s: missing template data", page)
			}
			if site.SiteName != "" {
				if !strings.Contains(html, `<a href="https://example.org/">example.org</a>`) ||
					!strings.Contains(html, `href="https://example.org/theme.css"`) {
					t.Errorf("%s: site link or stylesheet missing", page)
				}
			} else if strings.Contains(html, `class="gw-sep"`) {
				t.Errorf("%s: breadcrumb without a site", page)
			}
		}
	}
}

func TestTemplatesDirOverridesBlocks(t *testing.T) {
	dir := t.TempDir()
	theme := `{{define "header"}}<header class="mine">{{.Title}}</header>{{end}}`
	if err := os.WriteFile(filepath.Join(dir, "theme.html"), []byte(theme), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := loadPages(PagesConfig{TemplatesDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	if err := p.Render(&body, "waking.html", map[string]any{"Title": "App", "Timeout": 60}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.String(), `<header class="mine">App</header>`) ||
		strings.Contains(body.String(), `class="gw-header"`) {
		t.Fatal("override not applied")
	}
	if _, err := loadPages(PagesConfig{TemplatesDir: t.TempDir()}); err == nil {
		t.Fatal("an empty templates_dir must be an error, not a silent fallback")
	}
}
