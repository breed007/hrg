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

//go:embed templates/artifact.html
var artifactTmpl string

//go:embed templates/artifact.css
var artifactCSS string

// RenderOptions tunes artifact presentation without touching content.
type RenderOptions struct {
	// PaperSize is "letter" (default) or "a4". Applied via the CSS @page
	// descriptor, which Chrome honors for --print-to-pdf — no PDF-renderer
	// changes needed.
	PaperSize string
	// CustomCSS is user-supplied CSS appended after the base stylesheet, so
	// it overrides. Targets section IDs (#start-here, #contacts, #physical,
	// #network, #services, #backups, #appendix) and element classes
	// (.entry, .warn, .md, .kind, …).
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

// RenderHTML produces the single-file HTML artifact: inline CSS, inline
// Mermaid, no external references, no links back to HRG. The output is
// meant to live on a USB stick or in a printed binder.
func RenderHTML(doc *Document, opts RenderOptions) ([]byte, error) {
	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	funcs := template.FuncMap{
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
	t, err := template.New("artifact").Funcs(funcs).Parse(artifactTmpl)
	if err != nil {
		return nil, fmt.Errorf("parse artifact template: %w", err)
	}

	var out bytes.Buffer
	err = t.Execute(&out, map[string]any{
		"Doc": doc,
		"CSS": template.CSS(artifactCSS),
		// User CSS + paper size, applied after the base stylesheet.
		// template.CSS marks it trusted: this is the operator's own input,
		// same trust level as the collector tokens they enter.
		"ExtraCSS": template.CSS(opts.presentationCSS()),
		// Safe to inline: assets.MermaidJS is verified free of "</script".
		"MermaidJS": template.JS(assets.MermaidJS),
	})
	if err != nil {
		return nil, fmt.Errorf("render artifact: %w", err)
	}
	return out.Bytes(), nil
}
