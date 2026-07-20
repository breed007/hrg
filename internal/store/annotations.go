package store

import (
	"context"
	"fmt"
	"time"

	"github.com/breed007/hrg/internal/model"
)

// Annotation fields come in two sets, one per reader. They share a table so
// they inherit identity keying, orphan reattach, and config backup — but
// they are deliberately separate fields: the sentence that explains a NAS to
// a partner is not the sentence that explains it to an administrator, and
// collapsing them would force one text to serve both.
var (
	// HouseholdFields are written for the people who live here.
	HouseholdFields = []string{"plain_english", "household_importance", "safe_to_off", "monthly_cost"}
	// AdminFields are written for whoever does the technical work.
	AdminFields = []string{"purpose", "recovery", "credential_pointer", "note"}
)

// AnnotationFields are all valid slots, mirroring the schema CHECK.
var AnnotationFields = append(append([]string{}, HouseholdFields...), AdminFields...)

// Importance is how much the household would miss a thing — the survivor's
// triage key, and something no collector can infer.
const (
	ImportanceEssential    = "essential"
	ImportanceNice         = "nice"
	ImportanceExperimental = "experimental"
)

// ImportanceValues lists the vocabulary in descending order of "keep this".
var ImportanceValues = []string{ImportanceEssential, ImportanceNice, ImportanceExperimental}

// ImportanceRank orders resources for the household guide: the things the
// house actually needs come first. Unclassified sorts last — it's a gap.
func ImportanceRank(v string) int {
	switch v {
	case ImportanceEssential:
		return 0
	case ImportanceNice:
		return 1
	case ImportanceExperimental:
		return 2
	default:
		return 3
	}
}

func ValidImportance(v string) bool {
	for _, x := range ImportanceValues {
		if v == x {
			return true
		}
	}
	return false
}

func validAnnotationField(f string) bool {
	for _, v := range AnnotationFields {
		if f == v {
			return true
		}
	}
	return false
}

// Annotation is one typed markdown note attached to a resource identity.
type Annotation struct {
	Field     string
	BodyMD    string
	UpdatedAt string
}

// GetAnnotations returns a resource's annotations keyed by field.
func (s *Store) GetAnnotations(ctx context.Context, resourceID int64) (map[string]Annotation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT field, body_md, updated_at FROM annotations WHERE resource_id = ?`, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]Annotation{}
	for rows.Next() {
		var a Annotation
		if err := rows.Scan(&a.Field, &a.BodyMD, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out[a.Field] = a
	}
	return out, rows.Err()
}

// SetAnnotation stores one field's markdown body. An empty body deletes the
// annotation — the UI's "clear the textarea to remove" contract.
func (s *Store) SetAnnotation(ctx context.Context, resourceID int64, field, body string) error {
	if !validAnnotationField(field) {
		return fmt.Errorf("unknown annotation field %q", field)
	}
	// Importance is a classification, not prose — keep the vocabulary tight
	// so it can be counted and sorted on.
	if field == "household_importance" && body != "" && !ValidImportance(body) {
		return fmt.Errorf("household importance must be one of %v, got %q", ImportanceValues, body)
	}
	if body == "" {
		_, err := s.db.ExecContext(ctx,
			`DELETE FROM annotations WHERE resource_id = ? AND field = ?`, resourceID, field)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO annotations (resource_id, field, body_md, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (resource_id, field) DO UPDATE SET body_md = excluded.body_md, updated_at = excluded.updated_at`,
		resourceID, field, body, time.Now().UTC().Format(time.RFC3339))
	return err
}

