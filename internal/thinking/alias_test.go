package thinking_test

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/openai"
	"github.com/tidwall/gjson"
)

func TestApplyThinkingLevelAliasBeforeValidation(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-thinking-level-alias-" + t.Name()
	modelID := "alias-level-model"
	reg.RegisterClient(clientID, "openai", []*registry.ModelInfo{{
		ID: modelID,
		Thinking: &registry.ThinkingSupport{
			Levels:  []string{"high", "max", "xhigh"},
			Aliases: map[string]string{"medium": "high"},
		},
	}})
	t.Cleanup(func() {
		reg.UnregisterClient(clientID)
	})

	out, err := thinking.ApplyThinking([]byte(`{"reasoning_effort":"medium"}`), modelID, "openai", "openai", "openai")
	if err != nil {
		t.Fatalf("ApplyThinking() error = %v", err)
	}
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "high" {
		t.Fatalf("reasoning_effort = %q, want %q, body=%s", got, "high", string(out))
	}
}

func TestApplyThinkingLevelAliasSupportsSuffix(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-thinking-level-alias-suffix-" + t.Name()
	modelID := "alias-suffix-model"
	reg.RegisterClient(clientID, "openai", []*registry.ModelInfo{{
		ID: modelID,
		Thinking: &registry.ThinkingSupport{
			Levels:  []string{"high", "max", "xhigh"},
			Aliases: map[string]string{"medium": "high"},
		},
	}})
	t.Cleanup(func() {
		reg.UnregisterClient(clientID)
	})

	out, err := thinking.ApplyThinking([]byte(`{}`), modelID+"(medium)", "openai", "openai", "openai")
	if err != nil {
		t.Fatalf("ApplyThinking() error = %v", err)
	}
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "high" {
		t.Fatalf("reasoning_effort = %q, want %q, body=%s", got, "high", string(out))
	}
}
