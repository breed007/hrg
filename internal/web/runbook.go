package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/breed007/hrg/internal/model"
	"github.com/breed007/hrg/internal/runbook"
	"github.com/breed007/hrg/internal/schedule"
	"github.com/breed007/hrg/internal/store"
)

// Settings keys for the export, scheduling, and notification pipeline.
const (
	setExportDir  = "export_dir"
	setGitCommit  = "git_commit"
	setTitle      = "runbook_title"
	setPDF        = "export_pdf"
	setSchedule   = "schedule"
	setAutoExport = "auto_export"
	setNotifyURL  = "notify_url"
	setPaperSize  = "paper_size"
	setTheme      = "theme"
	setCustomCSS  = "custom_css"
)

// themePresets are the built-in, safe styling options — an alternative to
// hand-writing CSS. Each snippet is prepended to the artifact's stylesheet
// (before any custom CSS). "default" changes nothing.
var themePresets = map[string]string{
	"default": "",
	"compact": `body { font-size: 14px; line-height: 1.4; } h2 { margin-top: 1.6rem; padding-top: .5rem; } .entry { padding: .5rem .8rem; margin: .5rem 0; }`,
	"mono":    `body { font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace; } h1,h2,h3,h4 { font-family: inherit; }`,
	"serif":   `body { font-family: "Iowan Old Style", "Palatino Linotype", Palatino, Georgia, serif; } h1,h2,h3,h4,.toc,table,.kind { font-family: inherit; }`,
}

// ThemeNames lists the presets in display order.
var ThemeNames = []string{"default", "compact", "mono", "serif"}

func validTheme(t string) bool {
	_, ok := themePresets[t]
	return ok
}

// runbookConfig is the resolved export/schedule/notify configuration.
type runbookConfig struct {
	ExportDir  string
	Title      string
	GitCommit  bool
	PDF        bool
	Schedule   string
	AutoExport bool
	NotifyURL  string
	PaperSize  string // "letter" | "a4"
	Theme      string
	CustomCSS  string
}

// renderOptions maps the presentation settings onto the runbook renderer.
// The theme preset is prepended so the user's custom CSS still overrides it.
func (c runbookConfig) renderOptions() runbook.RenderOptions {
	css := themePresets[c.Theme]
	if c.CustomCSS != "" {
		css = css + "\n" + c.CustomCSS
	}
	return runbook.RenderOptions{PaperSize: c.PaperSize, CustomCSS: css}
}

func (s *Server) runbookConfig(settings map[string]string) runbookConfig {
	c := runbookConfig{
		ExportDir:  settings[setExportDir],
		Title:      settings[setTitle],
		GitCommit:  settings[setGitCommit] == "1",
		PDF:        settings[setPDF] == "1",
		Schedule:   settings[setSchedule],
		AutoExport: settings[setAutoExport] == "1",
		NotifyURL:  settings[setNotifyURL],
		PaperSize:  settings[setPaperSize],
		Theme:      settings[setTheme],
		CustomCSS:  settings[setCustomCSS],
	}
	if c.ExportDir == "" {
		c.ExportDir = "exports"
	}
	if c.Title == "" {
		c.Title = "Homelab Runbook"
	}
	if c.PaperSize == "" {
		c.PaperSize = "letter"
	}
	if !validTheme(c.Theme) {
		c.Theme = "default"
	}
	return c
}

// startHereSkeleton pre-fills the editor so the author starts from a
// decision tree, not a blank page.
const startHereSkeleton = `# START HERE

*(Written for someone who is not the network admin. Stay calm — most fixes are power cycles.)*

## The internet is down
1. Go to **[WHERE THE MODEM IS]**.
2. TODO: what to power-cycle, in what order, and how long to wait.
3. Still down after 15 minutes? It's probably the provider — see the contacts page.

## The TV / streaming doesn't work
1. Is the internet working on your phone (WiFi on)? If not, see above.
2. TODO: which box runs the media server, and how to restart it.

## Something is beeping
- TODO: the UPS location and what beeping means (usually: power is out, battery is carrying things).

## WiFi shows connected but nothing loads
- TODO: which box does DNS, and how to restart it.

## When to give up and call someone
- TODO: who to call, and what to say.
`

const contactsSkeleton = `# Emergency contacts & accounts

| What | Who / where | Notes |
|---|---|---|
| ISP support | TODO phone | account number hint: … |
| Password vault | TODO (e.g. 1Password family) | who has emergency access: … |
| Domain registrar | TODO | |
| The person to call | TODO | |

**Where the money lives:** TODO — which card/account pays for the ISP, domains, cloud backups.
`

