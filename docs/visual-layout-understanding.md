# Visual Layout Understanding Design

Status: P0.1 contract frozen
Date: 2026-11-11
Frozen: 2026-11-11
Related: [UI Adaptation Capability Design](./ui-adaptation-capability.md)
Normative contract: [Primary Perception Ownership and Tool Prompt Separation](./primary-perception-ownership-contract.md)

## 0. Document Authority

This document is the detailed upstream contract for image viewing, visual
evidence, and layout interpretation. The related documents keep narrower
responsibilities:

- [UI Adaptation Capability Design](./ui-adaptation-capability.md) is the
  end-to-end capability overview and downstream preview/implementation loop.
- This document is authoritative for `view_image`, `ui_evidence.v1`, and
  `ui_layout.v1` behavior.
- The [Primary Perception Ownership and Tool Prompt Separation
  Contract](./primary-perception-ownership-contract.md) is normative for tool
  ownership, prompt boundaries, capability fallback, and source/crop semantics.
- [Preview Contract v1.6](./capability-runtime-upgrade/PREVIEW_CONTRACT_V1_6.md)
  is authoritative for preview artifact transport, revisions, viewer security,
  and replay.
- Executable contract files under `schemas/` and fixtures under `testdata/` are
  authoritative over examples embedded in prose.

The status becomes frozen only after owner sign-off of the P0 scope in §16.

## 1. Summary

`ui-adaptation` describes the downstream loop from platform guidance and
existing code to interactive preview, implementation, and screenshot review.
The missing upstream capability is understanding what a supplied image actually
shows: its hierarchy, layout, visual priorities, interaction affordances, and
which properties should survive adaptation across iPhone Duo, iPhone, and iPad.

This design adds that upstream capability as **visual layout understanding**:

```text
source evidence
  -> view_image
  -> ui_evidence.v1
  -> ui_layout.v1
  -> user confirmation
  -> ui_preview device matrix
  -> screenshot review and implementation
```

The central decision is **not** to extend OCR into a layout parser. OCR recovers
text; it does not recover hierarchy, containers, repetition, scrolling, visual
priority, or responsive intent. A vision-capable main model should see the
original pixels. Deterministic client/runtime tools should provide geometry and
source hierarchy. The model should publish its interpretation as a reviewable
layout contract rather than silently acting on an OCR summary.

## 2. Problem

### 2.1 Current path

code-agent already has the important transport primitives:

- `LocalAssets` can be injected into a vision-capable provider request.
- `ToolResult.Assets` can carry image side channels and become vision input on
  the following model turn through `visionToolLocalAssets`.
- `tool_finished.assets` is persisted and replayable.
- AgentKit can present structured assets through its asset index and Inspector.
- `ui_preview` already publishes an interactive preview plus a static fallback.

ClientToolsKit also has a meaningful local visual pipeline:

- `AnalyzeLocalImageTool` performs OCR, barcode detection, and
  VisualGroundingKit analysis.
- OCR observations retain normalized bounding boxes.
- `AgentCompactPayload` retains text blocks, subjects, scene evidence, and
  uncertainty.

That is useful evidence, but it is not a UI layout model.

### 2.2 Why the current result is insufficient

1. **OCR-first discards structure.** Text plus bounding boxes does not identify
   parent containers, repeated list rows, scroll ownership, sticky controls, or
   which elements form one semantic group.
2. **The existing schema is photo/generation-oriented.** Subjects, scene, style,
   composition, motion hints, and prompt hints do not describe navigation,
   controls, information hierarchy, or responsive behavior.
3. **A screenshot contains no implementation truth.** Grouping and interaction
   affordances are often inferred. The model needs an explicit uncertainty and
   assumption channel.
4. **One-shot visual reasoning is not durable.** If the source image later leaves
   the active context, the interpretation should survive as a reviewed artifact.
5. **The source image is not consistently visible in the conversation.** A tool
   can analyze an image without exposing that exact image as a clickable asset.
6. **Resizing and multi-pass inspection are incomplete.** UI understanding often
   requires an overview followed by region-level inspection. Current image
   injection has a byte budget but no explicit visual-detail contract.

### 2.3 Root cause

The pipeline currently conflates three different objects:

- pixels the model should inspect;
- measured visual evidence;
- the model's semantic interpretation.

They must remain separate. They have different producers, confidence levels,
security properties, persistence rules, and presentation needs.

## 3. Goals

- Let a vision-capable main model inspect original image pixels directly.
- Show which image the agent inspected in the conversation and replay.
- Produce bounded, normalized visual evidence without treating OCR as truth.
- Represent layout structure and responsive intent in a reviewable schema.
- Support static design images, running-app screenshots, and web screenshots.
- Prefer DOM or accessibility hierarchy when it is available.
- Make inferred content explicitly distinguishable from observed content.
- Feed the existing `ui_preview` and screenshot-review loop.
- Preserve the tool-agnostic agent loop.

