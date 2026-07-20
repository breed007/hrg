package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Destination is one configured place copies of the runbook go. Secret is
// the sealed credential blob (internal/secrets); the store never sees
// plaintext, exactly as with collector configs.
type Destination struct {
	ID      int64
	Type    string // "folder", "email", "rclone"
	Name    string // human label, unique
	Config  json.RawMessage
	Secret  []byte
	Guides  []string // "household", "administrator"
	Formats []string // "pdf", "html"
	Enabled bool
}

// Sends reports whether this destination carries the given guide/format.
func (d Destination) Sends(guide, format string) bool {
	return contains(d.Guides, guide) && contains(d.Formats, format)
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// Delivery is one attempt to get a copy out. Name is denormalized so the
// history stays readable after its destination is deleted.
type Delivery struct {
	ID            int64
	DestinationID *int64
	Name          string
	CreatedAt     string
	Status        string // "ok" | "error"
	Detail        string
}

// OK reports success.
func (d Delivery) OK() bool { return d.Status == "ok" }

// Age returns how long ago this delivery happened, or 0 if unparseable.
func (d Delivery) Age() time.Duration {
	t, err := time.Parse(time.RFC3339, d.CreatedAt)
	if err != nil {
		return 0
	}
	return time.Since(t)
}

// validGuides and validFormats bound what can be stored. Anything else is
// a bug or a hand-edited database, and both should fail loudly.
var validGuides = map[string]bool{"household": true, "administrator": true}
var validFormats = map[string]bool{"pdf": true, "html": true}

// normalizeSet validates, de-duplicates and orders a selection.
func normalizeSet(vals []string, valid map[string]bool, what string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if !valid[v] {
			return nil, fmt.Errorf("unknown %s %q", what, v)
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("select at least one %s", what)
	}
	sort.Strings(out)
	return out, nil
}

func (s *Store) validateDestination(d *Destination) error {
	d.Name = strings.TrimSpace(d.Name)
	if d.Name == "" {
		return errors.New("destination needs a name")
	}
	if strings.TrimSpace(d.Type) == "" {
		return errors.New("destination needs a type")
	}
	var err error
	if d.Guides, err = normalizeSet(d.Guides, validGuides, "guide"); err != nil {
		return err
	}
	if d.Formats, err = normalizeSet(d.Formats, validFormats, "format"); err != nil {
		return err
	}
	if len(d.Config) == 0 {
		d.Config = json.RawMessage("{}")
	}
	return nil
}

func (s *Store) CreateDestination(ctx context.Context, d Destination) (int64, error) {
	if err := s.validateDestination(&d); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO destinations (type, name, config, secret, guides, formats, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		d.Type, d.Name, string(d.Config), d.Secret,
		strings.Join(d.Guides, ","), strings.Join(d.Formats, ","), d.Enabled)
	if err != nil {
		return 0, fmt.Errorf("create destination %q: %w", d.Name, err)
	}
	return res.LastInsertId()
}

// UpdateDestination updates everything but the type — changing the type
// would leave configuration from the old one behind. A nil Secret keeps
// the stored credential, so the form never has to echo it back.
func (s *Store) UpdateDestination(ctx context.Context, d Destination) error {
	if err := s.validateDestination(&d); err != nil {
		return err
	}
	q := `UPDATE destinations SET name = ?, config = ?, guides = ?, formats = ?, enabled = ? WHERE id = ?`
	args := []any{d.Name, string(d.Config), strings.Join(d.Guides, ","), strings.Join(d.Formats, ","), d.Enabled, d.ID}
	if d.Secret != nil {
		q = `UPDATE destinations SET name = ?, config = ?, guides = ?, formats = ?, enabled = ?, secret = ? WHERE id = ?`
		args = []any{d.Name, string(d.Config), strings.Join(d.Guides, ","), strings.Join(d.Formats, ","), d.Enabled, d.Secret, d.ID}
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

func (s *Store) DeleteDestination(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM destinations WHERE id = ?`, id)
	return err
}

func (s *Store) GetDestination(ctx context.Context, id int64) (*Destination, error) {
	d, err := scanDestination(s.db.QueryRowContext(ctx, `
		SELECT id, type, name, config, secret, guides, formats, enabled
		FROM destinations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

func (s *Store) ListDestinations(ctx context.Context) ([]Destination, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, type, name, config, secret, guides, formats, enabled
		FROM destinations ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Destination
	for rows.Next() {
		d, err := scanDestination(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

func scanDestination(r rowScanner) (*Destination, error) {
	var d Destination
	var cfg, guides, formats string
	if err := r.Scan(&d.ID, &d.Type, &d.Name, &cfg, &d.Secret, &guides, &formats, &d.Enabled); err != nil {
		return nil, err
	}
	d.Config = json.RawMessage(cfg)
	d.Guides = strings.Split(guides, ",")
	d.Formats = strings.Split(formats, ",")
	return &d, nil
}

// RecordDelivery appends to the delivery history.
func (s *Store) RecordDelivery(ctx context.Context, d Delivery) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO deliveries (destination_id, name, created_at, status, detail)
		VALUES (?, ?, ?, ?, ?)`,
		d.DestinationID, d.Name, time.Now().UTC().Format(time.RFC3339), d.Status, d.Detail)
	return err
}

// ListDeliveries returns the most recent attempts, newest first.
func (s *Store) ListDeliveries(ctx context.Context, limit int) ([]Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, destination_id, name, created_at, status, detail
		FROM deliveries ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.DestinationID, &d.Name, &d.CreatedAt, &d.Status, &d.Detail); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LastGoodDelivery answers the only question that matters here: when did a
// copy of this runbook last make it off this machine? nil means never.
func (s *Store) LastGoodDelivery(ctx context.Context) (*Delivery, error) {
	var d Delivery
	err := s.db.QueryRowContext(ctx, `
		SELECT id, destination_id, name, created_at, status, detail
		FROM deliveries WHERE status = 'ok' ORDER BY id DESC LIMIT 1`).
		Scan(&d.ID, &d.DestinationID, &d.Name, &d.CreatedAt, &d.Status, &d.Detail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// upsertDestination applies a backed-up destination, keyed by name — the
// same identity the UI shows, so re-importing updates rather than
// duplicating.
func (s *Store) upsertDestination(ctx context.Context, d DestinationDump) error {
	dest := Destination{
		Type: d.Type, Name: d.Name, Config: d.Config, Secret: d.Secret,
		Guides: d.Guides, Formats: d.Formats, Enabled: d.Enabled,
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM destinations WHERE name = ?`, d.Name).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err := s.CreateDestination(ctx, dest)
		return err
	case err != nil:
		return err
	default:
		dest.ID = id
		return s.UpdateDestination(ctx, dest)
	}
}