func (s *Server) handleRunbook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	startHere, err := s.store.GetPage(ctx, "start_here")
	if err != nil {
		s.fail(w, err)
		return
	}
	contacts, err := s.store.GetPage(ctx, "contacts")
	if err != nil {
		s.fail(w, err)
		return
	}
	settings, err := s.store.Settings(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	cfg := s.runbookConfig(settings)
	exports, err := s.store.ListExports(ctx, 20)
	if err != nil {
		s.fail(w, err)
		return
	}
	latest, err := s.store.LatestOKExport(ctx, "html")
	if err != nil {
		s.fail(w, err)
		return
	}
	latestPDF, err := s.store.LatestOKExport(ctx, "pdf")
	if err != nil {
		s.fail(w, err)
		return
	}

	edit := r.URL.Query().Get("edit")
	pageView := func(p store.Page, skeleton string) map[string]any {
		body := p.BodyMD
		if body == "" {
			body = skeleton // pre-fill the editor, never a blank page
		}
		return map[string]any{
			"Slug": p.Slug, "Body": p.BodyMD, "EditBody": body,
			"UpdatedAt": p.UpdatedAt, "Editing": edit == p.Slug,
		}
	}

	s.render(w, "runbook", "layout", map[string]any{
		"Title":     "Runbook",
		"StartHere": pageView(startHere, startHereSkeleton),
		"Contacts":  pageView(contacts, contactsSkeleton),
		"Cfg":       cfg,
		"HasChrome": s.chrome != "",
		"Themes":    ThemeNames,
		"Schedule":  s.scheduler.Spec(), "NextRun": s.scheduler.Next(),
		"Exports": exports, "Latest": latest, "LatestPDF": latestPDF,
		"Err": r.URL.Query().Get("err"),
	})
}

func (s *Server) handlePageSave(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	body := strings.TrimSpace(strings.ReplaceAll(r.FormValue("body"), "\r\n", "\n"))
	if err := s.store.SetPage(r.Context(), slug, body); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/runbook", http.StatusSeeOther)
}

func (s *Server) handleRunbookSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	checkbox := func(name string) string {
		if r.FormValue(name) == "on" {
			return "1"
		}
		return "0"
	}
	spec := strings.TrimSpace(r.FormValue("schedule"))
	// Reject an invalid or too-frequent schedule before persisting: a
	// schedule that silently never fires — or one that hammers your gear
	// every second — is worse than a rejected form.
	if err := schedule.Validate(spec, time.Now()); err != nil {
		s.renderRunbookError(w, r, err.Error())
		return
	}
	if err := s.scheduler.Set(spec); err != nil {
		s.renderRunbookError(w, r, err.Error())
		return
	}

	paper := r.FormValue("paper_size")
	if paper != "a4" {
		paper = "letter"
	}
	theme := r.FormValue("theme")
	if !validTheme(theme) {
		theme = "default"
	}
	// Sanitize custom CSS on save (defense in depth; the renderer sanitizes
	// again) so a </style> breakout can't be stored as XSS.
	css := runbook.SanitizeCSS(strings.ReplaceAll(r.FormValue("custom_css"), "\r\n", "\n"))
	vals := map[string]string{
		setExportDir:  strings.TrimSpace(r.FormValue("export_dir")),
		setTitle:      strings.TrimSpace(r.FormValue("title")),
		setNotifyURL:  strings.TrimSpace(r.FormValue("notify_url")),
		setSchedule:   spec,
		setGitCommit:  checkbox("git_commit"),
		setPDF:        checkbox("export_pdf"),
		setAutoExport: checkbox("auto_export"),
		setPaperSize:  paper,
		setTheme:      theme,
		setCustomCSS:  css,
	}
	for k, v := range vals {
		if err := s.store.SetSetting(ctx, k, v); err != nil {
			s.fail(w, err)
			return
		}
	}
	http.Redirect(w, r, "/runbook", http.StatusSeeOther)
}

// renderRunbookError re-renders the runbook page with an error banner
// (used when a settings save is rejected, e.g. a bad cron spec).
func (s *Server) renderRunbookError(w http.ResponseWriter, r *http.Request, msg string) {
	s.log.Warn("runbook settings rejected", "err", msg)
	http.Redirect(w, r, "/runbook?err="+url.QueryEscape(msg), http.StatusSeeOther)
}

// handleRunbookPreview streams a freshly built artifact — exactly what an
// export would write, without touching disk.
func (s *Server) handleRunbookPreview(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	cfg := s.runbookConfig(settings)
	doc, err := runbook.Build(r.Context(), s.store, cfg.Title)
	if err != nil {
		s.fail(w, err)
		return
	}
	out, err := runbook.RenderHTML(doc, cfg.renderOptions())
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(out)
}

