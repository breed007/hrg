package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/breed007/hrg/internal/deliver"
	"github.com/breed007/hrg/internal/runbook"
	"github.com/breed007/hrg/internal/store"
)

// staleDelivery is how long a copy can go without leaving the building
// before the dashboard starts nagging. A month is roughly the interval at
// which a household's technology changes enough to matter, and short
// enough that the copy in someone's inbox is still recognisable.
const staleDelivery = 30 * 24 * time.Hour

// contentTypes maps an export format to what a mail client needs to know
// to preview it rather than offer a download.
var contentTypes = map[string]string{
	"pdf":  "application/pdf",
	"html": "text/html; charset=utf-8",
}

// deliverAll sends every enabled destination the guides and formats it
// asked for, and records each attempt. One destination failing must never
// stop the others: the whole point is redundancy.
//
// files is keyed "guide-format", matching how generateRunbook records
// exports, so a destination can only ask for something actually generated.
func (s *Server) deliverAll(ctx context.Context, files map[string][]byte) {
	dests, err := s.store.ListDestinations(ctx)
	if err != nil {
		s.log.Error("list destinations", "err", err)
		return
	}
	for _, d := range dests {
		if !d.Enabled {
			continue
		}
		detail, err := s.sendTo(ctx, d, files)
		rec := store.Delivery{DestinationID: &d.ID, Name: d.Name, Status: "ok", Detail: detail}
		if err != nil {
			rec.Status, rec.Detail = "error", err.Error()
			s.log.Error("deliver", "destination", d.Name, "err", err)
		} else {
			s.log.Info("delivered", "destination", d.Name, "detail", detail)
		}
		if err := s.store.RecordDelivery(ctx, rec); err != nil {
			s.log.Error("record delivery", "err", err)
		}
	}
}

// sendTo builds the destination and hands it the subset of files it wants.
func (s *Server) sendTo(ctx context.Context, d store.Destination, files map[string][]byte) (string, error) {
	secret := ""
	if len(d.Secret) > 0 {
		var err error
		if secret, err = s.key.Open(d.Secret); err != nil {
			return "", fmt.Errorf("decrypt credential (was the key file replaced?): %w", err)
		}
	}
	dest, err := deliver.New(d.Type, d.Config, secret)
	if err != nil {
		return "", err
	}

	var out []deliver.File
	for _, guide := range runbook.Guides {
		for _, format := range []string{"pdf", "html"} {
			if !d.Sends(string(guide), format) {
				continue
			}
			body, ok := files[string(guide)+"-"+format]
			if !ok {
				// Asked for something this run didn't produce — most often
				// PDF with Chrome missing. Say so rather than silently
				// delivering less than the user configured.
				return "", fmt.Errorf("no %s %s was generated this run — check the export settings", guide.Title(), strings.ToUpper(format))
			}
			out = append(out, deliver.File{
				Name:        guide.Slug() + "." + format,
				ContentType: contentTypes[format],
				Bytes:       body,
			})
		}
	}
	if len(out) == 0 {
		return "", fmt.Errorf("nothing selected to send")
	}
	return dest.Send(ctx, out)
}

// --- HTTP ------------------------------------------------------------------

// destView is one row of the destinations page.
type destView struct {
	store.Destination
	Kind deliver.Kind
	// Summary is the configured target in one line, e.g. the folder path
	// or the recipient list — what the user needs to recognize the row.
	Summary string
	Last    *store.Delivery
}

func (s *Server) handleDestinations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dests, err := s.store.ListDestinations(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	deliveries, err := s.store.ListDeliveries(ctx, 20)
	if err != nil {
		s.fail(w, err)
		return
	}
	lastByDest := map[int64]*store.Delivery{}
	for i := range deliveries {
		d := deliveries[i]
		if d.DestinationID != nil {
			if _, seen := lastByDest[*d.DestinationID]; !seen {
				lastByDest[*d.DestinationID] = &d
			}
		}
	}

	var views []destView
	for _, d := range dests {
		k, _ := deliver.Lookup(d.Type)
		views = append(views, destView{
			Destination: d, Kind: k,
			Summary: destSummary(d),
			Last:    lastByDest[d.ID],
		})
	}

	// Which type's form to show — the wizard's "add a destination" step
	// links straight to ?type=email so it can skip the picker.
	addType := r.URL.Query().Get("type")
	addKind, showForm := deliver.Lookup(addType)

	var edit *store.Destination
	var editKind deliver.Kind
	var editConfig map[string]any
	if idStr := r.URL.Query().Get("edit"); idStr != "" {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err == nil {
			if edit, err = s.store.GetDestination(ctx, id); err == nil {
				editKind, _ = deliver.Lookup(edit.Type)
				_ = json.Unmarshal(edit.Config, &editConfig)
			}
		}
	}

	s.render(w, "destinations", "layout", map[string]any{
		"Title": "Destinations", "Destinations": views, "Deliveries": deliveries,
		"Kinds": deliver.Kinds(), "AddKind": addKind, "ShowForm": showForm,
		"Edit": edit, "EditKind": editKind, "EditConfig": editConfig,
		"LastGood": lastGood(deliveries),
		"Err":      r.URL.Query().Get("err"),
		"Sent":     r.URL.Query().Get("sent"),
	})
}

