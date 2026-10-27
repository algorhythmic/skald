# Recovery operations

`skald recovery-job --destination-dir /absolute/private/recovery` previews an
export. Add `--apply` to ask the daemon for a sealed backup, compress it into
that existing mode-0700 directory, and validate a fresh restore. The original
raw backup remains. Destination collisions fail; no recovery file is overwritten.
Destination free space is checked before compression; write errors still fail
without removing the raw backup. Configure an independent disk or mounted remote
volume yourself. A different path alone does not prove independent failure domains.
The command does not transfer credentials or configure remote storage.

`skald recovery-check --backup /absolute/recovery/archive-ID.sqlite.zst`
validates a fresh temporary restore, including compressed and original digests,
archive identity and SQLite integrity. Temporary validation files are removed.
Validation allows up to 16GiB expanded backups and checks temporary disk space
for three expanded copies plus 256MiB migration headroom before restoring.

`skald recovery-retain --recovery-dir /absolute/private/backups --keep-raw 1`
prints a preview. Add `--max-age 720h` to select old redundant raw files, or
`--max-bytes 1073741824` to select additional redundant raw files under pressure.
Only `--apply` performs retirement. The newest raw rollback always stays;
`--keep-raw` cannot be zero. Preview verifies exact raw/compressed manifest hashes without creating a restore.
Apply additionally validates a fresh compressed restore before retiring raw bytes.
Unpaired raw files are retained with diagnostics. Corrupt paired, linked, or
unsealed candidates stop planning. Compressed backups and manifests
are retained. Therefore `budget_satisfied` may be false; that is a request for
operator action, never automatic permission to delete the last recovery copy.
`allocated_bytes` and `remaining_bytes` use allocated filesystem blocks.

Select explicit private recovery directories, including the archive data root
when handling `pre-schema-*.sqlite` rollback files. Only immediate
`archive-*.sqlite` and `pre-schema-*.sqlite` children are eligible; nested
directories, live `archive.sqlite`, WAL, spool and provider files are never
selected. Accounting includes other files below that root without following
links. Advisory directory locking coordinates maintenance instances, and inode,
size, mtime and ctime checks detect candidate replacement before unlink. Keep
the directory private and avoid external writers during retention; external
programs must cooperate with the advisory lock. A plan is recalculated
and validated on apply; previews are not executable manifests.

`skald storage-usage /absolute/archive-root /absolute/spool-root
/absolute/independent-recovery` reports actual allocated file blocks including
WAL and backups. Supply disjoint roots to avoid double counting; directory and
filesystem metadata, shared extents and remote storage replication are excluded.
This is accounting, not a storage quota and not a deletion command.

The service/timer files in `examples/maintenance` are templates only. Set real
paths and an independent destination before installing them. No scheduler is
installed automatically. Retention has no scheduled apply template: preview and
review a policy before explicitly requesting deletion.
