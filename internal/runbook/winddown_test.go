package runbook

import (
	"context"
	"strings"
	"testing"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/model"
	"github.com/breed007/hrg/internal/store"
)

// windDownStore builds a chain deliberately deeper than one hop:
//
//	photos --runs_on--> docker --runs_on--> nas
//	k3s-lab --runs_on--> docker
//	loop-a <--depends_on--> loop-b   (a cycle, to prove termination)
//
// "photos" is essential, so switching off the NAS two hops below it must
// still be flagged — that transitive reach is the whole point.
func windDownStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	res := collector.Result{
		Resources: []model.Resource{
			{Kind: model.KindHost, SourceID: "nas", Name: "NAS"},
			{Kind: model.KindVM, SourceID: "docker", Name: "docker-host"},
			{Kind: model.KindService, SourceID: "photos", Name: "Photos"},
			{Kind: model.KindService, SourceID: "k3s-lab", Name: "k3s lab"},
			{Kind: model.KindService, SourceID: "mystery", Name: "Mystery box"},
			{Kind: model.KindService, SourceID: "loop-a", Name: "Loop A"},
			{Kind: model.KindService, SourceID: "loop-b", Name: "Loop B"},
		},
		Edges: []model.Edge{
			{Src: model.Ref{SourceID: "docker"}, Dst: model.Ref{SourceID: "nas"}, Relation: model.RelRunsOn},
			{Src: model.Ref{SourceID: "photos"}, Dst: model.Ref{SourceID: "docker"}, Relation: model.RelRunsOn},
			{Src: model.Ref{SourceID: "k3s-lab"}, Dst: model.Ref{SourceID: "docker"}, Relation: model.RelRunsOn},
			{Src: model.Ref{SourceID: "loop-a"}, Dst: model.Ref{SourceID: "loop-b"}, Relation: model.RelDependsOn},
			{Src: model.Ref{SourceID: "loop-b"}, Dst: model.Ref{SourceID: "loop-a"}, Relation: model.RelDependsOn},
		},
	}
	if _, err := st.Ingest(ctx, "test", res); err != nil {
		t.Fatal(err)
	}

	ids := map[string]int64{}
	rows, _ := st.ListResources(ctx, store.ListFilter{})
	for _, r := range rows {
		ids[r.SourceID] = r.ID
	}
	ann := func(src, field, body string) {
		t.Helper()
		if err := st.SetAnnotation(ctx, ids[src], field, body); err != nil {
			t.Fatal(err)
		}
	}
	ann("photos", "household_importance", store.ImportanceEssential)
	ann("k3s-lab", "household_importance", store.ImportanceExperimental)
	ann("loop-a", "household_importance", store.ImportanceExperimental)
	ann("loop-b", "household_importance", store.ImportanceExperimental)
	// docker and nas are unclassified on purpose: the plan must place them
	// by consequence, not by label.
	// mystery has no classification and no edges — the honest "ask first".
	return st
}

func names(items []WindDown) []string {
	out := make([]string, len(items))
	for i, w := range items {
		out[i] = w.Name
	}
	return out
}

func find(t *testing.T, items []WindDown, name string) WindDown {
	t.Helper()
	for _, w := range items {
		if w.Name == name {
			return w
		}
	}
	t.Fatalf("%q not found in %v", name, names(items))
	return WindDown{}
}

func TestWindDownTransitiveAndCycleSafe(t *testing.T) {
	doc, err := Build(context.Background(), windDownStore(t), "T")
	if err != nil {
		t.Fatal(err)
	}
	p := doc.WindDown

	// NAS is unclassified and two hops below an essential service. It must
	// land in Keep on consequence alone.
	nas := find(t, p.Keep, "NAS")
	if !contains(nas.EssentialBreaks, "Photos") {
		t.Errorf("NAS blast radius missed the essential service two hops up: %v", nas.Breaks)
	}
	if !contains(nas.Breaks, "docker-host") || !contains(nas.Breaks, "k3s lab") {
		t.Errorf("NAS blast radius not transitive: %v", nas.Breaks)
	}

	// The cycle must terminate and must not list the root as its own victim.
	a := find(t, p.Safe, "Loop A")
	if len(a.Breaks) != 1 || a.Breaks[0] != "Loop B" {
		t.Errorf("cycle handled wrong: %v", a.Breaks)
	}

	// Experimental with nothing above it is genuinely safe.
	k3s := find(t, p.Safe, "k3s lab")
	if len(k3s.Breaks) != 0 {
		t.Errorf("k3s lab should break nothing: %v", k3s.Breaks)
	}

	// Unclassified and inconsequential: the guide must not guess.
	find(t, p.Unknown, "Mystery box")
	for _, group := range [][]WindDown{p.Keep, p.Safe} {
		for _, w := range group {
			if w.Name == "Mystery box" {
				t.Error("unclassified, harmless resource must not be classified for the reader")
			}
		}
	}
}

func TestWindDownAuthorOverridesInference(t *testing.T) {
	ctx := context.Background()
	st := windDownStore(t)
	rows, _ := st.ListResources(ctx, store.ListFilter{})
	for _, r := range rows {
		if r.SourceID == "nas" {
			if err := st.SetAnnotation(ctx, r.ID, "safe_to_off",
				"Don't. The photos and the TVs all come off this box."); err != nil {
				t.Fatal(err)
			}
		}
	}
	doc, err := Build(ctx, st, "T")
	if err != nil {
		t.Fatal(err)
	}
	nas := find(t, doc.WindDown.Keep, "NAS")
	if nas.Inferred() {
		t.Error("hand-written safe_to_off should not be reported as inferred")
	}

	html, err := RenderHTML(doc, GuideHousehold, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := string(html)
	if !strings.Contains(got, "The photos and the TVs all come off this box.") {
		t.Error("household guide dropped the author's own wind-down sentence")
	}
	// The inference disclaimer must not appear for a resource that has a
	// hand-written answer — it would undercut the author's own words.
	nasBlock := got[strings.Index(got, "NAS"):]
	if end := strings.Index(nasBlock, "wd-item"); end > 0 {
		if strings.Contains(nasBlock[:end], "worked out from how things connect") {
			t.Error("inference disclaimer shown alongside a hand-written answer")
		}
	}
}

func TestWindDownEmptyNags(t *testing.T) {
	doc, err := Build(context.Background(), seed(t), "T")
	if err != nil {
		t.Fatal(err)
	}
	html, err := RenderHTML(doc, GuideHousehold, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The seed fixture has one runs_on edge and no classifications, so the
	// section is thin but not empty — it must still render, never claim
	// that unclassified things are safe.
	if strings.Contains(string(html), "Safe to switch off") {
		t.Error("nothing is classified, yet the guide told the reader something was safe to switch off")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
