# Talkify runtime readiness and stale endpoints

## Evidence, 2026-10-17

The user supplied a slow-launch trace from the actual app. Shell/process setup
took about 2.34 seconds; database open took 66 ms; daemon initialization through
port publication took 128 ms. The authenticated connection was published after
2,993 ms. The trace does not include the first list response or its UI publication,
so it does not establish where any subsequent long wait occurred.

A read-only aggregate check found approximately 830 conversations and no managed
worktrees in this database. Worktree reconciliation is therefore not responsible
for this launch. No worktree recovery changes are included in this fix.

## Confirmed lifecycle defects and changes

1. The registry retains the previous daemon's ephemeral port across app launches.
   The old root created Workspace immediately and could issue requests or start
   restoring sockets before the new daemon endpoint and bearer token were ready.
   The macOS root now renders a startup state until this launch has passed health
   checking, registration and token injection. Only then does it create Workspace.
   Local client construction is also guarded against this stale-endpoint window.
2. An unexpected daemon exit previously restarted only the child process. It did
   not register the new endpoint/token with the host. Restart now routes through
   the host's startup flow, cancels old peripheral bootstrap work, and publishes
   a fresh connection before creating another Workspace.
3. AgentKit now represents a missing runtime endpoint as waiting, even when the
   bounded initial retry window expires. It skips activity discovery while in
   that state. Ordinary response/authentication errors remain errors. Startup
   failure after the host's actual retries shows a retry action.

No database migration, timeout increase, or historical-status suppression is
part of this change. Provider onboarding is deferred during local startup.

## Diagnostics and verification

`[list-perf] head` reports endpoint/credential preparation, response wait and
decoding durations, plus response byte/row counts. It does not log endpoints,
tokens, cursors or conversation content. `[list-perf] first_page_published`
reports when the first page reaches the list ViewModel; this is not a rendered
frame measurement. Compare these with the host's authenticated-ready timestamp
on the next actual launch to locate any remaining delay.

- Fifteen list/startup/pagination tests passed, including waiting beyond the
  retry window, recovery once ready, and preservation of real failure reporting.
- The actual Talkify-MacDirect Debug-MacDirect macOS build succeeded with code
  signing disabled, using the original sibling AgentKit package.
- An isolated fake-child probe compiled the current daemon lifecycle source and
  verified crash routing through the host callback, endpoint/token rotation and
  explicit stop without restart. It did not stop the real daemon or use its DB.
- Repository diffs were checked for whitespace errors. The host's pre-existing
  Xcode signing configuration edits are outside this change.

The real app's post-fix cold-start duration and the source of any remaining
heartbeat failures still require an actual launch trace. The original supplied
trace did not contain the heartbeat failure text.