## 4. Non-goals

- Pixel-perfect reconstruction from an arbitrary screenshot.
- Automatic production-code edits from visual analysis alone.
- A hidden design agent, keyword router, or fixed visual state machine.
- Treating OCR or model prose as an accessibility tree or DOM.
- Adding image bytes to durable event JSON.
- Making a text-only model behave as though it saw an image without an explicit
  fallback tool and explicit model identity.
- Replacing official Apple platform guidance or the existing execution,
  approval, and preview security boundaries.

## 5. Reference Patterns

### 5.1 Codex

Codex treats `view_image` as a first-class visual input operation. It validates
model image capability, reads a local image, returns image data as an
`input_image`/`image_url` content item, and emits a visible image-view item in
the transcript. It also has a separate original-detail capability.

The relevant lesson is: **load pixels into the model context; do not substitute a
description for seeing the image.**

### 5.2 Hermes-style native vision tools

Hermes `vision_analyze` follows a useful dual path:

- when the active model supports native image input, attach the image directly;
- otherwise use an auxiliary vision model to return text;
- support region crops so the model can inspect the full image first and then
  zoom into small text or controls.

This design adopts the native fast path and region inspection, but keeps any
auxiliary vision bridge as a separately identified fallback rather than hiding
it inside `view_image`.

### 5.3 DeepSeek Harness

DeepSeek Harness separates durable attachments, normalized request images, and
provider-specific upload representations. Text-only history projects images as
placeholders rather than corrupting the session.

The relevant lesson is: image history and provider request images need explicit
budgets and model-capability checks. A tool should not silently drop a source
image and then claim it was understood.

## 6. Source Modes and Evidence Priority

The same screenshot can arrive from different sources with different evidence
quality.

| Source mode | Best available evidence | Required behavior |
| --- | --- | --- |
| Static design image | pixels, OCR bbox, rectangles, repeated-region heuristics | mark inferred hierarchy and interaction |
| Running-app screenshot | pixels plus accessibility tree/window hierarchy | use AX structure when safely available |
| Web screenshot | pixels plus DOM, computed styles, and scroll geometry | use DOM/CSS rather than guessing from pixels |
| User-provided image with no metadata | pixels only | state missing state/interaction facts |

Evidence priority:

```text
source hierarchy (DOM / AX)
  > deterministic geometry (OCR bbox, rectangles, pixel measurements)
  > model visual interpretation
  > OCR text alone
```

The agent must not describe a static screenshot as if it had live interaction or
DOM evidence.

## 7. Architecture

```text
source image or page
    |
    +-- view_image -------------------------> model sees pixels
    |
    +-- inspect_ui_image -------------------> ui_evidence.v1
    |       + Apple Vision OCR/rectangles
    |       + optional DOM/AX tree
    |       + source crops
    |
    +-- ui_layout (validator/producer) -----> ui_layout.v1 + annotated overlay
            |
            +-- user reviews interpretation
            +-- ui_preview target matrix
            +-- screenshots -> view_image -> correction
```

### 7.1 Layer responsibilities

| Layer | Owns | Must not own |
| --- | --- | --- |
| `view_image` tool | Reading one validated image, returning a visible image asset, exposing pixels to a capable model | OCR, semantic interpretation, responsive design |
| `inspect_ui_image` tool | Observable geometry and source hierarchy; bounded crops | Deciding the final layout or editing production code |
| `ui_evidence.v1` | Reproducible evidence records and confidence | Claims about user intent |
| `ui_layout.v1` | Model-authored structure, semantics, assumptions, and responsive contract | Pretending inferred fields are observed facts |
| `ui-adaptation` skill | Procedure, review points, platform research, and verification discipline | Runtime keyword dispatch or hidden automation |
| AgentKit | Generic image/asset/evidence presentation and replay | Choosing the next agent action |
| Agent loop | Tool orchestration through existing interfaces | Special cases for image or design tool names |

No new agent loop branch is required. `view_image` is an ordinary tool whose
result carries an image asset. `ui_evidence.v1` and `ui_layout.v1` are ordinary
tool output and/or workspace artifacts.

## 8. `view_image` Contract

### 8.1 Purpose

`view_image` loads one image from the workspace and makes it available to the
current model as an image content block. It does not analyze the image.

Perception ownership is normative: the main model is the primary visual perceiver
when it has image input capability. `view_image` is the required path for placing
source pixels in that model's context. OCR and UI evidence tools provide optional
facts; they never substitute for perception or choose the design.

### 8.2 Input

```json
{
  "path": "screenshots/product-detail.png",
  "detail": "original",
  "region": {
    "x": 0.0,
    "y": 0.0,
    "width": 1.0,
    "height": 1.0
  }
}
```

Rules:

- `path` is workspace-relative, normalized with forward slashes.
- Absolute paths, traversal, symlink escapes, and non-regular files are rejected.
- `detail` is `high` or `original`; default is `high`.
- `region` uses normalized top-left coordinates in the original image and is
  optional. It is converted to source pixels by the tool.
- `original` is accepted only when the selected model exposes original-detail
  support. Otherwise the tool fails closed or returns an explicit downgrade in
  `output.detail`; it must not silently pretend the original was preserved.
- MIME type is determined from bytes, not from the filename extension.

### 8.3 Output

Tool content is a short observation such as:

```text
Viewed image product-detail.png (1290x2796, original detail).
```

Structured output:

```json
{
  "schema": "view_image_result.v1",
  "kind": "image_view",
  "source_asset_id": "asset_turn_..._source_...",
  "crop_asset_id": null,
  "assets": [
    {
      "asset_id": "asset_turn_..._source_...",
      "asset_role": "source",
      "role": "source",
      "relative_path": "screenshots/product-detail.png",
      "filename": "product-detail.png",
      "mime_type": "image/png",
      "width": 1290,
      "height": 2796,
      "sha256": "64-hex"
    }
  ],
  "detail_requested": "original",
  "detail_effective": "original",
  "source_region": null
}
```

The executable schema is
[`schemas/view_image_result.v1.schema.json`](../schemas/view_image_result.v1.schema.json).
The source/crop uniqueness, ordering, parent linkage, and replay rules below are
normative and covered by contract tests.

Asset rules are fixed:

- `assets[]` contains exactly one source image.
- The source is always `assets[0]`, with `role = "source"`, and its asset ID
  equals `source_asset_id`.
- **No region:** `crop_asset_id` and `source_region` are null and no crop asset
  exists.
- **With region:** `assets[]` contains the source followed by exactly one crop;
  `crop_asset_id` equals the crop asset ID, and `source_region` equals the crop
  `source_bbox`.
- The crop records `source_asset_id` and `source_bbox` using normalized top-left
  coordinates. It never replaces or reorders the source.
- The existing loop stages returned images as `LocalAsset`s for the next model
  request and emits `tool_finished.assets` for AgentKit. No new loop event kind
  is required.
- If the source plus crop cannot fit the model request budget, `view_image` fails
  before returning success. It must not silently drop an image and claim it was
  viewed.

### 8.4 Region crops

When `region` is present:

- crop from the original source before provider-side resizing;
- persist the crop as a controlled workspace asset;
- return the source asset first and the crop asset second;
- keep the crop's mapping to normalized source coordinates;
- display the crop as a child of the viewed-image activity.

The corresponding `assets.Ref` metadata is:

```json
{
  "source_asset_id": "asset_turn_..._source_...",
  "asset_role": "crop",
  "source_bbox": {"x": 0.0, "y": 0.0, "width": 1.0, "height": 0.2},
  "purpose": "navigation"
}
```

The source asset uses `asset_role: "source"`. Replay reconstructs the parent-child
relationship from structured `output` metadata plus the persisted asset refs; it
does not parse the filenames or prose.

Recommended storage root: `.codeagent/assets/view-image/<session>/<turn>/`.
The exact path is an implementation detail, but it must be workspace-contained,
deduplicated by content hash, and safe on replay.

### 8.5 Capability model

The following capabilities must remain distinct:

| Capability | Meaning |
| --- | --- |
| `image_input` | The selected model accepts images in ordinary user messages |
| `tool_image_input` | The provider accepts image content inside a tool result |
| `image_detail_original` | The provider/model preserves original image resolution |
| `vision_bridge` | An explicitly configured auxiliary vision service is available |

`image_input` alone must not be treated as proof that tool-result images work.
If `tool_image_input` is unavailable, `view_image` reports an actionable error;
the skill may use `inspect_ui_image` for text evidence or an explicitly selected
vision bridge.

### 8.6 Capability matrix and fallback behavior

Capabilities are resolved from the selected model/provider and verified by
contract tests. A provider or model name alone is never sufficient.

| State | `view_image` behavior | Conversation behavior |
| --- | --- | --- |
| `image_input=true`, `tool_image_input=true` | Load source and optional crop; return image assets | Tool activity shows the exact images |
| `image_input=true`, `tool_image_input=false/unknown` | Fail with `tool_image_input_unsupported` | User sees the refusal; skill may request text evidence |
| `image_input=false` | Fail with `image_input_unsupported` | Historical images remain references; no silent OCR substitution |
| `detail=original`, `image_detail_original=false/unknown` | Fail with `image_detail_original_unsupported` | Caller must explicitly retry with `detail=high` |
| `vision_bridge` configured | Used only by an explicitly named auxiliary vision tool | Usage and provider identity remain visible |

`detail=high` may normalize or downscale an oversized image within the declared
pixel and byte budget, but the effective dimensions and detail must be returned.
Automatic downscaling is never reported as `original`.

