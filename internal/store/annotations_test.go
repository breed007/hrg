package store

import (
	"context"
	"strings"
	"testing"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/model"
)

func seedResources(t *testing.T, s *Store, ids ...string) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	var rs []model.Resource
	for _, id := range ids {
		rs = append(rs, rsrc(id, strings.ToUpper(id[:1])+id[1:], nil))
	}
	if _, err := s.Ingest(ctx, "test", collector.Result{Resources: rs}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListResources(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, r := range rows {
		out[r.SourceID] = r.ID
	}
	return out
}

func TestAnnotationCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ids := seedResources(t, s, "plex")

	if err := s.SetAnnotation(ctx, ids["plex"], "purpose", "Media server for the house."); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAnnotation(ctx, ids["plex"], "recovery", "- [ ] start NAS first\n- [ ] docker compose up"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAnnotation(ctx, ids["plex"], "bogus", "x"); err == nil {
		t.Error("unknown field accepted")
	}

	anns, err := s.GetAnnotations(ctx, ids["plex"])
	if err != nil {
		t.Fatal(err)
	}
	if len(anns) != 2 || anns["purpose"].BodyMD != "Media server for the house." {
		t.Fatalf("annotations wrong: %+v", anns)
	}

	// Update overwrites; empty body deletes.
	if err := s.SetAnnotation(ctx, ids["plex"], "purpose", "Media server."); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAnnotation(ctx, ids["plex"], "recovery", ""); err != nil {
		t.Fatal(err)
	}
	anns, _ = s.GetAnnotations(ctx, ids["plex"])
	if len(anns) != 1 || anns["purpose"].BodyMD != "Media server." {
		t.Fatalf("update/delete wrong: %+v", anns)
	}
}

func TestAnnotationsSurviveRecollection(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ids := seedResources(t, s, "plex")
	if err := s.SetAnnotation(ctx, ids["plex"], "purpose", "keep me"); err != nil {
		t.Fatal(err)
	}

	// Resource changes across two more runs; annotation must stay attached.
	if _, err := s.Ingest(ctx, "test", collector.Result{Resources: []model.Resource{
		rsrc("plex", "Plex", map[string]any{"port": 32400}),
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ingest(ctx, "test", collector.Result{Resources: []model.Resource{
		rsrc("plex", "Plex renamed", map[string]any{"port": 32401}),
	}}); err != nil {
		t.Fatal(err)
	}
	anns, _ := s.GetAnnotations(ctx, ids["plex"])
	if anns["purpose"].BodyMD != "keep me" {
		t.Fatal("annotation lost across re-collection")
	}
}

func TestManualEdges(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ids := seedResources(t, s, "plex", "nas")

	if err := s.CreateManualEdge(ctx, ids["plex"], ids["nas"], model.RelDependsOn); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateManualEdge(ctx, ids["plex"], ids["nas"], model.RelDependsOn); err == nil {
		t.Error("duplicate manual edge accepted")
	}
	if err := s.CreateManualEdge(ctx, ids["plex"], ids["plex"], model.RelDependsOn); err == nil {
		t.Error("self-edge accepted")
	}

	d, err := s.GetResource(ctx, ids["plex"])
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Edges) != 1 || d.Edges[0].Origin != "manual" || d.Edges[0].EdgeID == 0 {
		t.Fatalf("manual edge wrong: %+v", d.Edges)
	}

	if err := s.DeleteManualEdge(ctx, d.Edges[0].EdgeID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteManualEdge(ctx, d.Edges[0].EdgeID); err == nil {
		t.Error("deleting a gone edge should error")
	}
}

func TestOrphanQueueReattachForget(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ids := seedResources(t, s, "old-plex", "nas")
	if err := s.SetAnnotation(ctx, ids["old-plex"], "purpose", "media server"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAnnotation(ctx, ids["old-plex"], "note", "old note"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateManualEdge(ctx, ids["old-plex"], ids["nas"], model.RelDependsOn); err != nil {
		t.Fatal(err)
	}

	// old-plex disappears, new-plex appears.
	if _, err := s.Ingest(ctx, "test", collector.Result{Resources: []model.Resource{
		rsrc("nas", "Nas", nil),
		rsrc("new-plex", "Plex v2", nil),
	}}); err != nil {
		t.Fatal(err)
	}

	orphans, err := s.ListOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].SourceID != "old-plex" || orphans[0].Annotations != 2 {
		t.Fatalf("orphan queue wrong: %+v", orphans)
	}

	var newID int64
	rows, _ := s.ListResources(ctx, ListFilter{})
	for _, r := range rows {
		if r.SourceID == "new-plex" {
			newID = r.ID
		}
	}
	// Target already has a purpose — merge, don't overwrite.
	if err := s.SetAnnotation(ctx, newID, "purpose", "new purpose"); err != nil {
		t.Fatal(err)
	}

	if err := s.ReattachOrphan(ctx, orphans[0].ID, newID); err != nil {
		t.Fatal(err)
	}
	anns, _ := s.GetAnnotations(ctx, newID)
	if !strings.HasPrefix(anns["purpose"].BodyMD, "new purpose") || !strings.Contains(anns["purpose"].BodyMD, "media server") {
		t.Errorf("merge wrong: %q", anns["purpose"].BodyMD)
	}
	if anns["note"].BodyMD != "old note" {
		t.Errorf("plain move wrong: %+v", anns)
	}
	if left, _ := s.GetAnnotations(ctx, orphans[0].ID); len(left) != 0 {
		t.Errorf("orphan still has annotations: %+v", left)
	}
	// Manual edge re-pointed to the new resource.
	d, _ := s.GetResource(ctx, newID)
	foundDep := false
	for _, e := range d.Edges {
		if e.Origin == "manual" && e.Relation == model.RelDependsOn && e.PeerName == "Nas" {
			foundDep = true
		}
	}
	if !foundDep {
		t.Errorf("manual edge not re-pointed: %+v", d.Edges)
	}

	// Forget the orphan entirely.
	if err := s.DeleteResource(ctx, orphans[0].ID); err != nil {
		t.Fatal(err)
	}
	if orphans, _ = s.ListOrphans(ctx); len(orphans) != 0 {
		t.Errorf("orphan queue not empty after forget: %+v", orphans)
	}
	if _, err := s.GetResource(ctx, ids["old-plex"]); err != ErrNotFound {
		t.Errorf("forgotten resource still loads: %v", err)
	}
}

func TestCoverageAndMissingFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ids := seedResources(t, s, "plex", "nas", "backup-job")

	if err := s.SetAnnotation(ctx, ids["plex"], "purpose", "media"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAnnotation(ctx, ids["plex"], "recovery", "restart it"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAnnotation(ctx, ids["nas"], "credential_pointer", "1Password: NAS"); err != nil {
		t.Fatal(err)
	}

	cov, err := s.Coverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// All three are kind service (rsrc helper) → all critical.
	if cov.Annotatable != 3 || cov.WithPurpose != 1 || cov.CriticalTotal != 3 || cov.WithRecovery != 1 || cov.CredentialPointers != 1 {
		t.Errorf("coverage wrong: %+v", cov)
	}

	missing, err := s.ListResources(ctx, ListFilter{Missing: "purpose"})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 2 {
		t.Errorf("missing=purpose filter wrong: %d rows", len(missing))
	}
}

func TestBackupChecks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// rsrc() makes service-kind rows; make a real backup_job so Coverage
	// counts it.
	if _, err := s.Ingest(ctx, "test", collector.Result{Resources: []model.Resource{
		{Kind: model.KindBackupJob, SourceID: "job1", Name: "nightly"},
	}}); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	rows, _ := s.ListResources(ctx, ListFilter{})
	for _, r := range rows {
		if r.SourceID == "job1" {
			jobID = r.ID
		}
	}

	cov, _ := s.Coverage(ctx)
	if cov.BackupJobs != 1 || cov.BackupJobsVerified != 0 {
		t.Fatalf("pre-check coverage wrong: %+v", cov)
	}
	if checks, _ := s.BackupChecks(ctx); len(checks) != 0 {
		t.Errorf("expected no checks, got %v", checks)
	}

	if err := s.SetBackupCheck(ctx, jobID, "restored to scratch, booted OK"); err != nil {
		t.Fatal(err)
	}
	checks, err := s.BackupChecks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if checks[jobID] == "" {
		t.Error("backup check not recorded")
	}
	cov, _ = s.Coverage(ctx)
	if cov.BackupJobsVerified != 1 {
		t.Errorf("post-check coverage wrong: %+v", cov)
	}

	// Re-checking updates the timestamp, doesn't duplicate.
	if err := s.SetBackupCheck(ctx, jobID, "again"); err != nil {
		t.Fatal(err)
	}
	if checks, _ := s.BackupChecks(ctx); len(checks) != 1 {
		t.Errorf("re-check should upsert, got %d rows", len(checks))
	}
}

// TestHouseholdCoverage checks the second dimension: not "is it documented"
// but "would it mean anything to someone who didn't build it".
func TestHouseholdCoverage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ids := seedResources(t, s, "plex", "nas", "grafana")

	must := func(id int64, field, body string) {
		t.Helper()
		if err := s.SetAnnotation(ctx, id, field, body); err != nil {
			t.Fatal(err)
		}
	}
	must(ids["plex"], "household_importance", ImportanceEssential)
	must(ids["plex"], "plain_english", "Plays movies on the TVs.")
	must(ids["nas"], "household_importance", ImportanceEssential) // essential, unexplained
	must(ids["grafana"], "household_importance", ImportanceExperimental)
	must(ids["grafana"], "plain_english", "Graphs. Nobody needs them.")

	cov, err := s.Coverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Classified != 3 || cov.Unclassified() != 0 {
		t.Errorf("classification wrong: %+v", cov)
	}
	if cov.Essential != 2 || cov.EssentialExplained != 1 {
		t.Errorf("essential counts wrong: got %d essential, %d explained", cov.Essential, cov.EssentialExplained)
	}
	if cov.Described != 2 {
		t.Errorf("described wrong: %d", cov.Described)
	}

	missing, err := s.ListResources(ctx, ListFilter{Missing: "plain_english"})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0].SourceID != "nas" {
		t.Errorf("missing=plain_english filter wrong: %+v", missing)
	}
}

// TestImportanceVocabulary: the classification is a controlled vocabulary,
// not prose — free text here would silently break sorting and coverage.
func TestImportanceVocabulary(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := seedResources(t, s, "plex")["plex"]

	for _, v := range ImportanceValues {
		if err := s.SetAnnotation(ctx, id, "household_importance", v); err != nil {
			t.Errorf("rejected valid importance %q: %v", v, err)
		}
	}
	if err := s.SetAnnotation(ctx, id, "household_importance", "sort of important"); err == nil {
		t.Error("accepted free-text importance")
	}
	// Clearing is always allowed — it means "not classified".
	if err := s.SetAnnotation(ctx, id, "household_importance", ""); err != nil {
		t.Errorf("clearing importance failed: %v", err)
	}
	if ImportanceRank(ImportanceEssential) >= ImportanceRank(ImportanceNice) {
		t.Error("essential must sort before nice-to-have")
	}
	if ImportanceRank("") <= ImportanceRank(ImportanceExperimental) {
		t.Error("unclassified must sort last")
	}
}
