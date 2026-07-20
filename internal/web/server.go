// Package web serves the browsing UI: dashboard, resource list/detail, run
// history with per-run change logs, and collector configuration. Templates
// and static assets are embedded so the binary stays self-contained.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/breed007/hrg/internal/assets"
	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/model"
	"github.com/breed007/hrg/internal/notify"
	"github.com/breed007/hrg/internal/runbook"
	"github.com/breed007/hrg/internal/schedule"
	"github.com/breed007/hrg/internal/secrets"
	"github.com/breed007/hrg/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	store *store.Store
	key   *secrets.Key
	// static collectors run on every collection regardless of DB config
	// (currently just the manual resources.d collector).
	static       []collector.Collector
	resourcesDir string
	log          *slog.Logger
	pages        map[string]*template.Template
	mux          *http.ServeMux

	// chrome is the resolved headless-browser path for PDF export, or ""
	// when none is installed. scheduler drives cron-scheduled collection.
	chrome    string
	scheduler *schedule.Scheduler
	sessions  *sessionStore
	// dev enables developer-only affordances (e.g. the fixture_dir field in
	// the collector form, an arbitrary-file-read primitive that must not be
	// exposed in a normal install).
	dev bool
}

func NewServer(st *store.Store, key *secrets.Key, static []collector.Collector, resourcesDir string, log *slog.Logger, dev bool) (*Server, error) {
	s := &Server{store: st, key: key, static: static, resourcesDir: resourcesDir, log: log, dev: dev, pages: map[string]*template.Template{}}
	s.chrome = runbook.FindChrome()
	s.scheduler = schedule.New(s.ScheduledCollect, log)
	s.sessions = newSessionStore()

	// GFM without WithUnsafe: embedded raw HTML in annotations is dropped,
	// not rendered — markdown in, markup out, nothing else.
	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	funcs := template.FuncMap{
		"json": func(v any) string {
			b, err := json.Marshal(v)
			if err != nil {
				return fmt.Sprintf("%v", v)
			}
			return string(b)
		},
		"markdown": func(src string) template.HTML {
			var buf strings.Builder
			if err := md.Convert([]byte(src), &buf); err != nil {
				return template.HTML(template.HTMLEscapeString(src))
			}
			return template.HTML(buf.String())
		},
		// dict2 builds a map from key/value pairs for parameterized partials.
		"dict2": func(pairs ...any) (map[string]any, error) {
			if len(pairs)%2 != 0 {
				return nil, fmt.Errorf("dict2: odd number of arguments")
			}
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i < len(pairs); i += 2 {
				k, ok := pairs[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict2: key %v is not a string", pairs[i])
				}
				m[k] = pairs[i+1]
			}
			return m, nil
		},
	}
	partials := []string{"templates/layout.html", "templates/run-table.html", "templates/resource-table.html", "templates/annotation-block.html"}
	for _, page := range []string{"dashboard", "resources", "resource", "runs", "run", "collectors", "collector-form", "map", "ipplan", "orphans", "runbook", "login", "settings", "setup"} {
		t, err := template.New("layout").Funcs(funcs).ParseFS(templateFS,
			append(partials, "templates/"+page+".html")...)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", page, err)
		}
		s.pages[page] = t
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /resources", s.handleResources)
	mux.HandleFunc("GET /resources/{id}", s.handleResource)
	mux.HandleFunc("GET /runs", s.handleRuns)
	mux.HandleFunc("GET /runs/{id}", s.handleRun)
	mux.HandleFunc("GET /map", s.handleMap)
	mux.HandleFunc("GET /ipplan", s.handleIPPlan)
	mux.HandleFunc("POST /resources/{id}/annotations/{field}", s.handleAnnotationSave)
	mux.HandleFunc("POST /resources/{id}/edges", s.handleEdgeCreate)
	mux.HandleFunc("POST /edges/{id}/delete", s.handleEdgeDelete)
	mux.HandleFunc("GET /orphans", s.handleOrphans)
	mux.HandleFunc("POST /orphans/{id}/reattach", s.handleOrphanReattach)
	mux.HandleFunc("POST /orphans/{id}/forget", s.handleOrphanForget)
	mux.HandleFunc("POST /collect", s.handleCollect)
	mux.HandleFunc("GET /collectors", s.handleCollectorList)
	mux.HandleFunc("GET /collectors/new", s.handleCollectorNew)
	mux.HandleFunc("POST /collectors", s.handleCollectorCreate)
	mux.HandleFunc("GET /collectors/{id}/edit", s.handleCollectorEdit)
	mux.HandleFunc("POST /collectors/{id}", s.handleCollectorUpdate)
	mux.HandleFunc("POST /collectors/{id}/delete", s.handleCollectorDelete)
	mux.HandleFunc("GET /runbook", s.handleRunbook)
	mux.HandleFunc("POST /runbook/pages/{slug}", s.handlePageSave)
	mux.HandleFunc("POST /runbook/settings", s.handleRunbookSettings)
	mux.HandleFunc("POST /runbook/generate", s.handleRunbookGenerate)
	mux.HandleFunc("GET /runbook/preview/{guide}", s.handleRunbookPreview)
	mux.HandleFunc("GET /runbook/download/{guide}/{format}", s.handleRunbookDownload)
	mux.HandleFunc("POST /resources/{id}/backup-check", s.handleBackupCheck)
	mux.HandleFunc("POST /collectors/test", s.handleCollectorTest)
	mux.HandleFunc("POST /runbook/notify-test", s.handleNotifyTest)
	// Auth, settings, setup, health, and config backup/restore.
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("POST /auth/password", s.handleSetPassword)
	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("GET /setup", s.handleSetup)
	mux.HandleFunc("POST /setup/dismiss", s.handleSetupDismiss)
	mux.HandleFunc("GET /config/export", s.handleConfigExport)
	mux.HandleFunc("POST /config/import", s.handleConfigImport)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /static/mermaid.min.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(assets.MermaidJS)
	})
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	s.mux = mux
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CSRF + auth middleware runs before every route.
	if !s.guard(w, r) {
		return
	}
	s.mux.ServeHTTP(w, r)
}

