// Package model defines the resource graph vocabulary shared by collectors,
// the store, and the web UI: resource kinds, edge relations, and the identity
// and hashing rules that make annotations survive re-collection.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Kind classifies a resource. Collectors must use one of these values;
// anything that fits nowhere else is KindOther with a descriptive name.
type Kind string

const (
	KindHost      Kind = "host"
	KindVM        Kind = "vm"
	KindLXC       Kind = "lxc"
	KindContainer Kind = "container"
	KindNetwork   Kind = "network"
	KindVLAN      Kind = "vlan"
	KindService   Kind = "service"
	KindStorage   Kind = "storage"
	KindBackupJob Kind = "backup_job"
	KindDevice    Kind = "device"
	KindAccount   Kind = "account"
	KindLocation  Kind = "location"
	KindOther     Kind = "other"
)

// Kinds lists every valid Kind, in display order.
var Kinds = []Kind{
	KindHost, KindVM, KindLXC, KindContainer, KindNetwork, KindVLAN,
	KindService, KindStorage, KindBackupJob, KindDevice, KindAccount,
	KindLocation, KindOther,
}

func (k Kind) Valid() bool {
	for _, v := range Kinds {
		if k == v {
			return true
		}
	}
	return false
}

// Relation types an edge between two resources.
type Relation string

const (
	RelRunsOn     Relation = "runs_on"
	RelAttachedTo Relation = "attached_to"
	RelMemberOf   Relation = "member_of"
	RelDependsOn  Relation = "depends_on"
	RelBackedUpBy Relation = "backed_up_by"
	RelResolvesTo Relation = "resolves_to"
	RelLocatedIn  Relation = "located_in"
)

// Relations lists every valid Relation.
var Relations = []Relation{
	RelRunsOn, RelAttachedTo, RelMemberOf, RelDependsOn,
	RelBackedUpBy, RelResolvesTo, RelLocatedIn,
}

func (r Relation) Valid() bool {
	for _, v := range Relations {
		if r == v {
			return true
		}
	}
	return false
}

// Ref names a resource by its stable identity. An empty Collector means
// "the collector emitting this ref" and is resolved at ingest time.
type Ref struct {
	Collector string
	SourceID  string
}

func (r Ref) String() string {
	if r.Collector == "" {
		return r.SourceID
	}
	return r.Collector + "/" + r.SourceID
}

// Resource is one observed node in the infrastructure graph.
//
// SourceID is the collector's contract: it MUST be stable across runs for
// the same logical thing, because annotations are keyed to it. Attrs is
// freeform JSON-compatible data (string-keyed maps, slices, scalars) whose
// shape follows per-kind conventions.
type Resource struct {
	Kind     Kind
	SourceID string
	Name     string
	Attrs    map[string]any
}

// ContentHash returns a stable digest of the resource's observable state
// (kind, name, attrs). The store opens a new version only when this changes.
// encoding/json sorts map keys at every level, so semantically equal Attrs
// hash equally regardless of construction order.
func (r Resource) ContentHash() (string, error) {
	attrs, err := json.Marshal(r.Attrs)
	if err != nil {
		return "", fmt.Errorf("resource %q: attrs not JSON-encodable: %w", r.SourceID, err)
	}
	h := sha256.New()
	h.Write([]byte(r.Kind))
	h.Write([]byte{0})
	h.Write([]byte(r.Name))
	h.Write([]byte{0})
	h.Write(attrs)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Validate checks the invariants every collector must uphold.
func (r Resource) Validate() error {
	if r.SourceID == "" {
		return fmt.Errorf("resource %q: missing source id", r.Name)
	}
	if !r.Kind.Valid() {
		return fmt.Errorf("resource %q: unknown kind %q", r.SourceID, r.Kind)
	}
	if r.Name == "" {
		return fmt.Errorf("resource %q: missing name", r.SourceID)
	}
	return nil
}

// Edge is a typed, directed relationship between two resources.
type Edge struct {
	Src      Ref
	Dst      Ref
	Relation Relation
}

func (e Edge) Validate() error {
	if e.Src.SourceID == "" || e.Dst.SourceID == "" {
		return fmt.Errorf("edge %s -[%s]-> %s: missing endpoint", e.Src, e.Relation, e.Dst)
	}
	if !e.Relation.Valid() {
		return fmt.Errorf("edge %s -> %s: unknown relation %q", e.Src, e.Dst, e.Relation)
	}
	return nil
}
