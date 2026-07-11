// Package netmap derives network views from the resource graph: the
// documented-vs-observed IP plan reconciliation and the Mermaid topology
// diagram. Shared by the web UI and the runbook artifact renderer so both
// always agree.
package netmap

import (
	"fmt"
	"html"
	"net/netip"
	"sort"
	"strings"

	"github.com/breed007/hrg/internal/model"
	"github.com/breed007/hrg/internal/store"
)

// Ref points at a resource involved in a reconciliation row.
type Ref struct {
	ResourceID int64
	Name       string
	Collector  string
}

// PrefixRow reconciles one subnet across documented and observed sources.
type PrefixRow struct {
	CIDR       string
	Documented *Ref // NetBox prefix, nil if undocumented
	Detail     string
	Observed   []Ref
	Status     string // ok | undocumented | unobserved
}

// IPRow reconciles one address.
type IPRow struct {
	IP         string
	Documented *Ref
	DNS        string
	Observed   []Ref
	Status     string
}

// CollectorType extracts the type from an instance name ("unifi:home" →
// "unifi").
func CollectorType(instance string) string {
	t, _, _ := strings.Cut(instance, ":")
	return t
}

// HaveAuthority reports whether any resource comes from the authoritative
// IP-plan source (NetBox).
func HaveAuthority(resources []store.ResourceRow) bool {
	for _, r := range resources {
		if CollectorType(r.Collector) == "netbox" {
			return true
		}
	}
	return false
}

// Reconcile matches the documented IP plan (NetBox — authoritative) against
// what every other collector observes, by normalized CIDR and address.
func Reconcile(resources []store.ResourceRow) ([]PrefixRow, []IPRow) {
	prefixRows := map[netip.Prefix]*PrefixRow{}
	ipRows := map[netip.Addr]*IPRow{}

	prefixAt := func(p netip.Prefix) *PrefixRow {
		if row, ok := prefixRows[p]; ok {
			return row
		}
		row := &PrefixRow{CIDR: p.String()}
		prefixRows[p] = row
		return row
	}
	ipAt := func(a netip.Addr) *IPRow {
		if row, ok := ipRows[a]; ok {
			return row
		}
		row := &IPRow{IP: a.String()}
		ipRows[a] = row
		return row
	}

	for _, r := range resources {
		if r.Orphaned {
			continue
		}
		authoritative := CollectorType(r.Collector) == "netbox"
		ref := Ref{ResourceID: r.ID, Name: r.Name, Collector: r.Collector}

		if cidr, ok := r.Attrs["cidr"].(string); ok {
			if p, err := netip.ParsePrefix(cidr); err == nil {
				row := prefixAt(p.Masked())
				if authoritative {
					row.Documented = &ref
					var parts []string
					if d, ok := r.Attrs["description"].(string); ok && d != r.Name {
						parts = append(parts, d)
					}
					if st, ok := r.Attrs["status"].(string); ok {
						parts = append(parts, st)
					}
					row.Detail = strings.Join(parts, " · ")
				} else {
					row.Observed = append(row.Observed, ref)
				}
			}
		}

		// NetBox prefixes carry their IP assignments inline.
		if authoritative {
			if addrs, ok := r.Attrs["addresses"].([]any); ok {
				for _, a := range addrs {
					m, ok := a.(map[string]any)
					if !ok {
						continue
					}
					ipStr, _ := m["ip"].(string)
					addr, err := netip.ParseAddr(ipStr)
					if err != nil {
						continue
					}
					row := ipAt(addr)
					row.Documented = &ref
					if dns, ok := m["dns"].(string); ok {
						row.DNS = dns
					}
				}
			}
		}

		if ipStr, ok := r.Attrs["ip"].(string); ok {
			if addr, err := netip.ParseAddr(ipStr); err == nil {
				row := ipAt(addr)
				if authoritative {
					if row.Documented == nil {
						row.Documented = &ref
					}
					if row.DNS == "" {
						row.DNS = r.Name
					}
				} else {
					row.Observed = append(row.Observed, ref)
				}
			}
		}
	}

	pOut := make([]PrefixRow, 0, len(prefixRows))
	pKeys := make([]netip.Prefix, 0, len(prefixRows))
	for k := range prefixRows {
		pKeys = append(pKeys, k)
	}
	sort.Slice(pKeys, func(i, j int) bool {
		if c := pKeys[i].Addr().Compare(pKeys[j].Addr()); c != 0 {
			return c < 0
		}
		return pKeys[i].Bits() < pKeys[j].Bits()
	})
	for _, k := range pKeys {
		row := prefixRows[k]
		row.Status = status(row.Documented != nil, len(row.Observed) > 0)
		pOut = append(pOut, *row)
	}

	iOut := make([]IPRow, 0, len(ipRows))
	iKeys := make([]netip.Addr, 0, len(ipRows))
	for k := range ipRows {
		iKeys = append(iKeys, k)
	}
	sort.Slice(iKeys, func(i, j int) bool { return iKeys[i].Compare(iKeys[j]) < 0 })
	for _, k := range iKeys {
		row := ipRows[k]
		row.Status = status(row.Documented != nil, len(row.Observed) > 0)
		iOut = append(iOut, *row)
	}
	return pOut, iOut
}

