package runbook

import (
	"strings"
	"testing"
)

func TestSanitizeCSSNeutralizesBreakout(t *testing.T) {
	cases := []string{
		`</style><script>alert(1)</script><style>`,
		`</STYLE ><script>x</script>`,
		`a{}</ style>b`,
		`<!-- --> h1{color:red}`,
	}
	for _, in := range cases {
		out := SanitizeCSS(in)
		if strings.Contains(strings.ToLower(out), "</style") ||
			strings.Contains(out, "<!--") || strings.Contains(out, "-->") {
			t.Errorf("breakout survived sanitize: %q -> %q", in, out)
		}
	}
}

func TestSanitizeCSSLeavesValidCSSAlone(t *testing.T) {
	valid := `body { font-family: sans-serif; } .a > .b { color: #0a5; } #appendix { display: none; }`
	if got := SanitizeCSS(valid); got != valid {
		t.Errorf("valid CSS was altered:\n in: %q\nout: %q", valid, got)
	}
}

func TestSanitizeAppliedInRender(t *testing.T) {
	// End to end: a breakout payload in CustomCSS must not appear in the
	// rendered artifact.
	css := `</style><script>document.title='x'</script>`
	out := RenderOptions{CustomCSS: css}.presentationCSS()
	if strings.Contains(strings.ToLower(out), "</style") {
		t.Errorf("render did not sanitize custom CSS: %q", out)
	}
}
