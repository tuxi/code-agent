# Preview Fixture Server

`codeagent-preview-fixture` serves the frozen `preview.fixtures.v1` data over a
temporary authenticated loopback HTTP listener. It wires the production preview
service and Runtime mux; the fixture-only routes only expose frozen artifact and
event bytes.

## Start

From the repository root:

```sh
go run ./cmd/codeagent-preview-fixture \
  --fixtures docs/capability-runtime-upgrade/fixtures/preview/v1
```

The first stdout line is a JSON readiness record:

```json
{
  "base_url": "http://127.0.0.1:54321",
  "bearer_token": "<random 256-bit token>",
  "catalog_url": "http://127.0.0.1:54321/v1/preview-fixtures",
  "fixture_version": "preview.fixtures.v1",
  "contract": "preview_contract_v1",
  "evaluation_time": "2026-11-09T00:00:00Z",
  "authorization": "Bearer <random 256-bit token>"
}
```

Send the `authorization` value on every request. The process runs until
`SIGINT` or `SIGTERM`. The random listener port is available only from the
startup record.

## Endpoints

- `GET /v1/runtime/capabilities`: production capability snapshot with all four
  preview flags.
- `GET /v1/conversations/{session}/previews/{artifact}/revisions/{revision}/viewer`:
  production viewer resolver.
- `GET /v1/conversations/{session}/previews/{artifact}/revisions/{revision}/assets/{asset}/content`:
  production asset resolver.
- `GET /v1/conversations/{session}/events`: production event replay for the
  revision, duplicate, and conflict streams.
- `GET /v1/preview-fixtures`: scenario catalog with artifact, viewer, asset,
  raw-event, and Runtime replay URLs.
- `GET /v1/preview-fixtures/artifacts/{scenario}`: exact frozen artifact bytes.
- `GET /v1/preview-fixtures/events/{scenario}`: exact frozen replay sequence.

Raw fixture responses include `X-Preview-Fixture-SHA256` over the response body.

## Scenario Mapping

| Scenario | Artifact | Runtime replay |
| --- | --- | --- |
| `ready-interactive` | `preview_fixture_interactive@1` | `session_fixture` |
| `static-image` | `preview_fixture_static@1` | n/a |
| `pending` | `preview_fixture_pending@1` | n/a |
| `failed-with-fallback` | `preview_fixture_interactive@2` | n/a |
| `failed-without-fallback` | `preview_fixture_failed_no_fallback@1` | n/a |
| `unavailable` | `preview_fixture_unavailable@1` | n/a |
| `unavailable-with-fallback` | `preview_fixture_unavailable_with_fallback@1` | n/a |
| `expiry-derived` | `preview_fixture_expired@1` | returns HTTP 410 |
| `revision-replacement` | final `preview_fixture_interactive@2` | `session_fixture_revision_replacement` |
| `duplicate-revision` | `preview_fixture_interactive@1` | `session_fixture_duplicate_revision` |
| `revision-conflict` | preserved `preview_fixture_interactive@1` | `session_fixture_revision_conflict` |

## Verify

```sh
go test ./internal/preview/fixtureserver ./internal/preview/fixturetest
```
