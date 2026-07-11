package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/breed007/hrg/internal/model"
)

// ConfigBackup is HRG's non-regenerable state: everything a human authored
// or configured, which a fresh collection cannot recreate. Collected
// resources/versions/runs are intentionally excluded — they rebuild on the
// next collect. Annotations, manual edges, and backup checks are keyed by
// resource *identity* (collector + source id) so they survive a rebuild.
//
// The backup contains sealed collector tokens and the password hash: it is
// as sensitive as the database and only restorable alongside the same key
// file. Treat it accordingly.
type ConfigBackup struct {
	Version      int                   `json:"version"`
	GeneratedAt  string                `json:"generated_at"`
	Settings     map[string]string     `json:"settings"`
	Pages        []Page                `json:"pages"`
	Collectors   []CollectorConfigDump `json:"collectors"`
	Annotations  []AnnotationDump      `json:"annotations"`
	ManualEdges  []ManualEdgeDump      `json:"manual_edges"`
	BackupChecks []BackupCheckDump     `json:"backup_checks"`
}

type CollectorConfigDump struct {
	Type    string          `json:"type"`
	Name    string          `json:"name"`
	Config  json.RawMessage `json:"config"`
	Secret  []byte          `json:"secret,omitempty"` // sealed; JSON base64-encodes it
	Enabled bool            `json:"enabled"`
}

type AnnotationDump struct {
	Collector string `json:"collector"`
	SourceID  string `json:"source_id"`
	Field     string `json:"field"`
	BodyMD    string `json:"body_md"`
}

type ManualEdgeDump struct {
	SrcCollector string `json:"src_collector"`
	SrcSourceID  string `json:"src_source_id"`
	DstCollector string `json:"dst_collector"`
	DstSourceID  string `json:"dst_source_id"`
	Relation     string `json:"relation"`
}

type BackupCheckDump struct {
	Collector  string `json:"collector"`
	SourceID   string `json:"source_id"`
	VerifiedAt string `json:"verified_at"`
	Note       string `json:"note"`
}