// CollectAll runs the static collectors plus every enabled configured
// instance. Config is re-read each call, so edits take effect without a
// restart. One collector failing never blocks the others.
// CollectSummary aggregates one collection cycle across all collectors.
type CollectSummary struct {
	Added, Changed, Removed int
	Warnings                int
	Failures                []string // collector instance names that failed
}

// Drifted reports whether anything changed that a watcher should hear about.
func (cs CollectSummary) Drifted() bool {
	return cs.Added > 0 || cs.Changed > 0 || cs.Removed > 0
}

// CollectAll runs every enabled collector once and returns the aggregate.
func (s *Server) CollectAll(ctx context.Context) CollectSummary {
	var agg CollectSummary
	cols := append([]collector.Collector{}, s.static...)

	cfgs, err := s.store.ListCollectorConfigs(ctx)
	if err != nil {
		s.log.Error("list collector configs", "err", err)
	}
	for _, cfg := range cfgs {
		if !cfg.Enabled {
			continue
		}
		col, err := s.buildCollector(cfg)
		if err != nil {
			s.log.Error("build collector", "collector", cfg.InstanceName(), "err", err)
			agg.Failures = append(agg.Failures, cfg.InstanceName())
			if rerr := s.store.RecordFailedRun(ctx, cfg.InstanceName(), err); rerr != nil {
				s.log.Error("record failed run", "collector", cfg.InstanceName(), "err", rerr)
			}
			continue
		}
		cols = append(cols, col)
	}

	for _, c := range cols {
		res, err := c.Collect(ctx)
		if err != nil {
			s.log.Error("collect failed", "collector", c.Name(), "err", err)
			agg.Failures = append(agg.Failures, c.Name())
			if rerr := s.store.RecordFailedRun(ctx, c.Name(), err); rerr != nil {
				s.log.Error("record failed run", "collector", c.Name(), "err", rerr)
			}
			continue
		}
		sum, err := s.store.Ingest(ctx, c.Name(), res)
		if err != nil {
			s.log.Error("ingest failed", "collector", c.Name(), "err", err)
			agg.Failures = append(agg.Failures, c.Name())
			continue
		}
		agg.Added += sum.Added
		agg.Changed += sum.Changed
		agg.Removed += sum.Removed
		agg.Warnings += len(sum.Warnings)
		s.log.Info("collected", "collector", c.Name(),
			"seen", sum.Seen, "added", sum.Added, "changed", sum.Changed, "removed", sum.Removed, "warnings", len(sum.Warnings))
		for _, wn := range sum.Warnings {
			s.log.Warn("collect warning", "collector", c.Name(), "detail", wn)
		}
	}
	return agg
}

