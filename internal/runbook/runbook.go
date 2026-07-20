// Package runbook assembles and renders the artifact — the product.
//
// The design splits assembly from rendering: Build produces a Document (a
// renderer-agnostic tree of typed sections holding raw markdown), and
// separate HTML and Markdown renderers consume it. The artifact must
// survive the death of everything it documents, so renderers emit fully
// self-contained output: no links back to HRG, no external references.
package runbook

import (
	"context"
	"sort"
	"time"

	"github.com/breed007/hrg/internal/model"
	"github.com/breed007/hrg/internal/netmap"
	"github.com/breed007/hrg/internal/store"
)

// Guide identifies which of the two co-equal documents is being rendered.
// Both are generated from the same Document in the same run, so they can
// never disagree with each other.
type Guide string

const (
	// GuideHousehold is written for the person who did NOT build any of
	// this — a partner, family member, or executor. Plain language, short,
	// and focused on keeping the essentials alive or safely winding things
	// down.
	GuideHousehold Guide = "household"
	// GuideAdministrator is written for whoever actually does the work —
	// the technical friend the household calls, or future-you. Inventory,
	// topology, IP plan, recovery procedures.
	GuideAdministrator Guide = "administrator"
)

// Guides lists both, in generation order.
var Guides = []Guide{GuideHousehold, GuideAdministrator}

// Title is the human-facing name of the guide, used on its cover and in
// cross-references. Named for the READER, not the contents.
func (g Guide) Title() string {
	if g == GuideAdministrator {
		return "Administrator Guide"
	}
	return "Household Guide"
}

// Other returns the companion guide — used for cross-references, because
// the handoff moment (household calls their technical friend) is exactly
// when both documents are needed.
func (g Guide) Other() Guide {
	if g == GuideAdministrator {
		return GuideHousehold
	}
	return GuideAdministrator
}

// Slug is the filename stem for this guide's exports.
func (g Guide) Slug() string { return string(g) + "-guide" }

// Valid reports whether g is a known guide.
func (g Guide) Valid() bool {
	return g == GuideHousehold || g == GuideAdministrator
}

// Document is the assembled runbook, ready for any renderer.
type Document struct {
	Title       string
	GeneratedAt time.Time

	// Hand-authored pages; empty string = never written (renderers nag).
	// OverviewMD answers "what is all this?" in plain language — the first
	// thing a household reader needs and the one thing no collector knows.
	OverviewMD  string
	StartHereMD string
	ContactsMD  string

	Accounts  []Entry
	Locations []LocationGroup
	Unplaced  []Entry // devices/hosts with no located_in edge

	Prefixes   []netmap.PrefixRow
	IPs        []netmap.IPRow
	MermaidSrc string

	Services []Entry

	BackupJobs []BackupEntry
	Uncovered  []Entry // guests/containers with no backed_up_by edge

	Inventory []KindGroup // appendix: everything, orphans flagged
	Runs      []store.RunRow
	Coverage  *store.Coverage
}

// UnverifiedBackups counts backup jobs whose restore has never been tested.
func (d *Document) UnverifiedBackups() int {
	n := 0
	for _, b := range d.BackupJobs {
		if b.VerifiedAt == "" {
			n++
		}
	}
	return n
}

// Entry is one resource as the runbook presents it: identity, current
// attributes, its annotations (raw markdown), and its relationships.
type Entry struct {
	Name      string
	Kind      model.Kind
	Collector string
	SourceID  string
	Orphaned  bool
	Attrs     map[string]any

	PurposeMD  string
	RecoveryMD string
	CredMD     string
	NoteMD     string

	Edges []EdgeLine
}

// HasAnnotations reports whether any human knowledge is attached.
func (e Entry) HasAnnotations() bool {
	return e.PurposeMD != "" || e.RecoveryMD != "" || e.CredMD != "" || e.NoteMD != ""
}

// EdgeLine is one relationship, phrased from the entry's point of view.
type EdgeLine struct {
	Outbound bool
	Relation model.Relation
	PeerName string
	PeerKind model.Kind
	Origin   string
}

// LocationGroup is a physical place and what lives there.
type LocationGroup struct {
	Location Entry
	Items    []Entry
}

// BackupEntry is a backup job, what it covers, and when its restore was
// last verified. VerifiedAt is empty when never tested — the "last
// verified: never" flag the runbook prints in red.
type BackupEntry struct {
	Entry
	Covers     []string // names of covered resources
	VerifiedAt string   // RFC3339, or "" for never
}

// KindGroup is one appendix inventory section.
type KindGroup struct {
	Kind    model.Kind
	Entries []Entry
}

// serviceKinds appear in the service catalog.
var serviceKinds = map[model.Kind]bool{
	model.KindService: true, model.KindContainer: true,
	model.KindVM: true, model.KindLXC: true,
}

// backupNeedingKinds should be covered by some backup job.
var backupNeedingKinds = map[model.Kind]bool{
	model.KindVM: true, model.KindLXC: true, model.KindContainer: true,
}

// physicalKinds appear in the physical layer section.
var physicalKinds = map[model.Kind]bool{
	model.KindDevice: true, model.KindHost: true,
}

