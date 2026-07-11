package model

import "testing"

func TestContentHashStable(t *testing.T) {
	a := Resource{Kind: KindService, SourceID: "plex", Name: "Plex",
		Attrs: map[string]any{"port": 32400, "nested": map[string]any{"x": 1, "y": 2}}}
	b := Resource{Kind: KindService, SourceID: "plex", Name: "Plex",
		Attrs: map[string]any{"nested": map[string]any{"y": 2, "x": 1}, "port": 32400}}

	ha, err := a.ContentHash()
	if err != nil {
		t.Fatal(err)
	}
	hb, err := b.ContentHash()
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Errorf("hash not stable under map ordering: %s != %s", ha, hb)
	}
}

func TestContentHashChanges(t *testing.T) {
	base := Resource{Kind: KindService, SourceID: "plex", Name: "Plex",
		Attrs: map[string]any{"port": 32400}}
	h0, _ := base.ContentHash()

	renamed := base
	renamed.Name = "Plex Media Server"
	h1, _ := renamed.ContentHash()
	if h0 == h1 {
		t.Error("name change did not change hash")
	}

	rekind := base
	rekind.Kind = KindContainer
	h2, _ := rekind.ContentHash()
	if h0 == h2 {
		t.Error("kind change did not change hash")
	}

	reattr := Resource{Kind: KindService, SourceID: "plex", Name: "Plex",
		Attrs: map[string]any{"port": 32401}}
	h3, _ := reattr.ContentHash()
	if h0 == h3 {
		t.Error("attr change did not change hash")
	}
}

func TestValidate(t *testing.T) {
	ok := Resource{Kind: KindHost, SourceID: "pve1", Name: "pve1"}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid resource rejected: %v", err)
	}
	for _, bad := range []Resource{
		{Kind: KindHost, Name: "no source id"},
		{Kind: "flurb", SourceID: "x", Name: "bad kind"},
		{Kind: KindHost, SourceID: "x"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("invalid resource accepted: %+v", bad)
		}
	}

	if err := (Edge{Src: Ref{SourceID: "a"}, Dst: Ref{SourceID: "b"}, Relation: RelDependsOn}).Validate(); err != nil {
		t.Errorf("valid edge rejected: %v", err)
	}
	if err := (Edge{Src: Ref{SourceID: "a"}, Dst: Ref{SourceID: "b"}, Relation: "eats"}).Validate(); err == nil {
		t.Error("invalid relation accepted")
	}
}
