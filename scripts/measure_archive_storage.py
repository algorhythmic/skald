#!/usr/bin/env python3
"""Measure schema-8 payload storage without returning or inflating transcript text.

Requires an explicit archive path. --immutable is only for a sealed, consistent
copy with no pending WAL; the default read-only connection honors SQLite WAL.
See measure_archive_storage.md for attribution and measurement limitations.
"""

import argparse
from contextlib import contextmanager
import json
from pathlib import Path
import sqlite3
import sys


CONTENT_SQL = """
WITH first_seen AS (
    SELECT source_revision, MIN(date(first_observed_at)) AS day
    FROM artifact_versions GROUP BY source_revision
)
SELECT first_seen.day,
       CASE WHEN c.decoded_length >= 0 THEN 'compressed'
            WHEN c.decoded_length = -1 THEN 'raw_examined_incompressible'
            WHEN c.decoded_length IS NULL THEN 'raw_unexamined'
            ELSE 'invalid' END AS storage_state,
       count(*) AS row_count,
       sum(c.byte_length) AS stored_bytes,
       sum(CASE WHEN c.decoded_length >= 0 THEN c.decoded_length
                ELSE c.byte_length END) AS logical_bytes
FROM content_storage AS c
LEFT JOIN first_seen ON first_seen.source_revision = c.digest
GROUP BY first_seen.day, storage_state
ORDER BY first_seen.day, storage_state
"""

NORMALIZATION_SQL = """
SELECT date(v.first_observed_at) AS day,
       CASE WHEN n.compressed_envelope IS NOT NULL
                      AND n.decoded_length >= 0 THEN 'compressed'
            WHEN n.compressed_envelope IS NOT NULL THEN 'invalid'
            WHEN n.decoded_length = -1 THEN 'raw_examined_incompressible'
            WHEN n.decoded_length IS NULL THEN 'raw_unexamined'
            ELSE 'invalid' END AS storage_state,
       count(*) AS row_count,
       sum(length(CAST(n.envelope_json AS BLOB))
           + coalesce(length(n.compressed_envelope), 0)) AS stored_bytes,
       sum(CASE WHEN n.compressed_envelope IS NOT NULL
                THEN n.decoded_length
                ELSE length(CAST(n.envelope_json AS BLOB)) END) AS logical_bytes
FROM normalization_storage AS n
LEFT JOIN artifact_versions AS v
  ON v.record_key = n.record_key AND v.source_revision = n.source_revision
GROUP BY day, storage_state
ORDER BY day, storage_state
"""


@contextmanager
def open_readonly(path, *, immutable=False):
    """Open an existing file without database write access; never create an input.

    The caller must ensure an immutable input is a sealed, consistent SQLite
    copy. immutable=1 bypasses locks/WAL and is unsafe for a changing archive.
    """
    path = Path(path).resolve(strict=True)
    if not path.is_file():
        raise ValueError("archive must be an existing regular file")
    uri = path.as_uri() + "?mode=ro" + ("&immutable=1" if immutable else "")
    connection = sqlite3.connect(uri, uri=True, isolation_level=None)
    try:
        connection.execute("PRAGMA query_only=ON")
        connection.execute("PRAGMA trusted_schema=OFF")
        connection.execute("BEGIN")
        yield connection
    finally:
        connection.close()


def metrics(row_count, stored_bytes, logical_bytes):
    return {
        "row_count": row_count,
        "stored_bytes": stored_bytes,
        "logical_bytes": logical_bytes,
        "stored_to_logical_ratio": (
            round(stored_bytes / logical_bytes, 6) if logical_bytes else None
        ),
    }


def measure(path, *, immutable=False):
    """Return numeric day/state aggregates from one consistent read transaction."""
    daily = []
    totals = []
    with open_readonly(path, immutable=immutable) as connection:
        version = connection.execute(
            "SELECT schema_version FROM archive_meta WHERE singleton=1"
        ).fetchone()
        if version != (8,):
            raise ValueError("unsupported archive schema; schema 8 is required")
        for table, query in (
            ("content_storage", CONTENT_SQL),
            ("normalization_storage", NORMALIZATION_SQL),
        ):
            by_state = {}
            for day, state, count, stored, logical in connection.execute(query):
                if state == "invalid" or stored is None or logical is None:
                    raise ValueError("invalid schema-8 payload length metadata")
                values = metrics(count, stored, logical)
                daily.append({"table": table, "day": day, "storage_state": state, **values})
                aggregate = by_state.setdefault(state, [0, 0, 0])
                for index, value in enumerate((count, stored, logical)):
                    aggregate[index] += value
            for state, values in sorted(by_state.items()):
                totals.append({"table": table, "storage_state": state, **metrics(*values)})
            all_values = [sum(values[i] for values in by_state.values()) for i in range(3)]
            totals.append({"table": table, "storage_state": "all", **metrics(*all_values)})
    return {
        "format_version": 1,
        "archive_schema": 8,
        "read_mode": "read_only_immutable" if immutable else "read_only",
        "day_basis": "UTC first_observed_at; earliest artifact reference per content digest",
        "stored_bytes_basis": "payload columns only; normalizations include retained JSON plus compressed BLOB",
        "unattributed_day": "null means no parseable artifact capture date",
        "totals": totals,
        "daily": daily,
    }


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive", type=Path, help="explicit schema-8 SQLite archive or consistent copy")
    parser.add_argument(
        "--immutable", action="store_true",
        help="bypass locks/WAL ONLY for a sealed, consistent copy; never for a live archive",
    )
    arguments = parser.parse_args(argv)
    try:
        result = measure(arguments.archive, immutable=arguments.immutable)
    except (OSError, sqlite3.Error, ValueError) as error:
        # Avoid leaking archive paths, namespace strings or arbitrary SQLite text.
        reason = str(error) if isinstance(error, ValueError) else "archive unavailable or incompatible"
        print(f"measurement failed: {reason}", file=sys.stderr)
        return 1
    json.dump(result, sys.stdout, indent=2, sort_keys=True)
    print()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
