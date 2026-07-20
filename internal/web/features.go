package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/notify"
	"github.com/breed007/hrg/internal/store"
)

func urlEscape(s string) string { return url.QueryEscape(s) }

const setSetupDone = "setup_done"

// setupComplete reports whether the first-run wizard has been finished or
// dismissed. It's also considered done once the user has clearly gotten
// going (a collector configured or the START HERE page written), so the
// banner doesn't nag a returning user who never clicked "dismiss".
func (s *Server) setupComplete(ctx context.Context) bool {
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return true // don't block the UI on a settings read error
	}
	if settings[setSetupDone] == "1" {
		return true
	}
	if page, _ := s.store.GetPage(ctx, "start_here"); page.BodyMD != "" {
		return true
	}
	if cfgs, _ := s.store.ListCollectorConfigs(ctx); len(cfgs) > 0 {
		return true
	}
	return false
}

// handleSetup renders the first-run wizard: a guided checklist that gets a
// new user from an empty dashboard to a first runbook.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	settings, err := s.store.Settings(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	cfg := s.runbookConfig(settings)
	authOn, _ := s.authEnabled(ctx)
	startHere, _ := s.store.GetPage(ctx, "start_here")
	cfgs, _ := s.store.ListCollectorConfigs(ctx)
	latest, _ := s.store.LatestOKExport(ctx, "household-html")

	s.render(w, "setup", "layout", map[string]any{
		"Title": "Setup", "Cfg": cfg, "AuthOn": authOn,
		"Types":            collector.Types(),
		"StartHereWritten": startHere.BodyMD != "",
		"CollectorCount":   len(cfgs),
		"HasExport":        latest != nil,
		"Err":              r.URL.Query().Get("err"),
	})
}

func (s *Server) handleSetupDismiss(w http.ResponseWriter, r *http.Request) {
	if err := s.store.SetSetting(r.Context(), setSetupDone, "1"); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleSettings renders the settings page: access password and config
// backup/restore. (Runbook/export settings live on the Runbook page.)
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	authOn, _ := s.authEnabled(r.Context())
	s.render(w, "settings", "layout", map[string]any{
		"Title": "Settings", "AuthOn": authOn,
		"Dev": s.dev,
		"Err": r.URL.Query().Get("err"),
		"Msg": r.URL.Query().Get("msg"),
	})
}

// handleCollectorTest builds a collector from the submitted (unsaved) form
// and actually calls Collect, so the user learns a credential is wrong
// before saving — not later, buried in a scheduled run's logs. Returns an
// htmx fragment.
func (s *Server) handleCollectorTest(w http.ResponseWriter, r *http.Request) {
	typ := r.FormValue("type")
	if !validType(typ) {
		s.testResult(w, false, "Unknown collector type.")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "test"
	}
	cfgJSON, plainSecret := s.configFromForm(r, typ)

	// If editing and the secret field was left blank, use the stored secret.
	if plainSecret == "" {
		if idStr := r.FormValue("id"); idStr != "" {
			if cfg, err := s.collectorConfigByIDString(r.Context(), idStr); err == nil && len(cfg.Secret) > 0 {
				if sec, err := s.key.Open(cfg.Secret); err == nil {
					plainSecret = sec
				}
			}
		}
	}

	col, err := collector.Build(collector.Spec{Type: typ, Instance: typ + ":" + name, Config: cfgJSON, Secret: plainSecret})
	if err != nil {
		s.testResult(w, false, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	res, err := col.Collect(ctx)
	if err != nil {
		s.testResult(w, false, err.Error())
		return
	}
	msg := fmt.Sprintf("Success — reached the endpoint and read %d resource(s).", len(res.Resources))
	if len(res.Warnings) > 0 {
		msg += fmt.Sprintf(" %d warning(s).", len(res.Warnings))
	}
	s.testResult(w, true, msg)
}

func (s *Server) collectorConfigByIDString(ctx context.Context, idStr string) (*store.CollectorConfig, error) {
	var id int64
	if _, err := fmt.Sscan(idStr, &id); err != nil {
		return nil, err
	}
	return s.store.GetCollectorConfig(ctx, id)
}

func (s *Server) testResult(w http.ResponseWriter, ok bool, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	cls, label := "badge-err", "✗"
	if ok {
		cls, label = "badge-ok", "✓"
	}
	fmt.Fprintf(w, `<div class="test-result"><span class="badge %s">%s</span> %s</div>`,
		cls, label, html.EscapeString(msg))
}

// handleNotifyTest sends a test notification to the submitted (or saved)
// notify URL so the user can confirm the webhook works.
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	url := strings.TrimSpace(r.FormValue("notify_url"))
	if url == "" {
		if settings, err := s.store.Settings(r.Context()); err == nil {
			url = settings[setNotifyURL]
		}
	}
	if url == "" {
		s.testResult(w, false, "No notification URL set.")
		return
	}
	err := notify.Send(r.Context(), url, "HRG test notification",
		"This is a test from HRG. If you see this, drift alerts will reach you.")
	if err != nil {
		s.testResult(w, false, err.Error())
		return
	}
	s.testResult(w, true, "Sent — check your device.")
}

// handleConfigExport streams the full config backup as a JSON download.
func (s *Server) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	backup, err := s.store.ExportConfig(r.Context(), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		s.fail(w, err)
		return
	}
	body, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="hrg-config-backup.json"`)
	w.Write(body)
}

// handleConfigImport applies an uploaded config backup.
func (s *Server) handleConfigImport(w http.ResponseWriter, r *http.Request) {
	f, _, err := r.FormFile("backup")
	if err != nil {
		http.Redirect(w, r, "/settings?err="+urlEscape("No file uploaded."), http.StatusSeeOther)
		return
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 32<<20)) // 32 MiB cap
	if err != nil {
		s.fail(w, err)
		return
	}
	var backup store.ConfigBackup
	if err := json.Unmarshal(body, &backup); err != nil {
		http.Redirect(w, r, "/settings?err="+urlEscape("Not a valid HRG config backup: "+err.Error()), http.StatusSeeOther)
		return
	}
	warnings, err := s.store.ImportConfig(r.Context(), &backup)
	if err != nil {
		http.Redirect(w, r, "/settings?err="+urlEscape("Import failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	// Re-apply any imported schedule to the live scheduler.
	if settings, err := s.store.Settings(r.Context()); err == nil {
		_ = s.scheduler.Set(settings[setSchedule])
	}
	msg := "Config imported."
	if len(warnings) > 0 {
		msg = fmt.Sprintf("Config imported with %d item(s) skipped (run a collection, then re-import to attach annotations to resources).", len(warnings))
	}
	http.Redirect(w, r, "/settings?msg="+urlEscape(msg), http.StatusSeeOther)
}
