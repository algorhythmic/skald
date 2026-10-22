# Agent reads and Claude child capture receipt

Implemented and deployed on 2026-10-21 Pacific time (2026-10-22 UTC), using two
Sol implementation agents and an independent Sol reviewer. This completes the
selected native Claude child capture and scoped MCP batch, not the broader L3
Braid retrieval or producer-publication milestones.

## Verification

- `make check smoke tui-smoke discovery-smoke mcp-smoke` passed: Go tests, schema
  and manifest checks, vet, compiled capture/recovery, terminal restoration,
  discovery and MCP acceptance.
- `go test -race ./...` passed; capture and daemon race tests passed again after
  the final native control-record changes. `git diff --check` and local
  documentation link checks passed.
- Synthetic regressions cover separate child identity despite reused message
  UUIDs, native parent links, metadata/control prefixes, conflicting identity,
  malformed sidechain markers, misleading filenames, spool restart, opt-out and
  resume. All committed conversation fixtures remain synthetic.
- Live read-only verification identified 54 child files and eight primary
  sessions. Complete child reads consumed 9,554 records with zero gaps, blocked
  batches or pending tails. No personal transcript bodies were printed or
  copied into test fixtures. Native child shapes were sampled from Claude
  2.1.296 and 2.1.303; support is bounded to the verified identity/control shapes.
- MCP tests cover fixed namespace grants, exact revision attribution, transcript
  instruction filtering, native-note classification, redaction, revocation after
  daemon restart, archive replacement, bounds and idle SIGTERM. Feed snapshot
  cutoff is not misrepresented as the boundary of later tail events.

## Live configuration

The installed executable is `/home/david/.local/bin/skald`. The archive remains
`/home/david/.local/share/skald` and the socket remains
`/run/user/1000/skald/skald.sock`. Schema 6 and the archive instance are unchanged.
Only the daemon was restarted; the existing terminal UI was left running.

The dedicated Odin root at
`/home/david/.local/state/odin-executive-pilot/claude-login/projects` now has
`include_subagents: true`. Its capacity is 72, with 62 streams enrolled at
verification. Existing development Claude, Codex and Devin roots retain all
their enrollment; unused capacities were redistributed to 40, 128 and 16
respectively, preserving the global limit of 256. Other roots did not gain child
capture. Raw reads remain disabled.

The dedicated Claude profile's user configuration registers `skald mcp` with
only the Odin namespace. `claude mcp get skald` reports **Connected**. Existing
agent sessions may need an MCP reconnect or a new session to load the new tools;
task-specific permissions still apply. This does not modify task authority or
grant shell access.

Private recovery copies of the previous executable, source configuration and
Claude profile configuration are under
`/home/david/.local/share/skald/deployment-backups/agent-reads-20261022T004620Z`.
The machine-local `deployment.json` there records executable SHA-256, daemon
PIDs and pre/post-startup aggregate archive metadata. No schema upgrade was
required. Final live verification found all **62 sessions available** (54 children
and eight primary), **29,942 record versions**, **103,822,578 captured bytes**, no
capture issues and an empty delivery backlog. A real scoped MCP `status` tool
call succeeded. These counts are an observation of a changing live archive.

The earlier CPU/read performance concern remains separate; no claim is made
that this feature batch resolves it. Ranked search, other providers' child
capture, and general fork/alias tracking remain pending.
