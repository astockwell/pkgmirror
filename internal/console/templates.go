package console

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// loadTemplates walks the embedded (or dev-dir) template set and parses
// every .tmpl file. Returns the root *template.Template with the
// console's funcMap bound, including the per-set `tmpl` dispatcher that
// the layout uses to render the page body slot.
//
// The `tmpl` dispatcher is registered as a placeholder before parsing
// (the parser needs to know it exists when it sees `{{ tmpl ... }}`),
// then overridden with the real implementation after the template set
// is built. The real impl closes over `root` so it can dispatch to any
// other named template in the set.
func loadTemplates(devDir string) (*template.Template, error) {
	placeholder := template.FuncMap{
		"tmpl": func(name string, data any) (template.HTML, error) {
			return template.HTML(""), nil
		},
	}
	root := template.New("").Funcs(funcs()).Funcs(placeholder)

	// First pass: enumerate sources.
	var sources []templateSource
	if devDir != "" {
		s, err := devDirSources(devDir)
		if err != nil {
			return nil, err
		}
		sources = s
	} else {
		s, err := embeddedSources()
		if err != nil {
			return nil, err
		}
		sources = s
	}

	for _, s := range sources {
		if _, err := root.New(s.name).Parse(string(s.body)); err != nil {
			return nil, fmt.Errorf("parse %s: %w", s.name, err)
		}
	}

	// Override the placeholder with the real dispatcher now that the
	// template set is fully built. The closure binds `root` so any
	// named template in the set is dispatchable from any layout.
	root.Funcs(template.FuncMap{
		"tmpl": func(name string, data any) (template.HTML, error) {
			var buf bytes.Buffer
			if err := root.ExecuteTemplate(&buf, name, data); err != nil {
				return "", fmt.Errorf("tmpl %q: %w", name, err)
			}
			return template.HTML(buf.String()), nil //nolint:gosec // output is template-rendered
		},
	})

	return root, nil
}

type templateSource struct {
	name string
	body []byte
}

func embeddedSources() ([]templateSource, error) {
	var out []templateSource
	err := fs.WalkDir(embeddedFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".tmpl") {
			return nil
		}
		body, err := fs.ReadFile(embeddedFS, path)
		if err != nil {
			return err
		}
		// Strip the file extension and use the path as the template
		// name. e.g. layouts/base.tmpl -> "layouts/base".
		name := strings.TrimSuffix(path, ".tmpl")
		// Templates inside the file may define their own names via
		// {{ define "..." }}. Parsing with the file-path name registers
		// the file body itself; the defines override or coexist.
		out = append(out, templateSource{name: name, body: body})
		return nil
	})
	return out, err
}

func devDirSources(dir string) ([]templateSource, error) {
	var out []templateSource
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".tmpl") {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Normalize separators to forward slash so template names match
		// the embedded layout regardless of host OS.
		rel = filepath.ToSlash(rel)
		name := strings.TrimSuffix(rel, ".tmpl")
		out = append(out, templateSource{name: name, body: body})
		return nil
	})
	return out, err
}

// embeddedStaticFS returns the static subtree of embeddedFS, suitable
// for serving via http.FS.
func embeddedStaticFS() (fs.FS, error) {
	return fs.Sub(embeddedFS, "static")
}

// funcs returns the template func map shared across the entire set.
// The per-set `tmpl` dispatcher is bound separately in loadTemplates
// because it needs the parsed root.
func funcs() template.FuncMap {
	return template.FuncMap{
		"default":    defaultFunc,
		"dict":       dictFunc,
		"humanBytes": humanBytesFunc,
		"humanTime":  humanTimeFunc,
		"timefmt":    timefmtFunc,
		"lower":      strings.ToLower,
		"upper":      strings.ToUpper,
		"title":      titleFunc,
		"join":       strings.Join,
		"hasPrefix":  strings.HasPrefix,
		"contains":   strings.Contains,
	}
}

func defaultFunc(d any, v any) any {
	if v == nil {
		return d
	}
	if s, ok := v.(string); ok && s == "" {
		return d
	}
	return v
}

func dictFunc(args ...any) (map[string]any, error) {
	if len(args)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of args")
	}
	m := make(map[string]any, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		key, ok := args[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: non-string key at %d (%T)", i, args[i])
		}
		m[key] = args[i+1]
	}
	return m, nil
}

func humanBytesFunc(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := float64(unit), 0
	for x := float64(n) / unit; x >= unit && exp < 4; x /= unit {
		div *= unit
		exp++
	}
	suffix := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}[exp]
	return fmt.Sprintf("%.2f %s", float64(n)/div, suffix)
}

func humanTimeFunc(unixSec int64) string {
	if unixSec == 0 {
		return "never"
	}
	t := time.Unix(unixSec, 0)
	delta := time.Since(t)
	switch {
	case delta < time.Minute:
		return "just now"
	case delta < time.Hour:
		return fmt.Sprintf("%d min ago", int(delta.Minutes()))
	case delta < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(delta.Hours()))
	case delta < 7*24*time.Hour:
		return fmt.Sprintf("%d d ago", int(delta.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

func timefmtFunc(unixSec int64, layout string) string {
	if unixSec == 0 {
		return ""
	}
	return time.Unix(unixSec, 0).Format(layout)
}

func titleFunc(s string) string {
	if s == "" {
		return s
	}
	// Avoid strings.Title (deprecated). ASCII-only good-enough.
	r := []rune(s)
	for i, c := range r {
		if i == 0 || r[i-1] == ' ' || r[i-1] == '-' || r[i-1] == '_' {
			if c >= 'a' && c <= 'z' {
				r[i] = c - ('a' - 'A')
			}
		}
	}
	return string(r)
}
