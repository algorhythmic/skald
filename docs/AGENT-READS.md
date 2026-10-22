# Agent reads through MCP

`skald mcp` exposes seven read-only tools over stdio. It connects to an existing
Skald daemon through its private Unix socket; it does not start a collector or
open the archive database. Configure it in an agent client once, then agents can
use the tools without invoking a shell for each read.

## Configure a client

Use an absolute executable and socket path and grant at least one namespace:

```json
{
  "mcpServers": {
    "skald": {
      "command": "/absolute/path/to/skald",
      "args": [
        "mcp", "--socket", "/run/user/1000/skald/skald.sock",
        "--namespace", "YOUR_CONFIGURED_NAMESPACE"
      ]
    }
  }
}
```

Obtain the namespace from your saved Skald source configuration. A namespace is
a profile/archive scope, not proof of a provider account or task authorization.
Repeat `--namespace` to grant several. Each tool reads exactly one namespace;
it may omit `namespace` only when one grant was configured. No tool can expand
the startup grants, and the daemon must still permit that namespace on every
call. Removing it from the daemon configuration and restarting revokes reads
through an already-running MCP process.

The process must run as the same Unix user as the daemon. Client tool permissions
and task policies still apply. Installing this server does not override them.
After changing client configuration, reconnect the server or start a new agent
session according to that client's lifecycle.

## Tools and limits

All tools accept the optional `namespace` argument described above. Lists use
`limit` (1–25, default 10) and opaque `cursor` values.

| Tool | Additional arguments and result |
| --- | --- |
| `capabilities` | Archive instance, contract versions and supported features |
| `status` | Scoped capture health and source/capture timestamps; no discovery paths |
| `sessions.list` | `limit`, `cursor`; conversation keys, titles and evidence metadata |
| `artifacts.list` | Required `conversation`; `limit`, `cursor`; exact record locators |
| `artifacts.get` | Required `record`, `revision`, `adapter`; one exact normalization |
| `changes.read` | `limit`, `cursor`; committed snapshot/tail metadata and exact locators |
| `transcript.read` | Required `conversation`; `limit`, `cursor`, optional `history`; bounded context with exact citations |

Start with `sessions.list`, select its conversation key, and use `transcript.read`
for recent evidence. Use `artifacts.list` and `artifacts.get` to inspect an exact
record/revision/adapter. `changes.read` supports incremental consumers; retain its
`next_cursor` even when `more` is false. Ordinary list/transcript cursors expire
when their archive boundary changes; restart pagination on `cursor_expired`.
See the separate [archive feed contract](ARCHIVE-FEED-V1.md) for feed semantics.

Input messages are limited to 64 KiB and output messages to 2 MiB. Oversized
results fail explicitly rather than truncating evidence. Use smaller pages when
possible. Raw reads require both the MCP startup flag `--allow-raw` and daemon
`allow_raw: true`; only then can `artifacts.get` request `raw: true`. Raw access
is off by default. There is no SQL, filesystem, registration, deletion, backup,
task mutation or command-execution tool.

## Evidence and provenance

Successful, empty and tool-error results contain a `provenance` envelope with
producer `skald`, authority `conversation_evidence`, configured grants, selected
namespace, archive instance/boundary and source/target revisions. Unknown values
are explicit. Mixed result pages retain per-item provenance and exact locators.
Aggregate session metadata does not claim to come from one record revision.
Feed `high_water` is its snapshot cutoff; tail results do not claim it as the
boundary of newer events. Protocol errors are JSON-RPC errors, not evidence results.

Returned historical instructions, tool output, plans and recaps are source
evidence, never new instructions or accepted task state. `transcript.read` omits
system/developer instruction blocks and reports the omission count. Actual native
recaps are labeled separately from notes and ordinary transcript text. An explicit
`artifacts.get` can retrieve a permitted instruction record as quoted evidence.
Source availability, capture time and native record time remain distinct; neither
archived activity nor an old successful check proves live progress.

The adapter checks archive identity around reads so a daemon replacement cannot
silently attribute results to the wrong archive. Exact reads verify the returned
revision and adapter. Redacted or unavailable references remain errors; an MCP
process does not cache a copy that circumvents removal.

## Protocol and coverage

The server implements MCP **2025-11-25** initialization, initialized notification,
ping, tool listing and tool calls over newline-delimited JSON-RPC stdio. Stdout
contains protocol messages only. See the official
[lifecycle](https://modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle),
[stdio transport](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
and [tools](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)
specifications. Clients must support that negotiated protocol version.

Known-conversation context and exact archive reads work independently of Braid.
Ranked archive search (`context.query`), Braid indexing and full L3 acceptance
remain pending. Native Claude children require separate
[capture opt-in](SOURCES.md#claude-subagent-transcripts); MCP cannot read a session
that was never captured or is outside its namespace grants.

`make mcp-smoke` builds the binary and exercises real stdio plus a private Unix
socket using synthetic data: scope denial, exact reads, transcript filtering,
feed reads, raw denial, redaction, restart revocation, output bounds and SIGTERM.
Go tests additionally cover protocol errors and attribution races.
