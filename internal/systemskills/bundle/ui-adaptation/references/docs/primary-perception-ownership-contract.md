# Primary Perception Ownership and Tool Prompt Separation Contract

Status: normative, frozen for P0.1
Date: 2026-11-11
Frozen: 2026-11-11
Applies to: P0 and later visual-input work

Related contracts:

- [Visual Layout Understanding Design](./visual-layout-understanding.md)
- [UI Adaptation Capability Design](./ui-adaptation-capability.md)
- [`schemas/view_image_result.v1.schema.json`](../schemas/view_image_result.v1.schema.json)
- [`schemas/ui_evidence.v1.schema.json`](../schemas/ui_evidence.v1.schema.json)

This contract uses MUST, MUST NOT, SHOULD, and MAY normatively. A runtime that
violates MUST/MUST NOT behavior is non-compliant even if it produces a plausible
answer or preview.

## 1. Primary Perception Ownership

1. The **main model is the primary visual perceiver** when the selected model has
   image input capability.
2. When the main model needs to understand image content, `view_image` MUST be
   the path that puts the source pixels into the model context.
3. `view_image` is a general image-input capability. It is not UI-specific and
   MUST NOT contain UI layout or design workflow instructions.
4. The main model owns visual semantic interpretation: layout meaning, component
   choice, information hierarchy, responsive intent, and design decisions.
5. Evidence tools MUST NOT become the primary perceiver or make design decisions
   on behalf of the main model.
6. A client-side OCR, barcode, metadata, or photo-analysis result MUST NOT be
   treated as evidence that the model saw the image.

## 2. Tool Role Separation

### 2.1 `view_image`

`view_image` MUST:

- load one validated workspace image;
- expose the source pixels to a capable model;
- return the source image asset and structured metadata;
- allow an optional region crop while retaining the source;
- fail explicitly when the selected model cannot consume the required image
  content.

`view_image` MUST NOT:

- run OCR or barcode recognition as a substitute for viewing;
- infer UI hierarchy, select controls, or recommend layouts;
- call or depend on `analyze_local_image` as a prerequisite;
- silently convert the image to text.

### 2.2 `analyze_local_image`

`analyze_local_image` is optional local analysis for OCR, barcodes, metadata, or
photo-scene descriptors.

It MUST NOT:

- be described as the recommended first step for understanding an image;
- imply that calling it means the model saw the image;
- act as a prerequisite, fallback substitute, or gate for `view_image`;
- return layout/component decisions for UI adaptation.

Its tool description MUST language-equivalent the following:

> Analyze a local image for OCR, barcode, metadata, or photo-scene facts. Use only
> when those specific facts are needed. This tool does not expose the image to the
> model and is not a substitute for view_image.

The current ClientToolsKit description that says `auto` is "recommended for Agent
calls" is non-compliant. Correcting that text is a ClientToolsKit follow-up; this
document does not modify that repository.

### 2.3 `inspect_ui_image`

`inspect_ui_image` is P1 and MAY only return evidence conforming to
`ui_evidence.v1`: OCR bboxes, rectangles, repeated-region candidates, optional
DOM/AX structure, crops, observed/inferred labels, confidence, and uncertainties.

It MUST NOT return:

- `recommended_component`;
- `layout_should_be`;
- a selected navigation or presentation model;
- a production UI implementation decision;
- prose that tells the model which design to choose.

Any such field or conclusion makes the tool output non-compliant.

### 2.4 `ui_layout`

`ui_layout` validates and persists model-authored `ui_layout.v1`. It MAY render an
annotated overlay. It MUST NOT generate the semantic interpretation on behalf of
the main model.

## 3. Tool Prompt Separation

### 3.1 Tool descriptions

A tool description MUST describe only:

- what the tool does;
- accepted inputs;
- produced outputs;
- execution location;
- permissions and capability limits;
- concrete failure conditions.

A tool description MUST NOT define workflow order, such as:

- "call me first";
- "recommended for understanding UI";
- "use me before view_image";
- "then call another tool";
- "choose this tool when the user wants a good layout".

Workflow belongs to the system prompt or a loaded skill. Semantic judgment belongs
to the main model.

### 3.2 Tool outputs

Tool outputs MUST contain facts, evidence levels, confidence, uncertainty, and
structured references only.

Tool outputs MUST NOT contain model-directed design commands or hidden workflow
state. Forbidden examples include `recommended_component`, `layout_should_be`,
`use_this_view`, `next_call`, or `selected_design`.

Errors MAY identify a violated capability and an actionable remedy, but they MUST
NOT choose the user's design or silently alter the workflow.

