BEGIN IMMEDIATE;
ALTER TABLE archive_meta RENAME TO archive_meta_v2;
CREATE TABLE archive_meta (
 singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
 schema_version INTEGER NOT NULL CHECK(schema_version = 3),
 archive_instance TEXT NOT NULL UNIQUE,
 contract_version INTEGER NOT NULL CHECK(contract_version = 1),
 restored_from TEXT
) STRICT;
INSERT INTO archive_meta SELECT singleton,3,archive_instance,contract_version,restored_from FROM archive_meta_v2;
DROP TABLE archive_meta_v2;
ALTER TABLE session_titles ADD COLUMN origin TEXT NOT NULL DEFAULT 'native' CHECK(origin IN ('native','derived'));
PRAGMA user_version = 3;
COMMIT;