func (s *Server) handleRunbookGenerate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	settings, err := s.store.Settings(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	cfg := s.runbookConfig(settings)
	s.generateRunbook(ctx, cfg)
	http.Redirect(w, r, "/runbook", http.StatusSeeOther)
}

// generateRunbook writes every configured export format and records each
// attempt. Shared by the HTTP handler and the scheduler, so it never
// touches the response — it logs and records instead. A failure in one
// format never blocks the others.
func (s *Server) generateRunbook(ctx context.Context, cfg runbookConfig) {
	doc, err := runbook.Build(ctx, s.store, cfg.Title)
	if err != nil {
		s.log.Error("build runbook", "err", err)
		return
	}

	record := func(format, path, status, detail string) {
		if err := s.store.RecordExport(ctx, store.Export{Format: format, Path: path, Status: status, Detail: detail}); err != nil {
			s.log.Error("record export", "err", err)
		}
	}

	if err := os.MkdirAll(cfg.ExportDir, 0o755); err != nil {
		s.log.Error("create export dir", "err", err)
		return
	}

	// HTML artifact. 0600 everywhere: an export is a map of the network.
	htmlPath := filepath.Join(cfg.ExportDir, "runbook.html")
	htmlOut, err := runbook.RenderHTML(doc, cfg.renderOptions())
	if err != nil {
		record("html", htmlPath, "error", err.Error())
	} else if err := os.WriteFile(htmlPath, htmlOut, 0o600); err != nil {
		record("html", htmlPath, "error", err.Error())
	} else {
		record("html", htmlPath, "ok", fmt.Sprintf("%d KiB", len(htmlOut)/1024))
	}

	// PDF, rendered from the same HTML via headless Chrome (optional).
	if cfg.PDF {
		pdfPath := filepath.Join(cfg.ExportDir, "runbook.pdf")
		switch {
		case htmlOut == nil:
			record("pdf", pdfPath, "error", "HTML render failed; skipped PDF")
		default:
			pdf, err := runbook.RenderPDF(ctx, s.chrome, htmlOut)
			if err != nil {
				record("pdf", pdfPath, "error", err.Error())
			} else if err := os.WriteFile(pdfPath, pdf, 0o600); err != nil {
				record("pdf", pdfPath, "error", err.Error())
			} else {
				record("pdf", pdfPath, "ok", fmt.Sprintf("%d KiB", len(pdf)/1024))
			}
		}
	}

	// Markdown tree (+ optional git commit).
	mdDir := filepath.Join(cfg.ExportDir, "runbook-md")
	files := runbook.RenderMarkdown(doc)
	if err := runbook.WriteTree(mdDir, files); err != nil {
		record("markdown", mdDir, "error", err.Error())
	} else {
		detail := fmt.Sprintf("%d files", len(files))
		if cfg.GitCommit {
			msg, err := runbook.CommitTree(ctx, mdDir)
			if err != nil {
				detail += " · git: " + err.Error()
			} else {
				detail += " · git: " + msg
			}
		}
		record("markdown", mdDir, "ok", detail)
	}
}

// handleRunbookDownload serves the most recently exported artifact of the
// requested format ("html" or "pdf").
func (s *Server) handleRunbookDownload(w http.ResponseWriter, r *http.Request) {
	format := r.PathValue("format")
	contentType := "text/html; charset=utf-8"
	if format == "pdf" {
		contentType = "application/pdf"
	} else {
		format = "html"
	}
	latest, err := s.store.LatestOKExport(r.Context(), format)
	if err != nil {
		s.fail(w, err)
		return
	}
	if latest == nil {
		http.Error(w, "no "+format+" export yet — generate the runbook first", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="runbook.`+format+`"`)
	http.ServeFile(w, r, latest.Path)
}

// handleBackupCheck records a restore-test verification for a backup job.
func (s *Server) handleBackupCheck(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	// Validate existence and kind before writing — a restore test only means
	// something on a backup job, and this avoids a raw 500 on a bad id.
	detail, err := s.store.GetResource(r.Context(), id)
	if err != nil {
		s.fail(w, err) // ErrNotFound → 404
		return
	}
	if detail.Kind != model.KindBackupJob {
		http.Error(w, "not a backup job", http.StatusBadRequest)
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if err := s.store.SetBackupCheck(r.Context(), id, note); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/resources/%d", id), http.StatusSeeOther)
}
