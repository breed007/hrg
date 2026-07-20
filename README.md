<p align="center">
  <img src="docs/img/banner.svg" alt="HRG — Home Runbook Generator" width="820">
</p>

<p align="center">
  <img src="https://img.shields.io/badge/license-MIT-2f6f4f" alt="MIT License">
  <img src="https://img.shields.io/badge/Go-1.26+-00ADD8" alt="Go 1.26+">
  <img src="https://img.shields.io/badge/release-v0.2.0-2f6f4f" alt="v0.2.0">
  <img src="https://img.shields.io/badge/self--hosted-single%20binary-3d5a80" alt="self-hosted">
</p>

If you got hit by a bus tomorrow, could the people you live with keep the
house running?

Not "could they administer your Proxmox cluster" — could they get the TV
working, find out who to call about the internet, work out which of the
things humming in the basement actually matter and which are just your
projects, and cancel the subscriptions nobody needs?

**HRG documents your home's technology from the infrastructure itself,
keeps it current automatically, and generates two guides: one for the
people who live here, and one for whoever does the technical work.**

<p align="center">
  <img src="docs/img/how-it-works.svg" alt="How HRG works: collectors → temporal store → your annotations → the runbook artifact" width="900">
</p>

---

## Three principles that shape everything

1. **The person who needs this most is not technical.** They are your
   partner, your kid, your executor, or a neighbour holding your phone. The
   Household Guide is written for them — plain language, no jargon, no
   assumed knowledge — and it is not a summary of the technical guide. It
   is a different document for a different reader.
2. **The guides must survive the death of everything they document.** Every
   generation is a static, portable artifact with no links back to HRG, and
   HRG pushes copies off this machine automatically. A runbook that only
   exists on the server it documents is missing in exactly the situations
   it was written for.
3. **Collectors are read-only, and secrets are never stored.** HRG records
   *where* credentials live ("1Password vault 'Home', item 'UDM-Pro'"),
   never the credentials themselves. The only secrets it holds are its own
   collector tokens and delivery passwords, encrypted at rest.

## The two guides

Both are generated from the same data in the same run, so they can never
disagree about the same house.

### 📗 Household Guide

For the person who did not build any of this.

1. **What is all this?** — a few plain sentences you write once.
2. **If something is broken** — the internet is out, the TV won't play,
   something is beeping. Physical locations, power buttons, plain steps.
3. **Who to call.**
4. **Where the equipment is** — most problems are fixed by finding one of
   these and restarting it.
5. **What each thing does** — in plain English, sorted so *the house needs
   this* comes before somebody's k3s experiment, with what it costs and
   whose card it's on.
6. **Turning things off safely** — see below.

<p align="center">
  <img src="docs/img/artifact.svg" alt="The generated Household Guide" width="720">
</p>

### 📘 Administrator Guide

For whoever actually does the work — the technical friend the household
calls, or future-you. Topology diagram, IP plan, service catalog, backup
coverage and restore-test status, recovery procedures, contacts and
accounts, and a full inventory with change history.

Each guide ships as a self-contained HTML file and a printable PDF, plus a
git-committable Markdown tree of both.

## "Can I turn this off?"

The hardest question a survivor faces, and the one an inventory cannot
answer — because the answer lives in the relationships, not the list. The
UPS is only "nice to have" until you notice the modem is plugged into it.

HRG inverts the dependency graph and walks it transitively, then sorts
everything into the three answers a person actually needs:

- **Leave these alone** — essential, or something essential depends on it.
- **Safe to switch off** — not needed, and nothing needed depends on it.
- **Ask before switching these off** — nobody classified it, so the guide
  says so rather than guessing.

That third bucket matters. A guide that quietly sorted unclassified things
as "safe" would eventually tell someone to unplug something that mattered.

Your own words always win: if you wrote *"don't — the photos and the TVs
all come off this box"*, that is what appears. Where you didn't, HRG shows
what it worked out **and says that it worked it out.**

## Getting the guides off this machine

