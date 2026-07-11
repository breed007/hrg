-- HRG schema v1.
--
-- Identity vs. state: `resources` rows are permanent identities keyed by
-- (collector, source_id); annotations hang off them and survive forever.
-- `resource_versions` holds observed state, temporally versioned — a new
-- version opens only when the content hash changes. A resource whose
-- last_seen_run predates its collector's latest successful run is orphaned.

CREATE TABLE runs (
  id          INTEGER PRIMARY KEY,
  collector   TEXT NOT NULL,
  started_at  TEXT NOT NULL,            -- RFC3339 UTC
  finished_at TEXT,
  status      TEXT NOT NULL CHECK (status IN ('ok', 'error')),
  error       TEXT,
  stats       TEXT                      -- JSON: {"seen":n,"added":n,"changed":n,"removed":n}
);

CREATE INDEX runs_collector ON runs (collector, id);

CREATE TABLE resources (
  id             INTEGER PRIMARY KEY,
  collector      TEXT NOT NULL,
  source_id      TEXT NOT NULL,
  kind           TEXT NOT NULL,
  first_seen_run INTEGER NOT NULL REFERENCES runs(id),
  last_seen_run  INTEGER NOT NULL REFERENCES runs(id),
  UNIQUE (collector, source_id)
);

CREATE INDEX resources_kind ON resources (kind);

CREATE TABLE resource_versions (
  id             INTEGER PRIMARY KEY,
  resource_id    INTEGER NOT NULL REFERENCES resources(id),
  name           TEXT NOT NULL,
  attrs          TEXT NOT NULL,         -- canonical JSON
  content_hash   TEXT NOT NULL,
  valid_from_run INTEGER NOT NULL REFERENCES runs(id),
  valid_to_run   INTEGER REFERENCES runs(id)   -- NULL = current
);

CREATE INDEX resource_versions_current
  ON resource_versions (resource_id) WHERE valid_to_run IS NULL;

CREATE TABLE edges (
  id             INTEGER PRIMARY KEY,
  src_id         INTEGER NOT NULL REFERENCES resources(id),
  dst_id         INTEGER NOT NULL REFERENCES resources(id),
  relation       TEXT NOT NULL,
  origin         TEXT NOT NULL CHECK (origin IN ('discovered', 'manual')),
  collector      TEXT,                  -- provenance when discovered
  first_seen_run INTEGER REFERENCES runs(id),
  last_seen_run  INTEGER REFERENCES runs(id),
  UNIQUE (src_id, dst_id, relation)
);

CREATE INDEX edges_src ON edges (src_id);
CREATE INDEX edges_dst ON edges (dst_id);

-- Edges whose endpoints span collectors can arrive before the other
-- collector has reported the endpoint. They wait here and are re-resolved
-- after every ingest.
CREATE TABLE pending_edges (
  id            INTEGER PRIMARY KEY,
  src_collector TEXT NOT NULL,
  src_source_id TEXT NOT NULL,
  dst_collector TEXT NOT NULL,
  dst_source_id TEXT NOT NULL,
  relation      TEXT NOT NULL,
  origin        TEXT NOT NULL,
  collector     TEXT,
  seen_run      INTEGER NOT NULL REFERENCES runs(id),
  UNIQUE (src_collector, src_source_id, dst_collector, dst_source_id, relation)
);

-- Annotations reference identity, never a version, so re-collection can
-- never detach them. Populated by the M4 UI; created now because the
-- identity-keying is the load-bearing design decision.
CREATE TABLE annotations (
  id          INTEGER PRIMARY KEY,
  resource_id INTEGER NOT NULL REFERENCES resources(id),
  field       TEXT NOT NULL CHECK (field IN ('purpose', 'recovery', 'credential_pointer', 'note')),
  body_md     TEXT NOT NULL,
  updated_at  TEXT NOT NULL,
  UNIQUE (resource_id, field)
);
