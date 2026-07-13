package agent

import "code-agent/internal/model"

// referenceProtocolInstruction is host-neutral guidance for the Reference
// Ledger Protocol. It teaches the model how to consume handles without naming
// any particular MCP server, tool, or opaque identifier format.
const referenceProtocolInstruction = `

# Tool reference handles

Some tool results contain values such as $ref:ref_0001. These are opaque,
session-local handles. Copy a handle exactly when passing a referenced value to
another tool. Never derive or guess an opaque identifier from a generation,
counter, window label, AX text, or any other displayed value. If a tool returns
a reference error, use one of its listed available handles; if the required kind
is unavailable, obtain it from an appropriate earlier tool result before retrying.
`

// withReferenceProtocol returns a copy with the protocol guidance applied to
// the system message. It is deliberately ephemeral so existing persisted
// sessions receive the rule after a Runtime upgrade, while transcripts remain
// free of runtime-specific boilerplate.
func withReferenceProtocol(msgs []model.Message) []model.Message {
	if len(msgs) == 0 || msgs[0].Role != model.RoleSystem {
		return msgs
	}
	out := make([]model.Message, len(msgs))
	copy(out, msgs)
	out[0].Content += referenceProtocolInstruction
	return out
}
