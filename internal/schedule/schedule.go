// Package schedule runs a job on a cron schedule, in-process. No external
// queue, no system cron — homelab software should not itself be a homelab
// burden. Wraps robfig/cron so the rest of HRG needn't know the library.
package schedule

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"
)

// MinInterval is the floor on how often collection may be scheduled. A tool
// whose whole ethos is "don't be a homelab burden" must not let someone
// point a one-second loop at their production Proxmox.
const MinInterval = time.Minute

// standardParser matches what cron.New() accepts: 5-field cron plus the
// @descriptors and @every.
var standardParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// Validate checks that a schedule spec parses and does not fire more often
// than MinInterval. An empty spec is valid (disabled). Reference is the
// clock the interval is measured from.
func Validate(spec string, reference time.Time) error {
	if spec == "" {
		return nil
	}
	sched, err := standardParser.Parse(spec)
	if err != nil {
		return fmt.Errorf("invalid schedule %q: %w", spec, err)
	}
	first := sched.Next(reference)
	second := sched.Next(first)
	if second.Sub(first) < MinInterval {
		return fmt.Errorf("schedule %q fires more often than once per minute — that would hammer your infrastructure; use @hourly, @daily, or a cron like \"*/15 * * * *\"", spec)
	}
	return nil
}

// Scheduler runs one job on a cron spec. Reconfigurable at runtime: the
// settings UI can change the schedule without a restart.
type Scheduler struct {
	cron *cron.Cron
	job  func(context.Context)
	log  *slog.Logger
	spec string
	id   cron.EntryID
}

// New creates a stopped scheduler for job. Call Set to start it.
func New(job func(context.Context), log *slog.Logger) *Scheduler {
	return &Scheduler{
		cron: cron.New(),
		job:  job,
		log:  log,
	}
}

// Set installs (or replaces) the schedule. An empty spec disables it.
// Standard cron syntax with optional descriptors (@daily, @hourly, …).
func (s *Scheduler) Set(spec string) error {
	if s.id != 0 {
		s.cron.Remove(s.id)
		s.id = 0
	}
	s.spec = spec
	if spec == "" {
		return nil
	}
	id, err := s.cron.AddFunc(spec, func() {
		s.log.Info("scheduled run starting", "spec", spec)
		s.job(context.Background())
	})
	if err != nil {
		return fmt.Errorf("invalid schedule %q: %w", spec, err)
	}
	s.id = id
	return nil
}

// Spec returns the current schedule ("" if disabled).
func (s *Scheduler) Spec() string { return s.spec }

// Next returns the next fire time as a string, or "" if disabled.
func (s *Scheduler) Next() string {
	if s.id == 0 {
		return ""
	}
	entry := s.cron.Entry(s.id)
	if entry.Next.IsZero() {
		return ""
	}
	return entry.Next.Format("2006-01-02 15:04 MST")
}

// Start begins firing scheduled jobs.
func (s *Scheduler) Start() { s.cron.Start() }

// Stop halts scheduling and waits for a running job to finish.
func (s *Scheduler) Stop() { s.cron.Stop() }
