// Package adguard collects DNS rewrites (local records), upstream config,
// and DHCP state from an AdGuard Home instance via its control API with
// HTTP basic auth.
//
// Rewrites are folded into a single "Local DNS records" resource rather
// than one resource per record: the record set is the meaningful unit, and
// a change log entry of "records edited" beats fifty churning micro-resources.
package adguard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/collector/fixture"
	"github.com/breed007/hrg/internal/model"
)

// Type is the registry name.
const Type = "adguard"

// Config is the non-secret instance configuration.
type Config struct {
	// URL of the AdGuard Home web interface, e.g. http://192.168.1.5:3000
	URL string `json:"url"`
	// Username for basic auth; the password is the secret. Leave both
	// empty for an unprotected instance.
	Username string `json:"username,omitempty"`
	// InsecureTLS skips certificate verification.
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
	if cfg.FixtureDir == "" {
		if cfg.URL == "" {
			return nil, fmt.Errorf("%s: url is required", spec.Instance)
		}
		if cfg.Username != "" && spec.Secret == "" {
			return nil, fmt.Errorf("%s: password is required when a username is set", spec.Instance)
		}
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")

	c := &Collector{instance: spec.Instance, cfg: cfg, secret: spec.Secret}
	if cfg.FixtureDir != "" {
		c.client = fixture.Client(cfg.FixtureDir)
		c.cfg.URL = "http://fixture.invalid"
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

func (c *Collector) Collect(ctx context.Context) (collector.Result, error) {
	var res collector.Result

	var status struct {
		Version           string   `json:"version"`
		ProtectionEnabled bool     `json:"protection_enabled"`
		DNSAddresses      []string `json:"dns_addresses"`
	}
	if err := c.get(ctx, "/control/status", &status); err != nil {
		return res, err
	}
	var dnsInfo struct {
		UpstreamDNS []string `json:"upstream_dns"`
	}
	if err := c.get(ctx, "/control/dns_info", &dnsInfo); err != nil {
		return res, err
	}
	res.Resources = append(res.Resources, model.Resource{
		Kind:     model.KindService,
		SourceID: "service/dns",
		Name:     "AdGuard Home DNS",
		Attrs: map[string]any{
			"version":    status.Version,
			"protection": status.ProtectionEnabled,
			"listens_on": status.DNSAddresses,
			"upstreams":  dnsInfo.UpstreamDNS,
		},
	})

	var rewrites []struct {
		Domain string `json:"domain"`
		Answer string `json:"answer"`
	}
	if err := c.get(ctx, "/control/rewrite/list", &rewrites); err != nil {
		return res, err
	}
	records := map[string]any{}
	for _, r := range rewrites {
		records[r.Domain] = r.Answer
	}
	res.Resources = append(res.Resources, model.Resource{
		Kind:     model.KindService,
		SourceID: "dns/rewrites",
		Name:     "Local DNS records",
		Attrs:    map[string]any{"records": records, "count": len(records)},
	})

	var dhcp struct {
		Enabled bool `json:"enabled"`
		V4      struct {
			GatewayIP  string `json:"gateway_ip"`
			RangeStart string `json:"range_start"`
			RangeEnd   string `json:"range_end"`
			SubnetMask string `json:"subnet_mask"`
		} `json:"v4"`
		StaticLeases []struct {
			MAC      string `json:"mac"`
			IP       string `json:"ip"`
			Hostname string `json:"hostname"`
		} `json:"static_leases"`
	}
	if err := c.get(ctx, "/control/dhcp/status", &dhcp); err != nil {
		return res, err
	}
	if dhcp.Enabled {
		leases := make([]map[string]any, 0, len(dhcp.StaticLeases))
		for _, l := range dhcp.StaticLeases {
			leases = append(leases, map[string]any{
				"hostname": l.Hostname, "ip": l.IP, "mac": l.MAC,
			})
		}
		sort.Slice(leases, func(i, j int) bool {
			return leases[i]["ip"].(string) < leases[j]["ip"].(string)
		})
		attrs := map[string]any{
			"gateway":     dhcp.V4.GatewayIP,
			"range":       dhcp.V4.RangeStart + "–" + dhcp.V4.RangeEnd,
			"subnet_mask": dhcp.V4.SubnetMask,
		}
		if len(leases) > 0 {
			attrs["static_leases"] = leases
		}
		res.Resources = append(res.Resources, model.Resource{
			Kind:     model.KindService,
			SourceID: "service/dhcp",
			Name:     "DHCP (AdGuard Home)",
			Attrs:    attrs,
		})
	}

	return res, nil
}

func (c *Collector) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.URL+path, nil)
	if err != nil {
		return err
	}
	if c.cfg.Username != "" {
		req.SetBasicAuth(c.cfg.Username, c.secret)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: GET %s: %w", c.instance, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: GET %s: %s", c.instance, path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: GET %s: decode: %w", c.instance, path, err)
	}
	return nil
}
