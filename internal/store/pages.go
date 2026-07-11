package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Page is a hand-authored runbook page. Valid slugs: "start_here",
// "contacts". An absent row means the page hasn't been written — the
// runbook renders a nag in its place.
type Page struct {
	Slug      string
	BodyMD    string
	UpdatedAt string
}

// PageSlugs are the hand-authored pages the runbook expects.
var PageSlugs = []string{"start_here", "contacts"}

func validPageSlug(slug string) bool {
	for _, s := range PageSlugs {
		if s == slug {
			return true
		}
	}
	return false
}

// GetPage returns the page, or a zero-body Page if it was never written.
func (s *Store) GetPage(ctx context.Context, slug string) (Page, error) {
	p := Page{Slug: slug}
	if !validPageSlug(slug) {
		return p, errors.New("unknown page " + slug)
	}
	err := s.db.QueryRowContext(ctx,
		`SELECT body_md, updated_at FROM pages WHERE slug = ?`, slug).
		Scan(&p.BodyMD, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	return p, err
}

// SetPage stores a page body; empty body removes it (back to "unwritten").
func (s *Store) SetPage(ctx context.Context, slug, body string) error {
	if !validPageSlug(slug) {
		return errors.New("unknown page " + slug)
	}
	if body == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM pages WHERE slug = ?`, slug)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pages (slug, body_md, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (slug) DO UPDATE SET body_md = excluded.body_md, updated_at = excluded.updated_at`,
		slug, body, time.Now().UTC().Format(time.RFC3339))
	return err
}

// Export records one artifact generation attempt.
type Export struct {
	ID        int64
	CreatedAt string
	Format    string // "html" | "markdown"
	Path      string
	Status    string // "ok" | "error"
	Detail    string // git result, error text, …
}

func (s *Store) RecordExport(ctx context.Context, e Export) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO exports (created_at, format, path, status, detail)
		VALUES (?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), e.Format, e.Path, e.Status, e.Detail)
	return err
}

func (s *Store) ListExports(ctx context.Context, limit int) ([]Export, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, created_at, format, path, status, COALESCE(detail, '')
		FROM exports ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Export
	for rows.Next() {
		var e Export
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Format, &e.Path, &e.Status, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestOKExport returns the newest successful export of a format, or nil.
func (s *Store) LatestOKExport(ctx context.Context, format string) (*Export, error) {
	var e Export
	err := s.db.QueryRowContext(ctx, `
		SELECT id, created_at, format, path, status, COALESCE(detail, '')
		FROM exports WHERE format = ? AND status = 'ok'
		ORDER BY id DESC LIMIT 1`, format).
		Scan(&e.ID, &e.CreatedAt, &e.Format, &e.Path, &e.Status, &e.Detail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// Settings returns all key/value settings.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// AllAnnotations returns every annotation, keyed by resource id then field.
// The runbook builder needs the whole set; per-resource queries would be
// N+1 for no benefit.
func (s *Store) AllAnnotations(ctx context.Context) (map[int64]map[string]Annotation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT resource_id, field, body_md, updated_at FROM annotations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[string]Annotation{}
	for rows.Next() {
		var id int64
		var a Annotation
		if err := rows.Scan(&id, &a.Field, &a.BodyMD, &a.UpdatedAt); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string]Annotation{}
		}
		out[id][a.Field] = a
	}
	return out, rows.Err()
}
