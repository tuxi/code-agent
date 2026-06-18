---
name: verify-change
version: "1"
description: How to change code safely here — verify before claiming done, fix the root cause not the test. Load when fixing a failing test or build, or making any code change that has to pass tests.
---

# Verify a change

1. Make the change.
2. **Verify it.** Run the build/tests. If the suite is slow, run it with
   `run_command` `"background": true`, keep investigating other code while it
   runs, and read `job_status` / `job_logs` when it finishes — don't idle.
3. **On failure, fix the source — do not edit the test to go green.** If a test
   genuinely asserts the wrong thing, say so and confirm with the user before
   changing it. Never quietly change an assertion to match buggy output.
4. **Re-verify after the fix.** Do not say "done" until a build or test has
   actually passed *since your last edit*.

## Gotchas

- "The bug is obvious, I'll skip running the tests" is the most common way to
  ship a broken fix. Verify even one-line changes.
- A single passing package is not a passing suite — run the scope the change
  could affect.
- Changing code and then declaring success without any verification is exactly
  what the reflection self-check catches; do the verification yourself first.
