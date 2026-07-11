// Package unifi collects networks, VLANs, WLANs, devices, and a firewall
// summary from a UniFi Network controller.
//
// Auth is an API key (UniFi Network 9.0+: Settings → Control Plane →
// Integrations → Create API Key), sent as X-API-KEY against the classic
// REST endpoints. UniFi OS consoles (UDM, Cloud Key) proxy those under
// /proxy/network; self-hosted classic controllers serve them at the root —
// set "classic" for those.
//
// Source IDs use the controller's Mongo _ids (stable) for networks/WLANs
// and MAC addresses for devices: "network/<id>", "wlan/<id>",
// "device/<mac>", plus the singleton "firewall/summary".
package unifi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/collector/fixture"
	"github.com/breed007/hrg/internal/model"
)

// Type is the registry name.
const Type = "unifi"

// Config is the non-secret instance configuration.
type Config struct {
	// URL of the controller, e.g. https://192.168.1.1 (UniFi OS console)
	// or https://controller.lan:8443 (classic).
	URL string `json:"url"`
	// Site is the UniFi site name; "default" if empty.
	Site string `json:"site,omitempty"`
	// Classic marks a self-hosted controller that serves the API at the
	// root instead of under /proxy/network.
	Classic bool `json:"classic,omitempty"`
	// InsecureTLS skips certificate verification (self-signed console cert).
	InsecureTLS bool `json:"insecure_tls,omitempty"`
	// FixtureDir replays recorded API responses. For demos and tests.
	FixtureDir string `json:"fixture_dir,omitempty"`
}

type Collector struct {
	instance string
	cfg      Config
	secret   string
	client   *http.Client
}

// Factory builds instances from stored specs; registered in cmd/hrg.
func Factory(spec collector.Spec) (collector.Collector, error) {
	var cfg Config
	if err := json.Unmarshal(spec.Config, &cfg); err != nil {
		return nil, fmt.Errorf("%s: bad config: %w", spec.Instance, err)
	}
	if cfg.Site == "" {
		cfg.Site = "default"
	}
	if cfg.FixtureDir == "" {
		if cfg.URL == "" {
			return nil, fmt.Errorf("%s: url is required", spec.Instance)
		}
		if spec.Secret == "" {
			return nil, fmt.Errorf("%s: API key is required (UniFi Network 9.0+, Settings → Control Plane → Integrations)", spec.Instance)
		}
	}

	c := &Collector{instance: spec.Instance, cfg: cfg, secret: spec.Secret}
	if cfg.FixtureDir != "" {
		c.client = fixture.Client(cfg.FixtureDir)
		c.cfg.URL = "https://fixture.invalid"
	} else {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if cfg.InsecureTLS {
			transport.TLSClientConfig.InsecureSkipVerify = true
		}
		c.client = &http.Client{Transport: transport, Timeout: 30 * time.Second}
	}
	return c, nil
}

func (c *Collector) Name() string { return c.instance }

type networkConf struct {
	ID          string `json:"_id"`
	Name        string `json:"name"`
	Purpose     string `json:"purpose"` // corporate | guest | wan | vlan-only
	VLANEnabled bool   `json:"vlan_enabled"`
	VLAN        int    `json:"vlan"`
	IPSubnet    string `json:"ip_subnet"` // gateway-style: 10.0.10.1/24
	DHCPEnabled bool   `json:"dhcpd_enabled"`
	DHCPStart   string `json:"dhcpd_start"`
	DHCPStop    string `json:"dhcpd_stop"`
	DomainName  string `json:"domain_name"`
	WANType     string `json:"wan_type"`
}

