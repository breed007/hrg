package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/breed007/hrg/internal/model"
	"github.com/breed007/hrg/internal/store"
)

// annField describes one typed annotation slot for the UI. Choices, when
// present, render a dropdown instead of a textarea — a classification is
// not prose.
type annField struct {
	Key     string
	Label   string
	Help    string
	Choices []annChoice
}

type annChoice struct{ Value, Label string }

// householdFields are written for the people who live here; adminFields for
// whoever does the technical work. They are edited in separate groups so the
// author knows which reader they're writing for.
var householdFields = []annField{
	{Key: "plain_english", Label: "What is this, in plain English?",
		Help: "For someone who has never touched it. \"Stores our photos and lets the TVs play movies\" — not \"ZFS pool exported over NFS.\""},
	{Key: "household_importance", Label: "Does the household need it?",
		Help: "The triage key for someone deciding what to keep alive and what can go.",
		Choices: []annChoice{
			{"", "— not classified —"},
			{store.ImportanceEssential, "Essential — the house needs this"},
			{store.ImportanceNice, "Nice to have"},
			{store.ImportanceExperimental, "Just a project — not needed"},
		}},
	{Key: "safe_to_off", Label: "If it gets turned off",
		Help: "What stops working, in plain language. Leave blank and HRG will infer what it can from dependencies."},
	{Key: "monthly_cost", Label: "Cost & who pays",
		Help: "Monthly or yearly cost and which card/account it's on — so a survivor can find and stop it."},
}

var adminFields = []annField{
	{Key: "purpose", Label: "Purpose", Help: "What is this, and what breaks without it?"},
	{Key: "recovery", Label: "Recovery procedure", Help: "Step-by-step: how to restart or restore it. Markdown checklists (- [ ] step) render as checkboxes."},
	{Key: "credential_pointer", Label: "Credential pointer", Help: "Where credentials live — vault name and item. Never the credentials themselves."},
	{Key: "note", Label: "Notes", Help: "Anything else future-you needs to know."},
}

// annBlock is the render state of one annotation slot.
type annBlock struct {
	ResourceID int64
	annField
	Body      string
	UpdatedAt string
	Editing   bool
}

// SelectedLabel renders the human label for a chosen value (choice fields).
func (b annBlock) SelectedLabel() string {
	for _, c := range b.Choices {
		if c.Value == b.Body && c.Value != "" {
			return c.Label
		}
	}
	return ""
}

func blocksFor(fields []annField, resourceID int64, anns map[string]store.Annotation, editField string) []annBlock {
	out := make([]annBlock, 0, len(fields))
	for _, f := range fields {
		b := annBlock{ResourceID: resourceID, annField: f, Editing: f.Key == editField}
		if a, ok := anns[f.Key]; ok {
			b.Body = a.BodyMD
			b.UpdatedAt = a.UpdatedAt
		}
		out = append(out, b)
	}
	return out
}

// buildAnnBlocks returns the two reader-scoped groups.
func buildAnnBlocks(resourceID int64, anns map[string]store.Annotation, editField string) (household, admin []annBlock) {
	return blocksFor(householdFields, resourceID, anns, editField),
		blocksFor(adminFields, resourceID, anns, editField)
}

func (s *Server) handleAnnotationSave(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	field := r.PathValue("field")
	body := strings.TrimSpace(strings.ReplaceAll(r.FormValue("body"), "\r\n", "\n"))
	if err := s.store.SetAnnotation(r.Context(), id, field, body); err != nil {
		s.fail(w, err)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		anns, err := s.store.GetAnnotations(r.Context(), id)
		if err != nil {
			s.fail(w, err)
			return
		}
		hh, ad := buildAnnBlocks(id, anns, "")
		for _, b := range append(hh, ad...) {
			if b.Key == field {
				s.render(w, "resource", "annotation-block", b)
				return
			}
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/resources/%d#ann-%s", id, field), http.StatusSeeOther)
}

func (s *Server) handleEdgeCreate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	target, err := strconv.ParseInt(r.FormValue("target"), 10, 64)
	if err != nil {
		http.Error(w, "bad target", http.StatusBadRequest)
		return
	}
	rel := model.Relation(r.FormValue("relation"))
	src, dst := id, target
	if r.FormValue("direction") == "in" {
		src, dst = target, id
	}
	if err := s.store.CreateManualEdge(r.Context(), src, dst, rel); err != nil {
		// Duplicate or invalid input — send them back to the page; the
		// existing edge list already tells the story.
		s.log.Warn("manual edge rejected", "err", err)
	}
	http.Redirect(w, r, fmt.Sprintf("/resources/%d", id), http.StatusSeeOther)
}

func (s *Server) handleEdgeDelete(w http.ResponseWriter, r *http.Request) {
	eid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteManualEdge(r.Context(), eid); err != nil {
		s.fail(w, err)
		return
	}
	back := "/resources"
	if rid := r.FormValue("rid"); rid != "" {
		back = "/resources/" + rid
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) handleOrphans(w http.ResponseWriter, r *http.Request) {
	orphans, err := s.store.ListOrphans(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	resources, err := s.store.ListResources(r.Context(), store.ListFilter{})
	if err != nil {
		s.fail(w, err)
		return
	}
	// Reattach targets: live resources only.
	var targets []store.ResourceRow
	for _, res := range resources {
		if !res.Orphaned {
			targets = append(targets, res)
		}
	}
	s.render(w, "orphans", "layout", map[string]any{
		"Title": "Orphans", "Orphans": orphans, "Targets": targets,
	})
}

func (s *Server) handleOrphanReattach(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	target, err := strconv.ParseInt(r.FormValue("target"), 10, 64)
	if err != nil {
		http.Error(w, "bad target", http.StatusBadRequest)
		return
	}
	if err := s.store.ReattachOrphan(r.Context(), id, target); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/orphans", http.StatusSeeOther)
}

func (s *Server) handleOrphanForget(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	// Only queue members can be forgotten — this endpoint must never
	// delete a live resource.
	orphans, err := s.store.ListOrphans(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	isOrphan := false
	for _, o := range orphans {
		if o.ID == id {
			isOrphan = true
		}
	}
	if !isOrphan {
		http.Error(w, "not an orphan", http.StatusConflict)
		return
	}
	if err := s.store.DeleteResource(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/orphans", http.StatusSeeOther)
}
