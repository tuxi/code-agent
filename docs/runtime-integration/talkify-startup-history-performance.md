# Talkify startup and conversation history performance

## Actual application integration

The macOS host is `../chater/Talkify.xcodeproj`, scheme `Talkify-MacDirect`.
Its package reference resolves to `../AgentKit`, and its bundle contains
`../chater/build/codeagentd`. Changes in detached Codex worktrees alone do not
affect this application.

Talkify's build phase now invokes `scripts/prepare-codeagentd.sh`. It fingerprints
the sibling Go source and rebuilds the daemon when that source changes. Debug
uses the host architecture; distribution builds retain the universal binary.
The generated version includes the source revision and fingerprint. Copying and
signing the bundled daemon still uses a temporary file and atomic replacement.

The daemon still launches from the user's home directory, preserving the same
session database identity. No external test-server setup or data transfer is
needed when running the actual Talkify scheme.

## Observed baseline

Read-only inspection on 2026-10-15 found approximately 65.9 GB of SQLite data,
757 conversations, 94,935 messages and 2,480,767 stored events. The largest
event partition contained 1,894,072 events. Counts can change while the host is
running. Only aggregate counts, query plans and timings were inspected.

The old event index was `(session_id, at)`. Both reverse replay ordered by seq
and kind-filtered replay required a temporary sort. The old store also dropped
and recreated that index on every open. A direct complete list query took
roughly 10 ms in the warm native SQLite read-only check, so that result must
not be described as proof that the list SQL caused the long startup delay.

## Changes and verification boundaries

- Talkify coalesces overlapping startup requests and reports startup phases.
  Stopping a child process no longer waits synchronously on the MainActor.
- The authenticated daemon connection is published before peripheral sharing
  and provider synchronization finishes.
- AgentKit's history first paint requests the latest bounded page; full
  `/messages` loading is removed and detail metadata refresh runs separately.
- History pagination and forward reconnect cursors remain separate. Oversized
  turns use the fragment protocol described in the event contract.
- Conversation list pagination, bounded view retention and lightweight
  background restoration are implemented in the actual AgentKit directory.
- Backend startup, event query indexes, metadata reads and activity queries are
  covered by the backend performance design and focused tests.

## Verification record

- The actual `Talkify-MacDirect` scheme builds with the original sibling
  AgentKit source, including the second batch of list/resource changes.
- A temporary child-process probe verified overlapping starts share one child,
  cancellation prevents a late launch, a reported index migration receives its
  bounded grace period, and stop returns in 0–1 ms while the child is reaped
  separately. It used a fake daemon, not the real session DB.
- The original AgentKit directory's 418-test run has the same 12 failing cases
  / 15 failed assertions as the untouched HEAD baseline. Focused new checks,
  Web type checking and iOS Simulator compilation passed.
- Backend build, related-package vet and focused startup/query/history checks
  passed. Previously identified package/Flux failures were reproduced on HEAD.
- The bundled daemon's Mach-O UUID and Go build metadata match the newly built
  local daemon. The last complete app build passed before the final host-only
  migration-grace adjustment. That adjustment passed standalone Swift compilation
  and the child-process probe; the final app rebuild request was denied.

The production session database was not opened by a new daemon during this
change. New indexes require a one-time migration on the next actual startup;
its duration on the full production database has not been measured. Migration
logs report individual index work. Future opens must not rebuild those indexes.
Normal port-file discovery still allows 100 seconds. If the daemon explicitly
reports first-time index creation, discovery allows at most 300 seconds so the
normal timeout does not prematurely terminate that migration. This does not
publish an unready server or remove the upper bound.

## Manual acceptance

1. Build and run the existing Talkify-MacDirect scheme from `../chater`.
   Check that the build phase reports either a source-aware daemon build or
   a matching source fingerprint.
2. Observe `[startup]` logs separately from sidebar and history loading. The
   first index migration can take longer than subsequent restarts.
3. Open the existing long conversation. Its first event request should be
   `tail=200`, without a blocking full `/messages` request.
4. Scroll upward through several history pages and check position retention,
   fragment joining and continued live output.
5. Open more than three conversations and switch back. Recent views can be
   retained; evicted views may rebuild. Active approvals and execution channels
   must continue working.
6. Load older conversations using the sidebar's paging control. Search and
   workspace grouping must identify that their scope is the loaded list.

No GUI performance improvement or exact production cold-start duration is
implied by successful builds and focused tests; these checks exercise the real
application and its existing data.