// ExportConfig gathers the full non-regenerable state for backup.
func (s *Store) ExportConfig(ctx context.Context, generatedAt string) (*ConfigBackup, error) {
	b := &ConfigBackup{Version: 1, GeneratedAt: generatedAt}

	var err error
	if b.Settings, err = s.Settings(ctx); err != nil {
		return nil, err
	}
	for _, slug := range PageSlugs {
		p, err := s.GetPage(ctx, slug)
		if err != nil {
			return nil, err
		}
		if p.BodyMD != "" {
			b.Pages = append(b.Pages, p)
		}
	}
	cfgs, err := s.ListCollectorConfigs(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range cfgs {
		b.Collectors = append(b.Collectors, CollectorConfigDump{
			Type: c.Type, Name: c.Name, Config: c.Config, Secret: c.Secret, Enabled: c.Enabled,
		})
	}

	// Annotations, manual edges, backup checks — joined to identity.
	if err := s.dumpRows(ctx, `
		SELECT r.collector, r.source_id, a.field, a.body_md
		FROM annotations a JOIN resources r ON r.id = a.resource_id
		ORDER BY r.collector, r.source_id, a.field`,
		func(rows *sql.Rows) error {
			var a AnnotationDump
			if err := rows.Scan(&a.Collector, &a.SourceID, &a.Field, &a.BodyMD); err != nil {
				return err
			}
			b.Annotations = append(b.Annotations, a)
			return nil
		}); err != nil {
		return nil, err
	}

	if err := s.dumpRows(ctx, `
		SELECT s.collector, s.source_id, d.collector, d.source_id, e.relation
		FROM edges e JOIN resources s ON s.id = e.src_id JOIN resources d ON d.id = e.dst_id
		WHERE e.origin = 'manual'`,
		func(rows *sql.Rows) error {
			var m ManualEdgeDump
			if err := rows.Scan(&m.SrcCollector, &m.SrcSourceID, &m.DstCollector, &m.DstSourceID, &m.Relation); err != nil {
				return err
			}
			b.ManualEdges = append(b.ManualEdges, m)
			return nil
		}); err != nil {
		return nil, err
	}

	if err := s.dumpRows(ctx, `
		SELECT r.collector, r.source_id, bc.verified_at, bc.note
		FROM backup_checks bc JOIN resources r ON r.id = bc.resource_id`,
		func(rows *sql.Rows) error {
			var bcd BackupCheckDump
			if err := rows.Scan(&bcd.Collector, &bcd.SourceID, &bcd.VerifiedAt, &bcd.Note); err != nil {
				return err
			}
			b.BackupChecks = append(b.BackupChecks, bcd)
			return nil
		}); err != nil {
		return nil, err
	}

	return b, nil
}

func (s *Store) dumpRows(ctx context.Context, query string, scan func(*sql.Rows) error) error {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ImportConfig applies a backup. Settings, pages, and collector configs are
// upserted directly. Annotations, manual edges, and backup checks are keyed
// to resource identity: they apply only where the resource already exists,
// so the caller should collect first. Anything that can't be placed yet is
// returned as a warning rather than failing the whole import.
func (s *Store) ImportConfig(ctx context.Context, b *ConfigBackup) (warnings []string, err error) {
	for k, v := range b.Settings {
		if err := s.SetSetting(ctx, k, v); err != nil {
			return warnings, err
		}
	}
	for _, p := range b.Pages {
		if err := s.SetPage(ctx, p.Slug, p.BodyMD); err != nil {
			return warnings, err
		}
	}
	for _, c := range b.Collectors {
		if err := s.upsertCollectorConfig(ctx, c); err != nil {
			return warnings, fmt.Errorf("collector %s:%s: %w", c.Type, c.Name, err)
		}
	}

	resolve := func(collector, sourceID string) (int64, bool) {
		var id int64
		err := s.db.QueryRowContext(ctx,
			`SELECT id FROM resources WHERE collector = ? AND source_id = ?`, collector, sourceID).Scan(&id)
		return id, err == nil
	}

	for _, a := range b.Annotations {
		id, ok := resolve(a.Collector, a.SourceID)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("annotation for %s/%s (%s) skipped — resource not present; collect first, then re-import", a.Collector, a.SourceID, a.Field))
			continue
		}
		if err := s.SetAnnotation(ctx, id, a.Field, a.BodyMD); err != nil {
			return warnings, err
		}
	}
	for _, m := range b.ManualEdges {
		src, ok1 := resolve(m.SrcCollector, m.SrcSourceID)
		dst, ok2 := resolve(m.DstCollector, m.DstSourceID)
		if !ok1 || !ok2 {
			warnings = append(warnings, fmt.Sprintf("manual edge %s/%s -> %s/%s skipped — endpoint(s) not present", m.SrcCollector, m.SrcSourceID, m.DstCollector, m.DstSourceID))
			continue
		}
		if err := s.CreateManualEdge(ctx, src, dst, model.Relation(m.Relation)); err != nil {
			// A duplicate is fine on re-import; only surface real errors.
			if err.Error() != "that relationship already exists" {
				warnings = append(warnings, fmt.Sprintf("manual edge %s/%s -> %s/%s: %v", m.SrcCollector, m.SrcSourceID, m.DstCollector, m.DstSourceID, err))
			}
		}
	}
	for _, bc := range b.BackupChecks {
		id, ok := resolve(bc.Collector, bc.SourceID)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("backup check for %s/%s skipped — resource not present", bc.Collector, bc.SourceID))
			continue
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO backup_checks (resource_id, verified_at, note) VALUES (?, ?, ?)
			ON CONFLICT (resource_id) DO UPDATE SET verified_at = excluded.verified_at, note = excluded.note`,
			id, bc.VerifiedAt, bc.Note); err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}

func (s *Store) upsertCollectorConfig(ctx context.Context, c CollectorConfigDump) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO collector_configs (type, name, config, secret, enabled)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (type, name) DO UPDATE SET
		  config = excluded.config, secret = excluded.secret, enabled = excluded.enabled`,
		c.Type, c.Name, string(c.Config), c.Secret, c.Enabled)
	return err
}
