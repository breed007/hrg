package store

import (
	"context"
	"testing"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/model"
)

func TestConfigBackupRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := newTestStore(t)

	// Populate a realistic slice of non-regenerable state.
	if _, err := src.Ingest(ctx, "test", collector.Result{Resources: []model.Resource{
		rsrc("plex", "Plex", nil),
		{Kind: model.KindBackupJob, SourceID: "job1", Name: "nightly"},
	}}); err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	rows, _ := src.ListResources(ctx, ListFilter{})
	for _, r := range rows {
		ids[r.SourceID] = r.ID
	}
	if err := src.SetAnnotation(ctx, ids["plex"], "purpose", "media server"); err != nil {
		t.Fatal(err)
	}
	if err := src.CreateManualEdge(ctx, ids["plex"], ids["job1"], model.RelBackedUpBy); err != nil {
		t.Fatal(err)
	}
	if err := src.SetBackupCheck(ctx, ids["job1"], "restored ok"); err != nil {
		t.Fatal(err)
	}
	if err := src.SetPage(ctx, "start_here", "# do this"); err != nil {
		t.Fatal(err)
	}
	if err := src.SetSetting(ctx, "runbook_title", "My Homelab"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.CreateCollectorConfig(ctx, CollectorConfig{
		Type: "proxmox", Name: "pve1", Config: []byte(`{"url":"https://pve1"}`),
		Secret: []byte("sealed"), Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	backup, err := src.ExportConfig(ctx, "2026-07-11T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(backup.Collectors) != 1 || len(backup.Annotations) != 1 ||
		len(backup.ManualEdges) != 1 || len(backup.BackupChecks) != 1 || len(backup.Pages) != 1 {
		t.Fatalf("export incomplete: %+v", backup)
	}
	if string(backup.Collectors[0].Secret) != "sealed" {
		t.Error("sealed secret not exported")
	}

	// Import into a FRESH store that already has the resources collected
	// (simulating: restore config after a rebuild).
	dst := newTestStore(t)
	if _, err := dst.Ingest(ctx, "test", collector.Result{Resources: []model.Resource{
		rsrc("plex", "Plex", nil),
		{Kind: model.KindBackupJob, SourceID: "job1", Name: "nightly"},
	}}); err != nil {
		t.Fatal(err)
	}
	warnings, err := dst.ImportConfig(ctx, backup)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings when resources present: %v", warnings)
	}

	// Verify everything landed.
	settings, _ := dst.Settings(ctx)
	if settings["runbook_title"] != "My Homelab" {
		t.Error("setting not restored")
	}
	if p, _ := dst.GetPage(ctx, "start_here"); p.BodyMD != "# do this" {
		t.Error("page not restored")
	}
	cfgs, _ := dst.ListCollectorConfigs(ctx)
	if len(cfgs) != 1 || string(cfgs[0].Secret) != "sealed" {
		t.Errorf("collector config not restored: %+v", cfgs)
	}
	dids := map[string]int64{}
	drows, _ := dst.ListResources(ctx, ListFilter{})
	for _, r := range drows {
		dids[r.SourceID] = r.ID
	}
	anns, _ := dst.GetAnnotations(ctx, dids["plex"])
	if anns["purpose"].BodyMD != "media server" {
		t.Error("annotation not restored")
	}
	checks, _ := dst.BackupChecks(ctx)
	if checks[dids["job1"]] == "" {
		t.Error("backup check not restored")
	}
}

// Importing onto an empty store (no resources yet) should warn, not fail.
func TestImportWarnsWhenResourcesMissing(t *testing.T) {
	ctx := context.Background()
	backup := &ConfigBackup{
		Annotations: []AnnotationDump{{Collector: "test", SourceID: "ghost", Field: "purpose", BodyMD: "x"}},
	}
	dst := newTestStore(t)
	warnings, err := dst.ImportConfig(ctx, backup)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 {
		t.Errorf("expected 1 warning for missing resource, got %v", warnings)
	}
}
