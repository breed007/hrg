package runbook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/breed007/hrg/internal/netmap"
)

// marker names the sentinel file that lets WriteTree safely wipe and
// regenerate a directory: we only delete what we previously generated.
const marker = ".hrg-runbook"

// RenderMarkdown produces the git-committable file tree as path → content.
// Paths use forward slashes relative to the tree root. The topology ships
// as a ```mermaid fence, which GitHub renders natively.
func RenderMarkdown(doc *Document) map[string][]byte {
	files := map[string][]byte{}
	put := func(path string, b *strings.Builder) {
		files[path] = []byte(b.String())
	}

	// README.md — START HERE is the front page.
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", doc.Title)
	fmt.Fprintf(&b, "*Generated %s by HRG. Self-contained; treat as sensitive — it maps this home's infrastructure.*\n\n",
		doc.GeneratedAt.Format("2006-01-02 15:04 MST"))
	b.WriteString("## START HERE\n\n")
	if doc.StartHereMD != "" {
		b.WriteString(doc.StartHereMD + "\n\n")
	} else {
		b.WriteString("> ⚠ **This page has not been written yet.** The triage page — \"the internet is down, the TV doesn't work, something is beeping\" — only its author can write. Open HRG → Runbook and write it.\n\n")
	}
	b.WriteString("## Contents\n\n")
	b.WriteString("1. [Emergency contacts & accounts](contacts.md)\n")
	b.WriteString("2. [Physical layer](physical.md)\n")
	b.WriteString("3. [Network map & IP plan](network.md)\n")
	b.WriteString("4. [Service catalog](services.md)\n")
	b.WriteString("5. [Backup & restore](backups.md)\n")
	b.WriteString("6. [Appendix: inventory & change log](appendix/inventory.md)\n")
	put("README.md", &b)

	b = strings.Builder{}
	b.WriteString("# Emergency contacts & accounts\n\n")
	if doc.ContactsMD != "" {
		b.WriteString(doc.ContactsMD + "\n\n")
	} else {
		b.WriteString("> ⚠ Not written yet — ISP support, registrar, where the password vault is, who has emergency access.\n\n")
	}
	if len(doc.Accounts) > 0 {
		b.WriteString("## Accounts on record\n\n")
		for _, e := range doc.Accounts {
			writeEntryMD(&b, e, 3)
		}
	}
	put("contacts.md", &b)

	b = strings.Builder{}
	b.WriteString("# Physical layer\n\nWhere the equipment lives. Most home-network problems end with power-cycling something on this list.\n\n")
	for _, lg := range doc.Locations {
		fmt.Fprintf(&b, "## %s\n\n", lg.Location.Name)
		if d, ok := lg.Location.Attrs["directions"].(string); ok {
			fmt.Fprintf(&b, "*Directions: %s*\n\n", d)
		}
		if lg.Location.NoteMD != "" {
			b.WriteString(lg.Location.NoteMD + "\n\n")
		}
		for _, e := range lg.Items {
			writeEntryMD(&b, e, 3)
		}
	}
	if len(doc.Unplaced) > 0 {
		b.WriteString("## Not assigned to a location\n\n")
		for _, e := range doc.Unplaced {
			writeEntryMD(&b, e, 3)
		}
	}
	put("physical.md", &b)

	b = strings.Builder{}
	b.WriteString("# Network map & IP plan\n\n")
	if doc.MermaidSrc != "" {
		b.WriteString("```mermaid\n" + doc.MermaidSrc + "```\n\n")
	}
	if len(doc.Prefixes) > 0 {
		b.WriteString("## Subnets\n\n| Prefix | Documented as | Observed by | Status |\n|---|---|---|---|\n")
		for _, p := range doc.Prefixes {
			doc1 := "—"
			if p.Documented != nil {
				doc1 = p.Documented.Name
				if p.Detail != "" {
					doc1 += " (" + p.Detail + ")"
				}
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", p.CIDR, mdCell(doc1), mdCell(refNames(p.Observed)), p.Status)
		}
		b.WriteString("\n")
	}
	if len(doc.IPs) > 0 {
		b.WriteString("## Known addresses\n\n| IP | Name | Observed by | Status |\n|---|---|---|---|\n")
		for _, ip := range doc.IPs {
			name := ip.DNS
			if name == "" && ip.Documented != nil {
				name = ip.Documented.Name
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", ip.IP, mdCell(name), mdCell(refNames(ip.Observed)), ip.Status)
		}
		b.WriteString("\n")
	}
	put("network.md", &b)

	b = strings.Builder{}
	b.WriteString("# Service catalog\n\nEverything that runs. Missing purposes and recovery steps are flagged — those gaps are the difference between an inventory and a runbook.\n\n")
	for _, e := range doc.Services {
		writeEntryMD(&b, e, 2)
	}
	put("services.md", &b)

	b = strings.Builder{}
	b.WriteString("# Backup & restore\n\n")
	if len(doc.BackupJobs) == 0 {
		b.WriteString("> ⚠ No backup jobs are known to HRG.\n\n")
	}
	for _, j := range doc.BackupJobs {
		fmt.Fprintf(&b, "## %s\n\n", j.Name)
		if s, ok := j.Attrs["schedule"].(string); ok {
			fmt.Fprintf(&b, "- Schedule: `%s`\n", s)
		}
		if s, ok := j.Attrs["storage"].(string); ok {
			fmt.Fprintf(&b, "- Storage: `%s`\n", s)
		}
		if len(j.Covers) > 0 {
			fmt.Fprintf(&b, "- Covers: %s\n", strings.Join(j.Covers, ", "))
		}
		if j.VerifiedAt != "" {
			fmt.Fprintf(&b, "- Restore last verified: **%s**\n", j.VerifiedAt)
		} else {
			b.WriteString("- Restore last verified: ⚠ **never** — an untested backup is a hope, not a backup\n")
		}
		b.WriteString("\n")
		if j.RecoveryMD != "" {
			b.WriteString("### Restore procedure\n\n" + j.RecoveryMD + "\n\n")
		} else {
			b.WriteString("> ⚠ No restore procedure written.\n\n")
		}
	}
	if len(doc.Uncovered) > 0 {
		b.WriteString("## Not covered by any known backup job\n\n")
		for _, e := range doc.Uncovered {
			fmt.Fprintf(&b, "- %s (%s)\n", e.Name, e.Kind)
		}
		b.WriteString("\n")
	}
	put("backups.md", &b)

	b = strings.Builder{}
	b.WriteString("# Appendix: full inventory\n\n")
	fmt.Fprintf(&b, "Coverage at generation time: %d/%d resources with documented purpose; %d/%d things-that-can-be-down with recovery steps.\n\n",
		doc.Coverage.WithPurpose, doc.Coverage.Annotatable, doc.Coverage.WithRecovery, doc.Coverage.CriticalTotal)
	for _, g := range doc.Inventory {
		fmt.Fprintf(&b, "## %s (%d)\n\n| Name | Identity | Notes |\n|---|---|---|\n", g.Kind, len(g.Entries))
		for _, e := range g.Entries {
			flag := ""
			if e.Orphaned {
				flag = " ⚠ orphaned"
			}
			detail := "—"
			if e.HasAnnotations() {
				detail = "[details](resources/" + resourceFile(e) + ")"
			}
			fmt.Fprintf(&b, "| %s%s | `%s / %s` | %s |\n", mdCell(e.Name), flag, e.Collector, e.SourceID, detail)
		}
		b.WriteString("\n")
	}
	b.WriteString("## Recent collection runs\n\n| # | Collector | When | Status | Seen | Added | Changed | Removed |\n|---|---|---|---|---|---|---|---|\n")
	for _, r := range doc.Runs {
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %d | %d | %d | %d |\n",
			r.ID, r.Collector, r.StartedAt, r.Status,
			r.Summary.Seen, r.Summary.Added, r.Summary.Changed, r.Summary.Removed)
	}
	put("appendix/inventory.md", &b)

	// One file per annotated resource — the unit git diffs care about.
	for _, g := range doc.Inventory {
		for _, e := range g.Entries {
			if !e.HasAnnotations() {
				continue
			}
			b = strings.Builder{}
			fmt.Fprintf(&b, "# %s\n\n", e.Name)
			fmt.Fprintf(&b, "- Kind: %s\n- Identity: `%s / %s`\n", e.Kind, e.Collector, e.SourceID)
			if e.Orphaned {
				b.WriteString("- ⚠ **Orphaned** — no longer seen by its collector\n")
			}
			b.WriteString("\n")
			for _, f := range []struct{ label, body string }{
				{"Purpose", e.PurposeMD}, {"Recovery", e.RecoveryMD},
				{"Credentials live at", e.CredMD}, {"Notes", e.NoteMD},
			} {
				if f.body != "" {
					fmt.Fprintf(&b, "## %s\n\n%s\n\n", f.label, f.body)
				}
			}
			if len(e.Edges) > 0 {
				b.WriteString("## Relationships\n\n")
				for _, ed := range e.Edges {
					if ed.Outbound {
						fmt.Fprintf(&b, "- → %s: %s (%s)\n", ed.Relation, ed.PeerName, ed.PeerKind)
					} else {
						fmt.Fprintf(&b, "- ← %s by: %s (%s)\n", ed.Relation, ed.PeerName, ed.PeerKind)
					}
				}
				b.WriteString("\n")
			}
			if len(e.Attrs) > 0 {
				attrs, _ := json.MarshalIndent(e.Attrs, "", "  ")
				b.WriteString("## Attributes\n\n```json\n" + string(attrs) + "\n```\n")
			}
			put("appendix/resources/"+resourceFile(e), &b)
		}
	}

	return files
}

func writeEntryMD(b *strings.Builder, e Entry, level int) {
	h := strings.Repeat("#", level)
	fmt.Fprintf(b, "%s %s (%s)\n\n", h, e.Name, e.Kind)
	if e.PurposeMD != "" {
		b.WriteString(e.PurposeMD + "\n\n")
	} else {
		b.WriteString("> ⚠ No purpose documented.\n\n")
	}
	if pc, ok := e.Attrs["power_cycle"].(string); ok {
		fmt.Fprintf(b, "**Power cycle:** %s\n\n", pc)
	}
	if loc, ok := e.Attrs["location"].(string); ok {
		fmt.Fprintf(b, "*Location: %s*\n\n", loc)
	}
	if e.RecoveryMD != "" {
		fmt.Fprintf(b, "%s# Recovery\n\n%s\n\n", h, e.RecoveryMD)
	}
	if e.CredMD != "" {
		fmt.Fprintf(b, "%s# Credentials live at\n\n%s\n\n", h, e.CredMD)
	}
	if len(e.Edges) > 0 {
		for _, ed := range e.Edges {
			if ed.Outbound {
				fmt.Fprintf(b, "- → %s: %s (%s)\n", ed.Relation, ed.PeerName, ed.PeerKind)
			} else {
				fmt.Fprintf(b, "- ← %s by: %s (%s)\n", ed.Relation, ed.PeerName, ed.PeerKind)
			}
		}
		b.WriteString("\n")
	}
}

func refNames(refs []netmap.Ref) string {
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.Name)
	}
	return strings.Join(names, ", ")
}

