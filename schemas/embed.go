// Package schemas embeds the frozen executable JSON contracts. The JSON files
// remain the single source of truth; embedding does not alter their contents.
package schemas

import _ "embed"

//go:embed ui_layout.v1.schema.json
var UILayoutV1 []byte

//go:embed ui_layout_result.v1.schema.json
var UILayoutResultV1 []byte

//go:embed ui_evidence.v1.schema.json
var UIEvidenceV1 []byte

//go:embed ui_preview_lineage.v1.schema.json
var UIPreviewLineageV1 []byte

//go:embed ui_preview_result.v1.schema.json
var UIPreviewResultV1 []byte