// ScheduledCollect is the job the cron scheduler fires: collect, then on
// drift or failure send a notification, and optionally regenerate the
// runbook. Everything is best-effort — a scheduled cycle must never panic
// the process.
func (s *Server) ScheduledCollect(ctx context.Context) {
	summary := s.CollectAll(ctx)

	settings, err := s.store.Settings(ctx)
	if err != nil {
		s.log.Error("scheduled: load settings", "err", err)
		return
	}
	cfg := s.runbookConfig(settings)

	if cfg.NotifyURL != "" && (summary.Drifted() || len(summary.Failures) > 0) {
		title, body := driftMessage(summary)
		if err := notify.Send(ctx, cfg.NotifyURL, title, body); err != nil {
			s.log.Error("scheduled: notify", "err", err)
		}
	}

	if cfg.AutoExport && summary.Drifted() {
		s.log.Info("scheduled: drift detected, regenerating runbook")
		s.generateRunbook(ctx, cfg)
	}
}

func driftMessage(cs CollectSummary) (title, body string) {
	if len(cs.Failures) > 0 && !cs.Drifted() {
		return "HRG: collector failure", fmt.Sprintf("Collectors failed: %s", strings.Join(cs.Failures, ", "))
	}
	body = fmt.Sprintf("+%d new, ~%d changed, -%d gone since last run.", cs.Added, cs.Changed, cs.Removed)
	if len(cs.Failures) > 0 {
		body += fmt.Sprintf("\nCollectors that failed: %s", strings.Join(cs.Failures, ", "))
	}
	return "HRG: infrastructure drift detected", body
}

// StartScheduler applies the persisted schedule and starts the cron loop.
// Call once at startup after the store is ready. The loop is always started
// so a schedule saved later through the UI takes effect without a restart;
// an empty saved spec simply installs no entry.
func (s *Server) StartScheduler(ctx context.Context) {
	settings, err := s.store.Settings(ctx)
	if err != nil {
		s.log.Error("start scheduler: load settings", "err", err)
	} else if spec := settings[setSchedule]; spec != "" {
		if err := s.scheduler.Set(spec); err != nil {
			s.log.Error("start scheduler: invalid saved schedule", "spec", spec, "err", err)
		}
	}
	s.scheduler.Start()
	if spec := s.scheduler.Spec(); spec != "" {
		s.log.Info("scheduler started", "spec", spec, "next", s.scheduler.Next())
	}
}

// StopScheduler halts scheduled collection.
func (s *Server) StopScheduler() { s.scheduler.Stop() }

func (s *Server) buildCollector(cfg store.CollectorConfig) (collector.Collector, error) {
	secret := ""
	if len(cfg.Secret) > 0 {
		var err error
		if secret, err = s.key.Open(cfg.Secret); err != nil {
			return nil, fmt.Errorf("decrypt token (was the key file replaced?): %w", err)
		}
	}
	return collector.Build(collector.Spec{
		Type:     cfg.Type,
		Instance: cfg.InstanceName(),
		Config:   cfg.Config,
		Secret:   secret,
	})
}

func (s *Server) render(w http.ResponseWriter, page, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[page].ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render failed", "page", page, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.log.Error("request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.Stats(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	runs, err := s.store.ListRuns(r.Context(), 10)
	if err != nil {
		s.fail(w, err)
		return
	}
	cov, err := s.store.Coverage(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	startHere, err := s.store.GetPage(r.Context(), "start_here")
	if err != nil {
		s.fail(w, err)
		return
	}
	lastExport, err := s.store.LatestOKExport(r.Context(), "household-html")
	if err != nil {
		s.fail(w, err)
		return
	}
	health, err := s.store.CollectorHealthList(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "dashboard", "layout", map[string]any{
		"Title": "Dashboard", "Stats": stats, "Runs": runs,
		"Coverage":         cov,
		"PurposePct":       pct(cov.WithPurpose, cov.Annotatable),
		"RecoveryPct":      pct(cov.WithRecovery, cov.CriticalTotal),
		"BackupPct":        pct(cov.BackupJobsVerified, cov.BackupJobs),
		"StartHereWritten": startHere.BodyMD != "",
		"LastExport":       lastExport,
		"Health":           health,
		"SetupIncomplete":  !s.setupComplete(r.Context()),
	})
}

func pct(n, total int) int {
	if total == 0 {
		return 0
	}
	return n * 100 / total
}

func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	filter := store.ListFilter{
		Kind:      r.URL.Query().Get("kind"),
		Collector: r.URL.Query().Get("collector"),
		Missing:   r.URL.Query().Get("missing"),
		Query:     strings.TrimSpace(r.URL.Query().Get("q")),
	}
	resources, err := s.store.ListResources(r.Context(), filter)
	if err != nil {
		s.fail(w, err)
		return
	}
	// htmx filter changes swap just the table.
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, "resources", "resource-table", resources)
		return
	}
	stats, err := s.store.Stats(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "resources", "layout", map[string]any{
		"Title": "Resources", "Resources": resources, "Filter": filter,
		"Kinds": model.Kinds, "Collectors": stats.Collectors,
	})
}

