# Initial OHS implementation receipt

2026-10-19. Work began from Skald `22114ddce3e5bb82f685463e8a20b354b3e1558c`
with a clean working tree. This receipt describes the initial implementation
batch before a tagged producer release or consumer pin. Three subagents handled capture delivery, source health and Skald's
scope documents; the coordinating agent inspected the baseline, integrated the
standalone path and verified recovery.

The input was the proposed Odin–Heimdall–Skald work plan at
`/home/david/.codex/worktrees/23e7/odin/docs/design/ODIN-HEIMDALL-SKALD-WORK-PLAN.md`.
The [Skald producer plan](ODIN-INTEGRATION-PLAN.md) records the resulting scope
and open publication/consumer gates.

## Metadata-only source inventory

The existing configuration already selects Claude Code, Codex and Devin discovery
roots, all present on this machine. No source selection, hooks, installed binary,
service or live archive was changed. No conversation content was inspected or
copied into fixtures for this inventory.

The installed daemon's read-only status reported archive schema 5 and record
contract 1. Its version command incorrectly reported schema 4; this batch makes
the command use the archive's schema constant. The installed binary SHA-256 was
`8e8f7d6c54efec7e98a8522ca9f6615b8e27dc3d22c1147d4f632aa6cc6e5d45`.
That digest identifies the inspected binary, not a verified release/commit pin.

| Provider | Enrolled streams | Discovery state | Metadata diagnostics |
| --- | ---: | --- | --- |
| Claude Code | 32 | `partial` | 29 candidates; 5 `conversation_identity_unavailable`, 1 `duplicate_conversation_locator` |
| Codex | 128 | `probe_limit` | 454 candidates; 14 `conversation_identity_mismatch` |
| Devin | 14 | `partial` | 15 candidates; 1 `empty_session_store` |

Across 174 enrolled streams, health was 137 `available`, 7 `blocked`, 9
`unavailable` and 21 `capture_gaps`. Capture issue codes were 7 `capture_blocked`
and 9 `source_unavailable`. These counts are one observation, not complete source
coverage or a root-cause diagnosis. Discovery candidates are not promised enrolled
sessions. Investigating these specific sources and documenting attributable live
changes remain OHS-02 follow-up work.

The first socket attempt inside the sandbox returned `EPERM`; the approved
metadata-only retry reached the daemon. That initial `daemon_unavailable` CLI
result was an execution-environment limitation, not evidence of daemon failure.

## Verification scope

Baseline `make check` passed using cached Go package results, independent SQLite schema checks
and `go vet`. Baseline `make smoke tui-smoke discovery-smoke` also passed after
allowing temporary Unix sockets and PTYs outside the restricted sandbox.

The native smoke fixture exercises Claude Code and Codex with a private temporary
archive, including restart, exact original reads, source loss, online backup and
fresh restore. The updated fixture additionally checks scoped source positions
and freshness, source reappearance without duplicate records, and preservation of
capture metadata through restore. It runs without Odin or Heimdall endpoints.
All fixture transcript content is synthetic.

The integrated `make check smoke tui-smoke discovery-smoke` passed after the
fixture adjustment for `/tmp`'s `.git` marker. It includes the new delivery,
health, scope and recovery checks. The compiled version command reports schema 5;
whitespace and updated local documentation links also pass.
`go test -race ./...` passed with `.tools/go/bin/go` and the project build/module
caches; a final `make build` produced the updated local executable.

Final integrated command results are recorded in [STATUS.md](../STATUS.md).
This receipt does not close OHS-02/03/07 or Skald's broader release gates. Public
archive changes, durable spool and acknowledgement storage, producer publication,
Odin consumer pins, redaction/removal propagation and live integration remain
separate work. No peer decision, task migration or live cutover occurred.
