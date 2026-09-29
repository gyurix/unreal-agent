package main

import (
	"testing"
)

func getenvFunc(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func TestLoadConfigZenKeylessDefaults(t *testing.T) {
	config, err := loadConfig(getenvFunc(map[string]string{
		"UNREAL_HARNESS_LLM_PROVIDER": "opencode-zen",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if config.provider.Name != "opencode-zen" {
		t.Fatalf("provider = %q", config.provider.Name)
	}
	if config.baseURL != "https://opencode.ai/zen/v1" {
		t.Fatalf("baseURL = %q", config.baseURL)
	}
	if config.apiKey != "" {
		t.Fatalf("apiKey must default to empty for keyless free tier, got %q", config.apiKey)
	}
	if len(config.models) != 1 || config.models[0] != "space-bunny-free" || config.defaultModel != "space-bunny-free" {
		t.Fatalf("models = %v default = %q", config.models, config.defaultModel)
	}
	if config.addr != "127.0.0.1:8080" {
		t.Fatalf("addr = %q", config.addr)
	}
}

func TestLoadConfigZenOptionalKey(t *testing.T) {
	config, err := loadConfig(getenvFunc(map[string]string{
		"UNREAL_HARNESS_LLM_PROVIDER": "opencode-zen",
		"OPENCODE_ZEN_API_KEY":        "oc_sk_test",
		"BRIDGE_MODELS":               "space-bunny-free, glm-5",
		"BRIDGE_ADDR":                 "127.0.0.1:9090",
		"BRIDGE_API_KEY":              "bridge-secret",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// The provider entry leaves APIKeyEnvironment empty so keyless works;
	// loadConfig itself never requires a key for opencode-zen.
	if config.apiKey != "" {
		t.Fatalf("apiKey = %q", config.apiKey)
	}
	if len(config.models) != 2 || config.defaultModel != "space-bunny-free" {
		t.Fatalf("models = %v", config.models)
	}
	if config.addr != "127.0.0.1:9090" || config.bridgeAPIKey != "bridge-secret" {
		t.Fatalf("addr = %q bridgeAPIKey = %q", config.addr, config.bridgeAPIKey)
	}
	client, err := config.provider.NewClient(config.apiKey, config.baseURL, config.maxAttempts, getenvFunc(map[string]string{
		"OPENCODE_ZEN_API_KEY": "oc_sk_test",
	}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
}

func TestLoadConfigZenRequiresNoKey(t *testing.T) {
	// Keyless must not error even though other providers demand credentials.
	if _, err := loadConfig(getenvFunc(map[string]string{
		"UNREAL_HARNESS_LLM_PROVIDER": "opencode-zen",
		"BRIDGE_MODELS":               "space-bunny-free",
	})); err != nil {
		t.Fatalf("keyless zen must load without credentials: %v", err)
	}
}

func TestLoadConfigRejectsUnknownProvider(t *testing.T) {
	if _, err := loadConfig(getenvFunc(map[string]string{
		"UNREAL_HARNESS_LLM_PROVIDER": "nope",
	})); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestLoadConfigOpenRouterStillRequiresKey(t *testing.T) {
	if _, err := loadConfig(getenvFunc(map[string]string{
		"UNREAL_HARNESS_LLM_PROVIDER": "openrouter",
		"BRIDGE_MODELS":               "x/y:free",
	})); err == nil {
		t.Fatal("expected credential error for keyless openrouter")
	}
}
