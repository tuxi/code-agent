# Preview Contract Fixtures v1

These fixtures are shared by code-agent and AgentKit contract tests.

## Files

- `ready-interactive.json`, `static-image.json`, `pending.json`, `unavailable.json`.
- `failed-with-fallback.json`, `failed-without-fallback.json`: failed-state precedence.
- `unavailable-with-fallback.json`: unavailable-state fallback precedence.
- `expired-replay.json`: ready artifact whose past `expires_at` derives expiry.
- `event-*.json`, `replay-frames.json`: full wire envelopes and replay cases.
- `capability-matrix.json`: runtime/client capability intersection cases.
- `producer-wire.json`: server, hidden-client, and direct-client producer shape; producer identity is runtime-derived and never client-supplied.
- `tool-bindings.json`: `register_tools` visibility/executor binding shape.
- `resolver-contract.json`: shared HTTP/in-process resolver expectations.
- `manifest.json`: scenario IDs and expected final presentation states.
- `negative/`: navigation, network, bridge, path, digest, UTF-8, and size cases.
- `viewer.html`: trusted viewer body used to compute the viewer digest.
- `assets/fallback.png`: immutable raster fallback asset used to compute the asset digest.

## Digest rule

Fixture digests are SHA-256 over the exact file bytes:

```text
viewer.html: 52e183244f37b43eb8f29cd4e8811314ecb50083887bd7ea28804a2f942c9f88
assets/fallback.png: 5f8f80a22537ae2a13d82094cdcfc8166cffaa01be0b6f1a9586623467070ee0
```

The fixture artifact paths are logical runtime routes, not local filesystem paths.

All fixture JSON files are validated with `jq empty`. Artifact digests are fixed
and can be independently recomputed from the canonical artifact JSON excluding
`revision_sha256`.

`manifest.json` includes a fixed evaluation clock for expiry, per-file SHA-256
digests, and expected artifact/presentation outcomes. The 2 MiB and 2 MiB + 1
viewer files are generated and committed with fixed hashes.

`negative/viewer-2MiB.html` and `negative/viewer-2MiB-plus-1.html` are size
preflight fixtures only. They are intentionally not complete CSP-valid viewers;
`viewer.html` remains the CSP acceptance fixture.
