# Agent Wire local assets

`agent_input` with `kind = "text"` may carry files already staged in the
conversation workspace:

```json
{
  "type": "agent_input",
  "kind": "text",
  "request_id": "stable-client-id",
  "text": "",
  "local_assets": [
    {
      "id": "attachment-id",
      "relative_path": "user-assets/attachment-id/report.pdf",
      "filename": "report.pdf",
      "mime_type": "application/pdf",
      "kind": "pdf",
      "size_bytes": 1234,
      "sha256": "64-lowercase-hex-characters",
      "transfer_policy": "local_only"
    }
  ]
}
```

`request_id` is required when either `assets` or `local_assets` is present.
Text may be empty when at least one attachment is present. `local_assets` does
not require the `image_input` capability.

The Runtime accepts only workspace-relative, forward-slash paths without empty,
`.` or `..` segments. Absolute paths, URLs, backslashes, control characters,
duplicate IDs/paths, non-`local_only` policies, and fields outside the schema
are rejected. The client tool that opens the file remains responsible for
resolving the path against the current workspace and checking containment and
symlinks.

Gateway-owned `assets` and `local_assets` are separate delivery modes. Once a
file has been explicitly uploaded, send its Gateway reference in `assets` and
omit the corresponding local reference. A matching SHA-256 in both collections
is rejected. For a mixed submission containing different Gateway and local
files, every Gateway reference must include SHA-256; otherwise the Runtime
cannot prove the collections are disjoint and rejects the submission.

Local attachment metadata is persisted in the durable turn inbox and session
message history and is replayed on `turn_started` as `local_assets`. It is never
serialized into a Provider request. The model receives a text-only manifest
containing relative path, filename, kind, MIME type, and size, plus instructions
not to infer file contents and to call a client-side local analysis tool.

Configuring a Gateway uploader does not authorize screenshot or camera output
to be uploaded. Capture-result auto-upload is disabled unless a host supplies a
separate explicit authorization.
