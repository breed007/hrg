package manual

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/breed007/hrg/internal/model"
)

func TestCollectGood(t *testing.T) {
	res, err := New("testdata/good").Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Resources) != 3 {
		t.Fatalf("want 3 resources, got %d", len(res.Resources))
	}
	if len(res.Edges) != 3 {
		t.Fatalf("want 3 edges, got %d", len(res.Edges))
	}

	byID := map[string]model.Resource{}
	for _, r := range res.Resources {
		byID[r.SourceID] = r
	}
	modem, ok := byID["modem"]
	if !ok {
		t.Fatal("modem not parsed")
	}
	if modem.Kind != model.KindDevice || modem.Attrs["location"] != "basement closet, top shelf" {
		t.Errorf("modem parsed wrong: %+v", modem)
	}

	var crossCollector, local bool
	for _, e := range res.Edges {
		if e.Dst.Collector == "unifi:home" && e.Dst.SourceID == "device/udm-pro" {
			crossCollector = true
		}
		if e.Src.SourceID == "isp-account" && e.Dst.SourceID == "modem" && e.Dst.Collector == "" {
			local = true
		}
	}
	if !crossCollector {
		t.Error("cross-collector {collector, id} ref not parsed")
	}
	if !local {
		t.Error("same-collector string shorthand ref not parsed")
	}
}

func TestCollectMissingDirIsEmpty(t *testing.T) {
	res, err := New("testdata/nope").Collect(context.Background())
	if err != nil {
		t.Fatalf("missing dir should be empty result, got error: %v", err)
	}
	if len(res.Resources) != 0 {
		t.Errorf("want empty, got %d resources", len(res.Resources))
	}
}

// Bad input is a per-item WARNING now, not a run-killing error: the good
// resources still come through, and the problem is reported in Warnings.
func TestCollectWarnsNotFails(t *testing.T) {
	cases := []struct {
		dir, wantWarn string
	}{
		{"testdata/dup", "already defined"},
		{"testdata/badkind", "unknown kind"},
		{"testdata/badref", "unknown local resource"},
	}
	for _, c := range cases {
		res, err := New(c.dir).Collect(context.Background())
		if err != nil {
			t.Errorf("%s: bad input should warn, not error: %v", c.dir, err)
			continue
		}
		joined := strings.Join(res.Warnings, "\n")
		if !strings.Contains(joined, c.wantWarn) {
			t.Errorf("%s: want warning containing %q, got warnings: %v", c.dir, c.wantWarn, res.Warnings)
		}
	}
}

// A syntactically broken file is skipped; resources in sibling files in the
// same directory still collect.
func TestBadFileDoesNotSinkGoodFiles(t *testing.T) {
	dir := t.TempDir()
	good := "resources:\n  - id: keep\n    kind: service\n    name: Keeper\n"
	bad := "resources:\n  - id: x\n  name: broken indent\n    kind: device\n"
	if err := os.WriteFile(filepath.Join(dir, "a-good.yaml"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b-broken.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := New(dir).Collect(context.Background())
	if err != nil {
		t.Fatalf("one broken file should not fail the whole collect: %v", err)
	}
	if len(res.Resources) != 1 || res.Resources[0].SourceID != "keep" {
		t.Errorf("good file did not survive the broken sibling: %+v", res.Resources)
	}
	if len(res.Warnings) == 0 {
		t.Error("broken file should have produced a warning")
	}
}
