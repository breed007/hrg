package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ChangedResource pairs a resource with the version boundary a run created.
type ChangedResource struct {
	ID       int64
	SourceID string
	Kind     string
	OldName  string
	NewName  string
	OldAttrs map[string]any
	NewAttrs map[string]any
}

// RunDetail is one run plus everything it changed.
//
// Added/Changed are exact for any historical run (version boundaries are
// permanent). Removed is derived from last_seen_run, so it is exact for a
// collector's most recent run but degrades for older ones if a resource
// later returned — good enough for "what changed since last week".
type RunDetail struct {
	Run     RunRow
	Added   []ResourceRow
	Changed []ChangedResource
	Removed []ResourceRow
}

// GetRun returns one run row.
func (s *Store) GetRun(ctx context.Context, id int64) (*RunRow, error) {
	var r RunRow
	var stats string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, collector, started_at, COALESCE(finished_at, ''), status,
		       COALESCE(error, ''), COALESCE(stats, '{}')
		FROM runs WHERE id = ?`, id).
		Scan(&r.ID, &r.Collector, &r.StartedAt, &r.FinishedAt, &r.Status, &r.Error, &stats)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(stats), &r.Summary); err != nil {
		r.Summary = RunSummary{}
	}
	return &r, nil
}

// GetRunDetail loads a run and the resource-level diff it produced.
func (s *Store) GetRunDetail(ctx context.Context, runID int64) (*RunDetail, error) {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	d := &RunDetail{Run: *run}

	// Added: identities born in this run.
	d.Added, err = s.listResourcesWhere(ctx, `
		r.first_seen_run = ?1 AND v.valid_from_run = ?1`, runID)
	if err != nil {
		return nil, fmt.Errorf("run %d added: %w", runID, err)
	}

	// Changed: a version boundary at this run on a pre-existing resource.
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.source_id, r.kind, old.name, new.name, old.attrs, new.attrs
		FROM resources r
		JOIN resource_versions new ON new.resource_id = r.id AND new.valid_from_run = ?1
		JOIN resource_versions old ON old.resource_id = r.id AND old.valid_to_run = ?1
		WHERE r.first_seen_run != ?1
		ORDER BY r.kind, new.name COLLATE NOCASE`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c ChangedResource
		var oldAttrs, newAttrs string
		if err := rows.Scan(&c.ID, &c.SourceID, &c.Kind, &c.OldName, &c.NewName, &oldAttrs, &newAttrs); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(oldAttrs), &c.OldAttrs); err != nil {
			return nil, fmt.Errorf("resource %d: corrupt old attrs: %w", c.ID, err)
		}
		if err := json.Unmarshal([]byte(newAttrs), &c.NewAttrs); err != nil {
			return nil, fmt.Errorf("resource %d: corrupt new attrs: %w", c.ID, err)
		}
		d.Changed = append(d.Changed, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Removed: present in the previous successful run, absent in this one.
	if run.Status == "ok" {
		var prev sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `
			SELECT MAX(id) FROM runs
			WHERE collector = ? AND status = 'ok' AND id < ?`,
			run.Collector, runID).Scan(&prev); err != nil {
			return nil, err
		}
		if prev.Valid {
			d.Removed, err = s.listResourcesWhere(ctx, `
				r.collector = ?2 AND r.last_seen_run = ?1 AND v.valid_to_run IS NULL`,
				prev.Int64, run.Collector)
			if err != nil {
				return nil, fmt.Errorf("run %d removed: %w", runID, err)
			}
		}
	}

	return d, nil
}

// listResourcesWhere fetches resources joined with a version, filtered by a
// caller-supplied predicate over aliases r (resources) and v (versions).
func (s *Store) listResourcesWhere(ctx context.Context, where string, args ...any) ([]ResourceRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.collector, r.source_id, r.kind, v.name, v.attrs,
		       r.last_seen_run < COALESCE(
		           (SELECT MAX(id) FROM runs WHERE collector = r.collector AND status = 'ok'),
		           r.last_seen_run) AS orphaned
		FROM resources r
		JOIN resource_versions v ON v.resource_id = r.id
		WHERE `+where+`
		ORDER BY r.kind, v.name COLLATE NOCASE`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ResourceRow
	for rows.Next() {
		r, err := scanResourceRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
