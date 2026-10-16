# Conversation Fork at a Turn v1

> Status: implemented (Runtime), client rendering is a follow-up in AgentKit.
> Participants: code-agent (Runtime) + AgentKit (macOS/iOS client)

## 0. Problem

A conversation can be forked, but only as a whole: the child inherits the
source's entire latest checkpoint. There was no way to say "fork up to turn N" —
the case where a session *started* correctly and later went off track, and you
want to branch from the last good turn and continue there, leaving the source
untouched.

## 1. Semantics

- **Fork point** is a turn id (`turn_id`), the same id a client already holds
  from the `turn_started` event (equal to the user message's runtime-only
  `OriginTurnID`).
- The child's model context is the source's history from the first message
  through the **end of that turn**. Later turns are not copied.
- The cut is turn-aligned: it lands on the next user message, which is always a
  provider-valid boundary (no orphaned tool result).
- `through_turn_id` omitted keeps the previous whole-checkpoint behavior
  (backward compatible).
- **Compaction is fail-closed.** If the target turn is no longer in the verbatim
  history (folded into the summary by compaction), or is unknown / not yet
  terminal, the fork fails and no child is provisioned. The runtime never
  silently over-includes later context.

The source conversation is never modified.

## 2. Create a fork

### HTTP

```
POST /v1/conversations/{source_id}/forks
{
  "request_id": "client-stable-id",
  "name": "optional display name",
  "through_turn_id": "turn_abc",            // optional; omit for whole checkpoint
  "execution_policy": "shared_workspace",   // or isolated_worktree
  "worktree_name": "optional; isolated only"
}
```

`request_id` is mandatory and durable. A same-payload retry returns the same
child; a changed payload (including a different `through_turn_id`) conflicts with
409.

Response (`201`) is the same shape as before plus the echoed cut:

```json
{
  "id": "child-id",
  "parent_session_id": "source_id",
  "source_session_id": "source_id",
  "workspace_path": "...",
  "kind": "fork",
  "status": "open",
  "through_turn_id": "turn_abc"
}
```

If the cut turn is unavailable the endpoint returns `409` (the source owner
reports the failure).

### Tool

`fork_session` gains an optional `through_turn_id` parameter with the same
meaning, so the model can branch a session at a chosen turn.

## 3. Lineage (how a client knows it is a fork)

A forked conversation records lineage in its metadata, exposed on
`GET /v1/conversations/{id}`:

| Field | Meaning |
|-------|---------|
| `fork_source_session_id` | the source conversation id |
| `fork_through_turn_id` | the cut turn (absent for a whole-checkpoint fork) |

`fork_through_turn_id` is empty when the fork copied the whole checkpoint.

## 4. Reconstructing the inherited prefix

The child does **not** duplicate the source's event log. To render the history it
inherited, the client fetches the source's events bounded by the cut turn:

```
GET /v1/conversations/{source_id}/events?through_turn_id=turn_abc
```

- Returns the source's events up to and including that turn's terminal event
  (`turn_finished` / `turn_failed` / `turn_cancelled`), in emission order.
- Turn-aligned: never a half turn.
- Mutually exclusive with `?since` (both present → 400).
- Unknown or non-terminal turn → 404.
- Subject to the same replay byte budget as `?since=0`; if truncated, the
  response carries `X-Codeagent-Replay-Truncated: 1`. Very long prefixes are the
  same limitation the history-pagination work
  (`conversation-history-pagination-v1.md`) addresses.

Client rendering model:

1. Render the fetched prefix as a distinct **prefix partition** (its seqs belong
   to the source, so do not merge them with the child's own event seqs — the same
   partition approach pagination backfill uses).
2. Append the child's own live/replayed events (its own seqs start at 1).
3. On the immediate fork, the client can seed the prefix from its already-rendered
   source turns and reconcile with the durable fetch on reload.

## 5. Capability

The runtime advertises `conversation_fork_through_turn_v1` (both in the hello
capability list and in `GET /v1/runtime/capabilities`) whenever conversation fork
is available. Clients feature-detect it before exposing the per-turn Fork button.

## 6. Compatibility

- Existing callers that omit `through_turn_id` are unaffected; the idempotency
  hash of a fork request is unchanged for them.
- The owner RPC payload is additive (optional field); protocol version is
  unchanged.
- Turn-scoped replay requires an event store implementing
  `session.EventTurnPrefixStore`; stores that do not opt in return `501` for
  `?through_turn_id`.

## 7. Follow-ups

- Client: per-turn Fork button, prefix partition rendering, hide/disable Fork on
  turns whose content was compacted away.
- Robust full-prefix paging for very long source histories (pagination v1).