## 4. Capability Matrix

The following capabilities are independent:

| Capability | Meaning |
| --- | --- |
| `image_input` | The selected model accepts images in ordinary user messages |
| `tool_image_input` | The provider accepts image items inside a tool result |
| `image_detail_original` | The provider/model preserves original image detail |
| `vision_bridge` | An explicitly configured auxiliary vision service exists |

Fallback behavior is normative:

| State | Required behavior |
| --- | --- |
| `image_input=false` | `view_image` fails with `image_input_unsupported`; no silent OCR substitution |
| `tool_image_input=false/unknown` | `view_image` fails with `tool_image_input_unsupported`; the model MUST NOT claim it saw the tool result |
| `image_detail_original=false/unknown` and requested `original` | Fail with `image_detail_original_unsupported`; caller may explicitly retry `high` |
| Explicit `vision_bridge` | Used only through a separately named auxiliary tool with visible provider/model identity |

`detail=high` MAY normalize or downscale within a declared budget. Effective
dimensions and detail MUST be returned. A downscaled image MUST NOT be reported as
`original`.

After a model switch, historical image references remain assets. A text-only model
receives manifests/placeholders and MUST NOT receive a claim that it visually
perceived historical images.

## 5. `view_image` Asset and Replay Contract

The executable schema is `view_image_result.v1`. Its semantics are fixed:

1. `assets[]` MUST contain exactly one source image.
2. The source asset MUST be `assets[0]` and MUST have `role = "source"`.
3. The source asset ID MUST equal `source_asset_id`.
4. A region crop is optional and MUST be `assets[1]` when present.
5. At most one crop is allowed in P0.
6. `crop_asset_id` MUST be `null` when no crop exists and MUST equal
   `assets[1].asset_id` when a crop exists.
7. A crop MUST reference `source_asset_id` and carry a normalized `source_bbox`.
8. `source_region` MUST be `null` without a crop and MUST match the crop's
   `source_bbox` with a crop.
9. Asset IDs MUST be unique within one result.
10. The crop MUST NOT replace, reorder, or hide the source asset.
11. Source and crop asset metadata MUST be persisted with `tool_finished.assets`.
12. Live and replay MUST reconstruct the parent-child relationship from structured
    `view_image_result.v1` metadata, never from filenames or prose.
13. If the complete image set cannot fit the request budget, `view_image` MUST fail
    before returning success. It MUST NOT silently drop the crop or source.

## 6. AgentKit Presentation Contract

1. AgentKit MUST display the source as the primary image asset.
2. A crop MUST appear as a child of the source, not as a replacement.
3. The generic activity semantic is `output.kind = "image_view"`; AgentKit MUST
   use structured output/assets rather than branching on the `view_image` tool name.
4. The activity title SHOULD be `Viewed image <display_name>`.
5. Expanded presentation MUST expose source image, effective dimensions, effective
   detail, original-image entry, and optional crop.
6. Multiple image activities MUST preserve `assets[]` order.
7. Live and replay MUST use the same persisted metadata and produce the same
   parent-child structure.
8. AgentKit MUST NOT infer visual evidence or design state from prose.

Detailed sizing and interaction requirements remain in
[Visual Layout Understanding Design](./visual-layout-understanding.md#124-agentkit-image-view-presentation-contract).

## 7. P0 and P1 Scope

P0 is fixed to:

- `ui-adaptation` skill;
- `view_image` contract and implementation;
- source/crop assets;
- executable schemas and fixtures;
- AgentKit-visible generic image activity.

P0 does not include:

- `inspect_ui_image`;
- repeated-region detection;
- `ui_layout` overlay rendering;
- automatic multi-device design generation;
- a dedicated UI-grounding model.

P1 ClientToolsKit/VisualGroundingKit evidence work is a separate cross-repository
follow-up. This contract does not authorize changes to DreamLog, AgentKit, or
Talkify during P0 document work.

## 8. Compliance Checklist

- [ ] Main model sees original pixels through `view_image` when visual
      understanding is required.
- [ ] No client analysis tool is a prerequisite or substitute for `view_image`.
- [ ] Tool descriptions contain no workflow ordering or design recommendation.
- [ ] Tool outputs contain no component/layout imperative fields.
- [ ] Capability checks are independent and fail closed.
- [ ] Source asset is exactly one and first.
- [ ] Crop is optional, at most one, and references the source.
- [ ] Live/replay use the same structured metadata.
- [ ] AgentKit presentation is generic and not keyed to the tool name.
- [ ] P0 scope excludes P1/P2 visual understanding features.