// Build assembles the document from current store state.
func Build(ctx context.Context, st *store.Store, title string) (*Document, error) {
	doc := &Document{Title: title, GeneratedAt: time.Now()}

	for slug, dst := range map[string]*string{
		"overview":   &doc.OverviewMD,
		"start_here": &doc.StartHereMD,
		"contacts":   &doc.ContactsMD,
	} {
		p, err := st.GetPage(ctx, slug)
		if err != nil {
			return nil, err
		}
		*dst = p.BodyMD
	}

	resources, err := st.ListResources(ctx, store.ListFilter{})
	if err != nil {
		return nil, err
	}
	edges, err := st.ListEdges(ctx)
	if err != nil {
		return nil, err
	}
	anns, err := st.AllAnnotations(ctx)
	if err != nil {
		return nil, err
	}
	doc.Runs, err = st.ListRuns(ctx, 20)
	if err != nil {
		return nil, err
	}
	doc.Coverage, err = st.Coverage(ctx)
	if err != nil {
		return nil, err
	}
	backupChecks, err := st.BackupChecks(ctx)
	if err != nil {
		return nil, err
	}

	byID := map[int64]store.ResourceRow{}
	for _, r := range resources {
		byID[r.ID] = r
	}

	// Adjacency, phrased per endpoint.
	edgeLines := map[int64][]EdgeLine{}
	for _, e := range edges {
		src, sok := byID[e.SrcID]
		dst, dok := byID[e.DstID]
		if !sok || !dok {
			continue
		}
		edgeLines[e.SrcID] = append(edgeLines[e.SrcID], EdgeLine{
			Outbound: true, Relation: e.Relation, PeerName: dst.Name, PeerKind: dst.Kind,
		})
		edgeLines[e.DstID] = append(edgeLines[e.DstID], EdgeLine{
			Outbound: false, Relation: e.Relation, PeerName: src.Name, PeerKind: src.Kind,
		})
	}

	entry := func(r store.ResourceRow) Entry {
		en := Entry{
			Name: r.Name, Kind: r.Kind, Collector: r.Collector,
			SourceID: r.SourceID, Orphaned: r.Orphaned, Attrs: r.Attrs,
			Edges: edgeLines[r.ID],
		}
		if a := anns[r.ID]; a != nil {
			en.PurposeMD = a["purpose"].BodyMD
			en.RecoveryMD = a["recovery"].BodyMD
			en.CredMD = a["credential_pointer"].BodyMD
			en.NoteMD = a["note"].BodyMD
		}
		return en
	}

	// Precompute edge-derived lookups.
	locatedIn := map[int64]bool{}     // resource has an outbound located_in
	locMembers := map[int64][]int64{} // location id -> member ids
	backedUp := map[int64]bool{}      // resource has an outbound backed_up_by
	jobCovers := map[int64][]int64{}  // backup job id -> covered ids
	for _, e := range edges {
		switch e.Relation {
		case model.RelLocatedIn:
			locatedIn[e.SrcID] = true
			locMembers[e.DstID] = append(locMembers[e.DstID], e.SrcID)
		case model.RelBackedUpBy:
			backedUp[e.SrcID] = true
			jobCovers[e.DstID] = append(jobCovers[e.DstID], e.SrcID)
		}
	}

	for _, r := range resources {
		if r.Orphaned {
			continue
		}
		switch {
		case r.Kind == model.KindAccount:
			doc.Accounts = append(doc.Accounts, entry(r))

		case r.Kind == model.KindLocation:
			lg := LocationGroup{Location: entry(r)}
			for _, mid := range locMembers[r.ID] {
				if m, ok := byID[mid]; ok && !m.Orphaned {
					lg.Items = append(lg.Items, entry(m))
				}
			}
			sort.Slice(lg.Items, func(i, j int) bool { return lg.Items[i].Name < lg.Items[j].Name })
			doc.Locations = append(doc.Locations, lg)

		case r.Kind == model.KindBackupJob:
			be := BackupEntry{Entry: entry(r), VerifiedAt: backupChecks[r.ID]}
			for _, cid := range jobCovers[r.ID] {
				if c, ok := byID[cid]; ok && !c.Orphaned {
					be.Covers = append(be.Covers, c.Name)
				}
			}
			sort.Strings(be.Covers)
			doc.BackupJobs = append(doc.BackupJobs, be)
		}

		if physicalKinds[r.Kind] && !locatedIn[r.ID] {
			doc.Unplaced = append(doc.Unplaced, entry(r))
		}
		if serviceKinds[r.Kind] {
			doc.Services = append(doc.Services, entry(r))
		}
		if backupNeedingKinds[r.Kind] && !backedUp[r.ID] {
			doc.Uncovered = append(doc.Uncovered, entry(r))
		}
	}

	// Appendix: everything, grouped by kind, orphans included and flagged.
	groups := map[model.Kind][]Entry{}
	for _, r := range resources {
		groups[r.Kind] = append(groups[r.Kind], entry(r))
	}
	for _, k := range model.Kinds {
		if len(groups[k]) == 0 {
			continue
		}
		g := groups[k]
		sort.Slice(g, func(i, j int) bool { return g[i].Name < g[j].Name })
		doc.Inventory = append(doc.Inventory, KindGroup{Kind: k, Entries: g})
	}

	doc.Prefixes, doc.IPs = netmap.Reconcile(resources)
	doc.MermaidSrc = netmap.Mermaid(resources, edges, netmap.MermaidOptions{Links: false})

	return doc, nil
}
