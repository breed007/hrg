// Package manual implements the Manual/YAML collector: a resources.d/
// directory of YAML files describing anything without an API — the ISP
// account, the UPS on the shelf, the modem in the basement closet.
//
// It doubles as the reference collector implementation. File format:
//
//	resources:
//	  - id: modem            # stable source id — annotations key off this; never rename casually
//	    kind: device
//	    name: Arris SB8200 cable modem
//	    attrs:               # freeform; follows per-kind conventions
//	      location: basement closet
//	    edges:
//	      - relation: located_in
//	        to: basement-closet                      # same-file/dir shorthand
//	      - relation: attached_to
//	        to: { collector: "unifi:home", id: "device/udm-pro" }   # cross-collector
package manual

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/model"
)

// Name is the collector instance name. There is exactly one manual
// collector per HRG instance.
const Name = "manual"

type Collector struct {
	dir string
}

func New(dir string) *Collector {
	return &Collector{dir: dir}
}

func (c *Collector) Name() string { return Name }

// Collect parses every *.yaml / *.yml file under the resources.d directory.
// A missing directory is an empty result, not an error — a fresh install
// has nothing yet.
func (c *Collector) Collect(ctx context.Context) (collector.Result, error) {
	var res collector.Result

	entries, err := os.ReadDir(c.dir)
	if os.IsNotExist(err) {
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("read %s: %w", c.dir, err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := filepath.Ext(e.Name()); ext == ".yaml" || ext == ".yml" {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files) // deterministic order → deterministic error reporting

	definedIn := map[string]string{} // source id -> file, for duplicate detection
	type localEdge struct {
		file string
		edge model.Edge
	}
	var edges []localEdge

	// Each file is parsed independently: one malformed or invalid file
	// records a warning and is skipped, but never discards the resources in
	// the other files. A fat-fingered YAML edit must not blank out the
	// whole manual inventory.
	for _, name := range files {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		path := filepath.Join(c.dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v — skipped", name, err))
			continue
		}
		var doc fileDoc
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v — file skipped", name, err))
			continue
		}
		for _, rd := range doc.Resources {
			if prev, dup := definedIn[rd.ID]; dup {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: resource id %q already defined in %s — skipped", name, rd.ID, prev))
				continue
			}

			r := model.Resource{
				Kind:     model.Kind(rd.Kind),
				SourceID: rd.ID,
				Name:     rd.Name,
				Attrs:    rd.Attrs,
			}
			if err := r.Validate(); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v — resource skipped", name, err))
				continue
			}
			definedIn[rd.ID] = name
			res.Resources = append(res.Resources, r)

			for _, ed := range rd.Edges {
				e := model.Edge{
					Src:      model.Ref{SourceID: rd.ID},
					Dst:      model.Ref{Collector: ed.To.Collector, SourceID: ed.To.ID},
					Relation: model.Relation(ed.Relation),
				}
				if err := e.Validate(); err != nil {
					res.Warnings = append(res.Warnings, fmt.Sprintf("%s: resource %q: %v — edge skipped", name, rd.ID, err))
					continue
				}
				edges = append(edges, localEdge{file: name, edge: e})
			}
		}
	}

	// Same-collector refs must resolve within the directory — a typo'd
	// local ref is a warning and the edge is dropped, not a run-killer.
	// Cross-collector refs are allowed to dangle (parked as pending).
	for _, le := range edges {
		if le.edge.Dst.Collector == "" {
			if _, ok := definedIn[le.edge.Dst.SourceID]; !ok {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: resource %q: edge references unknown local resource %q — edge dropped (define it, or use {collector: ..., id: ...} for a cross-collector ref)",
					le.file, le.edge.Src.SourceID, le.edge.Dst.SourceID))
				continue
			}
		}
		res.Edges = append(res.Edges, le.edge)
	}

	return res, nil
}

type fileDoc struct {
	Resources []resourceDoc `yaml:"resources"`
}

type resourceDoc struct {
	ID    string         `yaml:"id"`
	Kind  string         `yaml:"kind"`
	Name  string         `yaml:"name"`
	Attrs map[string]any `yaml:"attrs"`
	Edges []edgeDoc      `yaml:"edges"`
}

type edgeDoc struct {
	Relation string `yaml:"relation"`
	To       refDoc `yaml:"to"`
}

// refDoc accepts either a bare string (same-collector shorthand) or a
// {collector, id} mapping for cross-collector references.
type refDoc struct {
	Collector string
	ID        string
}

func (r *refDoc) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		return node.Decode(&r.ID)
	case yaml.MappingNode:
		var m struct {
			Collector string `yaml:"collector"`
			ID        string `yaml:"id"`
		}
		if err := node.Decode(&m); err != nil {
			return err
		}
		if m.Collector == "" || m.ID == "" {
			return fmt.Errorf("line %d: cross-collector ref needs both 'collector' and 'id'", node.Line)
		}
		r.Collector, r.ID = m.Collector, m.ID
		return nil
	default:
		return fmt.Errorf("line %d: edge 'to' must be a string or {collector, id} mapping", node.Line)
	}
}
