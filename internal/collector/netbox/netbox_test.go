package netbox

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/model"
)

func TestCollect(t *testing.T) {
	cfg, _ := json.Marshal(Config{FixtureDir: "testdata/nb"})
	c, err := Factory(collector.Spec{Type: Type, Instance: "netbox:test", Config: cfg})
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
		byID[r.SourceID] = r
	}

	// 3 prefixes + 1 rack + 2 devices
	if len(res.Resources) != 6 {
		t.Fatalf("want 6 resources, got %d", len(res.Resources))
	}

	// IP assignments fold into their containing prefix, sorted.
	servers := byID["prefix/2"]
	if servers.Name != "Server VLAN" || servers.Attrs["cidr"] != "10.0.10.0/24" || servers.Attrs["vlan"] != 10 {
		t.Errorf("prefix parsed wrong: %+v", servers)
	}
	addrs, _ := servers.Attrs["addresses"].([]map[string]any)
	if len(addrs) != 2 || addrs[0]["ip"] != "10.0.10.5" || addrs[0]["dns"] != "nas.lan" || addrs[1]["ip"] != "10.0.10.10" {
		t.Errorf("addresses grouping wrong: %+v", addrs)
	}
	// The management IP lands in the management prefix.
	mgmt := byID["prefix/1"]
	maddrs, _ := mgmt.Attrs["addresses"].([]map[string]any)
	if len(maddrs) != 1 || maddrs[0]["ip"] != "10.0.1.1" {
		t.Errorf("management addresses wrong: %+v", maddrs)
	}
	// Reserved prefix has no addresses key at all.
	if _, has := byID["prefix/3"].Attrs["addresses"]; has {
		t.Error("empty prefix should not carry an addresses key")
	}

	// Devices: primary IP stripped of mask, rack edge emitted.
	pve := byID["device/21"]
	if pve.Kind != model.KindDevice || pve.Attrs["ip"] != "10.0.10.10" || pve.Attrs["model"] != "Dell PowerEdge R730" || pve.Attrs["role"] != "Hypervisor" {
		t.Errorf("device parsed wrong: %+v", pve)
	}
	if rack := byID["rack/31"]; rack.Kind != model.KindLocation || rack.Name != "Rack R1" {
		t.Errorf("rack parsed wrong: %+v", rack)
	}

	wantEdges := map[string]bool{
		"device/21|located_in|rack/31": false,
		"device/22|located_in|rack/31": false,
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
	cfg, _ := json.Marshal(Config{URL: "http://netbox.lan:8000"})
	if _, err := Factory(collector.Spec{Type: Type, Instance: "netbox:x", Config: cfg}); err == nil {
		t.Error("factory accepted config without token")
	}
}
