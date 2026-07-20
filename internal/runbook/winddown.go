package runbook

import (
	"sort"

	"github.com/breed007/hrg/internal/store"
)

// buildWindDown answers the question the inventory cannot: if I switch this
// off, what else goes with it?
//
// The graph records "A runs on B" / "A depends on B" — pointing from the
// dependent to the thing it needs. Wind-down asks the reverse: given B,
// which A's fall over? So we invert the edges and walk transitively, since
// a household reader has no way to chase a three-hop chain themselves.
func buildWindDown(
	resources []store.ResourceRow,
	edges []store.EdgePair,
	byID map[int64]store.ResourceRow,
	entry func(store.ResourceRow) Entry,
) WindDownPlan {
	// dependents[b] = things that stop working if b goes away.
	dependents := map[int64][]int64{}
	for _, e := range edges {
		if !breakingRelations[e.Relation] {
			continue
		}
		if _, ok := byID[e.SrcID]; !ok {
			continue
		}
		if _, ok := byID[e.DstID]; !ok {
			continue
		}
		dependents[e.DstID] = append(dependents[e.DstID], e.SrcID)
	}

	// blastRadius walks the inverted graph. The seen set doubles as cycle
	// protection — a mutual depends_on pair must not spin forever.
	blastRadius := func(root int64) []int64 {
		seen := map[int64]bool{root: true}
		var out []int64
		queue := []int64{root}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, dep := range dependents[cur] {
				if seen[dep] {
					continue
				}
				seen[dep] = true
				out = append(out, dep)
				queue = append(queue, dep)
			}
		}
		return out
	}

	importance := map[int64]string{}
	for _, r := range resources {
		importance[r.ID] = entry(r).Importance
	}

	var plan WindDownPlan
	for _, r := range resources {
		if r.Orphaned || !windDownKinds[r.Kind] {
			continue
		}
		w := WindDown{Entry: entry(r)}
		for _, id := range blastRadius(r.ID) {
			dep, ok := byID[id]
			if !ok || dep.Orphaned {
				continue
			}
			w.Breaks = append(w.Breaks, dep.Name)
			if importance[id] == store.ImportanceEssential {
				w.EssentialBreaks = append(w.EssentialBreaks, dep.Name)
			}
		}
		sort.Strings(w.Breaks)
		sort.Strings(w.EssentialBreaks)

		switch {
		case w.Importance == store.ImportanceEssential || len(w.EssentialBreaks) > 0:
			// Either the house needs it, or the house needs something
			// standing on top of it. Both mean: leave it alone.
			plan.Keep = append(plan.Keep, w)
		case w.Importance == "":
			// Nobody said. The guide must not guess on the reader's behalf.
			plan.Unknown = append(plan.Unknown, w)
		default:
			plan.Safe = append(plan.Safe, w)
		}
	}

	// Within each group: the things the household cares about most first,
	// then alphabetically. In Safe, that puts "just a project" last —
	// exactly the order you'd switch things off in.
	byImportanceThenName := func(g []WindDown) {
		sort.SliceStable(g, func(i, j int) bool {
			ri, rj := store.ImportanceRank(g[i].Importance), store.ImportanceRank(g[j].Importance)
			if ri != rj {
				return ri < rj
			}
			return g[i].Name < g[j].Name
		})
	}
	byImportanceThenName(plan.Keep)
	byImportanceThenName(plan.Safe)
	byImportanceThenName(plan.Unknown)
	return plan
}
