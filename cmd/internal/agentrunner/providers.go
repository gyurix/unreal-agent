package agentrunner

import (
	"encoding/json/jsontext"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm/clients/chatcompat"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/fireworks"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/ollama"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openai"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openrouter"
)

func DefaultProviders() []Provider {
	return []Provider{
		{
			Name:         "opencode-zen",
			BaseURL:      chatcompat.DefaultBaseURL,
			DefaultModel: "space-bunny-free",
			NewClient: func(_, baseURL string, maxAttempts int, getenv func(string) string, _ jsontext.Value) (Client, error) {
				return chatcompat.NewClient(chatcompat.Config{
					APIKey:      preferred(strings.TrimSpace(getenv(llmAPIKeyEnvironment)), strings.TrimSpace(getenv("OPENCODE_ZEN_API_KEY"))),
					BaseURL:     baseURL,
					MaxAttempts: &maxAttempts,
					ClientType:  strings.TrimSpace(getenv("OPENCODE_CLIENT_TYPE")),
					SessionID:   strings.TrimSpace(getenv("OPENCODE_SESSION_ID")),
					RequestID:   strings.TrimSpace(getenv("OPENCODE_REQUEST_ID")),
					ProjectID:   preferred(strings.TrimSpace(getenv("OPENCODE_PROJECT_ID")), "global"),
					UserAgent:   strings.TrimSpace(getenv("OPENCODE_USER_AGENT")),
					Stream:      true,
				})
			},
		},
		{
			Name:    "ollama",
			BaseURL: ollama.BaseURL,
			NewClient: func(_, baseURL string, maxAttempts int, _ func(string) string, _ jsontext.Value) (Client, error) {
				return ollama.NewClient(ollama.Config{BaseURL: baseURL, MaxAttempts: &maxAttempts})
			},
		},
		{
			Name:              "openai",
			BaseURL:           "https://api.openai.com/v1",
			DefaultModel:      "gpt-6-astra",
			APIKeyEnvironment: "OPENAI_API_KEY",
			NewClient: func(apiKey, baseURL string, maxAttempts int, _ func(string) string, _ jsontext.Value) (Client, error) {
				return openai.NewClient(openai.Config{APIKey: apiKey, BaseURL: baseURL, MaxAttempts: &maxAttempts})
			},
		},
		{
			Name:    "openai-codex",
			BaseURL: openaicodex.BaseURL,
			NewClient: func(_, baseURL string, maxAttempts int, getenv func(string) string, _ jsontext.Value) (Client, error) {
				config, err := openaicodex.EnvironmentConfig(getenv)
				if err != nil {
					return nil, err
				}
				config.BaseURL, config.MaxAttempts = baseURL, &maxAttempts
				return openaicodex.NewClient(config)
			},
		},

		{
			Name:              "openrouter",
			BaseURL:           "https://openrouter.ai/api/v1",
			APIKeyEnvironment: "OPENROUTER_API_KEY",
			ProviderRouting:   true,
			NewClient: func(apiKey, baseURL string, maxAttempts int, _ func(string) string, providerRouting jsontext.Value) (Client, error) {
				return openrouter.NewClient(openrouter.Config{
					APIKey: apiKey, BaseURL: baseURL, MaxAttempts: &maxAttempts, ProviderRouting: providerRouting,
				})
			},
		},
		{
			Name:              "fireworks",
			BaseURL:           "https://api.fireworks.ai/inference/v1",
			APIKeyEnvironment: "FIREWORKS_API_KEY",
			NewClient: func(apiKey, baseURL string, maxAttempts int, _ func(string) string, _ jsontext.Value) (Client, error) {
				return fireworks.NewClient(fireworks.Config{APIKey: apiKey, BaseURL: baseURL, MaxAttempts: &maxAttempts})
			},
		},
	}
}

func preferred(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