func lastGood(ds []store.Delivery) *store.Delivery {
	for i := range ds {
		if ds[i].OK() {
			return &ds[i]
		}
	}
	return nil
}

// destSummary renders the one field that identifies where this goes.
func destSummary(d store.Destination) string {
	var cfg map[string]any
	if err := json.Unmarshal(d.Config, &cfg); err != nil {
		return ""
	}
	for _, key := range []string{"to", "path", "remote"} {
		if v, ok := cfg[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// formToDestination reads the shared add/edit form.
func (s *Server) formToDestination(r *http.Request, typ string) (store.Destination, error) {
	k, ok := deliver.Lookup(typ)
	if !ok {
		return store.Destination{}, fmt.Errorf("unknown destination type %q", typ)
	}
	cfg := map[string]string{}
	for _, f := range k.Fields {
		v := strings.TrimSpace(r.FormValue("cfg_" + f.Key))
		if f.Required && v == "" {
			return store.Destination{}, fmt.Errorf("%s is required", f.Label)
		}
		cfg[f.Key] = v
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return store.Destination{}, err
	}

	d := store.Destination{
		Type:    typ,
		Name:    strings.TrimSpace(r.FormValue("name")),
		Config:  raw,
		Guides:  r.Form["guides"],
		Formats: r.Form["formats"],
		Enabled: r.FormValue("enabled") == "1",
	}
	if d.Name == "" {
		d.Name = k.Label
	}
	if secret := r.FormValue("secret"); secret != "" {
		sealed, err := s.key.Seal(secret)
		if err != nil {
			return store.Destination{}, err
		}
		d.Secret = sealed
	}
	return d, nil
}

func (s *Server) handleDestinationSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	typ := r.FormValue("type")
	d, err := s.formToDestination(r, typ)
	if err != nil {
		s.destError(w, r, err)
		return
	}
	// Build it once before storing: a destination that cannot be
	// constructed is a destination that will silently never deliver.
	secret := ""
	if len(d.Secret) > 0 {
		if secret, err = s.key.Open(d.Secret); err != nil {
			s.destError(w, r, err)
			return
		}
	}
	if _, err := deliver.New(typ, d.Config, secret); err != nil {
		s.destError(w, r, err)
		return
	}

	if idStr := r.FormValue("id"); idStr != "" {
		id, perr := strconv.ParseInt(idStr, 10, 64)
		if perr != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		d.ID = id
		err = s.store.UpdateDestination(r.Context(), d)
	} else {
		_, err = s.store.CreateDestination(r.Context(), d)
	}
	if err != nil {
		s.destError(w, r, err)
		return
	}
	http.Redirect(w, r, "/destinations", http.StatusSeeOther)
}

// destError sends the user back to the page with the message, rather than
// a 500 — every failure here is a typo in a form.
func (s *Server) destError(w http.ResponseWriter, r *http.Request, err error) {
	http.Redirect(w, r, "/destinations?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
}

func (s *Server) handleDestinationDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteDestination(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/destinations", http.StatusSeeOther)
}

// handleDestinationTest sends the current runbook to one destination right
// now. Configuring a delivery you never test is how people discover in the
// worst week of their life that the password was wrong.
func (s *Server) handleDestinationTest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	d, err := s.store.GetDestination(ctx, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	settings, err := s.store.Settings(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	files, err := s.renderForDelivery(ctx, s.runbookConfig(settings))
	if err != nil {
		s.destError(w, r, err)
		return
	}

	detail, sendErr := s.sendTo(ctx, *d, files)
	rec := store.Delivery{DestinationID: &d.ID, Name: d.Name, Status: "ok", Detail: "test · " + detail}
	if sendErr != nil {
		rec.Status, rec.Detail = "error", "test · "+sendErr.Error()
	}
	if err := s.store.RecordDelivery(ctx, rec); err != nil {
		s.log.Error("record delivery", "err", err)
	}
	if sendErr != nil {
		s.destError(w, r, sendErr)
		return
	}
	http.Redirect(w, r, "/destinations?sent="+url.QueryEscape(d.Name), http.StatusSeeOther)
}

// renderForDelivery renders the guides in memory, without touching the
// export directory — a test send must not overwrite the last good export.
func (s *Server) renderForDelivery(ctx context.Context, cfg runbookConfig) (map[string][]byte, error) {
	doc, err := runbook.Build(ctx, s.store, cfg.Title)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, guide := range runbook.Guides {
		html, err := runbook.RenderHTML(doc, guide, cfg.renderOptions())
		if err != nil {
			return nil, err
		}
		files[string(guide)+"-html"] = html
		if cfg.PDF {
			pdf, err := runbook.RenderPDF(ctx, s.chrome, html)
			if err != nil {
				// Not fatal: a destination wanting PDF will say so clearly,
				// and one wanting HTML should still get its test.
				s.log.Warn("pdf for delivery test", "guide", guide, "err", err)
				continue
			}
			files[string(guide)+"-pdf"] = pdf
		}
	}
	return files, nil
}