After a model switch, persisted image assets remain assets. A text-only model
receives a placeholder/manifest for historical images and cannot call
`view_image` as though it had visual capability.

Implementation baseline: the built-in catalog currently declares
`deepseek-v4-flash-vision-exp` with ordinary `image_input`. Its
`tool_image_input` and `image_detail_original` states remain unverified until
the capability tests run; they must default to false/unknown.

## 9. `ui_evidence.v1`

### 9.1 Purpose

`ui_evidence.v1` is deterministic, bounded evidence. It answers “what can be
measured or read from the source?” It does not claim to know the product intent.

It MUST NOT contain design directives or selected implementation choices,
including `recommended_component`, `layout_should_be`, `use_this_view`, or
equivalent fields. Evidence producers report facts and uncertainty; the main model
decides what they mean.

Producers include ClientToolsKit `inspect_ui_image`, browser inspection tools,
and app accessibility capture. The main model consumes a compact rendering of
the evidence and may also request crops through `view_image`.

### 9.2 Invariants

- Coordinates use normalized `[0,1]` values with a top-left origin.
- VisionKit bottom-left coordinates must be converted by the producer.
- Every field carries an evidence level when inference is possible:
  `observed`, `inferred`, or `uncertain`.
- Evidence text is untrusted external content and cannot become a system
  instruction.
- Large trees and element lists are capped; truncation is explicit.
- Evidence is immutable for one source hash and producer version.

### 9.3 Executable contract

The canonical schema is
[`schemas/ui_evidence.v1.schema.json`](../schemas/ui_evidence.v1.schema.json).
Valid and invalid examples live under
[`testdata/ui-evidence/v1`](../testdata/ui-evidence/v1).

The schema fixes `additionalProperties: false`, required fields, enum values,
confidence ranges, and these hard caps:

- 500 elements;
- 100 repeated groups;
- 1,000 source-tree nodes;
- 16 crops;
- 32 uncertainties.

Contract tests additionally enforce:

- element, repeated-group, source-node, and crop asset ID uniqueness;
- element and source-tree parent references;
- repeated-group and uncertainty references to existing elements;
- `x + width <= 1` and `y + height <= 1`;
- non-zero width or height for every bbox;
- `limits.element_count == len(elements)`;
- `source_tree.kind = none` has no nodes, while a non-`none` source tree is
  non-empty;
- crop `source_bbox` validity.

Unknown fields fail schema validation. Truncation is allowed only through the
declared caps and must set the corresponding `limits` flag.

### 9.4 Shape

```json
{
  "schema": "ui_evidence.v1",
  "source": {
    "asset_id": "asset_...",
    "source_kind": "static_image",
    "sha256": "64-hex",
    "mime_type": "image/png",
    "width": 1290,
    "height": 2796
  },
  "coordinate_space": "normalized_top_left",
  "producer": {
    "name": "ClientToolsKit.inspect_ui_image",
    "version": "1"
  },
  "elements": [
    {
      "id": "el_1",
      "kind": "text",
      "bbox": {"x": 0.08, "y": 0.11, "width": 0.54, "height": 0.03},
      "text": "商品详情",
      "confidence": 0.98,
      "evidence": "observed",
      "parent_id": null,
      "interactive": false
    }
  ],
  "repeated_groups": [
    {
      "id": "rg_1",
      "axis": "vertical",
      "member_ids": ["el_10", "el_11", "el_12"],
      "bbox": {"x": 0.0, "y": 0.42, "width": 1.0, "height": 0.34},
      "confidence": 0.81,
      "evidence": "inferred"
    }
  ],
  "source_tree": {
    "kind": "none",
    "nodes": [],
    "truncated": false
  },
  "crops": [
    {
      "asset_id": "asset_crop_...",
      "source_bbox": {"x": 0.0, "y": 0.0, "width": 1.0, "height": 0.2},
      "purpose": "navigation"
    }
  ],
  "uncertainties": [
    {
      "description": "The repeated region may be a list or a horizontal carousel.",
      "related_element_ids": ["el_10", "el_11", "el_12"]
    }
  ],
  "limits": {
    "element_count": 42,
    "elements_truncated": false,
    "tree_nodes_truncated": false
  }
}
```

### 9.5 Element kinds

Initial `kind` values:

- `text`
- `image`
- `icon`
- `control`
- `container`
- `separator`
- `unknown`

Semantic roles such as `navigation`, `tab_bar`, `card`, `button`, `input`, or
`list_row` are allowed only when their evidence level says they are inferred.

### 9.6 Source hierarchy

`source_tree.kind` is `accessibility`, `dom`, or `none`.

For `accessibility` and `dom`, the producer records a flat, capped node list with:

- stable node id;
- parent id;
- role;
- accessible label or text;
- bbox when available;
- visibility and enabled state;
- whether the node is interactive.

The tree is evidence, not a full DOM dump. Scripts, event handlers, credentials,
and raw HTML are excluded.

