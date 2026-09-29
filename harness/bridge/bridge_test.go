package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
)

type fakeAdapter struct {
	respond func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error)
	last    llm.Request
}

func (fake *fakeAdapter) Respond(ctx context.Context, request llm.Request, options llm.RequestOptions) (llm.Response, error) {
	fake.last = request
	return fake.respond(ctx, request, options)
}

func testHandler(fake *fakeAdapter) *Handler {
	handler, err := NewHandler(Config{Adapter: fake, Models: []string{"test-model"}, DefaultModel: "test-model"})
	if err != nil {
		panic(err)
	}
	return handler
}

func doRequest(t *testing.T, handler http.Handler, method, path, body, auth string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if auth != "" {
		request.Header.Set("Authorization", "Bearer "+auth)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestHealthz(t *testing.T) {
	fake := &fakeAdapter{respond: func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{}, nil
	}}
	recorder := doRequest(t, testHandler(fake), http.MethodGet, "/healthz", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d", recorder.Code)
	}
}

func TestModels(t *testing.T) {
	fake := &fakeAdapter{respond: func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{}, nil
	}}
	recorder := doRequest(t, testHandler(fake), http.MethodGet, "/v1/models", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("models status = %d", recorder.Code)
	}
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Data) != 1 || decoded.Data[0].ID != "test-model" {
		t.Fatalf("unexpected models: %v", decoded)
	}
}

func TestNonStreamingCompletion(t *testing.T) {
	fake := &fakeAdapter{respond: func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{
			ID:     "resp-1",
			Stop:   llm.StopComplete,
			Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "hello"}}},
			Usage:  llm.Usage{InputTokens: 3, OutputTokens: 5},
		}, nil
	}}
	body := `{"model":"test-model","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`
	recorder := doRequest(t, testHandler(fake), http.MethodPost, "/v1/chat/completions", body, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", recorder.Code, recorder.Body.String())
	}
	var decoded completionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Choices[0].Message.Content != "hello" || decoded.Choices[0].FinishReason != "stop" {
		t.Fatalf("unexpected completion: %+v", decoded)
	}
	if decoded.Usage.TotalTokens != 8 {
		t.Fatalf("unexpected usage: %+v", decoded.Usage)
	}
	if len(fake.last.Input) != 2 {
		t.Fatalf("expected 2 input items, got %d", len(fake.last.Input))
	}
}

func TestToolCallRoundTrip(t *testing.T) {
	fake := &fakeAdapter{respond: func(ctx context.Context, request llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		var sawResult bool
		for _, item := range request.Input {
			if item.Type == llm.ItemToolResult {
				sawResult = true
			}
		}
		if sawResult {
			return llm.Response{Stop: llm.StopComplete, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "done"}}}}, nil
		}
		return llm.Response{Stop: llm.StopComplete, Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call-1", Name: "lookup", Arguments: `{"q":"x"}`}}}}, nil
	}}
	handler := testHandler(fake)
	first := doRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{"model":"test-model","messages":[{"role":"user","content":"go"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}] }`, "")
	var decoded completionResponse
	if err := json.Unmarshal(first.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Choices[0].FinishReason != "tool_calls" || len(decoded.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected tool call, got %+v", decoded)
	}
	call := decoded.Choices[0].Message.ToolCalls[0]
	if call.ID != "call-1" || call.Function.Arguments != `{"q":"x"}` {
		t.Fatalf("bad tool call translation: %+v", call)
	}
	secondBody := `{"model":"test-model","messages":[{"role":"user","content":"go"},{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},{"role":"tool","tool_call_id":"call-1","content":"42"}]}`
	second := doRequest(t, handler, http.MethodPost, "/v1/chat/completions", secondBody, "")
	var decodedSecond completionResponse
	if err := json.Unmarshal(second.Body.Bytes(), &decodedSecond); err != nil {
		t.Fatal(err)
	}
	if decodedSecond.Choices[0].Message.Content != "done" {
		t.Fatalf("expected tool result to resolve, got %+v", decodedSecond)
	}
}

func TestStreaming(t *testing.T) {
	fake := &fakeAdapter{respond: func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{ID: "resp-s", Stop: llm.StopComplete,
			Output: []llm.Item{
				{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "hi there"}},
				{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "c1", Name: "f", Arguments: "{}"}},
			},
			Usage: llm.Usage{InputTokens: 1, OutputTokens: 2}}, nil
	}}
	recorder := doRequest(t, testHandler(fake), http.MethodPost, "/v1/chat/completions", `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("bad content type %q", contentType)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream missing terminator: %s", body)
	}
	scanner := bufio.NewScanner(strings.NewReader(body))
	var sawRole, sawContent, sawTool, sawFinish bool
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || strings.TrimSpace(line) == "data: [DONE]" {
			continue
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatal(err)
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Role == "assistant" {
				sawRole = true
			}
			if choice.Delta.Content != "" {
				sawContent = true
			}
			if len(choice.Delta.ToolCalls) != 0 {
				sawTool = true
				if choice.Delta.ToolCalls[0].ID != "c1" {
					t.Fatalf("bad streamed tool id: %+v", choice.Delta.ToolCalls[0])
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason == "tool_calls" {
				sawFinish = true
			}
		}
	}
	if !sawRole || !sawContent || !sawTool || !sawFinish {
		t.Fatalf("incomplete stream role=%v content=%v tool=%v finish=%v", sawRole, sawContent, sawTool, sawFinish)
	}
}

