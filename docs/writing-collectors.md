# Writing a collector

A collector teaches HRG to read one source of infrastructure truth — a
Proxmox cluster, a Docker host, a UPS with an API, whatever you have that
HRG doesn't yet speak to. This guide walks through writing one. The
existing collectors under [`internal/collector/`](../internal/collector/)
are the reference; the Docker one is a good template for an HTTP API.

The community will write collectors for gear the maintainer doesn't own, so
the bar to add one is kept deliberately low: implement one method, return
typed resources, register it.

## The contract

A collector is anything that satisfies this interface
([`internal/collector/collector.go`](../internal/collector/collector.go)):

```go
type Collector interface {
    Name() string                                  // instance name, e.g. "proxmox:pve1"
    Collect(ctx context.Context) (Result, error)   // one full observation
}

type Result struct {
    Resources []model.Resource
    Edges     []model.Edge
}
```

Two rules do almost all the work:

1. **Collectors are read-only.** Never write to the system you document.
   Create read-only API tokens and say so in your config help text.
2. **`Collect` returns a full snapshot every time.** Don't diff, don't
   track what changed, don't carry state between calls. The engine
   (`internal/store`) owns diffing, versioning, and orphan detection. You
   just report the world as it is right now.

## The one hard part: stable source IDs

Every resource carries a `SourceID`. It is the resource's permanent
identity — annotations, history, and manual relationships are all keyed to
`(collector instance, SourceID)`. **If your SourceID churns, the user's
notes detach.** This is the single most important decision in a collector.

Good source IDs are derived from something the upstream system treats as
stable:

- Proxmox reuses the API's own cluster IDs — `qemu/104`, `lxc/105`.
- Docker containers get `compose/{project}/{service}` (survives the
  recreate that changes the container ID) or `container/{name}` as a
  fallback — **not** the container ID, which changes on every `up`.
- UniFi devices use the MAC address; networks use the controller's `_id`.

Ask: "if the user reboots/recreates/renames this thing, does my ID stay the
same?" If not, find a more stable key.

## A minimal collector

```go
package widget

import (
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "time"

    "github.com/breed007/hrg/internal/collector"
    "github.com/breed007/hrg/internal/collector/fixture"
    "github.com/breed007/hrg/internal/model"
)

const Type = "widget"

// Config is the non-secret instance configuration, stored as JSON. The
// secret (API token/password) is passed separately, decrypted, in Spec.
type Config struct {
    URL        string `json:"url"`
    FixtureDir string `json:"fixture_dir,omitempty"` // replay recorded responses
}

type Collector struct {
    instance string
    cfg      Config
    secret   string
    client   *http.Client
}

// Factory validates config and builds the instance. Fail fast here — a
// misconfigured collector should error at save time, not mid-run.
func Factory(spec collector.Spec) (collector.Collector, error) {
    var cfg Config
    if err := json.Unmarshal(spec.Config, &cfg); err != nil {
        return nil, fmt.Errorf("%s: bad config: %w", spec.Instance, err)
    }
    if cfg.FixtureDir == "" && cfg.URL == "" {
        return nil, fmt.Errorf("%s: url is required", spec.Instance)
    }
    c := &Collector{instance: spec.Instance, cfg: cfg, secret: spec.Secret}
    if cfg.FixtureDir != "" {
        c.client = fixture.Client(cfg.FixtureDir) // replay from disk
    } else {
        c.client = &http.Client{Timeout: 30 * time.Second}
    }
    return c, nil
}

func (c *Collector) Name() string { return c.instance }

func (c *Collector) Collect(ctx context.Context) (collector.Result, error) {
    var res collector.Result
    // ... call c.client, parse the response ...
    res.Resources = append(res.Resources, model.Resource{
        Kind:     model.KindDevice,
        SourceID: "widget/" + id, // STABLE across runs
        Name:     name,
        Attrs:    map[string]any{"ip": ip, "firmware": fw},
    })
    // Relationships between resources (optional):
    res.Edges = append(res.Edges, model.Edge{
        Src:      model.Ref{SourceID: "widget/" + id},
        Dst:      model.Ref{SourceID: "widget/" + parentID},
        Relation: model.RelAttachedTo,
    })
    return res, nil
}
```

Register it in [`cmd/hrg/main.go`](../cmd/hrg/main.go) alongside the others:

```go
collector.Register(widget.Type, widget.Factory)
```

That's the whole integration — the config UI, encrypted token storage,
scheduling, diffing, and the runbook all pick it up automatically.

## Kinds and relations

Use the vocabulary in [`internal/model/model.go`](../internal/model/model.go).
Kinds: `host`, `vm`, `lxc`, `container`, `network`, `vlan`, `service`,
`storage`, `backup_job`, `device`, `account`, `location`, `other`.
Relations: `runs_on`, `attached_to`, `member_of`, `depends_on`,
`backed_up_by`, `resolves_to`, `located_in`.

`Attrs` is freeform JSON — put whatever the system gives you. A few keys are
conventions the runbook and network views read, so use them when they fit:

- `ip` (string) — a device/host's primary address (feeds the IP plan).
- `cidr` (string) — a network/VLAN's prefix, e.g. `10.0.10.0/24`.
- `location` (string) / `power_cycle` (string) — surfaced in the physical
  layer of the runbook.

Edges may reference resources owned by *other* collectors — set
`Ref.Collector` to the other instance name. Cross-collector edges that
arrive before their target exists are parked and resolved automatically once
both endpoints are present, so ordering never matters.

## Fixtures: every collector ships one

Each collector supports a **fixture mode** that replays recorded API
responses from a directory instead of hitting a live endpoint. This is how
tests and demos run without infrastructure, and the recorded files double as
documentation of the API's shape.

[`internal/collector/fixture`](../internal/collector/fixture/fixture.go)
maps a request URL path to a file: strip the leading slash, replace the rest
with underscores, append `.json`. So `GET /api2/json/cluster/resources` is
served from `api2_json_cluster_resources.json`.

Record real (redacted!) responses under `testdata/`, then a test is just:

```go
func TestCollect(t *testing.T) {
    cfg, _ := json.Marshal(Config{FixtureDir: "testdata/widget"})
    c, _ := Factory(collector.Spec{Type: Type, Instance: "widget:test", Config: cfg})
    res, err := c.Collect(context.Background())
    // ... assert on res.Resources and res.Edges ...
}
```

Assert that every resource validates (`r.Validate()`), that source IDs are
what you expect, and that edges connect the right endpoints. Copy the shape
of any existing `*_test.go` in `internal/collector/`.

## Checklist before you open a PR

- [ ] Read-only: no writes to the documented system.
- [ ] Source IDs are stable across reboots/recreates/renames.
- [ ] `Factory` validates config and fails fast with a helpful message.
- [ ] Fixture mode works, with recorded (redacted) `testdata/`.
- [ ] A test covers resources and edges against the fixtures.
- [ ] Registered in `cmd/hrg/main.go`.
- [ ] Config help text tells the user how to make a read-only credential.
- [ ] `go test ./...`, `go vet ./...`, and `gofmt -l .` are clean.
