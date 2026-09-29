package agentrunner

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunnerProviderRetries(t *testing.T) {
	for _, provider := range DefaultProviders() {
		for _, maxAttempts := range []int{1, 2} {
			t.Run(provider.Name+"/"+strconv.Itoa(maxAttempts), func(t *testing.T) {
				t.Parallel()
				var attempts atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					var body struct {
						MaxAttempts *int `json:"max_attempts"`
					}
					if err := json.UnmarshalRead(request.Body, &body); err != nil {
						t.Error(err)
					}
					if body.MaxAttempts != nil {
						t.Error("harness retry configuration leaked into provider request")
					}
					attempts.Add(1)
					writer.WriteHeader(http.StatusServiceUnavailable)
				}))
				defer server.Close()
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				code := RunMain(ctx,
					[]string{"-workspace", t.TempDir(), "-session-directory", t.TempDir()},
					func(name string) string {
						switch name {
						case "UNREAL_HARNESS_LLM_PROVIDER":
							return provider.Name
						case "UNREAL_HARNESS_LLM_BASE_URL":
							return server.URL
						case "OPENAI_CODEX_ACCESS_TOKEN":
							return "subscription-token"
						case "OPENAI_CODEX_ACCOUNT_ID":
							return "account-1"
						case "UNREAL_HARNESS_LLM_API_KEY":
							return "test-key"
						default:
							return ""
						}
					}, func() []string { return nil },
					strings.NewReader(`{"prompt":"hello","model":"test","max_attempts":`+strconv.Itoa(maxAttempts)+`}`),
					io.Discard, io.Discard, Config{Name: "unreal-agent-runner", ParseRequest: parseTestRequest, Providers: DefaultProviders()})
				if code != 1 || attempts.Load() != int64(maxAttempts) {
					t.Fatalf("exit = %d, attempts = %d, want %d", code, attempts.Load(), maxAttempts)
				}
			})
		}
	}
}

func TestRunnerProviderDefaultModels(t *testing.T) {
	for _, provider := range DefaultProviders() {
		want := ""
		if provider.Name == "openai" {
			want = "gpt-6-astra"
		}
		if provider.Name == "opencode-zen" {
			want = "space-bunny-free"
		}
		if provider.DefaultModel != want {
			t.Errorf("%s default model = %q, want %q", provider.Name, provider.DefaultModel, want)
		}
	}
}

