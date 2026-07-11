package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/breed007/hrg/internal/model"
)

// ResourceRow is a resource joined with its current version, as the UI
// consumes it.
type ResourceRow struct {
	ID        int64
	Collector string
	SourceID  string
	Kind      model.Kind
	Name      string
	Attrs     map[string]any
	Orphaned  bool
}

// ListFilter narrows ListResources. Zero values mean "no filter".
type ListFilter struct {
	Kind      string
	Collector string
	// Missing filters to resources lacking this annotation field.
	Missing string
	// Query is a case-insensitive substring match on name or source id.
	Query string
}

// ListResources returns all resources with their current version, orphans
// included (flagged), ordered by kind then name.
func (s *Store) ListResources(ctx context.Context, f ListFilter) ([]ResourceRow, error) {
	like := "%" + f.Query + "%"
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.collector, r.source_id, r.kind, v.name, v.attrs,
		       r.last_seen_run < COALESCE(
		           (SELECT MAX(id) FROM runs WHERE collector = r.collector AND status = 'ok'),
		           r.last_seen_run) AS orphaned
		FROM resources r
		JOIN resource_versions v ON v.resource_id = r.id AND v.valid_to_run IS NULL
		WHERE (?1 = '' OR r.kind = ?1) AND (?2 = '' OR r.collector = ?2)
		  AND (?3 = '' OR NOT EXISTS (
		      SELECT 1 FROM annotations a WHERE a.resource_id = r.id AND a.field = ?3))
		  AND (?4 = '' OR v.name LIKE ?5 COLLATE NOCASE OR r.source_id LIKE ?5 COLLATE NOCASE)
		ORDER BY r.kind, v.name COLLATE NOCASE`,
		f.Kind, f.Collector, f.Missing, f.Query, like)
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

func scanResourceRow(rows *sql.Rows) (ResourceRow, error) {
	var r ResourceRow
	var attrs string
	if err := rows.Scan(&r.ID, &r.Collector, &r.SourceID, &r.Kind, &r.Name, &attrs, &r.Orphaned); err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(attrs), &r.Attrs); err != nil {
		return r, fmt.Errorf("resource %d: corrupt attrs: %w", r.ID, err)
	}
	return r, nil
}

// EdgeRow is an edge with both endpoints' display data resolved.
type EdgeRow struct {
	EdgeID   int64
	Relation model.Relation
	Origin   string
	PeerID   int64
	PeerName string
	PeerKind model.Kind
	Outbound bool // true: this resource -> peer
}

// VersionRow is one historical version of a resource.
type VersionRow struct {
	Name         string
	Attrs        map[string]any
	ValidFromRun int64
	ValidToRun   *int64 // nil = current
}

// ResourceDetail is everything the detail page shows.
type ResourceDetail struct {
	ResourceRow
	FirstSeenRun int64
	LastSeenRun  int64
	Edges        []EdgeRow
	Versions     []VersionRow
}

// ErrNotFound is returned when a looked-up entity does not exist.
var ErrNotFound = errors.New("not found")

// GetResource loads one resource with its full version history and edges.
func (s *Store) GetResource(ctx context.Context, id int64) (*ResourceDetail, error) {
	var d ResourceDetail
	var attrs string
	err := s.db.QueryRowContext(ctx, `
		SELECT r.id, r.collector, r.source_id, r.kind, v.name, v.attrs,
		       r.first_seen_run, r.last_seen_run,
		       r.last_seen_run < COALESCE(
		           (SELECT MAX(id) FROM runs WHERE collector = r.collector AND status = 'ok'),
		           r.last_seen_run) AS orphaned
		FROM resources r
		JOIN resource_versions v ON v.resource_id = r.id AND v.valid_to_run IS NULL
		WHERE r.id = ?`, id).
		Scan(&d.ID, &d.Collector, &d.SourceID, &d.Kind, &d.Name, &attrs,
			&d.FirstSeenRun, &d.LastSeenRun, &d.Orphaned)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(attrs), &d.Attrs); err != nil {
		return nil, fmt.Errorf("resource %d: corrupt attrs: %w", id, err)
	}

	erows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.relation, e.origin, p.id, pv.name, p.kind, e.src_id = ?1 AS outbound
		FROM edges e
		JOIN resources p ON p.id = CASE WHEN e.src_id = ?1 THEN e.dst_id ELSE e.src_id END
		JOIN resource_versions pv ON pv.resource_id = p.id AND pv.valid_to_run IS NULL
		WHERE e.src_id = ?1 OR e.dst_id = ?1
		ORDER BY outbound DESC, e.relation`, id)
	if err != nil {
		return nil, err
	}
	defer erows.Close()
	for erows.Next() {
		var e EdgeRow
		if err := erows.Scan(&e.EdgeID, &e.Relation, &e.Origin, &e.PeerID, &e.PeerName, &e.PeerKind, &e.Outbound); err != nil {
			return nil, err
		}
		d.Edges = append(d.Edges, e)
	}
	if err := erows.Err(); err != nil {
		return nil, err
	}

	vrows, err := s.db.QueryContext(ctx, `
		SELECT name, attrs, valid_from_run, valid_to_run
		FROM resource_versions WHERE resource_id = ?
		ORDER BY valid_from_run DESC`, id)
	if err != nil {
		return nil, err
	}
	defer vrows.Close()
	for vrows.Next() {
		var v VersionRow
		var vattrs string
		if err := vrows.Scan(&v.Name, &vattrs, &v.ValidFromRun, &v.ValidToRun); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(vattrs), &v.Attrs); err != nil {
			return nil, fmt.Errorf("resource %d: corrupt version attrs: %w", id, err)
		}
		d.Versions = append(d.Versions, v)
	}
	return &d, vrows.Err()
}

