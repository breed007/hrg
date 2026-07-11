package runbook

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestRenderPDFNoChrome(t *testing.T) {
	// Empty chrome path must return a typed, actionable error rather than
	// shelling out to nothing.
	_, err := RenderPDF(context.Background(), "", []byte("<html></html>"))
	if err == nil {
		t.Fatal("expected error with no chrome")
	}
	var nc ErrNoChrome
	if !errors.As(err, &nc) {
		t.Errorf("want ErrNoChrome, got %T: %v", err, err)
	}
}

func TestRenderPDFWithChrome(t *testing.T) {
	// Headless Chrome is unreliable on CI runners (sandbox/display quirks
	// make --print-to-pdf hang), so this live integration test runs only
	// locally. The rest of the PDF path (ErrNoChrome, flag assembly) is
	// covered by the other tests, which don't launch a browser.
	if os.Getenv("CI") != "" {
		t.Skip("skipping live-Chrome PDF render on CI")
	}
	chrome := FindChrome()
	if chrome == "" {
		t.Skip("no Chrome/Chromium installed")
	}
	doc := seedDoc(t)
	html, err := RenderHTML(doc, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := RenderPDF(context.Background(), chrome, html)
	if err != nil {
		t.Fatal(err)
	}
	// A real PDF starts with %PDF- and is non-trivial in size.
	if len(pdf) < 1000 || string(pdf[:5]) != "%PDF-" {
		t.Errorf("output is not a PDF: %d bytes, prefix %q", len(pdf), pdf[:min(5, len(pdf))])
	}
}

func TestRenderOptions(t *testing.T) {
	doc := seedDoc(t)

	// Paper size maps to the CSS @page descriptor; custom CSS is appended
	// verbatim after the base stylesheet (so it overrides).
	out, err := RenderHTML(doc, RenderOptions{PaperSize: "a4", CustomCSS: "h2 { color: rebeccapurple; }"})
	if err != nil {
		t.Fatal(err)
	}
	html := string(out)
	if !strings.Contains(html, "@page { size: A4; }") {
		t.Error("A4 paper size not injected")
	}
	if !strings.Contains(html, "h2 { color: rebeccapurple; }") {
		t.Error("custom CSS not injected")
	}
	// Custom CSS must appear AFTER the base stylesheet to win the cascade.
	base := strings.Index(html, ".starthere") // a base-CSS selector
	custom := strings.Index(html, "rebeccapurple")
	if base == -1 || custom == -1 || custom < base {
		t.Errorf("custom CSS should follow base CSS: base=%d custom=%d", base, custom)
	}

	// Default is US Letter.
	def, _ := RenderHTML(doc, RenderOptions{})
	if !strings.Contains(string(def), "@page { size: letter; }") {
		t.Error("default paper size should be letter")
	}
}

// seedDoc builds a Document via the shared store seed for renderer tests.
func seedDoc(t *testing.T) *Document {
	t.Helper()
	st := seed(t)
	doc, err := Build(context.Background(), st, "Test Runbook")
	if err != nil {
		t.Fatal(err)
	}
	return doc
}
