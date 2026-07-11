// Package assets holds static files shared between the web UI and the
// runbook artifact renderer. The artifact must be self-contained, so
// anything it needs ships embedded in the binary and is inlined at
// generation time.
package assets

import _ "embed"

// MermaidJS renders topology diagrams. Served to the web UI at
// /static/mermaid.min.js and inlined verbatim into HTML artifacts.
// Verified free of "</script" sequences, so inlining is safe.
//
//go:embed mermaid.min.js
var MermaidJS []byte
