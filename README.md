# Skald

Skald provides an overview of conversation context across projects, terminals,
desktop apps and other supported environments. It complements Heimdall: Skald
preserves source conversations; Heimdall owns accepted task state. Each works
independently, with reusable parsing and explicit provenance at the boundary.

The archive daemon, CLI and native terminal UI are runnable on Linux. They retain
selected Claude/Codex transcripts independently and support exact
reads, backup and restore. Browse project groups, excerpts, recaps and transcript
history with `skald tui`. See [TUI usage](docs/TUI.md),
[archive setup](docs/ARCHIVE-SETUP.md) and [implementation status](STATUS.md).

- [Connect real conversation sources](docs/SOURCES.md)
- [Implementation plan](SKALD-IMPLEMENTATION-PLAN.md)
- [Heimdall amendment — context only](SKALD-HEIMDALL-AMENDMENT.md)
- [Session overview design](skald-window.html) and [transcript design](skald-transcript.html)
- [Record and checkpoint contract](docs/CONTRACT-V1.md)

## Build and run

Use Go with support for the pinned 1.27.1 toolchain. Make automatically uses
`.tools/go/bin/go` when present, otherwise `go` from PATH. You can also pass
`GO=/absolute/path/to/go`. The optional local toolchain and default build/module
caches under `.cache/` are ignored by version control. From this project:

```sh
make build
./bin/skald version
./bin/skald serve --config examples/fixture-sources.json
```

To connect real local conversations, preview and save your chosen sources:

```sh
./bin/skald sources --provider claude_code
./bin/skald sources --provider codex
./bin/skald connect --provider claude_code
./bin/skald connect --provider codex --max-sessions 128
./bin/skald serve
```

The saved roots are monitored for new conversations. See [source connection](docs/SOURCES.md)
for scope, capacity, and metadata-only previews. Stop the synthetic daemon before
starting the real one on the default socket, or use separate socket/data paths.

In another terminal:

```sh
./bin/skald tui
./bin/skald status
./bin/skald sessions
```

Once a configuration is saved, plain `./bin/skald` starts the daemon when the
socket has no live peer and then opens the TUI.

For a temporary synthetic demonstration, run `make demo` in a terminal.
Skald uses heimdall’s dark palette by default; press `t` to cycle through the
terminal (`desktop`) and original `amber` palettes, or start with
`skald tui --theme desktop`.

Read-only capture diagnostics remain available without the daemon:

```sh
./bin/skald probe --provider codex --provider-version 0.153.4
./bin/skald inspect --provider claude_code --namespace fixture:claude \
  --stream-id fixture-claude --file testdata/claude/session.jsonl
./bin/skald inspect --provider codex --namespace fixture:codex \
  --stream-id fixture-codex --file testdata/codex/session.jsonl
```

`inspect` is read-only. It prints normalized records, explicit gaps and a candidate
checkpoint; `--raw` also includes exact source bytes as base64. `--limit` bounds
records per invocation. To resume, save the `checkpoint` object from the batch to
its own file and pass `--checkpoint FILE`. Durable consumers must commit originals,
records, gaps and the checkpoint together; the inspection CLI does not do that.

`discover --root DIRECTORY` lists JSONL files only under a chosen root without
following nested symlinks. It does not ingest them. `probe` reports compatibility
with fixture-tested provider versions, not proof that a live application works.
Collection uses only your explicit configuration, including any discovery roots
you choose with `connect`. No provider hooks, user services
or desktop settings are installed.

## Verify

```sh
make check
```

This runs Go contract/capture/CLI tests, independent JSON Schema validation,
SQLite DDL checks (Python 3 standard library), and Go vet. `make build` produces
`bin/skald`. `make smoke` verifies the compiled daemon with temporary synthetic
sources and a real private Unix socket. Override `GO=/absolute/path/to/go` when Go is not on PATH.

All checked-in conversation fixtures are synthetic. Tests do not open personal
transcripts, contact Heimdall or make summarization/embedding requests.
