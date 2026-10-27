"""Synthetic storage fixtures: no live archive, transcript or service access."""

import hashlib
from pathlib import Path
import sqlite3
import tempfile
import unittest

from measure_archive_storage import measure, open_readonly


class StorageMeasurementTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / "archive #?.sqlite"
        connection = sqlite3.connect(self.path)
        connection.executescript("""
            CREATE TABLE archive_meta(singleton INTEGER PRIMARY KEY, schema_version INTEGER);
            INSERT INTO archive_meta VALUES(1,8);
            CREATE TABLE artifact_versions(record_key TEXT,source_revision TEXT,
                first_observed_at TEXT,PRIMARY KEY(record_key,source_revision));
            CREATE TABLE content_storage(digest TEXT PRIMARY KEY,byte_length INTEGER,
                original_bytes BLOB,decoded_length INTEGER);
            CREATE TABLE normalization_storage(record_key TEXT,source_revision TEXT,
                adapter_version TEXT,envelope_json TEXT,compressed_envelope BLOB,
                decoded_length INTEGER);
            CREATE VIEW content_objects AS SELECT skald_inflate(original_bytes,
                decoded_length,digest) AS original_bytes FROM content_storage;
            CREATE VIEW normalizations AS SELECT skald_inflate(compressed_envelope,
                decoded_length,source_revision) AS envelope_json FROM normalization_storage;
        """)
        # Deliberately not zlib: aggregates must never inflate compressed payloads.
        connection.executemany("INSERT INTO content_storage VALUES(?,?,?,?)", [
            ("packed", 4, b"pack", 100),
            ("raw", 3, b"raw", None),
            ("examined", 5, b"plain", -1),
            ("orphan", 0, b"", None),
        ])
        connection.executemany("INSERT INTO artifact_versions VALUES(?,?,?)", [
            ("early", "packed", "2026-10-22T01:00:00Z"),
            ("later-reference", "packed", "2026-10-23T01:00:00Z"),
            ("early", "raw", "2026-10-23T01:00:00Z"),
            # UTC date is October 22, despite the local date being October 23.
            ("examined", "examined", "2026-10-23T01:00:00+02:00"),
        ])
        connection.executemany("INSERT INTO normalization_storage VALUES(?,?,?,?,?,?)", [
            ("early", "packed", "adapter-1", "{}", b"pack", 100),
            ("early", "raw", "adapter-1", "é", None, None),
            ("examined", "examined", "adapter-1", "plain", None, -1),
        ])
        connection.commit()
        connection.close()

    def total(self, result, table, state="all"):
        return next(row for row in result["totals"]
                    if row["table"] == table and row["storage_state"] == state)

    def test_mixed_states_deduplicate_content_and_include_orphans(self):
        result = measure(self.path)
        content = self.total(result, "content_storage")
        self.assertEqual((content["row_count"], content["stored_bytes"], content["logical_bytes"]),
                         (4, 12, 108))
        self.assertEqual(content["stored_to_logical_ratio"], 0.111111)
        packed = [row for row in result["daily"]
                  if row["table"] == "content_storage" and row["storage_state"] == "compressed"]
        self.assertEqual(len(packed), 1)
        self.assertEqual((packed[0]["day"], packed[0]["row_count"]), ("2026-10-22", 1))
        orphan = next(row for row in result["daily"]
                      if row["table"] == "content_storage" and row["day"] is None)
        self.assertEqual(orphan["row_count"], 1)
        self.assertIsNone(orphan["stored_to_logical_ratio"])
        sentinel = self.total(result, "content_storage", "raw_examined_incompressible")
        self.assertEqual(sentinel["logical_bytes"], 5)

    def test_normalization_placeholder_utf8_sentinel_and_exact_version_dates(self):
        result = measure(self.path, immutable=True)
        normalization = self.total(result, "normalization_storage")
        self.assertEqual((normalization["row_count"], normalization["stored_bytes"],
                          normalization["logical_bytes"]), (3, 13, 107))
        packed = self.total(result, "normalization_storage", "compressed")
        self.assertEqual((packed["stored_bytes"], packed["logical_bytes"]), (6, 100))
        raw = next(row for row in result["daily"]
                   if row["table"] == "normalization_storage" and row["storage_state"] == "raw_unexamined")
        self.assertEqual((raw["day"], raw["logical_bytes"]), ("2026-10-23", 2))
        examined = next(row for row in result["daily"]
                        if row["table"] == "normalization_storage" and row["storage_state"] == "raw_examined_incompressible")
        self.assertEqual((examined["day"], examined["logical_bytes"]), ("2026-10-22", 5))

    def test_connections_reject_writes_and_measurement_preserves_input(self):
        before = hashlib.sha256(self.path.read_bytes()).digest()
        names = sorted(path.name for path in self.path.parent.iterdir())
        for immutable in (False, True):
            with self.subTest(immutable=immutable):
                with open_readonly(self.path, immutable=immutable) as connection:
                    with self.assertRaises(sqlite3.OperationalError):
                        connection.execute("DELETE FROM artifact_versions")
                    # mode=ro still protects input if query_only is disabled.
                    connection.execute("PRAGMA query_only=OFF")
                    with self.assertRaises(sqlite3.OperationalError):
                        connection.execute("DELETE FROM artifact_versions")
                measure(self.path, immutable=immutable)
        self.assertEqual(hashlib.sha256(self.path.read_bytes()).digest(), before)
        self.assertEqual(sorted(path.name for path in self.path.parent.iterdir()), names)

    def test_default_read_mode_includes_committed_wal(self):
        writer = sqlite3.connect(self.path)
        self.addCleanup(writer.close)
        self.assertEqual(writer.execute("PRAGMA journal_mode=WAL").fetchone()[0], "wal")
        writer.execute("PRAGMA wal_autocheckpoint=0")
        writer.execute("INSERT INTO content_storage VALUES('wal-only',3,?,NULL)", (b"wal",))
        writer.commit()
        self.assertTrue(Path(str(self.path) + "-wal").exists())
        total = self.total(measure(self.path), "content_storage")
        self.assertEqual((total["row_count"], total["stored_bytes"]), (5, 15))

    def test_missing_input_is_not_created(self):
        missing = self.path.parent / "missing.sqlite"
        with self.assertRaises(FileNotFoundError):
            measure(missing)
        self.assertFalse(missing.exists())

    def test_unsupported_schema_and_invalid_lengths_fail(self):
        connection = sqlite3.connect(self.path)
        connection.execute("UPDATE archive_meta SET schema_version=7")
        connection.commit()
        with self.assertRaisesRegex(ValueError, "schema 8"):
            measure(self.path)
        connection.execute("UPDATE archive_meta SET schema_version=8")
        connection.execute("UPDATE content_storage SET decoded_length=-2 WHERE digest='raw'")
        connection.commit()
        connection.close()
        with self.assertRaisesRegex(ValueError, "length metadata"):
            measure(self.path)

    def test_empty_archive_has_zero_totals(self):
        connection = sqlite3.connect(self.path)
        for table in ("normalization_storage", "artifact_versions", "content_storage"):
            connection.execute(f"DELETE FROM {table}")
        connection.commit()
        connection.close()
        result = measure(self.path)
        self.assertEqual(result["daily"], [])
        self.assertEqual(len(result["totals"]), 2)
        for row in result["totals"]:
            self.assertEqual((row["row_count"], row["stored_bytes"], row["logical_bytes"]), (0, 0, 0))
            self.assertIsNone(row["stored_to_logical_ratio"])


if __name__ == "__main__":
    unittest.main()
