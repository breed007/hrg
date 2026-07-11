package schedule

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSetValidatesSpec(t *testing.T) {
	s := New(func(context.Context) {}, quietLog())

	if err := s.Set("not a cron spec"); err == nil {
		t.Error("invalid spec accepted")
	}
	if err := s.Set("@daily"); err != nil {
		t.Errorf("descriptor rejected: %v", err)
	}
	if s.Spec() != "@daily" {
		t.Errorf("spec not recorded: %q", s.Spec())
	}
	if err := s.Set("0 3 * * *"); err != nil {
		t.Errorf("standard spec rejected: %v", err)
	}
}

func TestSetEmptyDisables(t *testing.T) {
	s := New(func(context.Context) {}, quietLog())
	if err := s.Set("@hourly"); err != nil {
		t.Fatal(err)
	}
	s.Start()
	defer s.Stop()
	if s.Next() == "" {
		t.Error("enabled schedule has no next time")
	}
	if err := s.Set(""); err != nil {
		t.Fatal(err)
	}
	if s.Next() != "" {
		t.Error("disabled schedule still reports a next time")
	}
}

func TestReplaceScheduleKeepsSingleEntry(t *testing.T) {
	s := New(func(context.Context) {}, quietLog())
	for _, spec := range []string{"@hourly", "@daily", "@weekly"} {
		if err := s.Set(spec); err != nil {
			t.Fatal(err)
		}
	}
	// Only the last schedule should remain — replacing must not stack.
	if got := len(s.cron.Entries()); got != 1 {
		t.Errorf("want 1 cron entry after replaces, got %d", got)
	}
}

func TestValidateInterval(t *testing.T) {
	ref := timeRef()
	// Too frequent → rejected.
	for _, spec := range []string{"@every 1s", "@every 30s", "@every 59s"} {
		if err := Validate(spec, ref); err == nil {
			t.Errorf("%q should be rejected as too frequent", spec)
		}
	}
	// Sane schedules → accepted.
	for _, spec := range []string{"", "@hourly", "@daily", "*/15 * * * *", "@every 1m", "@every 5m"} {
		if err := Validate(spec, ref); err != nil {
			t.Errorf("%q should be accepted: %v", spec, err)
		}
	}
	// Garbage → rejected.
	if err := Validate("not a schedule", ref); err == nil {
		t.Error("garbage spec should be rejected")
	}
}

func timeRef() time.Time { return time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC) }