func (s *Server) handleResource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	detail, err := s.store.GetResource(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	anns, err := s.store.GetAnnotations(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	all, err := s.store.ListResources(r.Context(), store.ListFilter{})
	if err != nil {
		s.fail(w, err)
		return
	}
	var targets []store.ResourceRow
	for _, t := range all {
		if t.ID != id {
			targets = append(targets, t)
		}
	}

	// Restore-verification state, for backup-job resources only.
	var backupVerified string
	if detail.Kind == model.KindBackupJob {
		checks, err := s.store.BackupChecks(r.Context())
		if err != nil {
			s.fail(w, err)
			return
		}
		backupVerified = checks[id]
	}

	s.render(w, "resource", "layout", map[string]any{
		"Title": detail.Name, "Resource": detail,
		"AnnBlocks": buildAnnBlocks(id, anns, r.URL.Query().Get("edit")),
		"Targets":   targets, "Relations": model.Relations,
		"IsBackupJob": detail.Kind == model.KindBackupJob, "BackupVerified": backupVerified,
	})
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListRuns(r.Context(), 100)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "runs", "layout", map[string]any{"Title": "Runs", "Runs": runs})
}

// changeView is one changed resource with its attribute-level diff.
type changeView struct {
	ID    int64
	Name  string
	Kind  string
	Diffs []keyDiff
}

type keyDiff struct {
	Key string
	Old string // JSON-rendered; "" means the key was absent
	New string
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	detail, err := s.store.GetRunDetail(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}

	changed := make([]changeView, 0, len(detail.Changed))
	for _, c := range detail.Changed {
		cv := changeView{ID: c.ID, Name: c.NewName, Kind: c.Kind}
		if c.OldName != c.NewName {
			cv.Diffs = append(cv.Diffs, keyDiff{Key: "name", Old: jsonStr(c.OldName), New: jsonStr(c.NewName)})
		}
		cv.Diffs = append(cv.Diffs, diffAttrs(c.OldAttrs, c.NewAttrs)...)
		changed = append(changed, cv)
	}

	s.render(w, "run", "layout", map[string]any{
		"Title":  fmt.Sprintf("Run #%d", detail.Run.ID),
		"Detail": detail, "Changed": changed,
	})
}

