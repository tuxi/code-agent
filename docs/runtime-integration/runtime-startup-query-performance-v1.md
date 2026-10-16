# Runtime startup and query changes

## Delivery target

The final source must be integrated into `/Users/xiaoyuan/Documents/work/git/code-agent`,
which Talkify packages. Building a separate worktree alone does not update the app.
Existing turn-scoped Fork changes are preserved. No commit or push is part of this work.

## Read-only production evidence

The database inspected was
`/Users/xiaoyuan/.codeagent/projects/xiaoyuan-32d10d908e8b/sessions.db`.
Inspection used SQLite `mode=ro` plus `PRAGMA query_only=ON`, with a query deadline.
No daemon or migration was run against it.

- Its only event index was `idx_session_events_session(session_id, at)`.
- Largest event partition: 1,894,072 rows.
- Budget replay and kind-filtered replay both used that index followed by
  `USE TEMP B-TREE FOR ORDER BY`.
- Initial attention enumeration scanned the full covering event index.
- A metadata-only read of 758 session rows took about 37 ms in this check.
  This does not establish the legacy list query as the main bottleneck; its
  existing SQL remains available unchanged for callers without pagination.

## Index migration

Ordinary SQLite open no longer drops/recreates the chronological event index.
Missing indexes are created individually and only when absent:

- `(session_id, id)` for seq replay/pages and head lookups.
- `(session_id, kind, id)` for kind ranges, counts, and terminal lookups.
- `(COALESCE(updated_at,''), id)` for list cursors.

The prior chronological and origin-turn indexes are retained. No separate
terminal-event index is needed: attention compares three indexed terminal-kind
maxima, avoiding another event-table index build.

`[startup]` reports schema, columns, and each missing index with
`started` / `completed elapsed=...`. Main startup logs also identify settings,
store open, registry, recovery, automation, managed-worktree reconciliation,
ownership claims, handler assembly, and listen/port-file readiness.

First creation of the event indexes can still take substantial time on a
65.9 GB database and occurs before the daemon is ready. Its production duration
has **not** been measured: running that migration was explicitly excluded.
The port file is not written early to hide this work. Warm reopen is tested to
leave `schema_version` unchanged.
Migration errors such as disk-full or database-busy now propagate normally;
only explicit SQLite CORRUPT/NOTADB results can trigger the existing quarantine
path. An index-build failure must not isolate a valid history database.

## Lightweight projections and recovery

Conversation detail counts/time come from payload-free event metadata queries.
Its session identity/workspace/lifecycle fields, WS existence checks, and empty
event-page existence checks use an optional single-session `LoadHeader` query
against `sessions` only. NeedsRebind/Rebind also use that header. Portable and
legacy iOS workspace refs are reanchored in memory on this read-only path;
partial headers are never saved. Older stores retain the full-Load fallback,
and actual execution/recovery continues to use the complete session Load.
Activity and control-plane ownership read only session metadata. Attention
baseline enumerates partition keys by indexed successor seeks, then fetches
each head; delta reads inspect only the new seq range. Attention never selects
event payloads.

Kind-filtered replay merges ordered per-kind index ranges instead of sorting
payload-bearing result sets. Reverse history pages retain the 8 MiB / 10000-row
caps and the explicit fragment contract in runtime-event-contract-v1.md §9.4.

Interrupted-session recovery updates only lifecycle JSON metadata and refreshes
the derived index's status field. It does not load/rewrite messages. The
server's cold global session-index rebuild is deferred until the first
cross-session tool query; HTTP readiness no longer waits for that scan. A
new Save before that first query does not suppress rebuilding older sessions.

Socket attachment refreshes only its validated session lease. Periodic
Heartbeat still reconciles the complete ownership set from lightweight
metadata, with concurrent heartbeats serialized. Managed-worktree recovery
remains synchronous because it establishes execution guards and storage state;
its duration is observable in startup logs.

## Optional conversation-list pagination

```
GET /v1/conversations?limit=50
GET /v1/conversations?before=<opaque-cursor>&limit=100&archived=false
```

- Capability: `conversation_list_pagination_v1` in runtime capabilities/hello.
- No `limit` or `before`: legacy complete list and existing response shape.
- With pagination: `limit` defaults to 50, valid range 1–200.
- Response remains `{trace_id, code, msg, data: [ConversationRef]}`.
- `X-Codeagent-Has-More: 0|1`; `X-Codeagent-Next-Cursor` is present only when
  another page exists. Clients return that cursor unchanged.
- Order: `updated_at DESC, id DESC`; the tuple boundary is exclusive.
- Cursor binds the archive partition. Malformed/mismatched cursor returns 400.
- Empty page is `data: []`, Has-More is 0, and Next-Cursor is absent.
- Page queries read only `sessions`. Names use persisted `name`, falling back
  to session ID; they do not fetch the first message body to derive a title.
- Like ordinary cursor pagination, this is not a frozen multi-request snapshot:
  a session updated after the first page moves toward the head. Refreshing the
  first page reconciles such changes; the client should deduplicate by ID.

## Validation scope

Tests cover index-preserving reopen, metadata-only recovery, list cursor ties,
archive-cursor validation, payload-free attention baseline/delta, bounded event
fragments, single-session claims, and deferred-index completeness.

The pre-existing failures reproduced on an unchanged HEAD snapshot are the
tool-stream persistence budget test, provider registry count test, two shared
enrollment tests, the router pre-accept failure test, and these four Flux tests:
planning failure lifecycle, generated-definition billing, cross-session DAG
admission, and cross-workspace Map completion. They are reported separately
from the changed-path checks.
