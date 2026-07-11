// Package netbox collects prefixes, IP assignments, devices, and racks from
// a NetBox instance. NetBox is the *documented* IP plan — HRG treats it as
// authoritative in reconciliation, with everything other collectors observe
// compared against it.
//
// IP addresses are folded into their containing prefix's attributes
// (attrs["addresses"]) instead of becoming individual resources: a /24 of
// assignments would otherwise drown the inventory, and the change log gets
// "IP plan edited" as a single prefix version boundary.
package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/collector/fixture"
	"github.com/breed007/hrg/internal/model"
)

// Type is the registry name.
const Type = "netbox"

// Config is the non-secret instance configuration.
type Config struct {
	// URL of the NetBox root, e.g. http://netbox.lan:8000
	URL string `json:"url"`
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
		if spec.Secret == "" {
			return nil, fmt.Errorf("%s: API token is required (create a read-only token under /user/api-tokens/)", spec.Instance)
		}
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")

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

type nbPrefix struct {
	ID          int64  `json:"id"`
	Prefix      string `json:"prefix"`
	Description string `json:"description"`
	Status      struct {
		Value string `json:"value"`
	} `json:"status"`
	VLAN *struct {
		VID  int    `json:"vid"`
		Name string `json:"name"`
	} `json:"vlan"`
}

type nbIP struct {
	Address     string `json:"address"`
	DNSName     string `json:"dns_name"`
	Description string `json:"description"`
}

type nbDevice struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Type struct {
		Model        string `json:"model"`
		Manufacturer struct {
			Name string `json:"name"`
		} `json:"manufacturer"`
	} `json:"device_type"`
	Role       *struct{ Name string } `json:"role"`        // NetBox 4.x
	DeviceRole *struct{ Name string } `json:"device_role"` // NetBox 3.x
	Site       *struct{ Name string } `json:"site"`
	Rack       *struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"rack"`
	PrimaryIP *struct {
		Address string `json:"address"`
	} `json:"primary_ip"`
	Status struct {
		Value string `json:"value"`
	} `json:"status"`
}

type nbRack struct {
	ID   int64                  `json:"id"`
	Name string                 `json:"name"`
	Site *struct{ Name string } `json:"site"`
}

func (c *Collector) Collect(ctx context.Context) (collector.Result, error) {
	var res collector.Result

	prefixes, err := getAll[nbPrefix](ctx, c, "/api/ipam/prefixes/")
	if err != nil {
		return res, err
	}
	ips, err := getAll[nbIP](ctx, c, "/api/ipam/ip-addresses/")
	if err != nil {
		return res, err
	}
	devices, err := getAll[nbDevice](ctx, c, "/api/dcim/devices/")
	if err != nil {
		return res, err
	}
	racks, err := getAll[nbRack](ctx, c, "/api/dcim/racks/")
	if err != nil {
		return res, err
	}

	// Assign each IP to its longest-match containing prefix.
	type parsedPrefix struct {
		nb     nbPrefix
		prefix netip.Prefix
	}
	var parsed []parsedPrefix
	for _, p := range prefixes {
		pp, err := netip.ParsePrefix(p.Prefix)
		if err != nil {
			continue
		}
		parsed = append(parsed, parsedPrefix{nb: p, prefix: pp.Masked()})
	}
	addrsByPrefix := map[int64][]map[string]any{}
	for _, ip := range ips {
		ipp, err := netip.ParsePrefix(ip.Address)
		if err != nil {
			continue
		}
		addr := ipp.Addr()
		best := -1
		for i, pp := range parsed {
			if pp.prefix.Contains(addr) && (best == -1 || pp.prefix.Bits() > parsed[best].prefix.Bits()) {
				best = i
			}
		}
		if best == -1 {
			continue
		}
		entry := map[string]any{"ip": addr.String()}
		if ip.DNSName != "" {
			entry["dns"] = ip.DNSName
		}
		if ip.Description != "" {
			entry["description"] = ip.Description
		}
		id := parsed[best].nb.ID
		addrsByPrefix[id] = append(addrsByPrefix[id], entry)
	}

	for _, pp := range parsed {
		attrs := map[string]any{
			"cidr":   pp.prefix.String(),
			"status": pp.nb.Status.Value,
		}
		if pp.nb.Description != "" {
			attrs["description"] = pp.nb.Description
		}
		if pp.nb.VLAN != nil {
			attrs["vlan"] = pp.nb.VLAN.VID
		}
		if addrs := addrsByPrefix[pp.nb.ID]; len(addrs) > 0 {
			sort.Slice(addrs, func(i, j int) bool {
				a, _ := netip.ParseAddr(addrs[i]["ip"].(string))
				b, _ := netip.ParseAddr(addrs[j]["ip"].(string))
				return a.Compare(b) < 0
			})
			attrs["addresses"] = addrs
		}
		name := pp.prefix.String()
		if pp.nb.Description != "" {
			name = pp.nb.Description
		}
		res.Resources = append(res.Resources, model.Resource{
			Kind:     model.KindNetwork,
			SourceID: fmt.Sprintf("prefix/%d", pp.nb.ID),
			Name:     name,
			Attrs:    attrs,
		})
	}

	rackNames := map[int64]bool{}
	for _, r := range racks {
		rackNames[r.ID] = true
		attrs := map[string]any{}
		if r.Site != nil {
			attrs["site"] = r.Site.Name
		}
		res.Resources = append(res.Resources, model.Resource{
			Kind:     model.KindLocation,
			SourceID: fmt.Sprintf("rack/%d", r.ID),
			Name:     "Rack " + r.Name,
			Attrs:    attrs,
		})
	}

	for _, d := range devices {
		name := d.Name
		if name == "" {
			name = fmt.Sprintf("device-%d", d.ID)
		}
		attrs := map[string]any{
			"model":  strings.TrimSpace(d.Type.Manufacturer.Name + " " + d.Type.Model),
			"status": d.Status.Value,
		}
		if d.Role != nil {
			attrs["role"] = d.Role.Name
		} else if d.DeviceRole != nil {
			attrs["role"] = d.DeviceRole.Name
		}
		if d.Site != nil {
			attrs["site"] = d.Site.Name
		}
		if d.PrimaryIP != nil {
			if p, err := netip.ParsePrefix(d.PrimaryIP.Address); err == nil {
				attrs["ip"] = p.Addr().String()
			}
		}
		sid := fmt.Sprintf("device/%d", d.ID)
		res.Resources = append(res.Resources, model.Resource{
			Kind: model.KindDevice, SourceID: sid, Name: name, Attrs: attrs,
		})
		if d.Rack != nil && rackNames[d.Rack.ID] {
			res.Edges = append(res.Edges, model.Edge{
				Src:      model.Ref{SourceID: sid},
				Dst:      model.Ref{SourceID: fmt.Sprintf("rack/%d", d.Rack.ID)},
				Relation: model.RelLocatedIn,
			})
		}
	}

	return res, nil
}

// getAll follows NetBox's cursor pagination until exhausted.
func getAll[T any](ctx context.Context, c *Collector, path string) ([]T, error) {
	u := c.cfg.URL + path
	var out []T
	for u != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Token "+c.secret)
		req.Header.Set("Accept", "application/json")

		resp, err := c.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%s: GET %s: %w", c.instance, path, err)
		}
		var page struct {
			Next    *string `json:"next"`
			Results []T     `json:"results"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: GET %s: %s", c.instance, path, resp.Status)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: GET %s: decode: %w", c.instance, path, err)
		}
		out = append(out, page.Results...)
		u = ""
		if page.Next != nil && *page.Next != "" {
			u = *page.Next
		}
	}
	return out, nil
}
