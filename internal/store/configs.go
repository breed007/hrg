package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// CollectorConfig is one configured collector instance. Secret is the
// sealed API token blob (internal/secrets); the store never sees plaintext.
type CollectorConfig struct {
	ID      int64
	Type    string // "proxmox", "docker"
	Name    string // instance suffix, e.g. "pve1"
	Config  json.RawMessage
	Secret  []byte
	Enabled bool
}

// InstanceName is the provenance key resources are recorded under.
func (c CollectorConfig) InstanceName() string {
	return c.Type + ":" + c.Name
}

func (s *Store) CreateCollectorConfig(ctx context.Context, c CollectorConfig) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO collector_configs (type, name, config, secret, enabled)
		VALUES (?, ?, ?, ?, ?)`,
		c.Type, c.Name, string(c.Config), c.Secret, c.Enabled)
	if err != nil {
		return 0, fmt.Errorf("create collector config %s: %w", c.InstanceName(), err)
	}
	return res.LastInsertId()
}

// UpdateCollectorConfig updates everything but the identity (type, name) —
// renaming an instance would strand its resources under the old provenance
// key. A nil Secret keeps the existing one.
func (s *Store) UpdateCollectorConfig(ctx context.Context, c CollectorConfig) error {
	var err error
	if c.Secret == nil {
		_, err = s.db.ExecContext(ctx, `
			UPDATE collector_configs SET config = ?, enabled = ? WHERE id = ?`,
			string(c.Config), c.Enabled, c.ID)
	} else {
		_, err = s.db.ExecContext(ctx, `
			UPDATE collector_configs SET config = ?, secret = ?, enabled = ? WHERE id = ?`,
			string(c.Config), c.Secret, c.Enabled, c.ID)
	}
	return err
}

// DeleteCollectorConfig removes the instance configuration. Its resources
// remain in the inventory (their history and annotations are the valuable
// part); with no future runs they simply stop updating.
func (s *Store) DeleteCollectorConfig(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM collector_configs WHERE id = ?`, id)
	return err
}

func (s *Store) GetCollectorConfig(ctx context.Context, id int64) (*CollectorConfig, error) {
	c, err := scanConfig(s.db.QueryRowContext(ctx, `
		SELECT id, type, name, config, secret, enabled
		FROM collector_configs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

func (s *Store) ListCollectorConfigs(ctx context.Context) ([]CollectorConfig, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, type, name, config, secret, enabled
		FROM collector_configs ORDER BY type, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CollectorConfig
	for rows.Next() {
		c, err := scanConfig(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanConfig(r rowScanner) (*CollectorConfig, error) {
	var c CollectorConfig
	var cfg string
	if err := r.Scan(&c.ID, &c.Type, &c.Name, &cfg, &c.Secret, &c.Enabled); err != nil {
		return nil, err
	}
	c.Config = json.RawMessage(cfg)
	return &c, nil
}
