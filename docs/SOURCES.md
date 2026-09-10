# Connecting local conversation data

Skald can now enroll primary Claude Code transcripts and local Codex rollouts,
including Codex desktop conversations that persist in the same native format.
Collection is read-only at the source. The archive and discovery registry survive
source removal and daemon restarts.

## Preview, connect, start

Build, then inspect a bounded metadata preview:

```sh
make build
./bin/skald sources --provider claude_code
./bin/skald sources --provider codex
```

The preview emits filenames, sizes, native identities, project/version metadata,
and capability labels. It emits no conversation bodies and creates no archive.
The default window is files modified in the last seven days; `--since all` includes
older files. `--since` also accepts an RFC3339 timestamp. `--limit` controls the
number of metadata previews, from 1 to 64 (default 10).

Save the roots you want to collect, then start the daemon:

```sh
./bin/skald connect --provider claude_code
./bin/skald connect --provider codex --max-sessions 128
./bin/skald serve
```

In another terminal:

```sh
./bin/skald tui
```

`connect` writes a private config, not a running daemon. Both `connect` and `serve`
default to `$XDG_CONFIG_HOME/skald/sources.json`, or `~/.config/skald/sources.json`.
Use `--config FILE` for a separate configuration. Writes are locked, validated,
atomic, and mode 0600. A repeated connection preserves its namespace, cutoff and
capacity unless those options are explicitly changed.

The default source directories are `${CLAUDE_CONFIG_DIR:-~/.claude}/projects` and
`${CODEX_HOME:-~/.codex}/sessions`. Pass `--root DIRECTORY` for a different profile,
export directory, or provider location. Only selected roots are inventoried.
Browser/cloud conversations and separate desktop chat databases are not covered
by these two adapters.

The initial cutoff is saved as a fixed timestamp. Existing older files are not
initially enrolled, but an older conversation modified after the cutoff becomes
eligible. Once enrolled, a conversation stays in the registry even if its source
is later unavailable. The mtime cutoff chooses collection scope; it is never
presented as native conversation activity or used to order transcript records.

## Discovery and identity

The daemon inventories configured roots at startup and approximately every ten
seconds. During large backfills, discovery waits for the current capture pass.
Each root has a cap of `max_sessions` enrolled files (default 64). The combined
capacity of explicit file registrations and discovery roots must not exceed 256.
When capacity is reached, existing files continue tailing and diagnostics show
`capacity_reached`; increase the configured cap and restart within that total.
No previously enrolled history is silently evicted to make space.

Each inventory visits at most 100,000 entries. A limit or traversal failure is
visible, and no partial walk is claimed as complete. Up to 64 new-file identity
probes run per root per pass, with rotating retries so incomplete/unsupported
files cannot permanently starve older valid candidates. Each probe examines at
most 256 complete records / 4 MiB. Subagent paths and native sidechain markers are
excluded pending a separate verified subagent identity contract.

Namespaces are generated from provider, stable host identity and canonical root,
then saved. Use `--namespace ID` to share an existing namespace deliberately.
Namespaces represent local profiles; they do not prove a provider account login.
Discovered logical stream tokens are random, persisted in the archive before
capture, and independent of filenames. Native conversation identity comes from
source records; titles, cwd and filename similarity never establish it.

A moved file retains its stream token only when the old locator is absent and the
new file matches the entire captured prefix. Competing copies produce
`duplicate_conversation_locator`; an unproven move produces
`relocation_prefix_mismatch`. These files are not silently merged or rekeyed.
Native ID conflicts found later in a transcript block capture explicitly.

To move a whole selected root intentionally:

```sh
./bin/skald connect --provider codex --root /new/sessions \
  --namespace EXISTING_NAMESPACE --previous-root /old/sessions
```

This preserves the saved namespace and root alias. Ordinary generation/prefix
checks still apply. Namespace/provider conflicts and capacity reductions below
the number of already enrolled files are refused.

## Status, scope changes and performance

Use `skald status` or `d` in the TUI for root discovery status, counts, sampled
per-file rejection reasons, and captured-file health. Root discovery and file
capture are separate: an available directory does not prove all conversations
were captured. `probe_limit`, `partial`, `capacity_reached` and `unavailable` remain
visible. Rejection counts can exceed the bounded 16-item diagnostic sample.

Configuration is loaded at daemon startup. Restart after connecting another root,
changing a cutoff or capacity, or removing a root. Removing a namespace from the
configuration removes it from the active read corpus but retains its archived
records. Keep an unavailable root configured to continue browsing its history.

Complete, unchanged files avoid full prefix reads on ordinary idle polls. The
collector checks device, inode, size, mtime and ctime every pass and performs a full
prefix audit at least once per minute when the source is scanned. Any detected
change, restart, pending tail or backfill requires ordinary prefix verification.
The cache is in memory only and never advances a checkpoint. Large changing
transcripts still pay full-prefix hashing and remain a performance release gate.

## Validation and remaining limits

The new flow is covered by metadata/confinement tests, configuration integrity and
idempotency checks, discovery/relocation/scope/capacity tests, preserved-mtime rewrite
checks, and compiled acceptance through a real Unix socket (`make discovery-smoke`).
All committed test data is synthetic.

A read-only local sample on 2026-09-10 identified Claude Code 2.1.263 and Codex
0.153.4, including `codex_work_desktop` origin metadata. Bounded native parsing
returned ordinary messages from both and a Claude recap without blocked records
or gaps in those samples. This is sample evidence, not a claim of complete
provider coverage. Personal conversation bodies are not saved as test fixtures.

Subagent/fork/alias coverage, encrypted/ephemeral recap extraction, browser/cloud
sources, fresh activity selection, archive-wide descriptions, retention and
broader scale/recovery gates remain separate work. No integration here requires
Heimdall, Braid, Herdr, a provider hook, or a summarization request.
