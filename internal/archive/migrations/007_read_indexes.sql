-- Keep scoped status counts on compact indexes rather than revisiting artifact
-- table pages for every historical version. No content or derived counters change.
BEGIN IMMEDIATE;
CREATE INDEX artifacts_namespace ON artifacts(namespace,record_key);
ALTER TABLE archive_meta RENAME TO archive_meta_v6;
CREATE TABLE archive_meta (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 schema_version INTEGER NOT NULL CHECK(schema_version=7),
 archive_instance TEXT NOT NULL UNIQUE,
 contract_version INTEGER NOT NULL CHECK(contract_version=1),
 restored_from TEXT
) STRICT;
INSERT INTO archive_meta SELECT singleton,7,archive_instance,contract_version,restored_from FROM archive_meta_v6;
DROP TABLE archive_meta_v6;
PRAGMA user_version=7;
COMMIT;
