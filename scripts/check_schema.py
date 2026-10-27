"""Exercise the proposed capture DDL with real SQLite; no external dependencies."""
import hashlib
from pathlib import Path
import sqlite3

ROOT = Path(__file__).resolve().parents[1]
db = sqlite3.connect(":memory:", isolation_level=None)
db.executescript((ROOT / "internal/archive/migrations/001_capture.sql").read_text())
assert db.execute("PRAGMA user_version").fetchone() == (1,)
assert db.execute("PRAGMA foreign_keys").fetchone() == (1,)


def refuses(sql, values=()):
    try:
        db.execute(sql, values)
    except sqlite3.IntegrityError:
        return
    raise AssertionError(f"constraint did not refuse: {sql}")


for ns in ("fixture:a", "fixture:b"):
    db.execute("INSERT INTO sources VALUES (?, 'claude_code', 'v1', '{}', '{}')", (ns,))
db.execute("INSERT INTO streams VALUES ('stream', 'fixture:a', 'logical')")
db.execute("INSERT INTO sessions VALUES ('session', 'fixture:a', 'native', 'available')")
raw = b'{"synthetic":"original bytes"}\n'
digest = "sha256:" + hashlib.sha256(raw).hexdigest()
db.execute("INSERT INTO content_objects VALUES (?, ?, 'application/jsonl', ?)", (digest, len(raw), raw))
for key in ("first-occurrence", "second-occurrence"):
    db.execute("INSERT INTO artifacts VALUES (?, 'fixture:a', 'conversation', 'session', 'session')", (key,))
    db.execute("INSERT INTO artifact_versions VALUES (?, ?, '2026-09-10T00:00:00Z')", (key, digest))
assert db.execute("SELECT count(*) FROM content_objects").fetchone() == (1,)
assert db.execute("SELECT count(*) FROM artifact_versions").fetchone() == (2,)
refuses("INSERT INTO artifact_versions VALUES ('first-occurrence', ?, 'later')", (digest,))
refuses("INSERT INTO artifacts VALUES ('cross-scope', 'fixture:b', 'conversation', 'session', 'session')")
refuses("INSERT INTO artifacts VALUES ('invented', 'fixture:a', 'project', '/fixture/project', 'session')")
db.execute("INSERT INTO artifacts VALUES ('note', 'fixture:a', 'project', '/fixture/project', NULL)")
refuses("UPDATE content_objects SET original_bytes = ?", (raw,))
refuses("UPDATE artifact_versions SET first_observed_at = 'later'")
refuses("DELETE FROM content_objects WHERE digest = ?", (digest,))
for version in ("v1", "v2"):
    db.execute("INSERT INTO normalizations VALUES ('first-occurrence', ?, ?, 1, 'message', NULL, 'g1', 0, '{}')", (digest, version))
assert db.execute("SELECT count(*) FROM normalizations").fetchone() == (2,)
refuses("UPDATE normalizations SET kind = 'native_recap'")
db.execute("INSERT INTO ingest_checkpoints VALUES ('stream', 'g1', 10, 10, 1, '{}', 0)")
db.execute("BEGIN IMMEDIATE")
db.execute("UPDATE ingest_checkpoints SET captured_offset = 20, ordinal = 2")
db.execute("INSERT INTO changes(change_kind, namespace, payload_json) VALUES ('record', 'fixture:a', '{}')")
db.execute("ROLLBACK")
assert db.execute("SELECT captured_offset FROM ingest_checkpoints").fetchone() == (10,)
assert db.execute("SELECT count(*) FROM changes").fetchone() == (0,)
refuses("UPDATE ingest_checkpoints SET parsed_offset = 99")
assert not db.execute("PRAGMA foreign_key_check").fetchall()
assert db.execute("PRAGMA integrity_check").fetchone() == ("ok",)

# The daemon migrations must apply cleanly on standalone SQLite as well.
db.executescript((ROOT / "internal/archive/migrations/002_daemon.sql").read_text())
db.executescript((ROOT / "internal/archive/migrations/003_titles.sql").read_text())
db.executescript((ROOT / "internal/archive/migrations/004_activity.sql").read_text())
db.executescript((ROOT / "internal/archive/migrations/005_continuation.sql").read_text())
db.execute("INSERT INTO archive_meta VALUES (1,5,'synthetic-schema-check',1,NULL)")
db.executescript((ROOT / "internal/archive/migrations/006_delivery.sql").read_text())
assert db.execute("PRAGMA user_version").fetchone() == (6,)
assert db.execute("SELECT schema_version FROM archive_meta").fetchone() == (6,)
db.executescript((ROOT / "internal/archive/migrations/007_read_indexes.sql").read_text())
assert db.execute("PRAGMA user_version").fetchone() == (7,)
assert db.execute("SELECT schema_version FROM archive_meta").fetchone() == (7,)
columns = [row[1] for row in db.execute("PRAGMA table_info(session_titles)")]
assert "origin" in columns
columns = [row[1] for row in db.execute("PRAGMA table_info(session_activity)")]
assert "signal" in columns and "seen_time" in columns
refuses("INSERT INTO session_titles VALUES ('session','first-occurrence',?, 'v1','t','stream',0,0,0,'invented')", (digest,))
assert db.execute("SELECT count(*) FROM capture_receipts").fetchone() == (0,)
assert db.execute("SELECT count(*) FROM redaction_receipts").fetchone() == (0,)
assert db.execute("SELECT namespaces_json FROM archive_scope WHERE singleton=1").fetchone() == ("[]",)
assert not db.execute("PRAGMA foreign_key_check").fetchall()

# Schema eight adds only metadata/views; no payload is compressed at startup.
# The decoder is a connection function in Go. Register a bounded equivalent for
# standalone schema checks that inspect the compatibility views.
import zlib

def inflate_payload(data, size, digest):
    if not isinstance(size, int) or size < 0 or size > 64 << 20:
        raise ValueError("payload_size_limit")
    decoder = zlib.decompressobj()
    raw = decoder.decompress(data, size + 1)
    if len(raw) != size or not decoder.eof or decoder.unused_data or decoder.unconsumed_tail:
        raise ValueError("payload_length_mismatch")
    if "sha256:" + hashlib.sha256(raw).hexdigest() != digest:
        raise ValueError("payload_digest_mismatch")
    return raw

db.create_function("skald_inflate", 3, inflate_payload, deterministic=True)
db.executescript((ROOT / "internal/archive/migrations/008_payload_compression.sql").read_text())
assert db.execute("PRAGMA user_version").fetchone() == (8,)
assert db.execute("SELECT schema_version FROM archive_meta").fetchone() == (8,)
assert db.execute("SELECT original_bytes FROM content_objects WHERE digest=?", (digest,)).fetchone() == (raw,)
assert not db.execute("PRAGMA foreign_key_check").fetchall()
assert db.execute("PRAGMA integrity_check").fetchone() == ("ok",)

print(f"SQLite {sqlite3.sqlite_version}: capture DDL, identity/scope constraints, immutable versions, normalization history, atomic checkpoint rollback, and daemon migrations pass")