Configuring destinations is not an optional extra — it is the step that
decides whether any of the rest matters.

| Destination | Why |
|---|---|
| **Sync folder** | One mechanism covers Dropbox, OneDrive, Google Drive and iCloud Drive — all four present as a local folder. No OAuth, no token to expire silently in three years. |
| **Email** | The only destination that survives losing every machine in the house, and the only one a non-technical person finds without being told how. |
| **rclone remote** | Box, S3, Backblaze, WebDAV and the rest. rclone owns the credentials and the token refresh — the part that rots. |

Pick per destination which guide and which format goes where, so the inbox
holding the household copy need not also hold a map of your network.
Defaults: **PDF** for the household (opens on any phone with no app,
previews inside email, prints) and **HTML** for the administrator (the only
format where the network diagram is readable).

Every attempt is recorded, there's a **Send now** button to prove it works
before it matters, and the dashboard nags at three distinct failures:
never configured, configured but never delivered, and last delivered over a
month ago.

## Is the document actually usable?

The dashboard scores readiness in **two dimensions**, because a resource
can be perfectly documented for an administrator and still be meaningless
to the person who lives here.

<p align="center">
  <img src="docs/img/dashboard.svg" alt="HRG dashboard: household and administrator readiness" width="820">
</p>

**Household readiness** asks whether someone who didn't build this house
could run it. Not "72% documented" — *"3 of your 6 essential systems have
no plain-English description"*, which is a sentence you can act on.

**Administrator readiness** tracks purpose, recovery procedures, credential
pointers, and backups nobody has restore-tested.

## Quick start

### Docker Compose (recommended)

```sh
git clone https://github.com/breed007/hrg && cd hrg
docker compose up -d
# → http://127.0.0.1:8080
```

The compose file publishes the UI on `127.0.0.1` only, and the image
bundles Chromium so PDF export works out of the box. Persistent state lives
in a named volume; your manual facts are read from `./resources.d`.

### Bare binary

