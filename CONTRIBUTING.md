# Contributing to HRG

Thanks for helping. HRG aims to be the kind of self-hosted tool that doesn't
become its own maintenance burden — so the codebase is deliberately small,
dependency-light, and buildable with nothing but a Go toolchain.

## Development setup

You need **Go 1.26+**. That's it — there is no Node toolchain, no bundler,
and no C compiler (SQLite is pure Go via `modernc.org/sqlite`).

```sh
git clone https://github.com/breed007/hrg
cd hrg
go build ./cmd/hrg
./hrg serve          # → http://127.0.0.1:8080
```

The web UI is server-rendered `html/template` + htmx; both htmx and Mermaid
are vendored into the binary via `embed.FS`, so the whole app is one static
binary with no external assets.

PDF export additionally needs a headless **Chromium or Chrome** on `PATH`
(the Docker image ships one). Everything else — HTML and Markdown export
included — works without it.

## Before you push

```sh
go test ./...      # all packages
go vet ./...
gofmt -l .         # must print nothing
```

CI runs these; a PR won't merge until they're green. Please add tests for
new behavior — every collector and the store are well covered, and the
runbook renderers assert on the generated artifact.

## Project layout

```
cmd/hrg/            entrypoint, flag parsing, collector registration
internal/
  model/            resource/edge vocabulary, identity + hashing rules
  store/            SQLite schema, migrations, the ingest diff engine
  collector/        the Collector interface, registry, fixture replay
    <name>/         one package per collector, each with testdata/
  secrets/          AES-GCM token encryption at rest
  netmap/           IP-plan reconciliation + Mermaid topology
  runbook/          the artifact: Document model + HTML/Markdown/PDF renderers
  schedule/         in-process cron
  notify/           ntfy/webhook notifications
  web/              handlers + embedded templates and static assets
  assets/           files shared between the web UI and the artifact
docs/               contributor documentation
resources.d/        manual YAML facts (example set under example/)
```

## Guiding principles

These shape most review feedback, so they're worth internalizing:

1. **The artifact survives the death of the thing it documents.** Exports
   are fully self-contained — no external references, no links back to the
   app. There is a test that enforces this; don't work around it.
2. **Collectors are read-only, and HRG never stores the documented
   systems' secrets** — only pointers to where they live. The only secrets
   HRG holds are its own collector API tokens, encrypted at rest.
3. **Smallest maintenance surface wins.** New dependencies need a real
   justification. Prefer the standard library; prefer shelling out to a
   tool the user already has (git, Chromium) over pulling a large library.
4. **Every collector ships a fixture mode** so tests and demos run without
   live infrastructure.

## Writing a collector

This is the most common and most welcome contribution — HRG can only
support gear someone has written a collector for. See
[docs/writing-collectors.md](docs/writing-collectors.md) for a full walk
through with a template. The short version: implement one `Collect` method
returning typed resources with **stable source IDs**, add a fixture-backed
test, and register it in `cmd/hrg/main.go`.

## Commit and PR style

- Keep commits focused; write messages that explain the *why*, not just the
  *what*.
- Match the surrounding code's style, comment density, and naming.
- If you change the artifact output or a collector's shape, update the
  relevant test fixtures in the same commit.

## Reporting bugs

Open an issue with what you ran, what you expected, and what happened.
Because collectors have fixture mode, a recorded (redacted) API response
that reproduces the problem is the most useful thing you can attach.

## License

By contributing, you agree that your contributions are licensed under the
project's [MIT License](LICENSE).
