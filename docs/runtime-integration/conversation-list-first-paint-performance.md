# Conversation list first paint and metadata prefetch

## Scope

Implemented on 2026-10-17 in the original sibling `../AgentKit` package consumed
by `../chater/Talkify.xcodeproj`, scheme `Talkify-MacDirect`.

The list already publishes its first page before activity discovery finishes.
Activity therefore cannot be called a proven first-paint blocker from the await
chain alone. The previous recent-list comparator did perform synchronous local
SQLite state reads during sorting, with repeated reads of each compared row.
List grouping, dictionaries and filtering were also rebuilt during body updates.

## Resulting behavior

- Keep Runtime's updated-at/id order instead of sorting by device-local draft
  timestamps. No local state database reads occur in the list projection.
- Cache filtering, project groups and lookup dictionaries. Rebuild on list
  revision, search text or project inputs; row activity observes the supervisor
  inside each row rather than the outer list.
- Coalesce the complete foreground list refresh and runtime state discovery.
  The visible-list callback owns initial sidebar refresh; the view no longer
  issues a second explicit request for the same activation.
- Keep the eight-second visible-list poll for metadata. Activity uses its own
  existing three/twenty-second monitor instead of being requested again by every
  list poll. Foreground activation still refreshes capabilities.
- Publish the first 100 records, then automatically fetch remaining active-list
  metadata in sequential pages with 100 ms between requests to allow rendering
  and input. Prefetch creates no history requests, controllers or sockets.
- Hide/background stops prefetch; canceled responses do not publish. Failures
  retain loaded records and their cursor for retry. Repeated cursors stop the
  pass instead of creating an unbounded loop.
- Archives remain lazy. Search/grouping covers the loaded metadata and expands
  as prefetch completes; a failed prefetch leaves the existing load-more control.

The server activity baseline and delta semantics are preserved. Removing old
completed sessions from the baseline without a device read-cursor contract
would lose unread/completion facts. No production database migration or daemon
launch was performed by the validation commands.

## Verification

- Actual `Talkify-MacDirect`, `Debug-MacDirect`, macOS build succeeded with
  `CODE_SIGNING_ALLOWED=NO` and the original sibling AgentKit package.
- Nineteen focused tests passed, including six new performance regressions:
  concurrent foreground refreshes share one list/capability/activity request;
  the first page publishes while activity is suspended; all 819 metadata rows
  become available without controllers; prefetch can retry after failure;
  cancellation suppresses an in-flight page; hiding/reopening stops and resumes
  automatic loading; projection preserves Runtime order and both search
  partitions. Existing pagination, selection, incremental attention and external
  archive checks also passed.
- The larger MultiConversation/history test run retained four existing failing
  tests. Their names/assertions match the saved pre-change baseline logs
  (`agentkit-original-final.log`, `agentkit-head-test.log`); no new failures were
  observed in that group.
- `git diff --check` passed for the modified AgentKit files.

These checks verify behavior and integration, not a measured production UI
speedup. Test the actual signed app from Xcode: cold start to first rows, wait
for older conversations to become searchable, switch recent/project views,
scroll while activity changes, and open an old result. Detail/history loading
should begin only after selection.
