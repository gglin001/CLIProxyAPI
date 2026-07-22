package helps

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestPrepareOpenAIResponsesForChatCompletionsBridgesPlaintextAgentMessage(t *testing.T) {
	payload := []byte(`{
		"input":[{
			"type":"agent_message",
			"author":"/root",
			"recipient":"/root/worker",
			"content":[
				{"type":"input_text","text":"Message Type: NEW_TASK\nTask name: worker\nSender: /root\nPayload:\n"},
				{"type":"encrypted_content","encrypted_content":"inspect the request path exactly"}
			]
		}],
		"tools":[{
			"type":"function",
			"name":"spawn_agent",
			"parameters":{
				"type":"object",
				"properties":{
					"message":{"type":"string","encrypted":true},
					"encrypted":{"type":"boolean"}
				}
			}
		}]
	}`)

	out := PrepareOpenAIResponsesForChatCompletions(payload)

	if got := gjson.GetBytes(out, "input.0.type").String(); got != "message" {
		t.Fatalf("input.0.type = %q, want message; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "input.0.role").String(); got != "user" {
		t.Fatalf("input.0.role = %q, want user; body=%s", got, string(out))
	}
	content := gjson.GetBytes(out, "input.0.content").String()
	if !strings.Contains(content, "Message Type: NEW_TASK") || !strings.Contains(content, "inspect the request path exactly") {
		t.Fatalf("bridged content lost agent message data: %q", content)
	}
	if gjson.GetBytes(out, "tools.0.parameters.properties.message.encrypted").Exists() {
		t.Fatalf("Responses-only encrypted schema marker still exists: %s", string(out))
	}
	if !gjson.GetBytes(out, "tools.0.parameters.properties.encrypted").Exists() {
		t.Fatalf("schema property named encrypted was removed: %s", string(out))
	}
}

func TestPrepareOpenAIResponsesForChatCompletionsPassesAgentPayloadUnchanged(t *testing.T) {
	payloadValue := "gAAAAABopaque_payload_that_must_be_forwarded_without_interpretation_or_rewriting_1234567890"
	payload := []byte(`{
		"input":[{
			"type":"agent_message",
			"author":"/root",
			"recipient":"/root/worker",
			"content":[{"type":"encrypted_content","encrypted_content":"` + payloadValue + `"}]
		}]
	}`)

	out := PrepareOpenAIResponsesForChatCompletions(payload)
	content := gjson.GetBytes(out, "input.0.content").String()
	if !strings.HasSuffix(content, "Payload:\n"+payloadValue) {
		t.Fatalf("agent payload was rewritten: %q", content)
	}
	if !strings.Contains(content, "Sender: /root") || !strings.Contains(content, "Recipient: /root/worker") {
		t.Fatalf("agent routing metadata missing: %q", content)
	}
}