## 10. `ui_layout.v1`

### 10.1 Purpose

`ui_layout.v1` is the model's reviewable interpretation of the source. It turns
geometry and hierarchy into an explicit layout contract that can drive device
adaptation and preview generation.

It is not authoritative pixel evidence. Every non-observable assertion must be
marked inferred or uncertain and reference the evidence that motivated it.

### 10.2 Producer flow

1. Inspect the full source with `view_image`.
2. Obtain `ui_evidence.v1` when available.
3. Inspect relevant crops or DOM/AX nodes.
4. Draft `ui_layout.v1` using only the permitted schema.
5. Validate the draft and render an annotated overlay.
6. Show the overlay and a concise natural-language interpretation to the user.
7. Treat the confirmed revision as the design contract for `ui_preview`.

The planned `ui_layout` tool should validate and persist model-supplied JSON and
render the overlay. It should not silently invent the layout itself.

### 10.3 Executable contract

The canonical schema is
[`schemas/ui_layout.v1.schema.json`](../schemas/ui_layout.v1.schema.json).
Valid and invalid examples live under
[`testdata/ui-layout/v1`](../testdata/ui-layout/v1).

The schema and contract tests enforce:

- `additionalProperties: false` at every object level;
- confidence values in `[0,1]`;
- normalized bbox validity;
- at most 1,000 total layout nodes;
- unique node and invariant IDs;
- node policies reference existing nodes;
- every `evidence_ids` reference resolves when matching `ui_evidence.v1` is
  supplied;
- `source.asset_id` and `source.source_asset_sha256` match the source evidence;
- at least one responsive invariant and target;
- target keys use stable lower-snake-case identifiers.

Schema evolution follows these rules:

- additive optional fields may remain in `v1` only when old readers can safely
  ignore them;
- any removed field, changed meaning, or required-field change requires
  `ui_layout.v2`;
- unknown fields fail closed rather than being silently discarded.

### 10.4 Shape

```json
{
  "schema": "ui_layout.v1",
  "source": {
    "asset_id": "asset_...",
    "evidence_schema": "ui_evidence.v1",
    "canvas": {"width": 1290, "height": 2796}
  },
  "screen": {
    "type": "commerce_product_detail",
    "primary_action": "购买",
    "content_priority": ["商品信息", "价格", "购买按钮"],
    "states": ["default"]
  },
  "root": {
    "id": "n_root",
    "role": "screen",
    "bbox": {"x": 0.0, "y": 0.0, "width": 1.0, "height": 1.0},
    "layout": {"mode": "vertical_stack", "gap": 0.02},
    "children": [],
    "evidence_ids": [],
    "confidence": 0.9,
    "evidence": "inferred"
  },
  "responsive": {
    "invariants": [
      {
        "id": "inv_primary_action",
        "statement": "The primary purchase action remains discoverable without scrolling past the product summary."
      }
    ],
    "targets": {
      "duo_folded": {"columns": 1, "navigation": "bottom_tabs"},
      "duo_unfolded": {"layout": "split_view", "columns": 2},
      "iphone_portrait": {"columns": 1, "navigation": "bottom_tabs"},
      "ipad_landscape": {"columns": 2, "navigation": "sidebar"}
    },
    "node_policies": [
      {
        "node_id": "n_product_grid",
        "policy": "reflow",
        "notes": "Increase columns when regular width is available."
      }
    ]
  },
  "assumptions": [
    {
      "statement": "The bottom action bar remains sticky while the product body scrolls.",
      "confidence": 0.74,
      "evidence": "inferred",
      "evidence_ids": ["el_42"]
    }
  ]
}
```

### 10.5 Node fields

Each node supports:

- `id`: stable within the layout document;
- `role`: semantic or structural role;
- `bbox`: normalized source coordinates when available;
- `content`: text or asset references;
- `layout`: mode, direction, distribution, alignment, wrap, sizing, and gap;
- `style`: visual hints such as emphasis or hierarchy, not guessed design tokens;
- `behavior`: scroll axis, sticky state, navigation action, or interaction role;
- `children`: nested child nodes;
- `evidence_ids`: evidence records that support the node;
- `confidence` and `evidence`: interpretation quality.

Initial layout modes: `vertical_stack`, `horizontal_stack`, `grid`, `overlay`,
`scroll`, `absolute`, and `unknown`.

Initial node policies: `preserve`, `reflow`, `replace`, `hide`, and `promote`.

### 10.6 Responsive contract

Responsive behavior is expressed as invariants plus target-specific rules, not
as screenshot percentages.

Targets should use stable form-factor identifiers while remaining extensible:

- `duo_folded`
- `duo_unfolded`
- `iphone_portrait`
- `iphone_landscape`
- `ipad_portrait`
- `ipad_landscape`

The layout should state:

- navigation model;
- column or pane strategy;
- content max width;
- scroll ownership;
- sticky controls;
- promoted/demoted content;
- modal versus inline presentation;
- safe-area and fold/posture constraints.

These are proposals, not universal rules. Official platform guidance and the
existing production code remain authoritative.

### 10.7 Preview lineage

A layout interpretation is useful only if a preview can be traced back to the
exact reviewed revision. P1 adds an optional `lineage` object to `ui_preview`
for previews derived from `ui_layout.v1`:

```json
{
  "schema": "ui_preview_lineage.v1",
  "layout_artifact_id": "layout_dreamlog_duo_home",
  "layout_revision": 3,
  "layout_revision_sha256": "sha256:...",
  "source_asset_id": "asset_turn_..._source_...",
  "source_asset_sha256": "64-hex",
  "approval": {
    "state": "user_confirmed",
    "evidence_ref": "turn_.../user_message_..."
  },
  "target_matrix": {
    "id": "matrix_dreamlog_duo_iphone_ipad",
    "targets": ["duo_folded", "duo_unfolded", "iphone_portrait", "ipad_landscape"]
  }
}
```

The canonical schema is
[`schemas/ui_preview_lineage.v1.schema.json`](../schemas/ui_preview_lineage.v1.schema.json).
The runtime stores the validated object under
`preview.Artifact.Metadata["ui_preview_lineage"]` and echoes it in the
`ui_preview` tool output. The preview artifact remains revision-addressed by the
existing preview contract.

Lineage rules:

- `layout_artifact_id`, `layout_revision`, and `layout_revision_sha256` identify
  the exact reviewed layout revision;
- `source_asset_id` and `source_asset_sha256` identify the source image;
- `approval.state = user_confirmed` requires a non-empty `approval.evidence_ref`;
- `approval` is provenance, not proof; clients must display it as recorded
  lineage and must not treat model-authored text alone as a user approval event;
- previews derived from a layout contract require lineage; standalone
  `ui_preview` calls omit lineage entirely;
- `approval.state = not_required` is only for a layout-derived preview whose
  workflow explicitly does not require user confirmation;
- missing, dangling, or digest-mismatched lineage fails the preview draft closed;
- replay returns the stored lineage unchanged with the preview revision.

### 10.8 Tool result contract

A successful `ui_layout` call returns a `ui_layout_result.v1` document on the
structured tool-output side-channel. The canonical schema is
[`schemas/ui_layout_result.v1.schema.json`](../schemas/ui_layout_result.v1.schema.json).
The document binds one persisted revision to its identity and to two assets:

- `overlay_asset`: the rendered annotated overlay as an `image/png` asset;
- `layout_json_asset`: the canonical `layout.json` as an `application/json`
  file asset, so a client can parse it without re-deriving a path.

Each asset descriptor carries `asset_id`, `asset_role`, `relative_path`,
`filename`, `mime_type`, and `sha256`. Every `asset_id` matches the corresponding
entry in the tool-result asset side-channel. `layout_json_asset.sha256` equals
the digest in `layout_revision_sha256`, and `evidence_sha256` is `null` when no
`ui_evidence.v1` document was bound. Targets are sorted, identical calls reuse
the same revision, and the result, `metadata.json`, and assets must agree.

### 10.9 Persistence and versioning

- Store canonical JSON as a workspace artifact linked by source asset hash.
- Store the rendered annotated overlay as an image asset.
- Never store image bytes inside the JSON.
- Schema changes that remove fields or change meanings require a new major
  schema version.
- Additive optional fields may remain within `v1` only when old readers can
  safely ignore them.
- A new layout revision for the same source is a new artifact revision, not a
  mutation of prior replay evidence.

### 10.10 Presentation

Users should not be asked to read raw JSON by default. The client presents:

- the source image and annotated overlay;
- a short hierarchy summary;
- observed facts versus key assumptions;
- unresolved questions;
- links to evidence crops and the raw layout artifact in the Inspector.

## 11. End-to-End Workflow

1. Resolve the source mode and target devices.
2. Inspect the source with `view_image`.
3. Gather `ui_evidence.v1` from DOM, AX, or Apple Vision.
4. Request region crops for ambiguous or small areas.
5. Draft `ui_layout.v1` and render an annotated overlay.
6. State the interpretation and unresolved assumptions to the user.
7. Obtain confirmation before producing device-specific designs or editing UI.
8. Build the target matrix and generate previews through `ui_preview`.
9. Capture target screenshots and inspect them with `view_image`.
10. Correct layout issues, implement production UI, and verify with builds and a
    real device/viewport matrix.

The skill owns this procedure. Runtime code does not enforce a multi-step design
state machine.

## 12. Runtime and Client Integration

### 12.1 code-agent

- Add `view_image` as a built-in tool that returns an image `assets.Ref`.
- Reuse `visionToolLocalAssets` and `withVisionContentParts`; do not add tool-name
  checks to the agent loop.
