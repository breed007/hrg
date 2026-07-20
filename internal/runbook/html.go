package runbook

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/breed007/hrg/internal/assets"
)

//go:embed templates/shared.html
var sharedTmpl string

//go:embed templates/household.html
var householdTmpl string

//go:embed templates/administrator.html
var administratorTmpl string

//go:embed templates/artifact.css
var artifactCSS string

// RenderOptions tunes artifact presentation without touching content.
type RenderOptions struct {
	// PaperSize is "letter" (default) or "a4". Applied via the CSS @page
	// descriptor, which Chrome honors for --print-to-pdf — no PDF-renderer
	// changes needed.
	PaperSize string
	// CustomCSS is user-supplied CSS appended after the base stylesheet, so
	// it overrides. Targets section IDs and element classes.
	CustomCSS string
}

// presentationCSS returns the paper-size rule plus the user's custom CSS,
// in override order.
func (o RenderOptions) presentationCSS() string {
	size := "letter"
	if strings.EqualFold(o.PaperSize, "a4") {
		size = "A4"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "@page { size: %s; }\n", size)
	b.WriteString(SanitizeCSS(o.CustomCSS))
	return b.String()
}

// styleBreakout matches any sequence that would close the <style> element
// the CSS is inlined into. Inside a <style> raw-text element, only "</style"
// ends it; nothing in that sequence is legitimate CSS. HTML comment markers
// are neutralized too, out of caution for older parsers.
var styleBreakout = regexp.MustCompile(`(?i)</\s*style|<!--|-->`)

// SanitizeCSS neutralizes attempts to break out of the inline <style> block
// (which would turn custom CSS into stored XSS). It only removes sequences
// that have no valid meaning in CSS, so legitimate stylesheets are
// unaffected. Applied at render time and, defensively, on save.
func SanitizeCSS(css string) string {
	return styleBreakout.ReplaceAllString(css, "")
}

func renderFuncs() template.FuncMap {
	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	return template.FuncMap{
		"markdown": func(src string) template.HTML {
			var buf strings.Builder
			if err := md.Convert([]byte(src), &buf); err != nil {
				return template.HTML(template.HTMLEscapeString(src))
			}
			return template.HTML(buf.String())
		},
		"json": func(v any) string {
			b, err := json.Marshal(v)
			if err != nil {
				return fmt.Sprintf("%v", v)
			}
			return string(b)
		},
	}
}

// RenderHTML produces one self-contained guide: inline CSS, no external
// references, no links back to the app. Both guides come from the same
// Document, so they can never disagree.
//
// Only the Administrator Guide embeds the diagram renderer — which keeps
// the Household Guide small enough to email comfortably.
func RenderHTML(doc *Document, guide Guide, opts RenderOptions) ([]byte, error) {
	body := householdTmpl
	if guide == GuideAdministrator {
		body = administratorTmpl
	}

	t, err := template.New("guide").Funcs(renderFuncs()).Parse(sharedTmpl)
	if err != nil {
		return nil, fmt.Errorf("parse shared partials: %w", err)
	}
	if _, err := t.Parse(body); err != nil {
		return nil, fmt.Errorf("parse %s template: %w", guide, err)
	}

	data := map[string]any{
		"Doc":   doc,
		"Guide": guide,
		"CSS":   template.CSS(artifactCSS),
		// User CSS + paper size, applied after the base stylesheet.
		"ExtraCSS":  template.CSS(opts.presentationCSS()),
		"MermaidJS": template.JS(""),
	}
	if guide == GuideAdministrator {
		// Safe to inline: assets.MermaidJS is verified free of "</script".
		data["MermaidJS"] = template.JS(assets.MermaidJS)
	}

	var out bytes.Buffer
	if err := t.ExecuteTemplate(&out, "guide", data); err != nil {
		return nil, fmt.Errorf("render %s guide: %w", guide, err)
	}
	return out.Bytes(), nil
}
