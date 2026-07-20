package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func rsrc(id, name string, attrs map[string]any) model.Resource {
	return model.Resource{Kind: model.KindService, SourceID: id, Name: name, Attrs: attrs}
}

func TestIngestLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Run 1: two new resources.
	sum, err := s.Ingest(ctx, "test", collector.Result{Resources: []model.Resource{
		rsrc("plex", "Plex", map[string]any{"port": 32400}),
		rsrc("nas", "NAS", nil),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Added != 2 || sum.Changed != 0 || sum.Removed != 0 {
		t.Fatalf("run 1: want added=2, got %+v", sum)
	}

	// Run 2: identical — nothing changes, no new versions.
	sum, err = s.Ingest(ctx, "test", collector.Result{Resources: []model.Resource{
		rsrc("plex", "Plex", map[string]any{"port": 32400}),
		rsrc("nas", "NAS", nil),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Added != 0 || sum.Changed != 0 || sum.Removed != 0 {
		t.Fatalf("run 2: want no-op, got %+v", sum)
	}

	// Run 3: plex attrs change, nas disappears.
	sum, err = s.Ingest(ctx, "test", collector.Result{Resources: []model.Resource{
		rsrc("plex", "Plex", map[string]any{"port": 32401}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Changed != 1 || sum.Removed != 1 || sum.Added != 0 {
		t.Fatalf("run 3: want changed=1 removed=1, got %+v", sum)
	}

	rows, err := s.ListResources(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 resources (orphan retained), got %d", len(rows))
	}
	for _, r := range rows {
		switch r.SourceID {
		case "nas":
			if !r.Orphaned {
				t.Error("nas should be orphaned")
			}
		case "plex":
			if r.Orphaned {
				t.Error("plex should not be orphaned")
			}
			if r.Attrs["port"] != float64(32401) {
				t.Errorf("plex attrs not updated: %v", r.Attrs)
			}
		}
	}

	// Version history: plex has 2 versions, current one open.
	var plexID int64
	for _, r := range rows {
		if r.SourceID == "plex" {
			plexID = r.ID
		}
	}
	d, err := s.GetResource(ctx, plexID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Versions) != 2 {
		t.Fatalf("want 2 versions of plex, got %d", len(d.Versions))
	}
	if d.Versions[0].ValidToRun != nil {
		t.Error("newest version should be open (valid_to_run NULL)")
	}
	if d.Versions[1].ValidToRun == nil {
		t.Error("older version should be closed")
	}

	// Run 4: nas returns — identity (and future annotations) reattach.
	sum, err = s.Ingest(ctx, "test", collector.Result{Resources: []model.Resource{
		rsrc("plex", "Plex", map[string]any{"port": 32401}),
		rsrc("nas", "NAS", nil),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Added != 0 {
		t.Fatalf("returning resource must reuse identity, got %+v", sum)
	}
	rows, _ = s.ListResources(ctx, ListFilter{})
	for _, r := range rows {
		if r.Orphaned {
			t.Errorf("%s still orphaned after return", r.SourceID)
		}
	}
}

func TestIngestRejectsDuplicateSourceIDs(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Ingest(context.Background(), "test", collector.Result{Resources: []model.Resource{
		rsrc("x", "One", nil),
		rsrc("x", "Two", nil),
	}})
	if err == nil {
		t.Fatal("duplicate source ids in one result must be rejected")
	}
}

func TestEdgesAndPendingResolution(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Collector A emits an edge to a resource collector B hasn't reported yet.
	_, err := s.Ingest(ctx, "a", collector.Result{
		Resources: []model.Resource{rsrc("app", "App", nil)},
		Edges: []model.Edge{{
			Src:      model.Ref{SourceID: "app"},
			Dst:      model.Ref{Collector: "b", SourceID: "db"},
			Relation: model.RelDependsOn,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	rows, _ := s.ListResources(ctx, ListFilter{Collector: "a"})
	d, err := s.GetResource(ctx, rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Edges) != 0 {
		t.Fatalf("edge should be pending, got %d edges", len(d.Edges))
	}

	// Collector B reports; the pending edge must resolve on its ingest.
	_, err = s.Ingest(ctx, "b", collector.Result{
		Resources: []model.Resource{{Kind: model.KindStorage, SourceID: "db", Name: "DB"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err = s.GetResource(ctx, rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Edges) != 1 {
		t.Fatalf("pending edge not resolved, got %d edges", len(d.Edges))
	}
	e := d.Edges[0]
	if e.Relation != model.RelDependsOn || !e.Outbound || e.PeerName != "DB" {
		t.Errorf("resolved edge wrong: %+v", e)
	}

	// The DB side sees the same edge inbound.
	brows, _ := s.ListResources(ctx, ListFilter{Collector: "b"})
	bd, err := s.GetResource(ctx, brows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bd.Edges) != 1 || bd.Edges[0].Outbound {
		t.Errorf("inbound edge wrong: %+v", bd.Edges)
	}
}

func TestFailedRunDoesNotOrphan(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Ingest(ctx, "test", collector.Result{Resources: []model.Resource{
		rsrc("plex", "Plex", nil),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFailedRun(ctx, "test", context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListResources(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Orphaned {
		t.Error("a failed run must not orphan resources")
	}

	runs, err := s.ListRuns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Status != "error" {
		t.Errorf("failed run not recorded: %+v", runs)
	}
}

// TestMigrateV4toV5 pins the riskiest part of adding the household fields:
// SQLite cannot widen a CHECK constraint in place, so v5 rebuilds the
// annotations table. Existing notes must come through untouched.
func TestMigrateV4toV5(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")

	// Stand up a database at exactly schema v4.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for v := 0; v < 4; v++ {
		if _, err := db.Exec(migrations[v]); err != nil {
			t.Fatalf("apply v%d: %v", v+1, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO runs (id, collector, started_at, finished_at, status)
		  VALUES (1, 'test', 't', 't', 'ok');
		INSERT INTO resources (id, collector, source_id, kind, first_seen_run, last_seen_run)
		  VALUES (1, 'test', 'plex', 'service', 1, 1);
		INSERT INTO resource_versions (resource_id, valid_from_run, name, attrs, content_hash)
		  VALUES (1, 1, 'Plex', '{}', 'h');
		INSERT INTO annotations (resource_id, field, body_md, updated_at)
		  VALUES (1, 'purpose', 'media server', '2026-01-01T00:00:00Z');`); err != nil {
		t.Fatalf("seed v4 data: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: migrate() should carry the row across the table rebuild.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrate to v5: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	anns, err := s.GetAnnotations(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := anns["purpose"].BodyMD; got != "media server" {
		t.Errorf("pre-existing annotation lost in v5 rebuild: %q", got)
	}
	if anns["purpose"].UpdatedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("updated_at not preserved: %q", anns["purpose"].UpdatedAt)
	}
	// And the widened CHECK now admits the household fields.
	if err := s.SetAnnotation(ctx, 1, "plain_english", "Plays movies."); err != nil {
		t.Errorf("household field rejected after migration: %v", err)
	}
}