// RunRow is one collector run for the history view.
type RunRow struct {
	ID         int64
	Collector  string
	StartedAt  string
	FinishedAt string
	Status     string
	Error      string
	Summary    RunSummary
}

// ListRuns returns the most recent runs, newest first.
func (s *Store) ListRuns(ctx context.Context, limit int) ([]RunRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, collector, started_at, COALESCE(finished_at, ''), status,
		       COALESCE(error, ''), COALESCE(stats, '{}')
		FROM runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RunRow
	for rows.Next() {
		var r RunRow
		var stats string
		if err := rows.Scan(&r.ID, &r.Collector, &r.StartedAt, &r.FinishedAt, &r.Status, &r.Error, &stats); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(stats), &r.Summary); err != nil {
			r.Summary = RunSummary{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EdgePair is a resolved edge by resource row id, for graph rendering.
type EdgePair struct {
	SrcID    int64
	DstID    int64
	Relation model.Relation
}

// ListEdges returns every edge in the graph.
func (s *Store) ListEdges(ctx context.Context) ([]EdgePair, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT src_id, dst_id, relation FROM edges`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EdgePair
	for rows.Next() {
		var e EdgePair
		if err := rows.Scan(&e.SrcID, &e.DstID, &e.Relation); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// KindCount is one slice of the dashboard's inventory breakdown.
type KindCount struct {
	Kind  model.Kind
	Count int
}

// Stats feeds the dashboard.
type Stats struct {
	TotalResources int
	OrphanCount    int
	ByKind         []KindCount
	Collectors     []string
}

func (s *Store) Stats(ctx context.Context) (*Stats, error) {
	var st Stats
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.kind, COUNT(*),
		       SUM(r.last_seen_run < COALESCE(
		           (SELECT MAX(id) FROM runs WHERE collector = r.collector AND status = 'ok'),
		           r.last_seen_run))
		FROM resources r GROUP BY r.kind ORDER BY COUNT(*) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kc KindCount
		var orphans int
		if err := rows.Scan(&kc.Kind, &kc.Count, &orphans); err != nil {
			return nil, err
		}
		st.ByKind = append(st.ByKind, kc)
		st.TotalResources += kc.Count
		st.OrphanCount += orphans
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	crows, err := s.db.QueryContext(ctx, `SELECT DISTINCT collector FROM resources ORDER BY collector`)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var c string
		if err := crows.Scan(&c); err != nil {
			return nil, err
		}
		st.Collectors = append(st.Collectors, c)
	}
	return &st, crows.Err()
}