func mdCell(s string) string {
	if s == "" {
		return "—"
	}
	return strings.ReplaceAll(s, "|", "\\|")
}

var unsafeFile = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func resourceFile(e Entry) string {
	name := e.Collector + "__" + e.SourceID
	return unsafeFile.ReplaceAllString(name, "-") + ".md"
}

// WriteTree writes the rendered files under dir, replacing a previous
// generation. Refuses to touch a non-empty directory it didn't create
// (identified by the marker file) — an export path typo must not eat a
// real directory.
func WriteTree(dir string, files map[string][]byte) error {
	if entries, err := os.ReadDir(dir); err == nil {
		// .git doesn't count toward "has content": a freshly git-init'ed
		// target is the documented workflow, and .git must survive wipes.
		var content []os.DirEntry
		for _, e := range entries {
			if e.Name() != ".git" {
				content = append(content, e)
			}
		}
		if len(content) > 0 {
			if _, err := os.Stat(filepath.Join(dir, marker)); err != nil {
				return fmt.Errorf("%s is not empty and was not generated by HRG (missing %s) — refusing to overwrite", dir, marker)
			}
		}
		for _, e := range content {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	} else if os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	} else {
		return err
	}

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, files[p], 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, marker), []byte("generated by HRG; this directory is wiped and rewritten on every export\n"), 0o644)
}
