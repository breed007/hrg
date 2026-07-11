package unifi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/model"
)

func TestCollect(t *testing.T) {
	cfg, _ := json.Marshal(Config{FixtureDir: "testdata/site"})
	c, err := Factory(collector.Spec{Type: Type, Instance: "unifi:test", Config: cfg})
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

	// 4 networks + 2 wlans + 3 devices + 1 firewall summary
	if len(res.Resources) != 10 {
		t.Fatalf("want 10 resources, got %d", len(res.Resources))
	}

	// VLAN network: gateway-style subnet normalized to a network prefix.
	servers := byID["network/60a1b2c3d4e5f60001aaaa02"]
	if servers.Kind != model.KindVLAN {
		t.Errorf("vlan network kind wrong: %+v", servers)
	}
	if servers.Attrs["cidr"] != "10.0.10.0/24" {
		t.Errorf("cidr not normalized from gateway form: %v", servers.Attrs["cidr"])
	}
	if servers.Attrs["vlan"] != 10 || servers.Attrs["dhcp"] != "10.0.10.100–10.0.10.254" {
		t.Errorf("vlan attrs wrong: %+v", servers.Attrs)
	}

	// Non-VLAN network stays kind network; WAN carries wan_type.
	if byID["network/60a1b2c3d4e5f60001aaaa04"].Kind != model.KindNetwork {
		t.Error("non-vlan network should be kind network")
	}
	if byID["network/60a1b2c3d4e5f60001aaaa01"].Attrs["wan_type"] != "dhcp" {
		t.Error("wan_type missing on WAN network")
	}

	// Devices keyed by MAC.
	udm := byID["device/aa:bb:cc:00:00:01"]
	if udm.Kind != model.KindDevice || udm.Name != "UDM Pro" || udm.Attrs["ip"] != "10.0.1.1" {
		t.Errorf("udm parsed wrong: %+v", udm)
	}

	fw := byID["firewall/summary"]
	if fw.Attrs["total"] != 4 || fw.Attrs["enabled"] != 3 {
		t.Errorf("firewall summary wrong: %+v", fw.Attrs)
	}

	wantEdges := map[string]bool{
		// physical uplinks: AP -> switch -> gateway
		"device/aa:bb:cc:00:00:02|attached_to|device/aa:bb:cc:00:00:01": false,
		"device/aa:bb:cc:00:00:03|attached_to|device/aa:bb:cc:00:00:02": false,
		// SSIDs bound to their networks
		"wlan/60a1b2c3d4e5f60001bbbb01|member_of|network/60a1b2c3d4e5f60001aaaa02": false,
		"wlan/60a1b2c3d4e5f60001bbbb02|member_of|network/60a1b2c3d4e5f60001aaaa03": false,
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
	cfg, _ := json.Marshal(Config{URL: "https://192.168.1.1"})
	if _, err := Factory(collector.Spec{Type: Type, Instance: "unifi:x", Config: cfg}); err == nil {
		t.Error("factory accepted config without API key")
	}
}
