package store

import (
	"context"
	"encoding/json"
)

// CollectorHealth is the latest-run status of one collector instance.
type CollectorHealth struct {
	Collector string
	Status    string // ok | error
	When      string
	Error     string
	Summary   RunSummary
}

// CollectorHealthList returns the most recent run per collector instance,
// so the dashboard can show at a glance whether each is succeeding — a
// collector that silently started failing otherwise hides in the run log.
func (s *Store) CollectorHealthList(ctx context.Context) ([]CollectorHealth, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.collector, r.status, COALESCE(r.finished_at, r.started_at),
		       COALESCE(r.error, ''), COALESCE(r.stats, '{}')
		FROM runs r
		JOIN (SELECT collector, MAX(id) AS latest FROM runs GROUP BY collector) m
		  ON m.collector = r.collector AND m.latest = r.id
		ORDER BY r.collector`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CollectorHealth
	for rows.Next() {
		var h CollectorHealth
		var stats string
		if err := rows.Scan(&h.Collector, &h.Status, &h.When, &h.Error, &stats); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(stats), &h.Summary); err != nil {
			h.Summary = RunSummary{}
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
