package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestDestinationRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	id, err := s.CreateDestination(ctx, Destination{
		Type: "email", Name: "Partner's inbox",
		Config:  json.RawMessage(`{"to":"partner@example.com"}`),
		Secret:  []byte("sealed-blob"),
		Guides:  []string{"household"},
		Formats: []string{"pdf"},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetDestination(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Sends("household", "pdf") {
		t.Error("destination should send the household PDF")
	}
	if got.Sends("administrator", "pdf") || got.Sends("household", "html") {
		t.Errorf("destination sends more than it was configured for: %+v", got)
	}
	if string(got.Secret) != "sealed-blob" {
		t.Error("sealed credential not round-tripped")
	}

	// A nil Secret on update must keep the stored credential — the form
	// never echoes a password back, so nil means "unchanged", not "clear".
	got.Secret = nil
	got.Name = "Partner + me"
	got.Formats = []string{"pdf", "html"}
	if err := s.UpdateDestination(ctx, *got); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetDestination(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if string(after.Secret) != "sealed-blob" {
		t.Errorf("update wiped the credential: %q", after.Secret)
	}
	if !after.Sends("household", "html") || after.Name != "Partner + me" {
		t.Errorf("update did not apply: %+v", after)
	}
}

func TestDestinationValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := Destination{Type: "folder", Name: "n", Guides: []string{"household"}, Formats: []string{"pdf"}}

	cases := map[string]Destination{
		"no name":        {Type: "folder", Guides: base.Guides, Formats: base.Formats},
		"no type":        {Name: "n", Guides: base.Guides, Formats: base.Formats},
		"no guide":       {Type: "folder", Name: "n", Formats: base.Formats},
		"no format":      {Type: "folder", Name: "n", Guides: base.Guides},
		"unknown guide":  {Type: "folder", Name: "n", Guides: []string{"neighbour"}, Formats: base.Formats},
		"unknown format": {Type: "folder", Name: "n", Guides: base.Guides, Formats: []string{"docx"}},
	}
	for name, d := range cases {
		if _, err := s.CreateDestination(ctx, d); err == nil {
			t.Errorf("%s: accepted invalid destination", name)
		}
	}
}

// The delivery history is how the dashboard answers "when did a copy last
// leave the building" — so it must outlive the destination it left through.
func TestDeliveryHistorySurvivesDeletion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	id, err := s.CreateDestination(ctx, Destination{
		Type: "folder", Name: "Dropbox", Config: json.RawMessage(`{"path":"/tmp/x"}`),
		Guides: []string{"household"}, Formats: []string{"pdf"}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []Delivery{
		{DestinationID: &id, Name: "Dropbox", Status: "error", Detail: "no such folder"},
		{DestinationID: &id, Name: "Dropbox", Status: "ok", Detail: "1 file(s)"},
	} {
		if err := s.RecordDelivery(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}

	last, err := s.LastGoodDelivery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if last == nil || last.Detail != "1 file(s)" {
		t.Fatalf("last good delivery wrong: %+v", last)
	}

	if err := s.DeleteDestination(ctx, id); err != nil {
		t.Fatal(err)
	}
	last, err = s.LastGoodDelivery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if last == nil {
		t.Fatal("deleting a destination erased the record that a copy ever left")
	}
	if last.DestinationID != nil {
		t.Error("dangling destination reference after delete")
	}
	if last.Name != "Dropbox" {
		t.Errorf("history lost the destination name: %q", last.Name)
	}
}

func TestLastGoodDeliveryNeverSent(t *testing.T) {
	s := newTestStore(t)
	last, err := s.LastGoodDelivery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if last != nil {
		t.Errorf("expected nil for a store that never delivered, got %+v", last)
	}
}