- Expose source images through `tool_finished.assets` for live and replay use.
- Add schema validation/fixtures for `ui_evidence.v1` and `ui_layout.v1`.
- Add model capabilities for tool-result images and original detail separately
  from ordinary image input.
- Keep image bytes out of durable event payloads.

### 12.2 ClientToolsKit / VisualGroundingKit

- Add a UI-specific profile instead of overloading the photo/generation profile.
- Return `ui_evidence.v1` from a client tool such as `inspect_ui_image`.
- Attach the analyzed source image and useful crops as `AgentAssetRef`s.
- Prefer DOM/AX when the host can provide them.
- Convert all Vision coordinates to normalized top-left coordinates.
- Keep `analyze_local_image` as optional OCR/barcode/metadata/photo analysis. Its
  description must not say it is recommended before `view_image` and it must not
  act as a prerequisite or substitute for direct visual perception.
- This work is P1 and cross-repository; P0 changes only document and validate the
  contract in code-agent.

### 12.3 AgentKit

- Render viewed images as first-class visual activity, not only path text.
- Show source image, region crop, dimensions, and original-detail status.
- Render the layout overlay in the conversation and raw evidence/IR in Inspector.
- Ensure live and replay views use the same structured assets.
- Avoid a tool-name-specific timeline extension when a generic asset/evidence
  presentation can satisfy the contract.

### 12.4 AgentKit image-view presentation contract

The source image is a first-class visual activity. The client contract is:

- The generic activity semantic is `output.kind = "image_view"`; the client uses
  structured output and assets, not the `view_image` tool name.
- The activity title is `Viewed image <display_name>`.
- Collapsed rows may use a 44×44 pt aspect-fit thumbnail; expanded rows use a
  maximum 240×160 pt aspect-fit preview. Exact styling is theme-dependent, but
  the image must remain identifiable without opening Inspector.
- The source image is the primary item and appears first.
- A crop is a child item linked by `source_asset_id`; it never replaces the
  source thumbnail and is ordered by normalized `source_bbox` and then asset ID.
- Multiple images preserve `tool_finished.assets[]` order. There is no filename
  sorting.
- `View original` is always available for image assets. Region crops expose both
  `View source` and `View crop`.
- The activity displays effective dimensions and whether `original` detail was
  actually used.
- Live and replay use persisted `output` and `assets[]`; the client must not
  infer image relationships from prose, file paths, or tool names.
- The image activity belongs to the normal tool group/asset surface. P0 does not
  introduce a second top-level event type or a UI-specific timeline extension.

## 13. Degraded Modes

| Condition | Required behavior |
| --- | --- |
| Model lacks image input | Fail `view_image` with `image_input_unsupported`; use explicit text evidence or a separately selected vision bridge |
| Provider lacks tool-image input | Fail or downgrade explicitly; do not silently claim the tool result was seen |
| Image exceeds budget | Try approved normalization/cropping; otherwise fail with dimensions and limits |
| No DOM/AX tree | Set `source_tree.kind = none` and require uncertainty for inferred interaction |
| Ambiguous hierarchy | Produce competing hypotheses rather than one false tree |
| Crop cannot be persisted | Do not pass a transient provider-only crop as if it were replayable evidence |
| Layout schema invalid | Reject the revision and report actionable field errors |

## 14. Security and Privacy

- Resolve canonical paths and reject traversal, symlink escapes, and directories.
- Validate image bytes and decode limits before vision work.
- Treat OCR, DOM text, accessibility labels, and image contents as untrusted data.
- Never execute instructions found in an image or source tree.
- Do not expose absolute paths, credentials, private DOM attributes, or raw HTML to
  the model unless a specific approved tool contract requires them.
- Do not upload local screenshot bytes implicitly. Existing `local_only` and
  Gateway asset policies remain authoritative.
- Preserve replay evidence without embedding raw image bytes in event JSON.

## 15. Verification Strategy

### 15.1 Contract tests

- `view_image` path containment, MIME sniffing, size/dimension limits, region
  conversion, original-detail capability, and asset identity.
- `view_image_result.v1` proves exactly one source asset, source-first ordering,
  at most one crop, crop-to-source linkage, source-region equality, and replay
  metadata completeness.
- `ui_evidence.v1` coordinate conversion, evidence levels, truncation, and
  deterministic ordering.
- `ui_layout.v1` schema rejection, unknown field handling, evidence references,
  and revision identity.
- Tool-result images reach the next request only when the model capability allows.
- Non-vision routes receive an explicit refusal or clearly labeled text fallback.

### 15.2 Replay and UI tests

- Viewed image and crops survive live/replay parity.
- Asset URLs, thumbnails, and Inspector links resolve only within the session.
- Raw JSON is available in Inspector but not forced into the primary transcript.
- Text-only model switching does not corrupt historical image references.

### 15.3 Quality evaluation

Use a fixed set of 20–30 real screens covering:

