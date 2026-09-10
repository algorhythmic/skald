# Skald terminal UI

Build with `make build`, start the archive daemon using the [setup guide](ARCHIVE-SETUP.md),
then run:

```sh
./bin/skald tui
./bin/skald tui --socket /absolute/path/skald.sock --namespace fixture:claude
./bin/skald tui --theme amber
```

For a disposable demonstration with three synthetic conversations, run `make demo`.
The demo creates a private temporary archive, starts its daemon and opens the TUI
in the current terminal. Quitting removes the demo archive and stops that demo's
daemon. An ordinary `skald tui` never stops its independently running daemon.

## Overview

Conversations appear under their observed project paths, with the selected
conversation expanded. A conversation observed in multiple projects appears in
each; associations retain source references in the API. Unbound conversations
remain under “no project.” Provider, source health and activity are separate fields.

| Key | Action |
| --- | --- |
| ↑/↓ or j/k | Select a conversation |
| Space | Expand or collapse recent context |
| Enter or 2 | Read the transcript |
| h | Open the transcript at a native recap on its current page |
| g | Group by project, provider, source namespace or all conversations |
| / | Filter titles, native IDs, providers, sources and projects on the loaded page |
| N / P | Next / previous session page |
| Home / End, PgUp / PgDn | Move through the loaded session page |

The list loads at most 100 sessions per page. Project associations are bounded at
32 per session, with explicit truncation metadata. A filter applies to the current
page; it is not a full archive search. Namespaces come from the daemon configuration.

Expanded descriptions select a native recap or meaningful assistant text from the
recent 25-record window. A newer assistant message becomes a labeled excerpt with
the earlier recap retained. Newer user/tool activity leaves the recap in place and
adds an activity notice. With neither, the native title remains the fallback.

This is a bounded display selection, not the plan's full archive-wide description
projection. An earlier recap outside the window requires history paging. No model
summarizes anything. Ordering ambiguity across streams or rewrite epochs is labeled;
source arrival time is never used to invent chronology. Unknown recap coverage
stays unknown. Activity remains `unknown`; archived lifecycle observations are
shown as historical evidence, not live working state or completed conversations.

## Transcript

The reader starts on the latest page, displayed in native source order. Stream and
generation boundaries are labeled; ordering between different streams is unknown.
Tool payloads are folded initially. Opaque records remain visible as markers with
exact references. Native recaps have explicit coverage labels.

| Key | Action |
| --- | --- |
| j/k or ↑/↓ | Scroll |
| PgUp / PgDn or Ctrl-U / Ctrl-D | Scroll a viewport |
| Home / End or g/G | First / last line of this page |
| [ / ] | Previous / next message turn on this page |
| n / p | Next / previous native recap on this page |
| Space | Fold / unfold tool details on this page |
| / | Find visible text on this page; unfold tools to search payload text |
| f / F | Next / previous match |
| N / P | Older / newer transcript page |
| v | Include / exclude historical revisions |
| i | Inspect the exact reference of the record at the top of the viewport |
| y | Send that reference to the terminal clipboard, when supported |
| Esc or 1 | Return to the overview |

The socket endpoint is `GET /v1/transcript` with `conversation`, optional
`namespace`, `limit` (1–25), `cursor`, and `history` (`true` or `false`). It returns
newest-first display records with a boundary and optional `next_cursor`; the TUI
reverses each page for reading. Cursors bind the conversation and revision mode.

Each page contains at most 25 records. Each record's display text and combined tool
payloads are independently limited to 16 KiB, with at most 16 displayed non-text
parts. Clipping is explicit. Full normalized records remain available through
`skald get --record KEY --revision DIGEST --adapter VERSION`; original source bytes
also require `--raw` and daemon opt-in. Display projections never overwrite originals.

## Connection, diagnostics and display

`r` refreshes, `d` opens source diagnostics, `?` opens scrollable help, and `q` or
Ctrl-C exits. The UI polls every five seconds without blocking keyboard input.
It keeps a labeled cached view while the daemon is unavailable, reconnects
automatically, and clears cached content when reads are denied or the selected
conversation leaves the read scope. Late requests cannot replace a newer selection.

The expanded overview refreshes on archive changes. A transcript page stays still
while reading and announces newer archive changes; `r` reloads the latest page.
Pagination cursors expire after an archive change or restore. Skald restarts the
page instead of mixing records from different archive boundaries.

Skald defaults to `--theme desktop`: your terminal's foreground, background and
ANSI palette supply the colors. When the terminal follows your desktop theme,
Skald follows it too, including palette changes applied by the terminal while
Skald is running. Selection keeps the normal background and uses a subdued side
line and accented pointer. Search matches use emphasis, so no fixed dark
background is imposed.

Use `--theme amber` for the original design palette, or press `t` to switch themes
in the running TUI. The toggle lasts for that run. Set `SKALD_THEME=desktop` or
`SKALD_THEME=amber` to choose a default; an explicit `--theme` takes precedence.
For example, `SKALD_THEME=amber make demo` runs the demo with the amber palette.
`NO_COLOR` is respected in both themes with readable emphasis and selection attributes.
Text wraps by Unicode grapheme and line boundaries. Escape sequences, control
characters and bidirectional overrides are made visible. Conversation text, shell
snippets and tool payloads are never executed. A 32×10 terminal is the minimum;
help and reference panels scroll in smaller layouts.

The TUI reads only through the daemon socket. It opens no database and needs no
Heimdall, Braid or Herdr connection. Source activation (`o`), ranked search (`s`),
Herdr/local group authoring and archive-wide description projections are not yet
implemented; unavailable actions say so explicitly.

## Verification

`make check` includes archive projection, exact-history, scope/cursor, Unicode,
monochrome, simulated terminal interaction and stale-response tests. Run
`make tui-smoke` for a compiled process with a real PTY and a temporary private Unix
socket. It checks keyboard input, resize, normal exit, SIGTERM terminal-mode
restoration, and that ordinary TUI exit leaves the daemon running.
