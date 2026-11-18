package agent

import "code-agent/internal/model"

// Older runtimes attached all image entries from list_files. Match the result
// to its own assistant tool batch because providers can reuse call IDs across
// batches. Filter the request copy so persisted history stays intact.
func withoutLegacyDirectoryImages(messages []model.Message) []model.Message {
	var batch []model.ToolCall
	var out []model.Message
	for i, msg := range messages {
		switch msg.Role {
		case model.RoleAssistant:
			batch = msg.ToolCalls
		case model.RoleTool:
			if len(msg.LocalAssets) == 0 || msg.ToolCallID == "" {
				continue
			}
			var matches int
			var name string
			for _, call := range batch {
				if call.ID == msg.ToolCallID {
					matches++
					name = call.Function.Name
				}
			}
			if matches == 1 && name == "list_files" {
				if out == nil {
					out = append([]model.Message(nil), messages...)
				}
				out[i].LocalAssets = nil
			}
		default:
			batch = nil
		}
	}
	if out == nil {
		return messages
	}
	return out
}
