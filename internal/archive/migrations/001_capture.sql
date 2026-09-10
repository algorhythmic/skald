-- L0 capture-storage contract. Applied by the future sole-writer daemon in L1.
-- SQLite >= 3.37 (STRICT tables). All original content remains inside SQLite.
PRAGMA foreign_keys = ON;
BEGIN IMMEDIATE;

CREATE TABLE archive_meta (
  singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
  schema_version INTEGER NOT NULL CHECK(schema_version = 1),
  archive_instance TEXT NOT NULL UNIQUE,
  contract_version INTEGER NOT NULL CHECK(contract_version = 1)
) STRICT;

CREATE TABLE sources (
  namespace TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  adapter_version TEXT NOT NULL,
  capabilities_json TEXT NOT NULL CHECK(json_valid(capabilities_json)),
  scope_json TEXT NOT NULL CHECK(json_valid(scope_json))
) STRICT;
CREATE TABLE source_roots (
  namespace TEXT NOT NULL REFERENCES sources(namespace),
  canonical_root TEXT NOT NULL,
  evidence TEXT NOT NULL,
  PRIMARY KEY(namespace, canonical_root)
) STRICT;
CREATE TABLE streams (
  stream_key TEXT PRIMARY KEY,
  namespace TEXT NOT NULL REFERENCES sources(namespace),
  logical_id TEXT NOT NULL,
  UNIQUE(namespace, logical_id)
) STRICT;
CREATE TABLE ingest_checkpoints (
  stream_key TEXT PRIMARY KEY REFERENCES streams(stream_key),
  generation TEXT NOT NULL,
  captured_offset INTEGER NOT NULL CHECK(captured_offset >= 0),
  parsed_offset INTEGER NOT NULL CHECK(parsed_offset BETWEEN 0 AND captured_offset),
  ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
  checkpoint_json TEXT NOT NULL CHECK(json_valid(checkpoint_json)),
  has_gaps INTEGER NOT NULL CHECK(has_gaps IN (0,1))
) STRICT;
CREATE TABLE sessions (
  conversation_key TEXT PRIMARY KEY,
  namespace TEXT NOT NULL REFERENCES sources(namespace),
  native_id TEXT NOT NULL,
  source_availability TEXT NOT NULL CHECK(source_availability IN ('available','partial','unsupported','opaque','unavailable','unknown')),
  UNIQUE(namespace, native_id),
  UNIQUE(conversation_key, namespace)
) STRICT;
CREATE TABLE session_aliases (
  namespace TEXT NOT NULL REFERENCES sources(namespace),
  native_alias TEXT NOT NULL,
  conversation_key TEXT NOT NULL,
  evidence TEXT NOT NULL,
  PRIMARY KEY(namespace, native_alias),
  FOREIGN KEY(conversation_key, namespace) REFERENCES sessions(conversation_key, namespace)
) STRICT;
CREATE TABLE content_objects (
  digest TEXT PRIMARY KEY CHECK(length(digest) = 71 AND substr(digest,1,7) = 'sha256:' AND substr(digest,8) NOT GLOB '*[^0-9a-f]*'),
  byte_length INTEGER NOT NULL CHECK(byte_length >= 0),
  media_type TEXT NOT NULL,
  original_bytes BLOB NOT NULL,
  CHECK(length(original_bytes) = byte_length)
) STRICT;
CREATE TABLE artifacts (
  record_key TEXT PRIMARY KEY,
  namespace TEXT NOT NULL REFERENCES sources(namespace),
  origin_kind TEXT NOT NULL CHECK(origin_kind IN ('conversation','project','source')),
  origin_key TEXT NOT NULL,
  conversation_key TEXT,
  FOREIGN KEY(conversation_key, namespace) REFERENCES sessions(conversation_key, namespace),
  CHECK((origin_kind = 'conversation' AND conversation_key IS NOT NULL AND origin_key = conversation_key)
    OR (origin_kind IN ('project','source') AND conversation_key IS NULL))
) STRICT;
CREATE INDEX artifacts_conversation ON artifacts(conversation_key, record_key);
CREATE TABLE artifact_versions (
  record_key TEXT NOT NULL REFERENCES artifacts(record_key),
  source_revision TEXT NOT NULL REFERENCES content_objects(digest),
  first_observed_at TEXT NOT NULL,
  PRIMARY KEY(record_key, source_revision)
) STRICT;
CREATE TABLE normalizations (
  record_key TEXT NOT NULL,
  source_revision TEXT NOT NULL,
  adapter_version TEXT NOT NULL,
  contract_version INTEGER NOT NULL CHECK(contract_version = 1),
  kind TEXT NOT NULL,
  source_time TEXT,
  stream_generation TEXT,
  source_ordinal INTEGER CHECK(source_ordinal >= 0),
  envelope_json TEXT NOT NULL CHECK(json_valid(envelope_json)),
  PRIMARY KEY(record_key, source_revision, adapter_version),
  FOREIGN KEY(record_key, source_revision) REFERENCES artifact_versions(record_key, source_revision)
) STRICT;
CREATE INDEX normalizations_order ON normalizations(stream_generation, source_ordinal);
CREATE INDEX normalizations_kind_time ON normalizations(kind, source_time);
-- Delivery/order evidence is separate from immutable native versions. Re-reading
-- a native-ID record in a new stream generation does not rewrite its first envelope.
CREATE TABLE record_observations (
  stream_key TEXT NOT NULL REFERENCES streams(stream_key),
  generation TEXT NOT NULL,
  ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
  record_key TEXT NOT NULL,
  source_revision TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  PRIMARY KEY(stream_key, generation, ordinal, record_key, source_revision),
  FOREIGN KEY(record_key, source_revision) REFERENCES artifact_versions(record_key, source_revision)
) STRICT;
CREATE TABLE capture_gaps (
  stream_key TEXT NOT NULL REFERENCES streams(stream_key),
  generation TEXT NOT NULL,
  source_offset INTEGER NOT NULL CHECK(source_offset >= 0),
  code TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  PRIMARY KEY(stream_key, generation, source_offset, code)
) STRICT;
CREATE TABLE changes (
  sequence INTEGER PRIMARY KEY AUTOINCREMENT,
  change_kind TEXT NOT NULL CHECK(change_kind IN ('record','gap','withdrawal','source')),
  namespace TEXT NOT NULL REFERENCES sources(namespace),
  record_key TEXT,
  source_revision TEXT,
  payload_json TEXT NOT NULL CHECK(json_valid(payload_json))
) STRICT;
CREATE TABLE consumer_checkpoints (
  consumer_id TEXT NOT NULL,
  scope_generation TEXT NOT NULL,
  sequence INTEGER NOT NULL CHECK(sequence >= 0),
  PRIMARY KEY(consumer_id, scope_generation)
) STRICT;
CREATE TABLE purge_tombstones (
  namespace TEXT NOT NULL REFERENCES sources(namespace),
  scope_kind TEXT NOT NULL CHECK(scope_kind IN ('source','conversation','record')),
  scope_key TEXT NOT NULL,
  revision INTEGER NOT NULL CHECK(revision > 0),
  suppressed_at TEXT NOT NULL,
  PRIMARY KEY(namespace, scope_kind, scope_key)
) STRICT;

CREATE TRIGGER immutable_content BEFORE UPDATE ON content_objects
BEGIN SELECT RAISE(ABORT, 'content objects are immutable'); END;
CREATE TRIGGER immutable_versions BEFORE UPDATE ON artifact_versions
BEGIN SELECT RAISE(ABORT, 'source revisions are immutable'); END;
CREATE TRIGGER immutable_normalizations BEFORE UPDATE ON normalizations
BEGIN SELECT RAISE(ABORT, 'normalization versions are immutable'); END;
PRAGMA user_version = 1;
COMMIT;
