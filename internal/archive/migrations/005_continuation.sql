-- Schema 5: session_titles gains a 'continuation' origin for compaction-resume
-- sessions labeled from their parent thread. The CHECK cannot be altered in
-- place, so the table is rebuilt inside one transaction.
BEGIN IMMEDIATE;
CREATE TABLE session_titles_new (
 conversation_key TEXT PRIMARY KEY REFERENCES sessions(conversation_key),
 record_key TEXT NOT NULL,
 source_revision TEXT NOT NULL,
 adapter_version TEXT NOT NULL,
 title TEXT NOT NULL,
 stream_key TEXT NOT NULL REFERENCES streams(stream_key),
 epoch INTEGER NOT NULL,
 ordinal INTEGER NOT NULL,
 ordering_ambiguous INTEGER NOT NULL DEFAULT 0 CHECK(ordering_ambiguous IN (0,1)),
 origin TEXT NOT NULL DEFAULT 'native' CHECK(origin IN ('native','derived','continuation')),
 FOREIGN KEY(record_key,source_revision,adapter_version) REFERENCES normalizations(record_key,source_revision,adapter_version)
) STRICT;
INSERT INTO session_titles_new SELECT * FROM session_titles;
DROP TABLE session_titles;
ALTER TABLE session_titles_new RENAME TO session_titles;
ALTER TABLE archive_meta RENAME TO archive_meta_v4;
CREATE TABLE archive_meta (
 singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
 schema_version INTEGER NOT NULL CHECK(schema_version = 5),
 archive_instance TEXT NOT NULL UNIQUE,
 contract_version INTEGER NOT NULL CHECK(contract_version = 1),
 restored_from TEXT
) STRICT;
INSERT INTO archive_meta SELECT singleton,5,archive_instance,contract_version,restored_from FROM archive_meta_v4;
DROP TABLE archive_meta_v4;
PRAGMA user_version = 5;
COMMIT;
