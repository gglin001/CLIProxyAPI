package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorBridgesMultiAgentV2ToChatCompletions(t *testing.T) {
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var errRead error
		upstreamBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read upstream request: %v", errRead)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl_multi_agent",
			"object":"chat.completion",
			"created":1774170000,
			"model":"deepseek-v4-pro",
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"content":null,
					"reasoning_content":"Delegate the focused inspection.",
					"tool_calls":[{
						"id":"call_spawn",
						"type":"function",
						"function":{"name":"collaboration__spawn_agent","arguments":"{\"task_name\":\"worker\",\"message\":\"inspect the exact request path\"}"}
					}]
				},
				"finish_reason":"tool_calls"
			}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
		}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"reasoning":{"effort":"high"},
		"input":[{
			"type":"agent_message",
			"author":"/root",
			"recipient":"/root/worker",
			"content":[
				{"type":"input_text","text":"Message Type: NEW_TASK\nTask name: worker\nSender: /root\nPayload:\n"},
				{"type":"encrypted_content","encrypted_content":"inspect multi_agent_v2 encryption handling"}
			]
		}],
		"tools":[{
			"type":"namespace",
			"name":"collaboration",
			"tools":[{
				"type":"function",
				"name":"spawn_agent",
				"parameters":{
					"type":"object",
					"properties":{
						"task_name":{"type":"string"},
						"message":{"type":"string","encrypted":true}
					},
					"required":["task_name","message"]
				}
			}]
		}],
		"tool_choice":"auto"
	}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-pro",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
		Stream:          false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if got := gjson.GetBytes(upstreamBody, "messages.0.role").String(); got != "user" {
		t.Fatalf("upstream agent message role = %q, want user; body=%s", got, string(upstreamBody))
	}
	upstreamContent := gjson.GetBytes(upstreamBody, "messages.0.content").String()
	if !strings.Contains(upstreamContent, "Message Type: NEW_TASK") || !strings.Contains(upstreamContent, "inspect multi_agent_v2 encryption handling") {
		t.Fatalf("upstream agent message lost data: %q", upstreamContent)
	}
	if got := gjson.GetBytes(upstreamBody, "tools.0.function.name").String(); got != "collaboration__spawn_agent" {
		t.Fatalf("upstream function name = %q, want collaboration__spawn_agent; body=%s", got, string(upstreamBody))
	}
	if gjson.GetBytes(upstreamBody, "tools.0.function.parameters.properties.message.encrypted").Exists() {
		t.Fatalf("Responses-only encrypted schema marker reached Chat Completions: %s", string(upstreamBody))
	}

	reasoning := gjson.GetBytes(resp.Payload, "output.#(type==\"reasoning\")")
	if got := reasoning.Get("summary.0.text").String(); got != "Delegate the focused inspection." {
		t.Fatalf("reasoning summary = %q, want upstream reasoning_content; response=%s", got, string(resp.Payload))
	}
	if reasoning.Get("encrypted_content").Exists() {
		t.Fatalf("response fabricated encrypted_content: %s", string(resp.Payload))
	}
	functionCall := gjson.GetBytes(resp.Payload, "output.#(type==\"function_call\")")
	if got := functionCall.Get("name").String(); got != "spawn_agent" {
		t.Fatalf("function call name = %q, want spawn_agent; response=%s", got, string(resp.Payload))
	}
	if got := functionCall.Get("namespace").String(); got != "collaboration" {
		t.Fatalf("function call namespace = %q, want collaboration; response=%s", got, string(resp.Payload))
	}
	if got := gjson.Get(functionCall.Get("arguments").String(), "message").String(); got != "inspect the exact request path" {
		t.Fatalf("spawn_agent message = %q, want exact plaintext; response=%s", got, string(resp.Payload))
	}
}

func TestOpenAICompatExecutorStreamDoesNotFabricateReasoningEncryption(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_stream\",\"object\":\"chat.completion.chunk\",\"created\":1774170000,\"model\":\"deepseek-v4-pro\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"Delegate first.\"},\"finish_reason\":null}]}\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_stream\",\"object\":\"chat.completion.chunk\",\"created\":1774170000,\"model\":\"deepseek-v4-pro\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_spawn\",\"type\":\"function\",\"function\":{\"name\":\"collaboration__spawn_agent\",\"arguments\":\"{\\\"task_name\\\":\\\"worker\\\",\\\"message\\\":\\\"inspect it\\\"}\"}}]},\"finish_reason\":null}]}\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_stream\",\"object\":\"chat.completion.chunk\",\"created\":1774170000,\"model\":\"deepseek-v4-pro\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"input":[{"type":"message","role":"user","content":"delegate"}],
		"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object"}}]}]
	}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-pro",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
		Stream:          true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamed strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		streamed.Write(chunk.Payload)
	}
	output := streamed.String()
	if strings.Contains(output, `"encrypted_content"`) {
		t.Fatalf("stream fabricated encrypted_content: %s", output)
	}
	if !strings.Contains(output, `"type":"summary_text","text":"Delegate first."`) {
		t.Fatalf("stream lost reasoning summary: %s", output)
	}
	if !strings.Contains(output, `"name":"spawn_agent"`) || !strings.Contains(output, `"namespace":"collaboration"`) || !strings.Contains(output, `inspect it`) {
		t.Fatalf("stream lost spawn_agent function call: %s", output)
	}
}
