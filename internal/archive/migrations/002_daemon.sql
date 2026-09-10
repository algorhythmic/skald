BEGIN IMMEDIATE;
ALTER TABLE archive_meta RENAME TO archive_meta_v1;
CREATE TABLE archive_meta (
 singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
 schema_version INTEGER NOT NULL CHECK(schema_version = 2),
 archive_instance TEXT NOT NULL UNIQUE,
 contract_version INTEGER NOT NULL CHECK(contract_version = 1),
 restored_from TEXT
) STRICT;
INSERT INTO archive_meta SELECT singleton,2,archive_instance,contract_version,NULL FROM archive_meta_v1;
DROP TABLE archive_meta_v1;
CREATE TABLE stream_locators (
 stream_key TEXT PRIMARY KEY REFERENCES streams(stream_key),
 root TEXT NOT NULL,
 relative_path TEXT NOT NULL,
 active INTEGER NOT NULL CHECK(active IN (0,1)),
 health TEXT NOT NULL,
 checked_at TEXT,
 conversation_id TEXT NOT NULL
) STRICT;
CREATE INDEX stream_locators_conversation_health ON stream_locators(conversation_id,active,health,stream_key);
CREATE TABLE record_heads (
 record_key TEXT PRIMARY KEY,
 source_revision TEXT NOT NULL,
 adapter_version TEXT NOT NULL,
 FOREIGN KEY(record_key,source_revision,adapter_version)
 REFERENCES normalizations(record_key,source_revision,adapter_version)
) STRICT;
CREATE TABLE stream_generations (
 stream_key TEXT NOT NULL REFERENCES streams(stream_key),
 generation TEXT NOT NULL,
 epoch INTEGER NOT NULL CHECK(epoch>=0),
 PRIMARY KEY(stream_key,generation),
 UNIQUE(stream_key,epoch)
) STRICT;
CREATE TABLE session_projects (
 conversation_key TEXT NOT NULL REFERENCES sessions(conversation_key),
 project TEXT NOT NULL,
 record_key TEXT NOT NULL,
 source_revision TEXT NOT NULL,
 PRIMARY KEY(conversation_key,project),
 FOREIGN KEY(record_key,source_revision) REFERENCES artifact_versions(record_key,source_revision)
) STRICT;
CREATE TABLE session_titles (
 conversation_key TEXT PRIMARY KEY REFERENCES sessions(conversation_key),
 record_key TEXT NOT NULL,
 source_revision TEXT NOT NULL,
 adapter_version TEXT NOT NULL,
 title TEXT NOT NULL,
 stream_key TEXT NOT NULL REFERENCES streams(stream_key),
 epoch INTEGER NOT NULL,
 ordinal INTEGER NOT NULL,
 ordering_ambiguous INTEGER NOT NULL DEFAULT 0 CHECK(ordering_ambiguous IN (0,1)),
 FOREIGN KEY(record_key,source_revision,adapter_version) REFERENCES normalizations(record_key,source_revision,adapter_version)
) STRICT;
PRAGMA user_version = 2;
COMMIT;
