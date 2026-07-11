package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/model"
)

// dbtx is satisfied by both *sql.DB and *sql.Tx.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// RunSummary reports what one ingest changed.
type RunSummary struct {
	RunID   int64 `json:"run_id"`
	Seen    int   `json:"seen"`
	Added   int   `json:"added"`
	Changed int   `json:"changed"`
	// Removed counts resources that were present in this collector's
	// previous successful run but absent now (newly orphaned).
	Removed int `json:"removed"`
	// Warnings are non-fatal problems the collector reported (a skipped
	// bad file, a dropped edge). The run still succeeds; these surface in
	// the UI so the user knows something needs attention.
	Warnings []string `json:"warnings,omitempty"`
}

// Ingest atomically records one collector observation: it diffs the result
// against current state, opens/closes resource versions, updates identity
// bookkeeping, resolves edges, and writes the run row. On error nothing is
// recorded (use RecordFailedRun for collector failures).
func (s *Store) Ingest(ctx context.Context, collectorName string, res collector.Result) (RunSummary, error) {
	var sum RunSummary

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sum, err
	}
	defer tx.Rollback()

	prevRun, err := latestOKRun(ctx, tx, collectorName)
	if err != nil {
		return sum, fmt.Errorf("find previous run: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	r, err := tx.ExecContext(ctx,
		`INSERT INTO runs (collector, started_at, status) VALUES (?, ?, 'ok')`,
		collectorName, now)
	if err != nil {
		return sum, fmt.Errorf("create run: %w", err)
	}
	runID, err := r.LastInsertId()
	if err != nil {
		return sum, err
	}
	sum.RunID = runID

	seen := make(map[string]bool, len(res.Resources))
	for _, rsrc := range res.Resources {
		if err := rsrc.Validate(); err != nil {
			return sum, fmt.Errorf("collector %s: %w", collectorName, err)
		}
		if seen[rsrc.SourceID] {
			return sum, fmt.Errorf("collector %s: duplicate source id %q", collectorName, rsrc.SourceID)
		}
		seen[rsrc.SourceID] = true

		if err := s.ingestResource(ctx, tx, collectorName, runID, rsrc, &sum); err != nil {
			return sum, err
		}
	}
	sum.Seen = len(res.Resources)
	sum.Warnings = res.Warnings

	if prevRun != 0 {
		// Newly orphaned: last seen exactly at the previous successful run.
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM resources
			WHERE collector = ? AND last_seen_run = ?`,
			collectorName, prevRun).Scan(&sum.Removed); err != nil {
			return sum, fmt.Errorf("count removed: %w", err)
		}
	}

	for _, e := range res.Edges {
		if err := e.Validate(); err != nil {
			return sum, fmt.Errorf("collector %s: %w", collectorName, err)
		}
		src, dst := e.Src, e.Dst
		if src.Collector == "" {
			src.Collector = collectorName
		}
		if dst.Collector == "" {
			dst.Collector = collectorName
		}
		if err := upsertEdge(ctx, tx, src, dst, e.Relation, collectorName, runID); err != nil {
			return sum, err
		}
	}

	if err := resolvePendingEdges(ctx, tx); err != nil {
		return sum, err
	}

	stats, _ := json.Marshal(sum)
	if _, err := tx.ExecContext(ctx, `
		UPDATE runs SET finished_at = ?, stats = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), string(stats), runID); err != nil {
		return sum, fmt.Errorf("finalize run: %w", err)
	}

	return sum, tx.Commit()
}

