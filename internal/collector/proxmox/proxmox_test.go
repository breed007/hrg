package proxmox

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/model"
)

func fixtureCollector(t *testing.T) collector.Collector {
	t.Helper()
	cfg, _ := json.Marshal(Config{FixtureDir: "testdata/pve"})
	c, err := Factory(collector.Spec{Type: Type, Instance: "proxmox:test", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCollect(t *testing.T) {
	res, err := fixtureCollector(t).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	byID := map[string]model.Resource{}
	for _, r := range res.Resources {
		if err := r.Validate(); err != nil {
			t.Errorf("invalid resource emitted: %v", err)
		}
		byID[r.SourceID] = r
	}

	// 1 node + 2 VMs + 1 LXC + 1 storage + 1 backup job
	if len(res.Resources) != 6 {
		t.Fatalf("want 6 resources, got %d: %v", len(res.Resources), keys(byID))
	}

	node := byID["node/pve1"]
	if node.Kind != model.KindHost || node.Name != "pve1" {
		t.Errorf("node parsed wrong: %+v", node)
	}

	vm := byID["qemu/104"]
	if vm.Kind != model.KindVM || vm.Name != "docker-host" || vm.Attrs["memory"] != "8.0 GiB" || vm.Attrs["tags"] != "prod" {
		t.Errorf("vm parsed wrong: %+v", vm)
	}
	if _, has := vm.Attrs["template"]; has {
		t.Error("false template flag should be pruned")
	}

	lxc := byID["lxc/105"]
	if lxc.Kind != model.KindLXC {
		t.Errorf("lxc kind wrong: %+v", lxc)
	}
	// HA state from /cluster/ha/resources merges into guest attrs.
	if lxc.Attrs["ha_state"] != "started" || lxc.Attrs["ha_group"] != "main" {
		t.Errorf("ha state not merged: %+v", lxc.Attrs)
	}

	st := byID["storage/pve1/local-zfs"]
	if st.Kind != model.KindStorage || st.Attrs["size"] != "1024.0 GiB" {
		t.Errorf("storage parsed wrong: %+v", st)
	}

	job := byID["backup/backup-f3a2c1d0"]
	if job.Kind != model.KindBackupJob || job.Name != "weekly guest backup" || job.Attrs["schedule"] != "sun 02:00" {
		t.Errorf("backup job parsed wrong: %+v", job)
	}

	wantEdges := map[string]bool{
		"qemu/104|runs_on|node/pve1":                   false,
		"qemu/110|runs_on|node/pve1":                   false,
		"lxc/105|runs_on|node/pve1":                    false,
		"storage/pve1/local-zfs|attached_to|node/pve1": false,
		"qemu/104|backed_up_by|backup/backup-f3a2c1d0": false,
		"lxc/105|backed_up_by|backup/backup-f3a2c1d0":  false,
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
	cfg, _ := json.Marshal(Config{URL: "https://pve1.lan:8006"})
	if _, err := Factory(collector.Spec{Type: Type, Instance: "proxmox:x", Config: cfg}); err == nil {
		t.Error("factory accepted config without token")
	}
	if _, err := Factory(collector.Spec{Type: Type, Instance: "proxmox:x", Config: json.RawMessage(`{}`)}); err == nil {
		t.Error("factory accepted empty config")
	}
}

func keys(m map[string]model.Resource) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
