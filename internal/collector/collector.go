// Package collector defines the interface every collector implements.
//
// Collectors are read-only observers: they report the world as they see it
// and never mutate the systems they document. The engine (internal/store)
// owns diffing, versioning, run bookkeeping, and orphan detection —
// a collector's only hard obligation is emitting stable SourceIDs.
package collector

import (
	"context"

	"github.com/breed007/hrg/internal/model"
)

// Collector observes one source of infrastructure truth.
type Collector interface {
	// Name is the collector instance name used as the provenance key,
	// e.g. "manual" or "proxmox:pve1". It must be unique per instance
	// and stable across runs — resource identity is (Name, SourceID).
	Name() string

	// Collect returns everything the collector can currently see.
	// It must not carry state between calls; each call is a full snapshot.
	Collect(ctx context.Context) (Result, error)
}

// Result is one complete observation of a collector's world.
type Result struct {
	Resources []model.Resource
	Edges     []model.Edge

	// Warnings are non-fatal problems encountered while collecting — a
	// single malformed input file, a resource that failed validation, an
	// edge to an unknown target. A collector should skip the bad item,
	// keep the good ones, and record why here rather than failing the
	// whole run. The engine attaches these to the run so they surface in
	// the UI. A collector that returns Warnings with a nil error still
	// ingests successfully.
	Warnings []string
}
