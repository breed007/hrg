package docker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/model"
)

func TestCollect(t *testing.T) {
	cfg, _ := json.Marshal(Config{FixtureDir: "testdata/engine"})
	c, err := Factory(collector.Spec{Type: Type, Instance: "docker:test", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	byID := map[string]model.Resource{}
	for _, r := range res.Resources {
		if err := r.Validate(); err != nil {
			t.Errorf("invalid resource emitted: %v", err)
		}
		if _, dup := byID[r.SourceID]; dup {
			t.Errorf("duplicate source id %q", r.SourceID)
		}
		byID[r.SourceID] = r
	}

	// 2 networks + 1 volume + 1 compose project + 3 containers
	if len(res.Resources) != 7 {
		t.Fatalf("want 7 resources, got %d", len(res.Resources))
	}

	// Compose containers get compose-based identity (survives recreates).
	plex, ok := byID["compose/media/plex"]
	if !ok {
		t.Fatal("compose container did not use compose/{project}/{service} identity")
	}
	if plex.Kind != model.KindContainer || plex.Name != "media-plex-1" {
		t.Errorf("plex parsed wrong: %+v", plex)
	}
	ports, _ := plex.Attrs["ports"].([]string)
	if len(ports) != 1 || ports[0] != "0.0.0.0:32400→32400/tcp" {
		t.Errorf("ports not deduped/formatted: %v", plex.Attrs["ports"])
	}

	// Non-compose containers fall back to name identity.
	if _, ok := byID["container/scratch-redis"]; !ok {
		t.Error("non-compose container did not use container/{name} identity")
	}

	// The compose project is a service-kind grouping resource.
	if proj, ok := byID["project/media"]; !ok || proj.Kind != model.KindService {
		t.Errorf("compose project resource wrong: %+v", proj)
	}

	wantEdges := map[string]bool{
		"compose/media/plex|member_of|project/media":           false,
		"compose/media/sonarr|member_of|project/media":         false,
		"compose/media/plex|attached_to|volume/plex-config":    false,
		"compose/media/plex|member_of|network/media_default":   false,
		"compose/media/sonarr|member_of|network/media_default": false,
		"container/scratch-redis|member_of|network/bridge":     false,
	}
	for _, e := range res.Edges {
		key := e.Src.SourceID + "|" + string(e.Relation) + "|" + e.Dst.SourceID
		if _, want := wantEdges[key]; !want {
			t.Errorf("unexpected edge %s", key)
			continue
		}
		wantEdges[key] = true
	}
	for k, seen := range wantEdges {
		if !seen {
			t.Errorf("missing edge %s", k)
		}
	}
}

func TestFactoryValidation(t *testing.T) {
	if _, err := Factory(collector.Spec{Type: Type, Instance: "docker:x", Config: json.RawMessage(`{}`)}); err == nil {
		t.Error("factory accepted empty config")
	}
	if _, err := Factory(collector.Spec{Type: Type, Instance: "docker:x", Config: json.RawMessage(`{"host":"ssh://nope"}`)}); err == nil {
		t.Error("factory accepted unsupported scheme")
	}
	if _, err := Factory(collector.Spec{Type: Type, Instance: "docker:x", Config: json.RawMessage(`{"host":"unix:///var/run/docker.sock"}`)}); err != nil {
		t.Errorf("factory rejected valid unix socket config: %v", err)
	}
}
