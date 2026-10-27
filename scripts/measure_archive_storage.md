# Read-only archive storage measurement

Run the standard-library Python tool against an explicitly selected schema-8
archive. It prints only aggregate JSON: UTC capture day, storage state, row
count, stored/logical bytes and their ratio. It does not print archive paths,
source identities or transcript text, register inflation functions, decompress
payloads, migrate, vacuum or compress the database.

```sh
python3 scripts/measure_archive_storage.py /path/to/archive.sqlite
python3 scripts/measure_archive_storage.py /path/to/sealed-copy.sqlite --immutable
python3 -m unittest discover -s scripts -p 'test_measure_archive_storage.py'
```

The default opens an existing file using SQLite `mode=ro`, enables `query_only`
and reads within one transaction. SQLite's normal locking and WAL handling
remain active. It never opens an input for database writes or creates a missing
input. Long measurements can retain a read snapshot; use a consistent copy when
measuring a busy archive.

`--immutable` additionally bypasses locks and WAL. Use it only for a sealed,
consistent copy produced through SQLite backup or the archive recovery procedure,
with all committed data in the database file and no pending WAL. A plain file
copy of a running WAL database is not sufficient. The tool cannot prove that an
input is sealed. It does not default to immutable mode or choose a live archive
path automatically.

## What the numbers mean

- Content rows are counted once per digest. Their day is the earliest UTC
  `artifact_versions.first_observed_at` date referencing that digest. Duplicate
  record references do not multiply content bytes. Unreferenced content appears
  under `day: null`, so totals still include it.
- Normalizations use the capture date of their exact record/revision. Each
  adapter normalization is counted separately. A null day means the reference
  has no parseable capture date.
- Content `decoded_length >= 0` means compressed; `-1` means examined but stored
  raw; `NULL` means raw and unexamined. Raw logical size is `byte_length`, never
  the negative sentinel. Normalization compression is identified by a non-null
  `compressed_envelope`; its logical size is `decoded_length`. Raw normalization
  size uses the UTF-8 byte length of `envelope_json`.
- Stored normalization bytes include **both** retained JSON and compressed BLOB
  columns, including the `{}` placeholder in compressed rows. Content stored
  bytes use `byte_length`. Ratios are stored/logical; zero logical bytes produce
  `null`, not division by zero. Invalid compression length states are rejected.
- These are current payload representations grouped by first capture date,
  **not historical physical database growth**. Later maintenance changes the
  representation of older rows. Capture date also includes backfill and is
  different from the native event date. A partial latest day is not a full-day
  rate.
- Database pages, indexes, other metadata, free pages, WAL, recovery copies and
  filesystem overhead are outside these payload totals. This tool is not an
  integrity check or a compression benchmark.