func (s *Store) ingestResource(ctx context.Context, tx dbtx, collectorName string, runID int64, rsrc model.Resource, sum *RunSummary) error {
	hash, err := rsrc.ContentHash()
	if err != nil {
		return fmt.Errorf("collector %s: %w", collectorName, err)
	}
	attrs, err := json.Marshal(rsrc.Attrs)
	if err != nil {
		return fmt.Errorf("collector %s: resource %q: %w", collectorName, rsrc.SourceID, err)
	}

	var resID int64
	var curHash, curName string
	err = tx.QueryRowContext(ctx, `
		SELECT r.id, COALESCE(v.content_hash, ''), COALESCE(v.name, '')
		FROM resources r
		LEFT JOIN resource_versions v ON v.resource_id = r.id AND v.valid_to_run IS NULL
		WHERE r.collector = ? AND r.source_id = ?`,
		collectorName, rsrc.SourceID).Scan(&resID, &curHash, &curName)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		ins, err := tx.ExecContext(ctx, `
			INSERT INTO resources (collector, source_id, kind, first_seen_run, last_seen_run)
			VALUES (?, ?, ?, ?, ?)`,
			collectorName, rsrc.SourceID, rsrc.Kind, runID, runID)
		if err != nil {
			return fmt.Errorf("insert resource %q: %w", rsrc.SourceID, err)
		}
		if resID, err = ins.LastInsertId(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO resource_versions (resource_id, name, attrs, content_hash, valid_from_run)
			VALUES (?, ?, ?, ?, ?)`,
			resID, rsrc.Name, string(attrs), hash, runID); err != nil {
			return fmt.Errorf("insert version for %q: %w", rsrc.SourceID, err)
		}
		sum.Added++

	case err != nil:
		return fmt.Errorf("look up resource %q: %w", rsrc.SourceID, err)

	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE resources SET last_seen_run = ?, kind = ? WHERE id = ?`,
			runID, rsrc.Kind, resID); err != nil {
			return fmt.Errorf("touch resource %q: %w", rsrc.SourceID, err)
		}
		if curHash != hash {
			if _, err := tx.ExecContext(ctx, `
				UPDATE resource_versions SET valid_to_run = ?
				WHERE resource_id = ? AND valid_to_run IS NULL`,
				runID, resID); err != nil {
				return fmt.Errorf("close version for %q: %w", rsrc.SourceID, err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO resource_versions (resource_id, name, attrs, content_hash, valid_from_run)
				VALUES (?, ?, ?, ?, ?)`,
				resID, rsrc.Name, string(attrs), hash, runID); err != nil {
				return fmt.Errorf("insert version for %q: %w", rsrc.SourceID, err)
			}
			sum.Changed++
		}
	}
	return nil
}

// RecordFailedRun writes a run row for a collector whose Collect call failed,
// so failures are visible in run history and don't silently orphan resources
// (orphan detection keys off the latest *successful* run).
func (s *Store) RecordFailedRun(ctx context.Context, collectorName string, collectErr error) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (collector, started_at, finished_at, status, error)
		VALUES (?, ?, ?, 'error', ?)`,
		collectorName, now, now, collectErr.Error())
	return err
}

func upsertEdge(ctx context.Context, tx dbtx, src, dst model.Ref, rel model.Relation, collectorName string, runID int64) error {
	srcID, srcOK, err := resourceID(ctx, tx, src)
	if err != nil {
		return err
	}
	dstID, dstOK, err := resourceID(ctx, tx, dst)
	if err != nil {
		return err
	}
	if !srcOK || !dstOK {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pending_edges (src_collector, src_source_id, dst_collector, dst_source_id, relation, origin, collector, seen_run)
			VALUES (?, ?, ?, ?, ?, 'discovered', ?, ?)
			ON CONFLICT DO UPDATE SET seen_run = excluded.seen_run`,
			src.Collector, src.SourceID, dst.Collector, dst.SourceID, rel, collectorName, runID); err != nil {
			return fmt.Errorf("park pending edge %s -> %s: %w", src, dst, err)
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO edges (src_id, dst_id, relation, origin, collector, first_seen_run, last_seen_run)
		VALUES (?, ?, ?, 'discovered', ?, ?, ?)
		ON CONFLICT (src_id, dst_id, relation) DO UPDATE SET last_seen_run = excluded.last_seen_run`,
		srcID, dstID, rel, collectorName, runID, runID); err != nil {
		return fmt.Errorf("upsert edge %s -> %s: %w", src, dst, err)
	}
	return nil
}

// resolvePendingEdges promotes parked cross-collector edges whose endpoints
// have both since appeared.
func resolvePendingEdges(ctx context.Context, tx dbtx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT p.id, s.id, d.id, p.relation, p.origin, p.collector, p.seen_run
		FROM pending_edges p
		JOIN resources s ON s.collector = p.src_collector AND s.source_id = p.src_source_id
		JOIN resources d ON d.collector = p.dst_collector AND d.source_id = p.dst_source_id`)
	if err != nil {
		return fmt.Errorf("scan pending edges: %w", err)
	}
	type resolved struct {
		pendingID, srcID, dstID, seenRun int64
		relation, origin                 string
		collector                        sql.NullString
	}
	var found []resolved
	for rows.Next() {
		var r resolved
		if err := rows.Scan(&r.pendingID, &r.srcID, &r.dstID, &r.relation, &r.origin, &r.collector, &r.seenRun); err != nil {
			rows.Close()
			return err
		}
		found = append(found, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, r := range found {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO edges (src_id, dst_id, relation, origin, collector, first_seen_run, last_seen_run)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (src_id, dst_id, relation) DO UPDATE SET last_seen_run = excluded.last_seen_run`,
			r.srcID, r.dstID, r.relation, r.origin, r.collector, r.seenRun, r.seenRun); err != nil {
			return fmt.Errorf("promote pending edge: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_edges WHERE id = ?`, r.pendingID); err != nil {
			return err
		}
	}
	return nil
}

func resourceID(ctx context.Context, tx dbtx, ref model.Ref) (int64, bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM resources WHERE collector = ? AND source_id = ?`,
		ref.Collector, ref.SourceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("resolve ref %s: %w", ref, err)
	}
	return id, true, nil
}
