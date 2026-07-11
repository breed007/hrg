package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/model"
)

func TestGetRunDetail(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Run 1: plex + nas.
	if _, err := s.Ingest(ctx, "test", collector.Result{Resources: []model.Resource{
		rsrc("plex", "Plex", map[string]any{"port": 32400}),
		rsrc("nas", "NAS", nil),
	}}); err != nil {
		t.Fatal(err)
	}

	// Run 2: plex changed, nas gone, jellyfin new.
	sum, err := s.Ingest(ctx, "test", collector.Result{Resources: []model.Resource{
		rsrc("plex", "Plex", map[string]any{"port": 32401}),
		rsrc("jellyfin", "Jellyfin", nil),
	}})
	if err != nil {
		t.Fatal(err)
	}

	d, err := s.GetRunDetail(ctx, sum.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Added) != 1 || d.Added[0].SourceID != "jellyfin" {
		t.Errorf("added wrong: %+v", d.Added)
	}
	if len(d.Changed) != 1 || d.Changed[0].SourceID != "plex" {
		t.Fatalf("changed wrong: %+v", d.Changed)
	}
	if d.Changed[0].OldAttrs["port"] != float64(32400) || d.Changed[0].NewAttrs["port"] != float64(32401) {
		t.Errorf("change diff wrong: old=%v new=%v", d.Changed[0].OldAttrs, d.Changed[0].NewAttrs)
	}
	if len(d.Removed) != 1 || d.Removed[0].SourceID != "nas" {
		t.Errorf("removed wrong: %+v", d.Removed)
	}

	// Run 1's detail: everything added, nothing changed/removed.
	d1, err := s.GetRunDetail(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(d1.Added) != 2 || len(d1.Changed) != 0 || len(d1.Removed) != 0 {
		t.Errorf("run 1 detail wrong: added=%d changed=%d removed=%d",
			len(d1.Added), len(d1.Changed), len(d1.Removed))
	}
}

func TestCollectorConfigCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	cfg := CollectorConfig{
		Type:    "proxmox",
		Name:    "pve1",
		Config:  json.RawMessage(`{"url":"https://pve1.lan:8006","token_id":"hrg@pam!readonly"}`),
		Secret:  []byte("sealed-blob"),
		Enabled: true,
	}
	id, err := s.CreateCollectorConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstanceName() != "proxmox:pve1" {
		t.Errorf("instance name wrong: %s", cfg.InstanceName())
	}

	// Duplicate (type, name) must be rejected.
	if _, err := s.CreateCollectorConfig(ctx, cfg); err == nil {
		t.Error("duplicate collector instance accepted")
	}

	got, err := s.GetCollectorConfig(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "proxmox" || got.Name != "pve1" || string(got.Secret) != "sealed-blob" || !got.Enabled {
		t.Errorf("roundtrip wrong: %+v", got)
	}

	// Update without secret keeps the old one.
	got.Config = json.RawMessage(`{"url":"https://pve1.lan:8006","token_id":"hrg@pam!ro2"}`)
	got.Secret = nil
	got.Enabled = false
	if err := s.UpdateCollectorConfig(ctx, *got); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetCollectorConfig(ctx, id)
	if string(got2.Secret) != "sealed-blob" {
		t.Error("nil-secret update clobbered the stored secret")
	}
	if got2.Enabled {
		t.Error("enabled flag not updated")
	}

	// Update with a new secret replaces it.
	got2.Secret = []byte("new-blob")
	if err := s.UpdateCollectorConfig(ctx, *got2); err != nil {
		t.Fatal(err)
	}
	got3, _ := s.GetCollectorConfig(ctx, id)
	if string(got3.Secret) != "new-blob" {
		t.Error("secret update did not stick")
	}

	list, err := s.ListCollectorConfigs(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list wrong: %v %d", err, len(list))
	}

	if err := s.DeleteCollectorConfig(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCollectorConfig(ctx, id); err != ErrNotFound {
		t.Errorf("want ErrNotFound after delete, got %v", err)
	}
}