func TestAuth(t *testing.T) {
	fake := &fakeAdapter{respond: func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{}, nil
	}}
	handler, _ := NewHandler(Config{Adapter: fake, Models: []string{"m"}, APIKey: "secret"})
	recorder := doRequest(t, handler, http.MethodGet, "/v1/models", "", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodGet, "/v1/models", "", "secret")
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
}

func TestErrorsAndValidation(t *testing.T) {
	fake := &fakeAdapter{respond: func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{}, nil
	}}
	handler := testHandler(fake)
	for _, test := range []struct {
		name   string
		body   string
		status int
	}{
		{"empty messages", `{"model":"test-model","messages":[]}`, http.StatusBadRequest},
		{"unknown model", `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`, http.StatusNotFound},
		{"bad role", `{"model":"test-model","messages":[{"role":"alien","content":"hi"}]}`, http.StatusBadRequest},
		{"image part", `{"model":"test-model","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x"}}]}]}`, http.StatusBadRequest},
		{"unsupported tool", `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"code_interpreter"}]}`, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := doRequest(t, handler, http.MethodPost, "/v1/chat/completions", test.body, "")
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, test.status, recorder.Body.String())
			}
			var envelope errorEnvelope
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Message == "" {
				t.Fatal("empty error message")
			}
		})
	}
}

func TestUpstreamRateLimit(t *testing.T) {
	fake := &fakeAdapter{respond: func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		err := &responsesapi.APIError{StatusCode: 429, Code: "rate_limit_exceeded", Message: "slow down", RetryAfter: 2 * time.Second}
		err.Retryable = true
		return llm.Response{}, err
	}}
	recorder := doRequest(t, testHandler(fake), http.MethodPost, "/v1/chat/completions", `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`, "")
	if recorder.Code != 429 {
		t.Fatalf("status = %d", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
}

func TestUpstreamFailureEnvelope(t *testing.T) {
	fake := &fakeAdapter{respond: func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{Failure: &llm.Failure{Code: "context_length_exceeded", Message: "too long"}}, nil
	}}
	recorder := doRequest(t, testHandler(fake), http.MethodPost, "/v1/chat/completions", `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`, "")
	if recorder.Code == http.StatusOK {
		t.Fatal("expected error status for failed response")
	}
}

func TestCancellation(t *testing.T) {
	fake := &fakeAdapter{respond: func(ctx context.Context, _ llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}}
	handler := testHandler(fake)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	cancel()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected empty body for canceled context, got %q", recorder.Body.String())
	}
}

func TestIndependentClientOverHTTP(t *testing.T) {
	fake := &fakeAdapter{respond: func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{Stop: llm.StopComplete,
			Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "via-http"}}},
			Usage:  llm.Usage{InputTokens: 2, OutputTokens: 3}}, nil
	}}
	server := httptest.NewServer(testHandler(fake))
	defer server.Close()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var decoded completionResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Choices[0].Message.Content != "via-http" || decoded.Usage.TotalTokens != 5 {
		t.Fatalf("unexpected independent-client response: %+v", decoded)
	}
}

func TestToolChoiceNone(t *testing.T) {
	var sawTools bool
	fake := &fakeAdapter{respond: func(_ context.Context, request llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		sawTools = len(request.Tools) != 0
		return llm.Response{Stop: llm.StopComplete, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "ok"}}}}, nil
	}}
	recorder := doRequest(t, testHandler(fake), http.MethodPost, "/v1/chat/completions", `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"none"}`, "")
	if recorder.Code != http.StatusOK || sawTools {
		t.Fatalf("tool_choice none should drop tools (status %d tools %v)", recorder.Code, sawTools)
	}
}

func TestMaxTokensMapping(t *testing.T) {
	fake := &fakeAdapter{respond: func(_ context.Context, request llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		if request.Model.MaxOutputTokens == nil || *request.Model.MaxOutputTokens != 17 {
			return llm.Response{}, errors.New("max tokens not mapped")
		}
		return llm.Response{Stop: llm.StopMaxOutputTokens, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "cut"}}}}, nil
	}}
	recorder := doRequest(t, testHandler(fake), http.MethodPost, "/v1/chat/completions", `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"max_tokens":17}`, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	var decoded completionResponse
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	if decoded.Choices[0].FinishReason != "length" {
		t.Fatalf("expected length finish, got %+v", decoded.Choices[0])
	}
}
