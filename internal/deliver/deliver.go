// Package deliver gets the runbook off the machine that generated it.
//
// This is the part that decides whether any of the rest matters. A perfect
// runbook sitting on a server in the basement is worth nothing in exactly
// the situations it was written for: the server is dead, the house was
// sold, or the person who built it is gone. So delivery is treated as a
// first-class pipeline with its own history, not a nice-to-have.
//
// Every destination is deliberately dumb — it takes finished bytes and puts
// them somewhere else. No destination knows what a runbook is.
package deliver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// File is one finished artifact, ready to leave the building.
type File struct {
	Name        string // e.g. "household-guide.pdf"
	ContentType string
	Bytes       []byte
}

// Destination sends files somewhere durable.
//
// Send must be idempotent per generation: a destination is re-sent every
// time the runbook regenerates, so "already there" is success, not an
// error. It must also never mutate the files it is given.
type Destination interface {
	// Send delivers every file, or returns the first error. Detail is a
	// short human-readable summary recorded in the delivery history —
	// "3 files to /Users/x/Dropbox/Home" — shown to someone checking that
	// their copy actually left.
	Send(ctx context.Context, files []File) (detail string, err error)
}

// Factory builds a destination from its stored configuration. secret is
// the decrypted credential (SMTP password, token) or "" when none is set —
// destinations never see the sealed blob or the key.
type Factory func(cfg json.RawMessage, secret string) (Destination, error)

// Kind describes one destination type for the UI: what it is, what it
// needs, and who it is for. The wizard renders straight from this.
type Kind struct {
	Type string
	// Label is what the type is called in the UI.
	Label string
	// Blurb is one sentence explaining when to pick it, written for
	// somebody who has not thought about backup topology before.
	Blurb string
	// NeedsSecret reports whether the form shows a password field.
	NeedsSecret bool
	// SecretLabel names that field ("SMTP password", "API token").
	SecretLabel string
	Fields      []Field
	New         Factory
}

// Field is one configuration input.
type Field struct {
	Key         string
	Label       string
	Help        string
	Placeholder string
	Required    bool
}

var kinds = map[string]Kind{}

// Register adds a destination type. Called from each destination's init.
func Register(k Kind) { kinds[k.Type] = k }

// Kinds lists every registered destination type, cheapest-to-explain
// first — the order the wizard offers them in.
func Kinds() []Kind {
	out := make([]Kind, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return order(out[i].Type) < order(out[j].Type) })
	return out
}

// order fixes the display sequence: the folder covers the most services
// for the least explaining (Dropbox, OneDrive, Drive and iCloud all expose
// one), email is the one destination that survives losing every device,
// and rclone is the power tool for everything else.
func order(t string) int {
	switch t {
	case "folder":
		return 0
	case "email":
		return 1
	case "rclone":
		return 2
	}
	return 99
}

// Lookup returns the kind, or false.
func Lookup(t string) (Kind, bool) {
	k, ok := kinds[t]
	return k, ok
}

// New builds a destination of the given type.
func New(t string, cfg json.RawMessage, secret string) (Destination, error) {
	k, ok := kinds[t]
	if !ok {
		return nil, fmt.Errorf("unknown destination type %q", t)
	}
	return k.New(cfg, secret)
}