Grab a build from the [releases page](https://github.com/breed007/hrg/releases),
or build it yourself — the only requirement is **Go 1.26+** (no Node, no C
compiler, no external services):

```sh
go build ./cmd/hrg
./hrg serve
# → http://127.0.0.1:8080
```

PDF export additionally wants a headless Chromium/Chrome on `PATH`; HTML and
Markdown export don't need it.

## First run: the setup wizard

A fresh install opens a six-step wizard:

1. **Name it & choose where exports go.**
2. **Set an access password** — HRG has no auth until you set one.
3. **Add a collector** — point HRG at your infrastructure (read-only), with
   a **Test connection** button that catches a bad credential *before* you
   save. Or skip and describe things by hand in `resources.d/`.
4. **Write the two pages only you can write** — "What is all this?" and
   "If something is broken". HRG pre-fills skeletons.
5. **Generate your two guides.**
6. **Send a copy somewhere else.** Pick at least two destinations that fail
   differently.

Re-run it any time at `/setup`.

## Collectors

Each collector is read-only; give it read-only credentials.

| Collector | Reads | Auth |
|---|---|---|
| **Proxmox** | nodes, VMs, LXCs, storage, backup jobs, HA state | API token (`PVEAuditor`) |
| **Docker** | containers, compose projects, volumes, networks | socket or TCP |
| **UniFi** | networks/VLANs, WLANs, devices + uplinks, firewall summary | API key (Network 9.0+) |
| **NetBox** | prefixes + IP assignments, devices, racks (authoritative IP plan) | read-only token |
| **AdGuard Home** | DNS rewrites, upstreams, DHCP + static leases | basic auth |
| **Manual (YAML)** | anything without an API — the modem, the ISP account, the UPS | — |

Collectors run on a schedule, **diff against the previous snapshot**, and
version what changed. A resource that disappears becomes an **orphan** —
flagged, never deleted, because your notes about it are the valuable part.
Every collector has a fixture mode for tests and demos, and
[writing a new one](docs/writing-collectors.md) is one method plus a test.

## The annotation layer

APIs answer *what*; only you answer *why* — and *why* has two audiences, so
every resource takes two separate sets of notes.

**For the household:** a plain-English description ("stores our photos and
lets the TVs play movies" — not "ZFS pool exported over NFS"), whether the
house needs it (essential / nice to have / just a project), what breaks if
it's switched off, and what it costs and who pays.

**For the administrator:** purpose, recovery procedure (checklists render as
checkboxes), credential pointer, and notes.

They're deliberately separate fields. The sentence that explains something
to a partner is not the sentence that explains it to an engineer, and
overloading one field would mean writing for both readers and serving
neither. Annotations are keyed to resource *identity*, so they survive
re-collection, attribute churn, and container recreates.

## Staying current

HRG's job is partly to *guilt* you into finishing the document, and to keep
it fresh once you have:

- **Scheduled collection** — an in-process cron (`@daily`, `0 3 * * *`, …);
  no system cron, no queue.
- **Drift notifications** — point HRG at an [ntfy](https://ntfy.sh) topic or
  any webhook; a scheduled run that finds changes (or a collector failure)
  sends an alert.
- **Auto-regeneration and re-delivery** on drift, so the copy in someone
  else's hands never goes stale.
- **Restore-test tracking** — record when you last actually tested a
  backup's restore. Untested backups show **"last verified: never"** in red.
  A backup nobody has restored is a hope, not a backup.

## Security posture

- **Authentication is optional and off until you set a password.** HRG is a
  single-user tool; one shared password gates the whole UI. The dashboard
  nags until it's set.
- **CSRF protection is always on** — state-changing requests are rejected
  unless they originate from HRG itself, so a malicious page in another tab
  can't drive your endpoints even with no password set.
- Binds `127.0.0.1` by default; sessions use `HttpOnly; SameSite=Strict`
  cookies. Terminate TLS at a reverse proxy for anything wider.
- **Email delivery refuses to send without STARTTLS.** Pushing a map of
  your network across the wire in the clear is not a tradeoff HRG offers.
- Collector tokens and delivery passwords are encrypted at rest
  (AES-256-GCM). The database, any export, and a config backup are all maps
  of your network — store them on encrypted disk or in a private repo.
- **The Household Guide is deliberately unencrypted.** A survivor who can't
  open the attachment has nothing at all. Choose its destinations with that
  in mind.

## Configuration

```
hrg [flags] serve      collect once, then serve the web UI (default)
hrg [flags] collect    run all collectors once and exit

  -addr       listen address (default 127.0.0.1:8080)
  -db         SQLite database path (default hrg.db)
  -key        token-encryption key file (default hrg.key, created on first use)
  -resources  manual resources.d directory (default resources.d)
  -dev        enable developer affordances — never use in production
  -version    print version and exit
```

Configuration backup/restore (Settings → Configuration backup) exports the
non-regenerable state — collector configs, destinations, pages, annotations,
settings — for disaster recovery and host migration.

## Contributing

The most valuable contribution is a **new collector** — HRG can only
document gear someone has taught it to read. It's one method and a
fixture-backed test; see [docs/writing-collectors.md](docs/writing-collectors.md).
Development needs only Go 1.26+. Full guide in [CONTRIBUTING.md](CONTRIBUTING.md).

## Status

**v0.2.0** — re-centred on the household. The single runbook became two
co-equal guides; resources gained household-facing annotations and a
two-dimensional readiness score; the dependency graph now answers "what is
safe to switch off"; and delivery to sync folders, email and rclone remotes
means copies leave the machine automatically.

**v0.1.0** — the first public release. Six collectors, temporal diffing, the
annotation layer, network map & IP-plan reconciliation, runbook generation
(HTML + Markdown + PDF), scheduling, drift notifications, freshness tracking,
optional auth, and a setup wizard.

Built as a single static binary with an embedded UI and SQLite.

## License

[MIT](LICENSE)