func TestRunnerCodexUsesSubscriptionWithoutAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer subscription-token" || r.Header.Get("ChatGPT-Account-ID") != "account" {
			t.Error("wrong authentication")
		}
		var body struct {
			Stream bool `json:"stream"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Error(err)
		}
		if !body.Stream {
			t.Errorf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[{\"id\":\"msg-1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"subscription works\"}]}]}}\n\n")
	}))
	defer server.Close()
	var output, stderr strings.Builder
	code := RunMain(t.Context(), []string{"-workspace", t.TempDir(), "-session-directory", t.TempDir()}, func(key string) string {
		return map[string]string{
			"UNREAL_HARNESS_LLM_PROVIDER": "openai-codex",
			"UNREAL_HARNESS_LLM_BASE_URL": server.URL,
			"OPENAI_CODEX_ACCESS_TOKEN":   "subscription-token",
			"OPENAI_CODEX_ACCOUNT_ID":     "account",
		}[key]
	}, func() []string { return nil }, strings.NewReader(`{"prompt":"hello","model":"gpt-test","system_prompt":"my system prompt"}`), &output, &stderr, Config{Name: "unreal-agent-runner", ParseRequest: parseTestRequest, Providers: DefaultProviders()})
	if code != 0 || !strings.Contains(output.String(), "subscription works") {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(output.String(), "subscription-token") {
		t.Fatal("credential leaked into session output")
	}
}

func TestRunnerReportsTerminalProviderFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		stream     bool
		retryAfter string
		body       string
		want       map[string]any
	}{
		{
			name: "rate limit", retryAfter: "30",
			body: `{"error":{"code":"rate_limit_exceeded","message":"Rate limit reached."}}`,
			want: map[string]any{"type": "error", "code": "rate_limit_exceeded", "retryable": true, "retry_after_seconds": 30.0, "http_status": 429.0},
		},
		{
			name: "rate limit message hint",
			body: `{"error":{"code":"rate_limit_exceeded","message":"Please try again in 1.2s."}}`,
			want: map[string]any{"type": "error", "code": "rate_limit_exceeded", "retryable": true, "retry_after_seconds": 2.0, "http_status": 429.0},
		},
		{
			name: "usage limit",
			body: `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_in_seconds":5400}}`,
			want: map[string]any{"type": "error", "code": "usage_limit_reached", "retryable": false, "retry_after_seconds": 5400.0, "http_status": 429.0},
		},
		{
			name: "bare unauthorized", status: http.StatusUnauthorized, body: "Unauthorized",
			want: map[string]any{"type": "error", "retryable": false, "http_status": 401.0},
		},
		{
			name: "streamed response failure", stream: true,
			body: "data: " + `{"type":"response.failed","response":{"id":"r","status":"failed","output":[],` +
				`"error":{"code":"rate_limit_exceeded","message":"Please try again in 3s."}}}` + "\n\n",
			want: map[string]any{"type": "error", "code": "rate_limit_exceeded", "retryable": true, "retry_after_seconds": 3.0, "http_status": 200.0},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if test.retryAfter != "" {
					writer.Header().Set("Retry-After", test.retryAfter)
				}
				if test.stream {
					writer.Header().Set("Content-Type", "text/event-stream")
				} else {
					writer.WriteHeader(cmp.Or(test.status, http.StatusTooManyRequests))
				}
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			var stdout, stderr strings.Builder
			code := RunMain(t.Context(), []string{"-workspace", t.TempDir(), "-session-directory", t.TempDir()}, func(name string) string {
				return map[string]string{
					"UNREAL_HARNESS_LLM_BASE_URL": server.URL,
					"UNREAL_HARNESS_LLM_API_KEY":  "test-key",
				}[name]
			}, func() []string { return nil }, strings.NewReader(`{"prompt":"hello","model":"test","max_attempts":1}`),
				&stdout, &stderr, Config{Name: "unreal-agent-runner", ParseRequest: parseTestRequest, Providers: DefaultProviders()})
			if code != 1 {
				t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
			}
			lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			if test.stream && !strings.Contains(lines[len(lines)-2], `"Kind":"model_response"`) {
				t.Fatalf("failed response was not persisted before the error: %s", stdout.String())
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
				t.Fatal(err)
			}
			if message, _ := got["message"].(string); !strings.Contains(message, "responses API error") && !strings.Contains(message, "responses API request failed") {
				t.Fatalf("message = %q", message)
			}
			delete(got, "message")
			if !maps.Equal(got, test.want) {
				t.Fatalf("error event = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRunnerProviderRouting(t *testing.T) {
	const routing = `{"order":["deepinfra/fp8","novita"],"quantizations":["fp8"],"allow_fallbacks":true,"sort":"price"}`
	for _, test := range []struct {
		name, provider, routing, wantErr string
	}{
		{name: "openrouter", provider: "openrouter", routing: routing},
		{name: "null is absent", provider: "openai", routing: "null"},
		{name: "other provider", provider: "openai", routing: routing, wantErr: `provider_routing is not supported by provider "openai"`},
		{name: "not an object", provider: "openrouter", routing: `["deepinfra"]`, wantErr: "provider_routing must be a JSON object"},
	} {
		t.Run(test.name, func(t *testing.T) {
			bodies := make(chan map[string]jsontext.Value, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var body map[string]jsontext.Value
				if err := json.UnmarshalRead(request.Body, &body); err != nil {
					t.Error(err)
				}
				bodies <- body
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
			}))
			defer server.Close()
			var stdout, stderr strings.Builder
			code := RunMain(t.Context(), []string{"-workspace", t.TempDir(), "-session-directory", t.TempDir()}, func(name string) string {
				return map[string]string{
					"UNREAL_HARNESS_LLM_PROVIDER": test.provider,
					"UNREAL_HARNESS_LLM_BASE_URL": server.URL,
					"UNREAL_HARNESS_LLM_API_KEY":  "test-key",
				}[name]
			}, func() []string { return nil }, strings.NewReader(`{"prompt":"hello","model":"test","provider_routing":`+test.routing+`}`),
				&stdout, &stderr, Config{Name: "unreal-agent-runner", ParseRequest: parseTestRequest, Providers: DefaultProviders()})
			if test.wantErr != "" {
				if code != 1 || !strings.Contains(stderr.String(), test.wantErr) {
					t.Fatalf("exit = %d, stderr = %q, want %q", code, stderr.String(), test.wantErr)
				}
				return
			}
			if code != 0 {
				t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
			}
			body := <-bodies
			if test.provider == "openrouter" && string(body["provider"]) != routing {
				t.Fatalf("provider = %s, want %s", body["provider"], routing)
			}
			if _, ok := body["provider"]; test.provider != "openrouter" && ok {
				t.Fatalf("provider routing sent to %s: %s", test.provider, body["provider"])
			}
		})
	}
}
