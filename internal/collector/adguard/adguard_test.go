package adguard

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/model"
)

func TestCollect(t *testing.T) {
	cfg, _ := json.Marshal(Config{FixtureDir: "testdata/agh"})
	c, err := Factory(collector.Spec{Type: Type, Instance: "adguard:test", Config: cfg})
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

	// DNS service + rewrites + DHCP (enabled in fixture)
	if len(res.Resources) != 3 {
		t.Fatalf("want 3 resources, got %d", len(res.Resources))
	}

	dns := byID["service/dns"]
	if dns.Kind != model.KindService || dns.Attrs["version"] != "v0.107.52" || dns.Attrs["protection"] != true {
		t.Errorf("dns service parsed wrong: %+v", dns)
	}
	ups, _ := dns.Attrs["upstreams"].([]string)
	if len(ups) != 2 || ups[0] != "https://dns.quad9.net/dns-query" {
		t.Errorf("upstreams wrong: %v", dns.Attrs["upstreams"])
	}

	rw := byID["dns/rewrites"]
	records, _ := rw.Attrs["records"].(map[string]any)
	if rw.Attrs["count"] != 3 || records["plex.lan"] != "10.0.10.5" || records["*.apps.lan"] != "10.0.10.20" {
		t.Errorf("rewrites parsed wrong: %+v", rw.Attrs)
	}

	dhcp := byID["service/dhcp"]
	if dhcp.Attrs["range"] != "10.0.20.100–10.0.20.200" || dhcp.Attrs["gateway"] != "10.0.20.1" {
		t.Errorf("dhcp parsed wrong: %+v", dhcp.Attrs)
	}
	leases, _ := dhcp.Attrs["static_leases"].([]map[string]any)
	if len(leases) != 2 || leases[0]["hostname"] != "front-door-cam" {
		t.Errorf("static leases wrong: %+v", leases)
	}
}

func TestFactoryValidation(t *testing.T) {
	cfg, _ := json.Marshal(Config{URL: "http://10.0.10.53:3000", Username: "admin"})
	if _, err := Factory(collector.Spec{Type: Type, Instance: "adguard:x", Config: cfg}); err == nil {
		t.Error("factory accepted username without password")
	}
	// No auth at all is legal (unprotected instance).
	cfg2, _ := json.Marshal(Config{URL: "http://10.0.10.53:3000"})
	if _, err := Factory(collector.Spec{Type: Type, Instance: "adguard:x", Config: cfg2}); err != nil {
		t.Errorf("factory rejected auth-less config: %v", err)
	}
}
