package config

import "testing"

func TestParseConfigBytesNormalizesOpenAICompatibilityThinkingAliases(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
openai-compatibility:
  - name: mirror
    base-url: https://vastapi.vastaitech.com/v1
    models:
      - name: deepseek-v4-pro
        alias: gpt-5.4
        thinking:
          levels: ["high", "max", "xhigh"]
          aliases:
            MEDIUM: HIGH
            invalid: high
            low: not-a-level
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if got := len(cfg.OpenAICompatibility); got != 1 {
		t.Fatalf("openai compatibility count = %d, want 1", got)
	}
	model := cfg.OpenAICompatibility[0].Models[0]
	if model.Thinking == nil {
		t.Fatal("model thinking should be set")
	}
	if got := model.Thinking.Aliases["medium"]; got != "high" {
		t.Fatalf("medium alias = %q, want high", got)
	}
	if _, ok := model.Thinking.Aliases["invalid"]; ok {
		t.Fatal("invalid alias key should be dropped")
	}
	if _, ok := model.Thinking.Aliases["low"]; ok {
		t.Fatal("invalid alias value should be dropped")
	}
}
