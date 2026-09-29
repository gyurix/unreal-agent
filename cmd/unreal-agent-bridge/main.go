// Command unreal-agent-bridge serves an OpenAI-compatible Chat Completions
// endpoint backed by one configured harness LLM provider.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/bridge"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	config, err := loadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unreal-agent-bridge: %v\n", err)
		return 1
	}
	client, err := config.provider.NewClient(config.apiKey, config.baseURL, config.maxAttempts, os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unreal-agent-bridge: create %s client: %v\n", config.provider.Name, err)
		return 1
	}
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "unreal-agent-bridge: close client: %v\n", err)
		}
	}()
	handler, err := bridge.NewHandler(bridge.Config{
		Adapter:      client,
		Models:       config.models,
		APIKey:       config.bridgeAPIKey,
		DefaultModel: config.defaultModel,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "unreal-agent-bridge: %v\n", err)
		return 1
	}
	server := &http.Server{
		Addr:              config.addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	fmt.Fprintf(os.Stderr, "unreal-agent-bridge: provider=%s models=%s addr=%s\n", config.provider.Name, strings.Join(config.models, ","), config.addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "unreal-agent-bridge: serve: %v\n", err)
		return 1
	}
	return 0
}

type serverConfig struct {
	provider     agentrunner.Provider
	apiKey       string
	baseURL      string
	maxAttempts  int
	models       []string
	defaultModel string
	addr         string
	bridgeAPIKey string
}

func loadConfig(getenv func(string) string) (serverConfig, error) {
	providers := bridgeProviders()
	name := strings.TrimSpace(getenv("UNREAL_HARNESS_LLM_PROVIDER"))
	if name == "" {
		name = "openai"
	}
	var selected *agentrunner.Provider
	for index := range providers {
		if providers[index].Name == name {
			selected = &providers[index]
			break
		}
	}
	if selected == nil {
		names := make([]string, 0, len(providers))
		for _, provider := range providers {
			names = append(names, provider.Name)
		}
		return serverConfig{}, fmt.Errorf("unsupported provider %q; available providers: %s", name, strings.Join(names, ", "))
	}
	baseURL := strings.TrimSpace(getenv("UNREAL_HARNESS_LLM_BASE_URL"))
	if baseURL == "" {
		baseURL = selected.BaseURL
	}
	var apiKey string
	if selected.APIKeyEnvironment != "" {
		apiKey = getenv("UNREAL_HARNESS_LLM_API_KEY")
		if strings.TrimSpace(apiKey) == "" {
			apiKey = getenv(selected.APIKeyEnvironment)
		}
		if strings.TrimSpace(apiKey) == "" {
			return serverConfig{}, fmt.Errorf("%s or %s must be set", "UNREAL_HARNESS_LLM_API_KEY", selected.APIKeyEnvironment)
		}
	}
	maxAttempts := responsesapi.DefaultMaxAttempts
	if value := strings.TrimSpace(getenv("UNREAL_HARNESS_LLM_MAX_ATTEMPTS")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return serverConfig{}, fmt.Errorf("parse UNREAL_HARNESS_LLM_MAX_ATTEMPTS: %w", err)
		}
		maxAttempts = parsed
	}
	if maxAttempts <= 0 {
		return serverConfig{}, errors.New("max attempts must be positive")
	}
	models := splitList(getenv("BRIDGE_MODELS"))
	if len(models) == 0 {
		models = splitList(getenv("UNREAL_HARNESS_LLM_MODEL"))
	}
	if len(models) == 0 && selected.DefaultModel != "" {
		models = []string{selected.DefaultModel}
	}
	if len(models) == 0 {
		return serverConfig{}, errors.New("BRIDGE_MODELS or UNREAL_HARNESS_LLM_MODEL must list at least one model")
	}
	return serverConfig{
		provider:     *selected,
		apiKey:       apiKey,
		baseURL:      baseURL,
		maxAttempts:  maxAttempts,
		models:       models,
		defaultModel: models[0],
		addr:         firstNonEmpty(getenv("BRIDGE_ADDR"), "127.0.0.1:8080"),
		bridgeAPIKey: strings.TrimSpace(getenv("BRIDGE_API_KEY")),
	}, nil
}

func bridgeProviders() []agentrunner.Provider {
	return agentrunner.DefaultProviders()
}

func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
