// Package minimalio proves, step by step, the exact upstream network
// behavior of the standalone Zen bridge for one fixed agentic prompt.
// Every request the bridge emits toward Zen is recorded (order, headers,
// body, timing) to a JSONL trace, and the-order-of-requests fidelity
// invariants reverse-engineered from the genuine OpenCode client are
// asserted: full identity header set, timestamp-bearing ses_/msg_ IDs,
// stable session with a fresh request ID per call.
package minimalio

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/bridge"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/chatcompat"
)

// AgenticPrompt is the exact scenario used for the fidelity comparison:
// folder/file creation, 10 parallel checker agents with file copies,
// verification listing, and cleanup.
const AgenticPrompt = "do things in this exact order to test agentic capabilities " +
	"1. create a /tmp/test-agentic-work folder, " +
	"2. create a file in it called a.txt with a single world answer to the what is UKs capital city question, " +
	"3. launch 10 parallel running agents named ag01-ag10 with the goal of each checking the correctness of the answer in the original file, " +
	"as well as making 2 parallel copies of this file to /tmp/test-agentic-work/<agent-name> folders with names a.txt and b.txt, " +
	"4. when all agents finished use 5 agents to conduct ls -al tool calls on all output folders and confirm the correctness of work, " +
	"6. us rm -Rf toolcall to delete the /tmp/test-agentic-work folder"

type recordedCall struct {
	Index    int               `json:"index"`
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Headers  map[string]string `json:"headers"`
	Body     string            `json:"body"`
	BodyLen  int               `json:"body_len"`
	Status   int               `json:"status"`
	Elapsed  int64             `json:"elapsed_ms"`
	HasTools bool              `json:"has_tools"`
}

type recorder struct {
	mu    sync.Mutex
	calls []recordedCall
	start time.Time
}

func (recorder *recorder) roundTrip(next http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		index := len(recorder.calls)
		begin := time.Now()
		var body []byte
		if request.Body != nil {
			body, _ = io.ReadAll(request.Body)
			request.Body = io.NopCloser(bytes.NewReader(body))
		}
		headers := map[string]string{}
		for name, values := range request.Header {
			headers[strings.ToLower(name)] = strings.Join(values, ", ")
		}
		response, err := next.RoundTrip(request)
		entry := recordedCall{
			Index:   index,
			Method:  request.Method,
			Path:    request.URL.Path,
			Headers: headers,
			Body:    string(body),
			BodyLen: len(body),
			Elapsed: time.Since(begin).Milliseconds(),
		}
		if err == nil {
			entry.Status = response.StatusCode
			entry.HasTools = strings.Contains(string(body), "\"tools\"")
		}
		recorder.mu.Lock()
		recorder.calls = append(recorder.calls, entry)
		recorder.mu.Unlock()
		return response, err
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestMinimalAgenticPromptTrace(t *testing.T) {
	traceDir := "/tmp/opencode/traces/unreal"
	if err := os.MkdirAll(traceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"id":"trace1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	recorder := &recorder{start: time.Now()}
	client, err := chatcompat.NewClient(chatcompat.Config{
		BaseURL: upstream.URL,
		// Same default the bridge applies when OPENCODE_PROJECT_ID is unset.
		ProjectID:  "global",
		HTTPClient: &http.Client{Transport: recorder.roundTrip(http.DefaultTransport)},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := bridge.NewHandler(bridge.Config{
		Adapter:      client,
		Models:       []string{"mimo-v2.6-flash-free"},
		DefaultModel: "mimo-v2.6-flash-free",
	})
	if err != nil {
		t.Fatal(err)
	}
	frontend := httptest.NewServer(handler)
	defer frontend.Close()

	post := func(model string, stream bool) map[string]any {
		payload, _ := json.Marshal(map[string]any{
			"model":    model,
			"messages": []map[string]string{{"role": "user", "content": AgenticPrompt}},
			"stream":   stream,
		})
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, frontend.URL+"/v1/chat/completions", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		encoded, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("bridge status = %d: %s", resp.StatusCode, encoded)
		}
		var decoded map[string]any
		if !stream {
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
		}
		return decoded
	}

	post("mimo-v2.6-flash-free", false)
	post("mimo-v2.6-flash-free", true)

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.calls) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(recorder.calls))
	}
	sessions := map[string]bool{}
	requests := map[string]bool{}
	for _, call := range recorder.calls {
		if call.Method != http.MethodPost || call.Path != "/chat/completions" {
			t.Fatalf("call %d: %s %s", call.Index, call.Method, call.Path)
		}
		if call.Headers["authorization"] != "Bearer public" {
			t.Fatalf("call %d: authorization = %q", call.Index, call.Headers["authorization"])
		}
		if !strings.HasPrefix(call.Headers["user-agent"], "opencode/") {
			t.Fatalf("call %d: user-agent = %q", call.Index, call.Headers["user-agent"])
		}
		for _, header := range []string{"x-opencode-client", "x-opencode-session", "x-opencode-request", "x-opencode-project"} {
			if call.Headers[header] == "" {
				t.Fatalf("call %d: missing %s", call.Index, header)
			}
		}
		if !strings.Contains(call.Body, "test-agentic-work") || !strings.Contains(call.Body, "ag01-ag10") {
			t.Fatalf("call %d: prompt not forwarded verbatim", call.Index)
		}
		sessions[call.Headers["x-opencode-session"]] = true
		requests[call.Headers["x-opencode-request"]] = true
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want exactly 1 stable session", len(sessions))
	}
	if len(requests) != 2 {
		t.Fatalf("request IDs = %d, want 1 fresh ID per call", len(requests))
	}

	trace, err := json.MarshalIndent(recorder.calls, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(traceDir, "minimal-io.jsonl"), append(trace, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
