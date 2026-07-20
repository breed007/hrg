package runbook

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/model"
	"github.com/breed007/hrg/internal/store"
)

// seed builds a small but structurally complete homelab in an in-memory
// store: location + device chain, service with annotations, backup job
// with coverage, an uncovered guest, and an orphan.
func seed(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	res := collector.Result{
		Resources: []model.Resource{
			{Kind: model.KindLocation, SourceID: "closet", Name: "Basement closet",
				Attrs: map[string]any{"directions": "down the stairs, first door left"}},
			{Kind: model.KindDevice, SourceID: "modem", Name: "Cable modem",
				Attrs: map[string]any{"power_cycle": "unplug 30s", "ip": "10.0.1.9"}},
			{Kind: model.KindDevice, SourceID: "shelf-thing", Name: "Unplaced widget"},
			{Kind: model.KindAccount, SourceID: "isp", Name: "ISP account"},
			{Kind: model.KindService, SourceID: "plex", Name: "Plex"},
			{Kind: model.KindVM, SourceID: "vm1", Name: "docker-host"},
			{Kind: model.KindVM, SourceID: "vm2", Name: "win-test"},
			{Kind: model.KindBackupJob, SourceID: "job1", Name: "nightly vzdump",
				Attrs: map[string]any{"schedule": "02:00", "storage": "nas"}},
			{Kind: model.KindVLAN, SourceID: "vlan10", Name: "Servers",
				Attrs: map[string]any{"cidr": "10.0.10.0/24"}},
		},
		Edges: []model.Edge{
			{Src: model.Ref{SourceID: "modem"}, Dst: model.Ref{SourceID: "closet"}, Relation: model.RelLocatedIn},
			{Src: model.Ref{SourceID: "vm1"}, Dst: model.Ref{SourceID: "job1"}, Relation: model.RelBackedUpBy},
			{Src: model.Ref{SourceID: "plex"}, Dst: model.Ref{SourceID: "vm1"}, Relation: model.RelRunsOn},
		},
	}
	if _, err := st.Ingest(ctx, "test", res); err != nil {
		t.Fatal(err)
	}

	// Annotate Plex.
	var plexID, orphanToBe int64
	rows, _ := st.ListResources(ctx, store.ListFilter{})
	for _, r := range rows {
		switch r.SourceID {
		case "plex":
			plexID = r.ID
		case "vm2":
			orphanToBe = r.ID
		}
	}
	_ = orphanToBe
	if err := st.SetAnnotation(ctx, plexID, "purpose", "Media server — TV breaks without it."); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAnnotation(ctx, plexID, "recovery", "- [ ] start NAS\n- [ ] `docker compose up`"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAnnotation(ctx, plexID, "credential_pointer", "1Password vault Home, item Plex"); err != nil {
		t.Fatal(err)
	}

	// Second run: vm2 disappears -> orphan.
	res2 := res
	res2.Resources = nil
	for _, r := range res.Resources {
		if r.SourceID != "vm2" {
			res2.Resources = append(res2.Resources, r)
		}
	}
	if _, err := st.Ingest(ctx, "test", res2); err != nil {
		t.Fatal(err)
	}

	// Hand-authored pages.
	if err := st.SetPage(ctx, "start_here", "# If something is broken\n\nBreathe. Check the closet."); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestBuild(t *testing.T) {
	st := seed(t)
	doc, err := Build(context.Background(), st, "Test Runbook")
	if err != nil {
		t.Fatal(err)
	}

	if doc.StartHereMD == "" || doc.ContactsMD != "" {
		t.Errorf("pages wrong: start=%q contacts=%q", doc.StartHereMD, doc.ContactsMD)
	}
	if len(doc.Accounts) != 1 || doc.Accounts[0].Name != "ISP account" {
		t.Errorf("accounts wrong: %+v", doc.Accounts)
	}
	if len(doc.Locations) != 1 || len(doc.Locations[0].Items) != 1 || doc.Locations[0].Items[0].Name != "Cable modem" {
		t.Errorf("locations wrong: %+v", doc.Locations)
	}
	if len(doc.Unplaced) != 1 || doc.Unplaced[0].Name != "Unplaced widget" {
		t.Errorf("unplaced wrong: %+v", doc.Unplaced)
	}

	// Services: plex + vm1 (vm2 is orphaned, must be excluded).
	names := map[string]Entry{}
	for _, s := range doc.Services {
		names[s.Name] = s
	}
	if len(doc.Services) != 2 {
		t.Fatalf("want 2 services, got %+v", doc.Services)
	}
	if _, has := names["win-test"]; has {
		t.Error("orphan leaked into service catalog")
	}
	plex := names["Plex"]
	if plex.PurposeMD == "" || plex.RecoveryMD == "" || plex.CredMD == "" {
		t.Errorf("plex annotations missing: %+v", plex)
	}
	if len(plex.Edges) != 1 || !plex.Edges[0].Outbound || plex.Edges[0].PeerName != "docker-host" {
		t.Errorf("plex edges wrong: %+v", plex.Edges)
	}

	if len(doc.BackupJobs) != 1 || len(doc.BackupJobs[0].Covers) != 1 || doc.BackupJobs[0].Covers[0] != "docker-host" {
		t.Errorf("backup jobs wrong: %+v", doc.BackupJobs)
	}
	// The seed records no restore test, so the job is unverified — the
	// "last verified: never" nag must fire.
	if doc.BackupJobs[0].VerifiedAt != "" {
		t.Errorf("unverified backup should have empty VerifiedAt, got %q", doc.BackupJobs[0].VerifiedAt)
	}
	if doc.UnverifiedBackups() != 1 {
		t.Errorf("UnverifiedBackups = %d, want 1", doc.UnverifiedBackups())
	}
	// vm1 covered; vm2 orphaned — nothing live is uncovered.
	if len(doc.Uncovered) != 0 {
		t.Errorf("uncovered wrong: %+v", doc.Uncovered)
	}

	// Appendix includes the orphan, flagged.
	foundOrphan := false
	for _, g := range doc.Inventory {
		for _, e := range g.Entries {
			if e.Name == "win-test" && e.Orphaned {
				foundOrphan = true
			}
		}
	}
	if !foundOrphan {
		t.Error("orphan missing from appendix inventory")
	}

	if doc.MermaidSrc == "" || len(doc.Prefixes) != 1 {
		t.Errorf("network views missing: mermaid=%d chars, prefixes=%d", len(doc.MermaidSrc), len(doc.Prefixes))
	}
}

// The artifact must be self-contained: no external URLs in fetchable
// positions, mermaid inlined, content present.
func TestRenderHTMLSelfContained(t *testing.T) {
	st := seed(t)
	doc, err := Build(context.Background(), st, "Test Runbook")
	if err != nil {
		t.Fatal(err)
	}
	out, err := RenderHTML(doc, GuideAdministrator, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	html := string(out)

	for _, want := range []string{
		"Media server — TV breaks without it.", // annotation rendered
		"1Password vault Home",                 // credential pointer
		"mermaid.initialize",                   // inline diagram runtime
		"10.0.10.0/24",                         // IP plan
		"No contacts have been written down",   // shared contacts nag
		"nightly vzdump",                       // backup section
	} {
		if !strings.Contains(html, want) {
			t.Errorf("administrator guide missing %q", want)
		}
	}

	// No external fetches: src/href must never point at http(s)://,
	// and no app-relative links like /resources/…
	extRef := regexp.MustCompile(`(?:src|href)\s*=\s*["'](?:https?:)?//`)
	if m := extRef.FindString(html); m != "" {
		t.Errorf("artifact references external resource: %q", m)
	}
	// Sections are wrapped so custom CSS can target them.
	for _, id := range []string{"topology", "network", "physical", "services", "backups", "accounts", "appendix"} {
		if !strings.Contains(html, `<section id="`+id+`">`) {
			t.Errorf("administrator guide missing section wrapper #%s", id)
		}
	}

	if strings.Contains(html, `href="/`) {
		t.Error("artifact links back into the HRG app")
	}
	if strings.Contains(html, "checklists (- [ ]") {
		t.Error("placeholder help text leaked into artifact")
	}
}

// The two guides are co-equal but genuinely different documents: each is
// aimed at its own reader, they share the sections both readers need, and
// they point at each other.
func TestTwoGuidesDiffer(t *testing.T) {
	st := seed(t)
	doc, err := Build(context.Background(), st, "Test Runbook")
	if err != nil {
		t.Fatal(err)
	}
	house, err := RenderHTML(doc, GuideHousehold, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := RenderHTML(doc, GuideAdministrator, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h, a := string(house), string(admin)

	// Household-only content.
	if !strings.Contains(h, "Check the closet.") {
		t.Error("household guide missing the START HERE triage content")
	}
	if !strings.Contains(h, "Household Guide") {
		t.Error("household guide missing its edition label")
	}
	// Engineer content must NOT be in the household guide.
	for _, unwanted := range []string{"10.0.10.0/24", "mermaid.initialize", "IP plan"} {
		if strings.Contains(h, unwanted) {
			t.Errorf("household guide leaked technical content: %q", unwanted)
		}
	}

	// Shared sections appear in both — authored once, rendered twice.
	for _, shared := range []string{"Basement closet", "No contacts have been written down"} {
		if !strings.Contains(h, shared) {
			t.Errorf("household guide missing shared content %q", shared)
		}
		if !strings.Contains(a, shared) {
			t.Errorf("administrator guide missing shared content %q", shared)
		}
	}

	// Each points at the other, because the handoff is the failure point.
	if !strings.Contains(h, "Administrator Guide") {
		t.Error("household guide does not cross-reference the Administrator Guide")
	}
	if !strings.Contains(a, "Household Guide") {
		t.Error("administrator guide does not cross-reference the Household Guide")
	}

	// The household guide carries no diagram runtime, so it stays small
	// enough to email — the whole point of splitting them.
	if len(house) > 200*1024 {
		t.Errorf("household guide should be small (no Mermaid); got %d KiB", len(house)/1024)
	}
	if len(admin) <= len(house) {
		t.Error("administrator guide should be the larger document (it embeds the diagram renderer)")
	}
}

func TestRenderMarkdownTree(t *testing.T) {
	st := seed(t)
	doc, err := Build(context.Background(), st, "Test Runbook")
	if err != nil {
		t.Fatal(err)
	}
	files := RenderMarkdown(doc)

	// The root README is an index that routes each reader to their guide.
	readme := string(files["README.md"])
	for _, want := range []string{"Household Guide", "Administrator Guide"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README index missing link to %q", want)
		}
	}

	// Household guide: one readable file, with the triage content.
	house := string(files["household-guide.md"])
	if !strings.Contains(house, "Check the closet.") {
		t.Error("household-guide.md missing START HERE content")
	}
	if strings.Contains(house, "10.0.10.0/24") {
		t.Error("household-guide.md leaked IP-plan detail")
	}

	network := string(files["administrator-guide/network.md"])
	if !strings.Contains(network, "```mermaid") {
		t.Error("administrator network.md missing mermaid fence")
	}
	if !strings.Contains(network, "| `10.0.10.0/24` |") {
		t.Error("administrator network.md missing subnet table row")
	}
	// Annotated resources get their own file; unannotated ones don't.
	plexFile := ""
	for p := range files {
		if strings.HasPrefix(p, "administrator-guide/appendix/resources/") && strings.Contains(p, "plex") {
			plexFile = p
		}
	}
	if plexFile == "" {
		t.Fatalf("no per-resource file for plex: %v", keys(files))
	}
	if !strings.Contains(string(files[plexFile]), "docker compose up") {
		t.Error("plex file missing recovery")
	}

	// Write + rewrite honors the safety marker.
	dir := filepath.Join(t.TempDir(), "runbook-md")
	if err := WriteTree(dir, files); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "administrator-guide", "appendix", "inventory.md")); err != nil {
		t.Errorf("tree not written: %v", err)
	}
	if err := WriteTree(dir, files); err != nil {
		t.Errorf("regeneration over own tree failed: %v", err)
	}

	// A foreign non-empty directory is refused.
	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreign, "precious.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteTree(foreign, files); err == nil {
		t.Fatal("WriteTree overwrote a directory it didn't generate")
	}
	if _, err := os.Stat(filepath.Join(foreign, "precious.txt")); err != nil {
		t.Error("foreign file was destroyed")
	}

	// A freshly git-init'ed directory (only .git inside) is fair game —
	// that's the documented history workflow — and .git survives rewrites.
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteTree(repoDir, files); err != nil {
		t.Fatalf("WriteTree refused a git-init'ed empty dir: %v", err)
	}
	if err := WriteTree(repoDir, files); err != nil {
		t.Fatalf("rewrite over git-init'ed tree failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		t.Error(".git was destroyed by rewrite")
	}
}

func TestCommitTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()

	// Not a repo: soft result, no error.
	msg, err := CommitTree(ctx, dir)
	if err != nil || !strings.Contains(msg, "not a git repository") {
		t.Fatalf("non-repo handling wrong: %q %v", msg, err)
	}

	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg, err = CommitTree(ctx, dir)
	if err != nil || !strings.HasPrefix(msg, "committed ") {
		t.Fatalf("first commit wrong: %q %v", msg, err)
	}

	// No changes -> no commit.
	msg, err = CommitTree(ctx, dir)
	if err != nil || msg != "no changes since last commit" {
		t.Fatalf("no-change handling wrong: %q %v", msg, err)
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