type wlanConf struct {
	ID        string `json:"_id"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Security  string `json:"security"`
	NetworkID string `json:"networkconf_id"`
}

type device struct {
	MAC     string `json:"mac"`
	Name    string `json:"name"`
	Model   string `json:"model"`
	Type    string `json:"type"` // ugw | usw | uap | udm
	IP      string `json:"ip"`
	Version string `json:"version"`
	State   int    `json:"state"` // 1 = connected
	Uplink  struct {
		UplinkMAC string `json:"uplink_mac"`
	} `json:"uplink"`
}

type firewallRule struct {
	Enabled bool   `json:"enabled"`
	Ruleset string `json:"ruleset"`
}

func (c *Collector) Collect(ctx context.Context) (collector.Result, error) {
	var res collector.Result

	var nets []networkConf
	if err := c.get(ctx, "/rest/networkconf", &nets); err != nil {
		return res, err
	}
	netSource := map[string]string{} // _id -> source id, for WLAN edges
	for _, n := range nets {
		sid := "network/" + n.ID
		netSource[n.ID] = sid

		kind := model.KindNetwork
		attrs := map[string]any{"purpose": n.Purpose}
		if n.Purpose == "wan" {
			attrs["wan_type"] = n.WANType
		} else {
			if n.VLANEnabled && n.VLAN > 0 {
				kind = model.KindVLAN
				attrs["vlan"] = n.VLAN
			}
			if cidr := normalizeCIDR(n.IPSubnet); cidr != "" {
				attrs["cidr"] = cidr
				attrs["gateway"] = hostPart(n.IPSubnet)
			}
			if n.DHCPEnabled && n.DHCPStart != "" {
				attrs["dhcp"] = n.DHCPStart + "–" + n.DHCPStop
			}
			if n.DomainName != "" {
				attrs["domain"] = n.DomainName
			}
		}
		res.Resources = append(res.Resources, model.Resource{
			Kind: kind, SourceID: sid, Name: n.Name, Attrs: attrs,
		})
	}

	var wlans []wlanConf
	if err := c.get(ctx, "/rest/wlanconf", &wlans); err != nil {
		return res, err
	}
	for _, w := range wlans {
		sid := "wlan/" + w.ID
		res.Resources = append(res.Resources, model.Resource{
			Kind:     model.KindNetwork,
			SourceID: sid,
			Name:     w.Name,
			Attrs: map[string]any{
				"type": "wifi", "security": w.Security, "enabled": w.Enabled,
			},
		})
		if dst, ok := netSource[w.NetworkID]; ok {
			res.Edges = append(res.Edges, model.Edge{
				Src:      model.Ref{SourceID: sid},
				Dst:      model.Ref{SourceID: dst},
				Relation: model.RelMemberOf,
			})
		}
	}

	var devices []device
	if err := c.get(ctx, "/stat/device", &devices); err != nil {
		return res, err
	}
	knownMAC := map[string]bool{}
	for _, d := range devices {
		knownMAC[d.MAC] = true
	}
	for _, d := range devices {
		name := d.Name
		if name == "" {
			name = d.Model + " " + d.MAC
		}
		res.Resources = append(res.Resources, model.Resource{
			Kind:     model.KindDevice,
			SourceID: "device/" + d.MAC,
			Name:     name,
			Attrs: map[string]any{
				"model": d.Model, "type": d.Type, "ip": d.IP,
				"version": d.Version, "connected": d.State == 1,
			},
		})
		// Uplink MACs draw the physical topology: AP -> switch -> gateway.
		if d.Uplink.UplinkMAC != "" && knownMAC[d.Uplink.UplinkMAC] {
			res.Edges = append(res.Edges, model.Edge{
				Src:      model.Ref{SourceID: "device/" + d.MAC},
				Dst:      model.Ref{SourceID: "device/" + d.Uplink.UplinkMAC},
				Relation: model.RelAttachedTo,
			})
		}
	}

	var rules []firewallRule
	if err := c.get(ctx, "/rest/firewallrule", &rules); err != nil {
		return res, err
	}
	enabled := 0
	byRuleset := map[string]any{}
	for _, r := range rules {
		if r.Enabled {
			enabled++
		}
		if n, ok := byRuleset[r.Ruleset].(int); ok {
			byRuleset[r.Ruleset] = n + 1
		} else {
			byRuleset[r.Ruleset] = 1
		}
	}
	res.Resources = append(res.Resources, model.Resource{
		Kind:     model.KindOther,
		SourceID: "firewall/summary",
		Name:     "Firewall rules",
		Attrs: map[string]any{
			"total": len(rules), "enabled": enabled, "by_ruleset": byRuleset,
		},
	})

	return res, nil
}

// get calls a controller endpoint under the site path and decodes the
// {"data": [...]} envelope.
func (c *Collector) get(ctx context.Context, sitePath string, out any) error {
	prefix := "/proxy/network"
	if c.cfg.Classic {
		prefix = ""
	}
	path := prefix + "/api/s/" + c.cfg.Site + sitePath

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.URL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-KEY", c.secret)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: GET %s: %w", c.instance, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: GET %s: %s", c.instance, path, resp.Status)
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("%s: GET %s: decode: %w", c.instance, path, err)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("%s: GET %s: decode data: %w", c.instance, path, err)
	}
	return nil
}

// normalizeCIDR turns UniFi's gateway-style subnet ("10.0.10.1/24") into
// the network prefix ("10.0.10.0/24") so IP-plan reconciliation can match
// it against NetBox, which stores proper prefixes.
func normalizeCIDR(gatewayCIDR string) string {
	p, err := netip.ParsePrefix(gatewayCIDR)
	if err != nil {
		return ""
	}
	return p.Masked().String()
}

func hostPart(cidr string) string {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return ""
	}
	return p.Addr().String()
}
