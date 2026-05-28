package testing

import (
	"errors"
	"testing"
)

func TestNewLLMClientVisionInheritsLLMModelAndKey(t *testing.T) {
	t.Setenv("DW_BROWSER_LLM_MODEL", "google/gemma-4-26b-a4b-it")
	t.Setenv("DW_BROWSER_LLM_API_KEY", "shared-key")
	t.Setenv("DW_BROWSER_VISION_MODEL", "")
	t.Setenv("DW_BROWSER_VISION_API_KEY", "")

	client := NewLLMClient(RoleVision, "https://openrouter.ai/api/v1", "", "")
	if client.model != "google/gemma-4-26b-a4b-it" {
		t.Fatalf("vision model = %q", client.model)
	}
	if client.apiKey != "shared-key" {
		t.Fatalf("vision api key did not inherit llm key")
	}
}

func TestLLMClientOpenAIProviderIsCaseInsensitive(t *testing.T) {
	t.Setenv("DW_BROWSER_LLM_PROVIDER", "OpenAI")
	client := NewLLMClient(RoleLLM, "http://localhost:9999", "model", "")
	if !client.isOpenAI() {
		t.Fatal("OpenAI provider should force OpenAI-compatible protocol")
	}
}

func TestLLMClientOllamaProviderOverridesV1EndpointHeuristic(t *testing.T) {
	t.Setenv("DW_BROWSER_LLM_PROVIDER", "ollama")
	client := NewLLMClient(RoleLLM, "http://localhost:11434/v1", "model", "")
	if client.isOpenAI() {
		t.Fatal("ollama provider should force Ollama protocol even when endpoint contains /v1")
	}
}

func TestIsTransientLLMError(t *testing.T) {
	if !isTransientLLMError(errors.New("Post https://openrouter.ai/api/v1/chat/completions: EOF")) {
		t.Fatal("EOF should be transient")
	}
	if isTransientLLMError(errors.New("LLM returned status 401: unauthorized")) {
		t.Fatal("401 should not be transient")
	}
}
