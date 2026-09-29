package chatcompat

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
)

func TestRequiresBaseURL(t *testing.T) {
	if _, err := NewClient(Config{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestKeylessClientSendsPublicAuthHeader(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		_, _ = writer.Write([]byte(`{"id":"r1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Respond(t.Context(), llm.Request{
		Model: llm.Model{ID: "m"},
		Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "hi"}}},
	}, llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer public" {
		t.Fatalf("authorization = %q", authorization)
	}
	if response.Stop != llm.StopComplete || response.Usage.InputTokens != 1 || response.Usage.OutputTokens != 2 {
		t.Fatalf("response = %#v", response)
	}
}

func TestBearerHeaderAndMaxTokens(t *testing.T) {
	var authorization string
	var fields map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		_ = json.UnmarshalRead(request.Body, &fields)
		_, _ = writer.Write([]byte(`{"id":"r1","choices":[{"finish_reason":"length","message":{"role":"assistant","content":"cut"}}]}`))
	}))
	defer server.Close()
	maxTokens := int64(9)
	client, err := NewClient(Config{APIKey: "secret", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Respond(t.Context(), llm.Request{
		Model: llm.Model{ID: "m", MaxOutputTokens: &maxTokens},
		Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "hi"}}},
	}, llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer secret" {
		t.Fatalf("authorization = %q", authorization)
	}
	if fields["max_tokens"] != float64(9) {
		t.Fatalf("max_tokens = %v", fields)
	}
	if response.Stop != llm.StopMaxOutputTokens {
		t.Fatalf("stop = %q", response.Stop)
	}
}

func TestToolCallDecodingAndHistoryMerge(t *testing.T) {
	var messages []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var decoded struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.UnmarshalRead(request.Body, &decoded)
		messages = decoded.Messages
		_, _ = writer.Write([]byte(`{"id":"r1","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"calc","arguments":"{\"expr\":\"2+2\"}"}}]}}]}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Respond(t.Context(), llm.Request{
		Model: llm.Model{ID: "m"},
		Input: []llm.Item{
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: "sys"}},
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "q"}},
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "thinking"}},
			{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "c0", Name: "calc", Arguments: "{}"}},
			{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "c0", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "4"}}}},
			{Type: llm.ItemReasoning, Data: llm.Reasoning{Summary: []string{"s"}}},
		},
		Tools: []llm.Tool{{Type: llm.ToolFunction, Name: "calc"}},
	}, llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages = %v", messages)
	}
	assistant, ok := messages[2]["tool_calls"].([]any)
	if !ok || len(assistant) != 1 {
		t.Fatalf("assistant merge = %v", messages[2])
	}
	if messages[3]["role"] != "tool" || messages[3]["tool_call_id"] != "c0" {
		t.Fatalf("tool message = %v", messages[3])
	}
	if len(response.Output) != 1 || response.Output[0].Type != llm.ItemToolCall {
		t.Fatalf("response = %#v", response)
	}
}

func TestUpstreamErrorShapes(t *testing.T) {
	for _, body := range []string{
		`{"type":"error","error":{"type":"FreeTierError","message":"only in OpenCode"}}`,
		`{"error":{"type":"server_error","message":"unavailable"}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusBadGateway)
			_, _ = writer.Write([]byte(body))
		}))
		client, err := NewClient(Config{BaseURL: server.URL, MaxAttempts: new(1)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Respond(t.Context(), llm.Request{
			Model: llm.Model{ID: "m"},
			Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "hi"}}},
		}, llm.RequestOptions{})
		server.Close()
		var apiErr *responsesapi.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway {
			t.Fatalf("body %s: err = %v", body, err)
		}
	}
}

func TestRateLimitRetryAfter(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			writer.Header().Set("Retry-After", "0")
			writer.WriteHeader(http.StatusTooManyRequests)
			_, _ = writer.Write([]byte(`{"error":{"message":"slow","code":"rate_limit"}}`))
			return
		}
		_, _ = writer.Write([]byte(`{"id":"r","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, MaxAttempts: new(2)})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Respond(t.Context(), llm.Request{
		Model: llm.Model{ID: "m"},
		Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "hi"}}},
	}, llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || len(response.Output) != 1 {
		t.Fatalf("attempts=%d response=%#v", attempts, response)
	}
}

func TestCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.Respond(ctx, llm.Request{
		Model: llm.Model{ID: "m"},
		Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "hi"}}},
	}, llm.RequestOptions{})
	if err == nil || !strings.Contains(err.Error(), "canceled") && !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenCodeHeadersExplicit(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		got = request.Header.Clone()
		_, _ = writer.Write([]byte(`{"id":"r1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{
		BaseURL:    server.URL,
		ClientType: "desktop",
		SessionID:  "ses_AAAAAAAAAAAAAAAAAAAAAAAAAA",
		RequestID:  "msg_BBBBBBBBBBBBBBBBBBBBBBBBBB",
		ProjectID:  "global",
		UserAgent:  "opencode/9.9.9",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Respond(t.Context(), llm.Request{
		Model: llm.Model{ID: "m"},
		Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "hi"}}},
	}, llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("User-Agent") != "opencode/9.9.9" {
		t.Fatalf("User-Agent = %q", got.Get("User-Agent"))
	}
	if got.Get("x-opencode-client") != "desktop" {
		t.Fatalf("x-opencode-client = %q", got.Get("x-opencode-client"))
	}
	if got.Get("x-opencode-session") != "ses_AAAAAAAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("x-opencode-session = %q", got.Get("x-opencode-session"))
	}
	if got.Get("x-opencode-request") != "msg_BBBBBBBBBBBBBBBBBBBBBBBBBB" {
		t.Fatalf("x-opencode-request = %q", got.Get("x-opencode-request"))
	}
	if got.Get("x-opencode-project") != "global" {
		t.Fatalf("x-opencode-project = %q", got.Get("x-opencode-project"))
	}
}

func TestOpenCodeHeadersDefault(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		got = request.Header.Clone()
		_, _ = writer.Write([]byte(`{"id":"r1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Respond(t.Context(), llm.Request{
		Model: llm.Model{ID: "m"},
		Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "hi"}}},
	}, llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("User-Agent") != DefaultUserAgent {
		t.Fatalf("User-Agent = %q", got.Get("User-Agent"))
	}
	if got.Get("x-opencode-client") != "cli" {
		t.Fatalf("x-opencode-client = %q", got.Get("x-opencode-client"))
	}
	checkTimestampID(t, "x-opencode-session", got.Get("x-opencode-session"), "ses_", true)
	checkTimestampID(t, "x-opencode-request", got.Get("x-opencode-request"), "msg_", false)
	if got.Get("x-opencode-project") != "" {
		t.Fatalf("x-opencode-project = %q, want empty", got.Get("x-opencode-project"))
	}
}

// checkTimestampID enforces the identifier.ts layout (timestamp-bearing
// 12-hex prefix) and requires the embedded timestamp to be recent: the
// Zen free-tier gate rejects fully random IDs.
func checkTimestampID(t *testing.T, header, value, prefix string, descending bool) {
	t.Helper()
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+26 {
		t.Fatalf("%s = %q", header, value)
	}
	core := value[len(prefix) : len(prefix)+12]
	var digits string
	for _, c := range core {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("%s time part = %q, want lowercase hex", header, core)
		}
		digits += string(c)
	}
	var low48 uint64
	for _, c := range digits {
		low48 <<= 4
		switch {
		case c >= '0' && c <= '9':
			low48 |= uint64(c - '0')
		default:
			low48 |= uint64(c-'a') + 10
		}
	}
	const mask = uint64(0xFFFFFFFFFFFF)
	if descending {
		low48 ^= mask
	}
	embedded := int64(low48 >> 12)
	now := time.Now().UnixMilli() & int64(0xFFFFFFFFF)
	delta := now - embedded
	if delta < 0 {
		delta = -delta
	}
	if delta > 5*60*1000 {
		t.Fatalf("%s timestamp drift = %dms", header, delta)
	}
}

func TestRequestIDFreshPerCall(t *testing.T) {
	var sessions, requests []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		sessions = append(sessions, request.Header.Get("x-opencode-session"))
		requests = append(requests, request.Header.Get("x-opencode-request"))
		mu.Unlock()
		_, _ = writer.Write([]byte(`{"id":"r1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	input := llm.Request{
		Model: llm.Model{ID: "m"},
		Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "hi"}}},
	}
	if _, err := client.Respond(t.Context(), input, llm.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Respond(t.Context(), input, llm.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || len(requests) != 2 {
		t.Fatalf("calls = %d", len(requests))
	}
	if sessions[0] == "" || sessions[0] != sessions[1] {
		t.Fatalf("session not stable: %q", sessions)
	}
	if requests[0] == "" || requests[0] == requests[1] {
		t.Fatalf("request not fresh per call: %q", requests)
	}
	checkTimestampID(t, "x-opencode-request#1", requests[0], "msg_", false)
	checkTimestampID(t, "x-opencode-request#2", requests[1], "msg_", false)
}

func new(value int) *int { return &value }