// ReattachOrphan moves an orphan's knowledge to another resource: its
// annotations (merged, never overwriting, when the target already has the
// field) and its manual edges. The orphan row itself stays — use
// DeleteResource afterwards if the old record should go too.
func (s *Store) ReattachOrphan(ctx context.Context, fromID, toID int64) error {
	if fromID == toID {
		return fmt.Errorf("cannot reattach a resource to itself")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Annotations: move where the target slot is empty, merge where not.
	rows, err := tx.QueryContext(ctx, `
		SELECT field, body_md FROM annotations WHERE resource_id = ?`, fromID)
	if err != nil {
		return err
	}
	type ann struct{ field, body string }
	var anns []ann
	for rows.Next() {
		var a ann
		if err := rows.Scan(&a.field, &a.body); err != nil {
			rows.Close()
			return err
		}
		anns = append(anns, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for _, a := range anns {
		res, err := tx.ExecContext(ctx, `
			UPDATE annotations SET resource_id = ?1
			WHERE resource_id = ?2 AND field = ?3
			  AND NOT EXISTS (SELECT 1 FROM annotations t WHERE t.resource_id = ?1 AND t.field = ?3)`,
			toID, fromID, a.field)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Target already has this field — append rather than lose notes.
			if _, err := tx.ExecContext(ctx, `
				UPDATE annotations
				SET body_md = body_md || char(10) || char(10) || '---' || char(10) || char(10) || ?, updated_at = ?
				WHERE resource_id = ? AND field = ?`,
				a.body, now, toID, a.field); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM annotations WHERE resource_id = ? AND field = ?`, fromID, a.field); err != nil {
				return err
			}
		}
	}

	// Manual edges: re-point to the target, dropping any that would
	// duplicate an existing edge.
	for _, col := range []string{"src_id", "dst_id"} {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			INSERT OR IGNORE INTO edges (src_id, dst_id, relation, origin, collector, first_seen_run, last_seen_run)
			SELECT CASE WHEN src_id = ?1 THEN ?2 ELSE src_id END,
			       CASE WHEN dst_id = ?1 THEN ?2 ELSE dst_id END,
			       relation, origin, collector, first_seen_run, last_seen_run
			FROM edges WHERE %s = ?1 AND origin = 'manual'`, col),
			fromID, toID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`DELETE FROM edges WHERE %s = ?1 AND origin = 'manual'`, col), fromID); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// DeleteResource permanently removes a resource, its versions, edges, and
// annotations. Meant for the orphan queue's "forget" action — the caller is
// responsible for confirming intent.
func (s *Store) DeleteResource(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var collectorName, sourceID string
	if err := tx.QueryRowContext(ctx,
		`SELECT collector, source_id FROM resources WHERE id = ?`, id).
		Scan(&collectorName, &sourceID); err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM annotations WHERE resource_id = ?1`,
		`DELETE FROM backup_checks WHERE resource_id = ?1`,
		`DELETE FROM edges WHERE src_id = ?1 OR dst_id = ?1`,
		`DELETE FROM resource_versions WHERE resource_id = ?1`,
		`DELETE FROM resources WHERE id = ?1`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	// Parked cross-collector edges pointing at this identity would recreate
	// dangling references forever; clear them too.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM pending_edges
		WHERE (src_collector = ?1 AND src_source_id = ?2)
		   OR (dst_collector = ?1 AND dst_source_id = ?2)`,
		collectorName, sourceID); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateManualEdge records a human-asserted relationship.
func (s *Store) CreateManualEdge(ctx context.Context, srcID, dstID int64, rel model.Relation) error {
	if !rel.Valid() {
		return fmt.Errorf("unknown relation %q", rel)
	}
	if srcID == dstID {
		return fmt.Errorf("a resource cannot relate to itself")
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO edges (src_id, dst_id, relation, origin)
		VALUES (?, ?, ?, 'manual')`, srcID, dstID, rel)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("that relationship already exists")
	}
	return nil
}

// DeleteManualEdge removes a manual edge. Discovered edges are collector
// facts and cannot be deleted by hand — they'd only come back next run.
func (s *Store) DeleteManualEdge(ctx context.Context, edgeID int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM edges WHERE id = ? AND origin = 'manual'`, edgeID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such manual edge")
	}
	return nil
}

// OrphanRow is one entry in the orphan review queue.
type OrphanRow struct {
	ResourceRow
	LastSeenAt  string
	Annotations int
}

// ListOrphans returns resources absent from their collector's latest
// successful run, most recently seen first.
func (s *Store) ListOrphans(ctx context.Context) ([]OrphanRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.collector, r.source_id, r.kind, v.name, v.attrs,
		       COALESCE(runs.finished_at, runs.started_at),
		       (SELECT COUNT(*) FROM annotations a WHERE a.resource_id = r.id)
		FROM resources r
		JOIN resource_versions v ON v.resource_id = r.id AND v.valid_to_run IS NULL
		JOIN runs ON runs.id = r.last_seen_run
		WHERE r.last_seen_run < (SELECT MAX(id) FROM runs WHERE collector = r.collector AND status = 'ok')
		ORDER BY r.last_seen_run DESC, v.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OrphanRow
	for rows.Next() {
		var o OrphanRow
		var attrs string
		if err := rows.Scan(&o.ID, &o.Collector, &o.SourceID, &o.Kind, &o.Name, &attrs,
			&o.LastSeenAt, &o.Annotations); err != nil {
			return nil, err
		}
		o.Orphaned = true
		out = append(out, o)
	}
	return out, rows.Err()
}

// RecoveryKinds are the kinds a runbook must have recovery steps for —
// things that run and can therefore be down. Devices are included because
// the modem in the basement closet is exactly what a stressed non-expert
// will be power-cycling.
var RecoveryKinds = []model.Kind{
	model.KindHost, model.KindDevice, model.KindVM, model.KindLXC, model.KindContainer, model.KindService,
}

// Coverage measures how much of the inventory is actually documented, in
// two dimensions — because a resource can be perfectly documented for an
// administrator and still be meaningless to the person who lives here.
type Coverage struct {
	Annotatable int // live (non-orphaned) resources

	// Administrator readiness.
	WithPurpose        int
	CriticalTotal      int // live resources of RecoveryKinds
	WithRecovery       int
	CredentialPointers int
	BackupJobs         int // live backup-job resources
	BackupJobsVerified int // …with a recorded restore test

	// Household readiness.
	Classified         int // live resources with an importance set
	Essential          int // …marked essential
	EssentialExplained int // …essential AND described in plain English
	Described          int // live resources with a plain-English description
}

// Unclassified counts resources nobody has marked essential or not — a
// survivor can't tell what matters from what's a hobby project.
func (c Coverage) Unclassified() int { return c.Annotatable - c.Classified }

func (s *Store) Coverage(ctx context.Context) (*Coverage, error) {
	var c Coverage
	const live = `r.last_seen_run >= COALESCE(
		(SELECT MAX(id) FROM runs WHERE collector = r.collector AND status = 'ok'), r.last_seen_run)`

	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(EXISTS(SELECT 1 FROM annotations a WHERE a.resource_id = r.id AND a.field = 'purpose')), 0)
		FROM resources r WHERE `+live).Scan(&c.Annotatable, &c.WithPurpose)
	if err != nil {
		return nil, err
	}

	kindSet := "'host','device','vm','lxc','container','service'"
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(EXISTS(SELECT 1 FROM annotations a WHERE a.resource_id = r.id AND a.field = 'recovery')), 0)
		FROM resources r WHERE r.kind IN (`+kindSet+`) AND `+live).Scan(&c.CriticalTotal, &c.WithRecovery)
	if err != nil {
		return nil, err
	}

	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM annotations WHERE field = 'credential_pointer'`).Scan(&c.CredentialPointers)
	if err != nil {
		return nil, err
	}

	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(EXISTS(SELECT 1 FROM backup_checks b WHERE b.resource_id = r.id)), 0)
		FROM resources r WHERE r.kind = 'backup_job' AND `+live).
		Scan(&c.BackupJobs, &c.BackupJobsVerified)
	if err != nil {
		return nil, err
	}

	// Household dimension: is this house legible to someone who didn't
	// build it? "Essential things explained in plain English" is the number
	// that actually predicts whether the Household Guide is usable.
	err = s.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(imp.body_md IS NOT NULL), 0),
		  COALESCE(SUM(imp.body_md = 'essential'), 0),
		  COALESCE(SUM(imp.body_md = 'essential' AND pe.body_md IS NOT NULL), 0),
		  COALESCE(SUM(pe.body_md IS NOT NULL), 0)
		FROM resources r
		LEFT JOIN annotations imp ON imp.resource_id = r.id AND imp.field = 'household_importance'
		LEFT JOIN annotations pe  ON pe.resource_id  = r.id AND pe.field  = 'plain_english'
		WHERE `+live).
		Scan(&c.Classified, &c.Essential, &c.EssentialExplained, &c.Described)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// SetBackupCheck records that a restore test was performed now.
func (s *Store) SetBackupCheck(ctx context.Context, resourceID int64, note string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO backup_checks (resource_id, verified_at, note) VALUES (?, ?, ?)
		ON CONFLICT (resource_id) DO UPDATE SET verified_at = excluded.verified_at, note = excluded.note`,
		resourceID, time.Now().UTC().Format(time.RFC3339), note)
	return err
}

// BackupChecks returns resource id → verified_at for every recorded
// restore test.
func (s *Store) BackupChecks(ctx context.Context) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT resource_id, verified_at FROM backup_checks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var at string
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at
	}
	return out, rows.Err()
}