func status(documented, observed bool) string {
	switch {
	case documented && observed:
		return "ok"
	case observed:
		return "undocumented"
	default:
		return "unobserved"
	}
}

// topologyKinds are the network-layer kinds drawn on the map.
var topologyKinds = map[model.Kind]bool{
	model.KindHost:     true,
	model.KindDevice:   true,
	model.KindNetwork:  true,
	model.KindVLAN:     true,
	model.KindLocation: true,
}

// MermaidOptions tunes diagram generation per consumer.
type MermaidOptions struct {
	// Links adds click-through URLs to /resources/{id}. Only the live web
	// UI wants these — the artifact must not reference the app.
	Links bool
}

// Mermaid renders the network-layer resource graph as a Mermaid flowchart.
// Returns "" when there is nothing to draw.
func Mermaid(resources []store.ResourceRow, edges []store.EdgePair, opts MermaidOptions) string {
	nodes := map[int64]store.ResourceRow{}
	for _, r := range resources {
		if topologyKinds[r.Kind] && !r.Orphaned {
			nodes[r.ID] = r
		}
	}
	if len(nodes) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("flowchart TD\n")
	b.WriteString("  classDef device fill:#2f6f4f,color:#fff,stroke:none\n")
	b.WriteString("  classDef host fill:#3d5a80,color:#fff,stroke:none\n")
	b.WriteString("  classDef network fill:#8a5a12,color:#fff,stroke:none\n")
	b.WriteString("  classDef vlan fill:#8a5a12,color:#fff,stroke:none\n")
	b.WriteString("  classDef location fill:#666,color:#fff,stroke-dasharray:4\n")

	for _, r := range resources {
		if _, ok := nodes[r.ID]; !ok {
			continue
		}
		label := html.EscapeString(r.Name) + "<br/><small>" + string(r.Kind) + kindDetail(r) + "</small>"
		fmt.Fprintf(&b, "  n%d[\"%s\"]:::%s\n", r.ID, label, r.Kind)
		if opts.Links {
			fmt.Fprintf(&b, "  click n%d \"/resources/%d\"\n", r.ID, r.ID)
		}
	}

	for _, e := range edges {
		if _, ok := nodes[e.SrcID]; !ok {
			continue
		}
		if _, ok := nodes[e.DstID]; !ok {
			continue
		}
		arrow := "---"
		switch e.Relation {
		case model.RelMemberOf, model.RelLocatedIn:
			arrow = "-.-"
		case model.RelRunsOn, model.RelDependsOn:
			arrow = "-->"
		}
		fmt.Fprintf(&b, "  n%d %s n%d\n", e.SrcID, arrow, e.DstID)
	}
	return b.String()
}

// kindDetail appends the discriminating attribute to a node's kind line:
// VLANs show their subnet, devices their IP.
func kindDetail(r store.ResourceRow) string {
	if v, ok := r.Attrs["cidr"].(string); ok && v != "" {
		return " · " + html.EscapeString(v)
	}
	if v, ok := r.Attrs["ip"].(string); ok && v != "" {
		return " · " + html.EscapeString(v)
	}
	return ""
}
