BEGIN IMMEDIATE;
ALTER TABLE archive_meta RENAME TO archive_meta_v3;
CREATE TABLE archive_meta (
 singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
 schema_version INTEGER NOT NULL CHECK(schema_version = 4),
 archive_instance TEXT NOT NULL UNIQUE,
 contract_version INTEGER NOT NULL CHECK(contract_version = 1),
 restored_from TEXT
) STRICT;
INSERT INTO archive_meta SELECT singleton,4,archive_instance,contract_version,restored_from FROM archive_meta_v3;
DROP TABLE archive_meta_v3;
CREATE TABLE session_activity (
 conversation_key TEXT PRIMARY KEY REFERENCES sessions(conversation_key),
 stream_key TEXT NOT NULL REFERENCES streams(stream_key),
 signal TEXT NOT NULL CHECK(signal IN ('none','working','idle','input')),
 signal_epoch INTEGER NOT NULL DEFAULT 0,
 signal_ordinal INTEGER NOT NULL DEFAULT 0,
 signal_time TEXT,
 seen_epoch INTEGER NOT NULL,
 seen_ordinal INTEGER NOT NULL,
 seen_time TEXT,
 ordering_ambiguous INTEGER NOT NULL DEFAULT 0 CHECK(ordering_ambiguous IN (0,1))
) STRICT;
PRAGMA user_version = 4;
COMMIT;
