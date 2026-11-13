# P1-C Freeze Record

Status: frozen
Date: 2026-11-13
Scope: P1-C cross-repository closeout only

`ui_layout.v1` is not implemented, generated, or included in this freeze. This
record does not claim that `ui_layout.v1` is complete.

## Final repository commits

| Repository | Local branch | Commit |
| --- | --- | --- |
| ClientToolsKit | `main` | `ba09721c44c8146e7cab5ac5d02a481a690775f6` |
| code-agent | `main` | `4bb807b424082744cee631e7a5df8a1df43cd3c0` |
| AgentKit | `main` | `477272f90cb62068d3724154337414e91782a558` |
| Talkify | `main` | `189fd9f2c62b046f4fd02008e2276900d29e28e6` |

ClientToolsKit includes detector checkpoint `e421aea` plus format-only commit
`ba09721`, which normalizes `baseline-v2/*.csv` line endings without changing
metric values or detector behavior. code-agent `4bb807b` contains `45da5f5`
and the final system-skill bundle used by AgentKit.

## Acceptance

- Tool acceptance: 10/10 passed.
- Live model acceptance: 8/8 passed.
- Repeated-group precision: 0.8461538461538461.
- Repeated-group recall: 0.6111111111111112.
- Repeated-group F1: 0.7096774193548387.
- Crop integrity: 90/90; stale crop artifacts: 0; hard failures: 0.

The acceptance counts above are frozen checkpoint results. This closeout reran
the repository build and unit checks but did not rerun the live model matrix.

## Evidence limits

- `auto` and `accurate` OCR are bounded. If the accurate recognizer is still
  loading, times out, or fails, evidence can degrade to fast OCR. Fast OCR must
  not be represented as equivalent to accurate OCR. Host warm-up can move model
  load to launch, but does not guarantee an accurate first call.
- Vision rectangles are weak geometry only (`role=vision_rectangle`). They do
  not establish semantic containers and do not seed repeated groups.
- Image-only evidence uses `source_tree.kind=none`. P1-C does not promise AX or
  DOM source-tree evidence.
- Reliable repeated-group output is limited by the recorded precision/recall
  gates; speculative groups remain omitted.

## Runtime data and performance

- Session database cleanup reduced storage from 66.9 GB to 3.4 GB.
- Cold start remains a follow-up performance debt. This freeze does not claim a
  measured cold-start improvement.

## Closeout validation

- code-agent relevant Go tests and `go build ./...`: passed.
- AgentKit `swift build`: passed.
- AgentKit `UIEvidencePresentationTests`: 13/13 passed.
- ClientToolsKit `UIEvidenceBenchmarkCoreTests`: 5/5 passed from the existing
  built test bundle. A forced SwiftPM rebuild was blocked by the Xcode CoreML
  compiler sandbox reading `yolov8n.mlpackage/.../weight.bin`; this was an
  environment failure before test execution, not a benchmark assertion failure.
- Talkify `Talkify-MacDirect` macOS build: passed with cached package resolution
  and signing disabled.
- System-skill bundle validation: passed. code-agent and AgentKit manifest and
  `ui-adaptation/SKILL.md` bytes match.