// diffAttrs returns per-key differences between two attribute maps.
func diffAttrs(old, new map[string]any) []keyDiff {
	keySet := map[string]bool{}
	for k := range old {
		keySet[k] = true
	}
	for k := range new {
		keySet[k] = true
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []keyDiff
	for _, k := range keys {
		ov, oldHas := old[k]
		nv, newHas := new[k]
		if oldHas && newHas && reflect.DeepEqual(ov, nv) {
			continue
		}
		d := keyDiff{Key: k}
		if oldHas {
			d.Old = jsonStr(ov)
		}
		if newHas {
			d.New = jsonStr(nv)
		}
		out = append(out, d)
	}
	return out
}

func jsonStr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func (s *Server) handleCollect(w http.ResponseWriter, r *http.Request) {
	s.CollectAll(r.Context())
	http.Redirect(w, r, "/runs", http.StatusSeeOther)
}

// --- Collector configuration ---

// configView adapts a stored config for the list template.
type configView struct {
	ID       int64
	Instance string
	Type     string
	Endpoint string
	Enabled  bool
}

func (s *Server) handleCollectorList(w http.ResponseWriter, r *http.Request) {
	cfgs, err := s.store.ListCollectorConfigs(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	views := make([]configView, 0, len(cfgs))
	for _, c := range cfgs {
		views = append(views, configView{
			ID: c.ID, Instance: c.InstanceName(), Type: c.Type,
			Endpoint: endpointOf(c.Config), Enabled: c.Enabled,
		})
	}
	s.render(w, "collectors", "layout", map[string]any{
		"Title": "Collectors", "Configs": views,
		"Types": collector.Types(), "ResourcesDir": s.resourcesDir,
	})
}

// endpointOf pulls a display endpoint out of config JSON without knowing
// the collector type's schema.
func endpointOf(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, key := range []string{"url", "host"} {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	if v, ok := m["fixture_dir"].(string); ok && v != "" {
		return "fixtures: " + v
	}
	return ""
}

var instanceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (s *Server) handleCollectorNew(w http.ResponseWriter, r *http.Request) {
	typ := r.URL.Query().Get("type")
	if !validType(typ) {
		http.Redirect(w, r, "/collectors", http.StatusSeeOther)
		return
	}
	s.renderCollectorForm(w, formState{IsNew: true, Type: typ, Vals: map[string]string{}})
}

// formState carries collector-form data (and re-renders it on errors).
type formState struct {
	IsNew    bool
	ID       int64
	Type     string
	Name     string
	Instance string
	Enabled  bool
	Vals     map[string]string
	Error    string
}

func (s *Server) renderCollectorForm(w http.ResponseWriter, f formState) {
	s.render(w, "collector-form", "layout", map[string]any{
		"Title": "Collectors", "IsNew": f.IsNew, "ID": f.ID, "Type": f.Type,
		"Name": f.Name, "Instance": f.Instance, "Enabled": f.Enabled,
		"Vals": f.Vals, "Error": f.Error, "Dev": s.dev,
	})
}

func (s *Server) handleCollectorCreate(w http.ResponseWriter, r *http.Request) {
	typ := r.FormValue("type")
	if !validType(typ) {
		http.Error(w, "unknown collector type", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	cfgJSON, plainSecret := s.configFromForm(r, typ)
	f := formState{IsNew: true, Type: typ, Name: name, Vals: valsFromForm(r)}

	if !instanceNameRe.MatchString(name) {
		f.Error = "Instance name must be lowercase letters, digits, and hyphens."
		s.renderCollectorForm(w, f)
		return
	}
	cfg := store.CollectorConfig{Type: typ, Name: name, Config: cfgJSON, Enabled: true}

	// Build once to fail fast on bad config before storing anything.
	if _, err := collector.Build(collector.Spec{Type: typ, Instance: cfg.InstanceName(), Config: cfgJSON, Secret: plainSecret}); err != nil {
		f.Error = err.Error()
		s.renderCollectorForm(w, f)
		return
	}

	if plainSecret != "" {
		sealed, err := s.key.Seal(plainSecret)
		if err != nil {
			s.fail(w, err)
			return
		}
		cfg.Secret = sealed
	}
	if _, err := s.store.CreateCollectorConfig(r.Context(), cfg); err != nil {
		f.Error = "Could not save — is there already a " + cfg.InstanceName() + " instance?"
		s.renderCollectorForm(w, f)
		return
	}
	http.Redirect(w, r, "/collectors", http.StatusSeeOther)
}

func (s *Server) handleCollectorEdit(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadConfig(w, r)
	if cfg == nil {
		_ = err
		return
	}
	var vals map[string]string
	if err := json.Unmarshal(cfg.Config, &jsonToStrings{&vals}); err != nil {
		s.fail(w, err)
		return
	}
	s.renderCollectorForm(w, formState{
		ID: cfg.ID, Type: cfg.Type, Name: cfg.Name, Instance: cfg.InstanceName(),
		Enabled: cfg.Enabled, Vals: vals,
	})
}

func (s *Server) handleCollectorUpdate(w http.ResponseWriter, r *http.Request) {
	cfg, _ := s.loadConfig(w, r)
	if cfg == nil {
		return
	}
	cfgJSON, plainSecret := s.configFromForm(r, cfg.Type)
	f := formState{
		ID: cfg.ID, Type: cfg.Type, Name: cfg.Name, Instance: cfg.InstanceName(),
		Enabled: r.FormValue("enabled") == "on", Vals: valsFromForm(r),
	}

	// Validate with the effective secret: the newly entered one, or the
	// stored one when the field was left blank.
	effective := plainSecret
	if effective == "" && len(cfg.Secret) > 0 {
		var err error
		if effective, err = s.key.Open(cfg.Secret); err != nil {
			s.fail(w, err)
			return
		}
	}
	if _, err := collector.Build(collector.Spec{Type: cfg.Type, Instance: cfg.InstanceName(), Config: cfgJSON, Secret: effective}); err != nil {
		f.Error = err.Error()
		s.renderCollectorForm(w, f)
		return
	}

	update := store.CollectorConfig{ID: cfg.ID, Config: cfgJSON, Enabled: f.Enabled}
	if plainSecret != "" {
		sealed, err := s.key.Seal(plainSecret)
		if err != nil {
			s.fail(w, err)
			return
		}
		update.Secret = sealed
	}
	if err := s.store.UpdateCollectorConfig(r.Context(), update); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/collectors", http.StatusSeeOther)
}

func (s *Server) handleCollectorDelete(w http.ResponseWriter, r *http.Request) {
	cfg, _ := s.loadConfig(w, r)
	if cfg == nil {
		return
	}
	if err := s.store.DeleteCollectorConfig(r.Context(), cfg.ID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/collectors", http.StatusSeeOther)
}

// loadConfig fetches the config in the {id} path segment, writing the HTTP
// error itself; a nil return means the response is already sent.
func (s *Server) loadConfig(w http.ResponseWriter, r *http.Request) (*store.CollectorConfig, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return nil, err
	}
	cfg, err := s.store.GetCollectorConfig(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return nil, err
	}
	return cfg, nil
}

func validType(typ string) bool {
	for _, t := range collector.Types() {
		if t == typ {
			return true
		}
	}
	return false
}

// configFromForm builds the non-secret config JSON for a collector type from
// submitted form fields, returning the plaintext secret separately.
func (s *Server) configFromForm(r *http.Request, typ string) (json.RawMessage, string) {
	m := map[string]any{}
	switch typ {
	case "proxmox":
		m["url"] = strings.TrimSpace(r.FormValue("url"))
		m["token_id"] = strings.TrimSpace(r.FormValue("token_id"))
		if r.FormValue("insecure_tls") == "on" {
			m["insecure_tls"] = true
		}
	case "docker":
		m["host"] = strings.TrimSpace(r.FormValue("host"))
	case "unifi":
		m["url"] = strings.TrimSpace(r.FormValue("url"))
		if site := strings.TrimSpace(r.FormValue("site")); site != "" {
			m["site"] = site
		}
		if r.FormValue("classic") == "on" {
			m["classic"] = true
		}
		if r.FormValue("insecure_tls") == "on" {
			m["insecure_tls"] = true
		}
	case "netbox":
		m["url"] = strings.TrimSpace(r.FormValue("url"))
		if r.FormValue("insecure_tls") == "on" {
			m["insecure_tls"] = true
		}
	case "adguard":
		m["url"] = strings.TrimSpace(r.FormValue("url"))
		if u := strings.TrimSpace(r.FormValue("username")); u != "" {
			m["username"] = u
		}
		if r.FormValue("insecure_tls") == "on" {
			m["insecure_tls"] = true
		}
	}
	// fixture_dir is a developer/testing affordance that can read arbitrary
	// JSON files off the server's disk. Only honor it in dev mode so it
	// can't be used as a file-disclosure primitive in a normal install.
	if s.dev {
		if fd := strings.TrimSpace(r.FormValue("fixture_dir")); fd != "" {
			m["fixture_dir"] = fd
		}
	}
	b, _ := json.Marshal(m)
	return b, r.FormValue("secret")
}

// valsFromForm echoes submitted values back into the form on error.
func valsFromForm(r *http.Request) map[string]string {
	vals := map[string]string{}
	for _, k := range []string{"url", "token_id", "host", "site", "username", "fixture_dir"} {
		vals[k] = strings.TrimSpace(r.FormValue(k))
	}
	for _, k := range []string{"insecure_tls", "classic"} {
		if r.FormValue(k) == "on" {
			vals[k] = "true"
		}
	}
	return vals
}

// jsonToStrings unmarshals arbitrary JSON object values into their string
// forms for form display.
type jsonToStrings struct {
	out *map[string]string
}

func (j *jsonToStrings) UnmarshalJSON(b []byte) error {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*j.out = map[string]string{}
	for k, v := range m {
		switch t := v.(type) {
		case string:
			(*j.out)[k] = t
		case bool:
			(*j.out)[k] = strconv.FormatBool(t)
		case float64:
			(*j.out)[k] = strconv.FormatFloat(t, 'f', -1, 64)
		}
	}
	return nil
}
