package collector

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Spec is everything needed to build one configured collector instance.
type Spec struct {
	Type     string          // registered collector type, e.g. "proxmox"
	Instance string          // full provenance name, e.g. "proxmox:pve1"
	Config   json.RawMessage // non-secret configuration
	Secret   string          // decrypted API token ("" if none)
}

// Factory builds a collector from a spec. Factories must validate their
// config and fail fast — a misconfigured collector should error at build
// time, not mid-run.
type Factory func(Spec) (Collector, error)

var factories = map[string]Factory{}

// Register makes a collector type buildable. Call once per type at startup.
func Register(typ string, f Factory) {
	if _, dup := factories[typ]; dup {
		panic(fmt.Sprintf("collector type %q registered twice", typ))
	}
	factories[typ] = f
}

// Build constructs a collector instance from its spec.
func Build(spec Spec) (Collector, error) {
	f, ok := factories[spec.Type]
	if !ok {
		return nil, fmt.Errorf("unknown collector type %q", spec.Type)
	}
	return f(spec)
}

// Types lists registered collector types, sorted, for the config UI.
func Types() []string {
	out := make([]string, 0, len(factories))
	for t := range factories {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
