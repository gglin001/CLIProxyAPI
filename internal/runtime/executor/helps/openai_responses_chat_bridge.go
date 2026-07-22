package helps

import (
	"bytes"
	"encoding/json"
	"strings"
)

// PrepareOpenAIResponsesForChatCompletions adapts Responses-only input shapes
// before the regular Responses-to-Chat Completions translator runs.
func PrepareOpenAIResponsesForChatCompletions(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}

	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return payload
	}

	stripEncryptedSchemaMarkers(root["tools"])
	if input, ok := root["input"].([]any); ok {
		for i, rawItem := range input {
			item, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			switch stringValue(item["type"]) {
			case "agent_message":
				input[i] = bridgeAgentMessage(item)
			case "additional_tools":
				stripEncryptedSchemaMarkers(item["tools"])
			}
		}
	}

	out, err := json.Marshal(root)
	if err != nil {
		return payload
	}
	return out
}

func bridgeAgentMessage(item map[string]any) map[string]any {
	var content strings.Builder
	hasInputText := false
	if parts, ok := item["content"].([]any); ok {
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				appendAgentMessagePart(&content, jsonValue(rawPart))
				continue
			}
			switch stringValue(part["type"]) {
			case "input_text":
				hasInputText = true
				appendAgentMessagePart(&content, stringValue(part["text"]))
			case "encrypted_content":
				// The proxy treats this as an opaque transport field. Codex decides
				// whether the value is encrypted; the Chat request receives it unchanged.
				appendAgentMessagePart(&content, stringValue(part["encrypted_content"]))
			default:
				appendAgentMessagePart(&content, jsonValue(part))
			}
		}
	}

	contentText := content.String()
	if !hasInputText {
		var header strings.Builder
		header.WriteString("Message Type: AGENT_MESSAGE\n")
		if author := stringValue(item["author"]); author != "" {
			header.WriteString("Sender: ")
			header.WriteString(author)
			header.WriteByte('\n')
		}
		if recipient := stringValue(item["recipient"]); recipient != "" {
			header.WriteString("Recipient: ")
			header.WriteString(recipient)
			header.WriteByte('\n')
		}
		header.WriteString("Payload:\n")
		if contentText != "" {
			header.WriteString(contentText)
		}
		contentText = header.String()
	}

	return map[string]any{
		"type":    "message",
		"role":    "user",
		"content": contentText,
	}
}

func appendAgentMessagePart(out *strings.Builder, value string) {
	if value == "" {
		return
	}
	if out.Len() > 0 {
		current := out.String()
		if !strings.HasSuffix(current, "\n") && !strings.HasPrefix(value, "\n") {
			out.WriteByte('\n')
		}
	}
	out.WriteString(value)
}

func stripEncryptedSchemaMarkers(value any) {
	switch current := value.(type) {
	case []any:
		for _, item := range current {
			stripEncryptedSchemaMarkers(item)
		}
	case map[string]any:
		if _, ok := current["encrypted"].(bool); ok {
			delete(current, "encrypted")
		}
		for _, child := range current {
			stripEncryptedSchemaMarkers(child)
		}
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func jsonValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}