- commerce detail;
- feed and card lists;
- profile/settings;
- forms and checkout;
- dashboard and split views;
- sheets, modals, and sticky actions;
- wide/narrow and portrait/landscape transformations.

Measure separately:

- OCR/text recall;
- element and group accuracy;
- hierarchy and reading-order accuracy;
- responsive invariant satisfaction;
- user correction rate;
- visual defects found in target screenshots.

Do not combine these into a generic “looks good” score.

### 15.4 Benchmark manifest

The benchmark is stored as `ui_benchmark.v1` fixtures before quality thresholds
are interpreted. Each case contains:

- stable case ID and source asset reference;
- source kind and target form factors;
- human-labeled text blocks and bboxes;
- human-labeled groups and hierarchy;
- expected reading order;
- responsive invariants and unacceptable regressions;
- optional reviewer notes.

The initial regression set is the DreamLog Duo sample plus at least ten screens
covering commerce, feed, settings, forms, checkout, dashboard, sheets, and
split-view layouts.

### 15.5 Acceptance gates

These P0 gates are normative:

- all JSON Schemas (`view_image_result.v1`, `ui_evidence.v1`, `ui_layout.v1`,
  and `ui_preview_lineage.v1`) compile and resolve;
- every valid fixture passes schema and semantic validation;
- every invalid fixture fails at least one validation layer;
- unknown fields fail closed;
- `view_image` tests prove source-always/crop-optional assets, deterministic
  order, crop parent linkage, replay linkage, and no silent budget loss;
- no tool-name branch is added to `internal/agent/loop.go`;
- image activity remains visible after replay and model switching.

P1 quality thresholds are provisional until the first benchmark baseline is
reviewed:

- readable OCR text recall ≥ 0.95 for text at least 14 px high;
- median text-block bbox IoU ≥ 0.85;
- repeated-group detection F1 ≥ 0.80;
- hierarchy and reading-order accuracy ≥ 0.90 on the labeled set;
- DOM/AX-derived ordering is exact when the source tree is complete;
- fewer than 10% of reviewed layout nodes require semantic correction.

No P1 claim is accepted without the fixture manifest, evaluator, and raw
per-case results. A model-quality claim cannot be inferred from a successful
preview alone.

## 16. Delivery Phases and P0 Decision

### 16.1 Proposed phases

**P0 — foundation**

- `ui-adaptation` skill;
- built-in `view_image`;
- source image visible in AgentKit tool activity;
- schemas and fixtures for `view_image_result.v1`, `ui_evidence.v1`, and
  `ui_layout.v1`;
- no automatic layout inference yet.

**P1 — evidence extraction**

- `inspect_ui_image` client tool;
- OCR/rectangle/repeated-region evidence;
- optional DOM/AX evidence;
- region crops.

**P2 — interpretation artifact**

- `ui_layout` validation/persistence tool;
- annotated overlay;
- user review of layout tree and assumptions.

**P3 — adaptation loop**

- `ui_preview` device matrix;
- screenshot comparison;
- production implementation and visual verification.

### 16.2 Selected P0 scope

P0 is the lean foundation:

- `ui-adaptation` skill;
- built-in `view_image`;
- source image and optional crops visible in AgentKit tool activity;
- executable `view_image_result.v1`, `ui_evidence.v1`, `ui_layout.v1`, and
  preview-lineage schemas;
- valid/invalid fixtures and contract tests;
- no automatic layout inference.

P0 explicitly excludes:

- `inspect_ui_image` and repeated-region detection;
- `ui_layout` overlay rendering;
- automatic multi-device design generation;
- a dedicated UI-grounding model;
- runtime enforcement of the full design workflow.

### 16.3 P0 exit criteria

- The skill validates and is discoverable in the project skill index.
- `view_image` obeys the source/crop asset contract and capability matrix.
- AgentKit renders the viewed image during live execution and replay.
- All contract fixtures pass the P0 gates in §15.5.
- The implementation diff contains no UI/image tool branch in the agent loop.
- The observed capability matrix is recorded for every tested provider/model.
- A short end-to-end fixture demonstrates ordinary viewing without claiming
  layout understanding that P0 does not implement.

P1 starts only after these exit criteria are reviewed. P1 owns automatic UI
evidence extraction; P2 owns layout validation and annotated overlays.

## 17. Open Questions

1. Should the first `inspect_ui_image` implementation live in ClientToolsKit or
   in a host extension that can also access AX/DOM?
2. Which provider/model combinations can be verified for tool-result image input
   and original-detail support?
3. What is the maximum accepted pixel count for a full UI screenshot, and when
   should semantic crops replace arbitrary downscaling?
4. Should `ui_layout` store its canonical JSON beside the preview artifact or in
   a separate layout artifact namespace?
5. Which authoritative source supplies iPhone Duo folded/unfolded dimensions and
   safe-area/posture rules for the first evaluation matrix?
