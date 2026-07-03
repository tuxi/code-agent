# Review Dimensions — Detailed Checklist

Load this when the user asks for a deep/exhaustive review, or when the change
is in a high-risk area (auth, data persistence, concurrency primitives, public
API surface).

## Correctness

- [ ] Every `if` condition is the right polarity.
- [ ] Every `switch` has a `default` case or exhaustively covers the type.
- [ ] Integer arithmetic: overflow, underflow, division by zero.
- [ ] Slice bounds: `len()` check before index access, `copy` dst capacity.
- [ ] Map access: `v, ok := m[k]` used when zero value is ambiguous.
- [ ] Deferred calls: arguments evaluated at `defer` time (use closure if you
  want the latest value).
- [ ] `defer` in a loop: accumulates until function return — intentional?
- [ ] Mutex held across I/O or channel operations (deadlock risk).
- [ ] `sync.Once` usage: `once.Do` must not call `once.Do` recursively.
- [ ] Time: monotonic vs wall clock, `time.Since` vs `time.Now().Sub`, DST
  boundary handling.
- [ ] String indexing: `s[i]` returns a byte, not a rune — correct for the
  use case?

## Error Handling

- [ ] Every `err != nil` branch either wraps + returns, or handles
  definitively (not both).
- [ ] `errors.Join` used to collect multiple independent errors; not used to
  mask a single error.
- [ ] Custom error types implement `Is` / `As` targets correctly.
- [ ] Sentinel errors pre-allocated (`var ErrX = errors.New(...)`) not
  allocated per use.
- [ ] Error messages: lowercase, no trailing period, describe what failed not
  what to do.
- [ ] No PII or secrets in error messages.
- [ ] `%w` at internal boundaries, `%v` at API/system boundaries that should
  not leak internals.

## Concurrency

- [ ] Goroutine lifetime: started goroutines have a clear stop condition.
- [ ] No goroutine leak: every `go func()` has a path to exit.
- [ ] `context.Context` is the first parameter and is passed (not stored in a
  struct for long-lived objects).
- [ ] `select` with `default` used for non-blocking sends; without `default`
  for blocking coordination.
- [ ] Channels closed by the sender, not the receiver.
- [ ] `sync.Mutex`: unlock in `defer` immediately after lock.
- [ ] `sync.RWMutex`: read lock not upgraded to write lock (deadlock).
- [ ] `atomic` operations used for simple counters, not as a general
  concurrency primitive (use mutex for multi-field invariants).
- [ ] `sync.Map` only used when the use case matches its documented strengths
  (entry stable over time, disjoint key sets across goroutines).

## Resource Management

- [ ] Files, connections, response bodies: closed via `defer` immediately
  after successful open.
- [ ] `defer resp.Body.Close()` after checking `err != nil` (close nil is
  fine, but missing the close isn't).
- [ ] Buffers and large allocations: pooled or bounded; no unbounded growth.
- [ ] Timers: `timer.Stop()` called when no longer needed to free resources.

## Testing

- [ ] New code paths have tests.
- [ ] Table-driven tests for functions with multiple input categories.
- [ ] `t.Parallel()` used where safe (no shared mutable state between test
  cases).
- [ ] `t.Cleanup` or `defer` for test resource cleanup.
- [ ] `t.TempDir()` instead of hand-rolled temp directory management.
- [ ] Time-dependent code uses injectable clock or `testing/synctest` (Go
  1.24+), not `time.Sleep`.
- [ ] No `os.Getenv` in tests without `t.Setenv`.
- [ ] Test helper functions call `t.Helper()`.

## Design

- [ ] New exported API: minimal, clear, hard to misuse.
- [ ] Package responsibilities: does the new code belong in this package, or
  should it be elsewhere?
- [ ] Interface segregation: interface defined at the consumer, not the
  producer.
- [ ] Accept interfaces, return structs.
- [ ] Functional options pattern used over constructor with many parameters.
- [ ] No global mutable state (package-level `var` that gets modified).
- [ ] `init()` functions: only when truly necessary (registering drivers,
  side-effect imports in main/test).

## Security (when relevant)

- [ ] Input validation: user input, HTTP parameters, file paths.
- [ ] Path traversal: `filepath.Clean` + containment check for file
  operations based on user input.
- [ ] SQL injection: parameterized queries, no string concatenation.
- [ ] Command injection: no user input passed to `exec.Command` shell
  strings.
- [ ] Sensitive data: not logged, not included in error messages.
- [ ] Randomness: `crypto/rand` for security-sensitive values, `math/rand`
  only for non-security uses.

## Documentation

- [ ] Every exported symbol has a doc comment starting with the symbol name.
- [ ] Package doc comment present and explains what the package provides (not
  how it is implemented).
- [ ] Deprecated symbols marked with `// Deprecated:` comment.
