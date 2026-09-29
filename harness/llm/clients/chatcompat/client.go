package chatcompat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

const DefaultBaseURL = "https://opencode.ai/zen/v1"

// DefaultUserAgent mirrors the genuine OpenCode client fingerprint observed
// on the wire (Bun verbose-fetch trace): the AI SDK appends its provider
// utilities version and runtime to opencode/<version>.
const DefaultUserAgent = "opencode/1.18.33 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"

// publicAPIKey is what the genuine client sends when no Zen key is
// configured (packages/opencode/src/provider/provider.ts assigns
// options.apiKey "public"); the Zen server maps it to anonymous access.
const publicAPIKey = "public"

type Config struct {
	APIKey      string
	BaseURL     string
	MaxAttempts *int
	HTTPClient  *http.Client
	// OpenCode identity headers forwarded to Zen to match the genuine
	// client protocol. These are non-secret, locally-generated markers
	// (see packages/opencode/src/session/llm/request.ts).
	ClientType string // "cli", "desktop", or "app"
	SessionID  string
	RequestID  string
	ProjectID  string
	UserAgent  string
	Stream     bool // Request and decode Chat Completions SSE, as required by Zen free models.
}

type Client struct {
	apiKey      string
	baseURL     string
	maxAttempts int
	httpClient  *http.Client
	clientType  string
	sessionID   string
	requestID   string
	projectID   string
	userAgent   string
	stream      bool
}

var _ llm.Adapter = (*Client)(nil)

func NewClient(config Config) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		return nil, errors.New("chat API base URL must be set")
	}
	maxAttempts := responsesapi.DefaultMaxAttempts
	if config.MaxAttempts != nil {
		maxAttempts = *config.MaxAttempts
	}
	if maxAttempts <= 0 {
		return nil, errors.New("max attempts must be positive")
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Minute}
	}
	clientType := strings.TrimSpace(config.ClientType)
	if clientType == "" {
		clientType = "cli"
	}
	userAgent := strings.TrimSpace(config.UserAgent)
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	apiKey := strings.TrimSpace(config.APIKey)
	if apiKey == "" {
		apiKey = publicAPIKey
	}
	sessionID := strings.TrimSpace(config.SessionID)
	if sessionID == "" {
		sessionID = opencodeID("ses_", true)
	}
	// requestID stays empty for per-request minting unless explicitly
	// configured: the genuine client sends a fresh user-message ID per
	// turn, and the free-tier gate rejects reused request IDs.
	requestID := strings.TrimSpace(config.RequestID)
	return &Client{
		apiKey:      apiKey,
		baseURL:     baseURL,
		maxAttempts: maxAttempts,
		httpClient:  httpClient,
		clientType:  clientType,
		sessionID:   sessionID,
		requestID:   requestID,
		projectID:   strings.TrimSpace(config.ProjectID),
		userAgent:   userAgent,
		stream:      config.Stream,
	}, nil
}

func (client *Client) Close() error { return nil }

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string          `json:"type"`
	Function chatFunctionDef `json:"function"`
}

type chatFunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

func (client *Client) Respond(ctx context.Context, request llm.Request, _ llm.RequestOptions) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	if strings.TrimSpace(request.Model.ID) == "" {
		return llm.Response{}, errors.New("model must be set")
	}
	messages, err := requestMessages(request.Input)
	if err != nil {
		return llm.Response{}, err
	}
	tools, err := requestTools(request.Tools)
	if err != nil {
		return llm.Response{}, err
	}
	fields := map[string]any{
		"model":      request.Model.ID,
		"messages":   messages,
		"tools":      toolsOrNil(tools),
		"max_tokens": request.Model.MaxOutputTokens,
		"stream":     client.stream,
	}
	if client.stream {
		fields["stream_options"] = map[string]bool{"include_usage": true}
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return llm.Response{}, fmt.Errorf("encode chat request: %w", err)
	}
	// max_tokens is omitted when unset; encoding a nil pointer would send null.
	if request.Model.MaxOutputTokens == nil {
		var fields map[string]jsontext.Value
		if err := json.Unmarshal(body, &fields); err != nil {
			return llm.Response{}, fmt.Errorf("encode chat request: %w", err)
		}
		delete(fields, "max_tokens")
		if len(tools) == 0 {
			delete(fields, "tools")
		}
		body, err = json.Marshal(fields)
		if err != nil {
			return llm.Response{}, fmt.Errorf("encode chat request: %w", err)
		}
	}
	if request.Model.MaxOutputTokens != nil && *request.Model.MaxOutputTokens <= 0 {
		return llm.Response{}, errors.New("max_output_tokens must be positive")
	}
	endpoint := client.baseURL + "/chat/completions"
	requestID := client.requestID
	if requestID == "" {
		requestID = opencodeID("msg_", false)
	}
	var lastErr error
	for attempt := 1; attempt <= client.maxAttempts; attempt++ {
		response, retry, retryAfter, err := client.exchange(ctx, endpoint, body, requestID)
		if err == nil {
			return response, nil
		}
		lastErr = err
		if !retry || attempt >= client.maxAttempts {
			return llm.Response{}, err
		}
		if err := sleepContext(ctx, retryDelay(attempt, retryAfter)); err != nil {
			return llm.Response{}, err
		}
	}
	return llm.Response{}, lastErr
}

func toolsOrNil(tools []chatTool) any {
	if len(tools) == 0 {
		return nil
	}
	return tools
}

func requestMessages(items []llm.Item) ([]chatMessage, error) {
	var messages []chatMessage
	var pending *chatMessage
	flush := func() {
		if pending != nil {
			messages = append(messages, *pending)
			pending = nil
		}
	}
	for index, item := range items {
		switch item.Type {
		case llm.ItemMessage:
			message, ok := item.Data.(llm.Message)
			if !ok {
				return nil, fmt.Errorf("input item %d: message data must be llm.Message, got %T", index, item.Data)
			}
			switch message.Role {
			case llm.RoleSystem:
				flush()
				messages = append(messages, chatMessage{Role: "system", Content: message.Text})
			case llm.RoleUser:
				flush()
				messages = append(messages, chatMessage{Role: "user", Content: message.Text})
			case llm.RoleAssistant:
				flush()
				pending = &chatMessage{Role: "assistant", Content: message.Text}
			default:
				return nil, fmt.Errorf("input item %d: unsupported message role %q", index, message.Role)
			}
		case llm.ItemToolCall:
			call, ok := item.Data.(llm.ToolCall)
			if !ok {
				return nil, fmt.Errorf("input item %d: tool_call data must be llm.ToolCall, got %T", index, item.Data)
			}
			if strings.TrimSpace(call.CallID) == "" || strings.TrimSpace(call.Name) == "" {
				return nil, fmt.Errorf("input item %d: tool call must carry id and name", index)
			}
			if pending == nil {
				pending = &chatMessage{Role: "assistant"}
			} else if pending.Role != "assistant" {
				flush()
				pending = &chatMessage{Role: "assistant"}
			}
			args := call.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			pending.ToolCalls = append(pending.ToolCalls, chatToolCall{
				ID:       call.CallID,
				Type:     "function",
				Function: chatFunctionCall{Name: call.Name, Arguments: args},
			})
		case llm.ItemToolResult:
			result, ok := item.Data.(llm.ToolResult)
			if !ok {
				return nil, fmt.Errorf("input item %d: tool_result data must be llm.ToolResult, got %T", index, item.Data)
			}
			var builder strings.Builder
			for _, output := range result.Output {
				switch output.Kind {
				case llm.ToolResultText:
					builder.WriteString(output.Value)
				default:
					return nil, fmt.Errorf("input item %d: tool result kind %q is not supported by chat completions", index, output.Kind)
				}
			}
			flush()
			messages = append(messages, chatMessage{Role: "tool", ToolCallID: result.CallID, Content: builder.String()})
		case llm.ItemReasoning:
			continue
		default:
			return nil, fmt.Errorf("input item %d: unsupported item type %q", index, item.Type)
		}
	}
	flush()
	if len(messages) == 0 {
		return nil, errors.New("messages must not be empty")
	}
	return messages, nil
}

func requestTools(source []llm.Tool) ([]chatTool, error) {
	tools := make([]chatTool, 0, len(source))
	for index, tool := range source {
		if tool.Type != llm.ToolFunction {
			return nil, fmt.Errorf("tool %d: only function tools are supported, got %q", index, tool.Type)
		}
		if strings.TrimSpace(tool.Name) == "" {
			return nil, fmt.Errorf("tool %d: name must be set", index)
		}
		tools = append(tools, chatTool{
			Type:     "function",
			Function: chatFunctionDef{Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters},
		})
	}
	return tools, nil
}

type chatResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role      string         `json:"role"`
			Content   *string        `json:"content"`
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

func (client *Client) exchange(ctx context.Context, endpoint string, body []byte, requestID string) (llm.Response, bool, time.Duration, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return llm.Response{}, false, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	if client.stream {
		request.Header.Set("Accept", "*/*")
	}
	if client.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+client.apiKey)
	}
	request.Header.Set("User-Agent", client.userAgent)
	request.Header.Set("x-opencode-client", client.clientType)
	if client.sessionID != "" {
		request.Header.Set("x-opencode-session", client.sessionID)
	}
	if requestID != "" {
		request.Header.Set("x-opencode-request", requestID)
	}
	if client.projectID != "" {
		request.Header.Set("x-opencode-project", client.projectID)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		var netErr net.Error
		retry := ctx.Err() == nil && (errors.As(err, &netErr) && netErr.Timeout() || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF))
		return llm.Response{}, retry, 0, err
	}
	defer func() { _ = response.Body.Close() }()
	const limit = 4 << 20
	encoded, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return llm.Response{}, response.StatusCode >= 500, 0, err
	}
	if len(encoded) > limit {
		return llm.Response{}, false, 0, errors.New("chat response exceeds 4 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		apiErr := chatError(response.StatusCode, response.Header, encoded)
		return llm.Response{}, apiErr.Retryable, apiErr.RetryAfter, fmt.Errorf("create chat completion: %w", &upstreamError{APIError: apiErr})
	}
	var converted llm.Response
	if client.stream {
		if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
			return llm.Response{}, false, 0, fmt.Errorf("chat response: expected text/event-stream, got %q", response.Header.Get("Content-Type"))
		}
		converted, err = streamToResponse(encoded)
	} else {
		converted, err = chatToResponse(encoded)
	}
	if err != nil {
		return llm.Response{}, false, 0, err
	}
	return converted, false, 0, nil
}

func chatToResponse(encoded []byte) (llm.Response, error) {
	var decoded chatResponse
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return llm.Response{}, fmt.Errorf("decode chat response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return llm.Response{}, errors.New("chat response has no choices")
	}
	choice := decoded.Choices[0]
	response := llm.Response{ID: decoded.ID}
	switch choice.FinishReason {
	case "stop", "tool_calls", "function_call":
		response.Stop = llm.StopComplete
	case "length":
		response.Stop = llm.StopMaxOutputTokens
	case "content_filter", "refusal":
		response.Stop = llm.StopRefused
	case "":
		response.Stop = llm.StopComplete
	default:
		return llm.Response{}, fmt.Errorf("unsupported finish reason %q", choice.FinishReason)
	}
	var content string
	if choice.Message.Content != nil {
		content = *choice.Message.Content
	}
	if content != "" {
		response.Output = append(response.Output, llm.Item{
			Type: llm.ItemMessage,
			Data: llm.Message{Role: llm.RoleAssistant, Text: content},
		})
	}
	for _, call := range choice.Message.ToolCalls {
		if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Function.Name) == "" {
			return llm.Response{}, errors.New("chat response tool call must carry id and function name")
		}
		response.Output = append(response.Output, llm.Item{
			Type: llm.ItemToolCall,
			Data: llm.ToolCall{CallID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments},
		})
	}
	if decoded.Usage != nil {
		response.Usage = llm.Usage{InputTokens: decoded.Usage.PromptTokens, OutputTokens: decoded.Usage.CompletionTokens, Raw: jsontext.Value(mustMarshal(decoded.Usage))}
	}
	return response, nil
}

type streamChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

func streamToResponse(encoded []byte) (llm.Response, error) {
	var result llm.Response
	var content strings.Builder
	var calls []llm.ToolCall
	var finishReason string
	var done bool
	encoded = bytes.ReplaceAll(encoded, []byte("\r\n"), []byte("\n"))
	for _, frame := range bytes.Split(encoded, []byte("\n\n")) {
		data := primitives.SSEData(frame)
		if data == nil {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			done = true
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return llm.Response{}, fmt.Errorf("decode chat stream event: %w", err)
		}
		if chunk.ID != "" {
			if result.ID != "" && result.ID != chunk.ID {
				return llm.Response{}, errors.New("chat stream response id changed")
			}
			result.ID = chunk.ID
		}
		if chunk.Usage != nil {
			result.Usage = llm.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens, Raw: jsontext.Value(mustMarshal(chunk.Usage))}
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			if choice.Delta.Content != nil {
				content.WriteString(*choice.Delta.Content)
			}
			for _, part := range choice.Delta.ToolCalls {
				if part.Index < 0 || part.Index > len(calls) {
					return llm.Response{}, fmt.Errorf("chat stream tool call index %d out of sequence", part.Index)
				}
				if part.Index == len(calls) {
					calls = append(calls, llm.ToolCall{})
				}
				call := &calls[part.Index]
				if part.ID != "" {
					if call.CallID != "" && call.CallID != part.ID {
						return llm.Response{}, errors.New("chat stream tool call id changed")
					}
					call.CallID = part.ID
				}
				call.Name += part.Function.Name
				call.Arguments += part.Function.Arguments
			}
			if choice.FinishReason != "" {
				if finishReason != "" && finishReason != choice.FinishReason {
					return llm.Response{}, errors.New("chat stream finish reason changed")
				}
				finishReason = choice.FinishReason
				switch choice.FinishReason {
				case "stop", "tool_calls", "function_call":
					result.Stop = llm.StopComplete
				case "length":
					result.Stop = llm.StopMaxOutputTokens
				case "content_filter", "refusal":
					result.Stop = llm.StopRefused
				default:
					return llm.Response{}, fmt.Errorf("unsupported finish reason %q", choice.FinishReason)
				}
			}
		}
	}
	if finishReason == "" || !done {
		return llm.Response{}, errors.New("chat stream ended before finish reason and [DONE]")
	}
	if content.Len() > 0 {
		result.Output = append(result.Output, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: content.String()}})
	}
	for _, call := range calls {
		if strings.TrimSpace(call.CallID) == "" || strings.TrimSpace(call.Name) == "" {
			return llm.Response{}, errors.New("chat stream tool call must carry id and function name")
		}
		result.Output = append(result.Output, llm.Item{Type: llm.ItemToolCall, Data: call})
	}
	return result, nil
}

func mustMarshal(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}

func chatError(statusCode int, header http.Header, body []byte) *responsesapi.APIError {
	message, code, errType := parseChatErrorBody(body)
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(statusCode)
	}
	apiErr := &responsesapi.APIError{StatusCode: statusCode, Code: code, Type: errType, Message: message}
	apiErr.Retryable = statusCode == http.StatusTooManyRequests || statusCode >= 500
	apiErr.RetryAfter = retryAfterHeader(header)
	return apiErr
}

type upstreamError struct {
	*responsesapi.APIError
}

func (err *upstreamError) Error() string {
	if err == nil || err.APIError == nil {
		return "chat completion request failed"
	}
	if err.Code != "" {
		return fmt.Sprintf("chat completion error %s: %s", err.Code, err.Message)
	}
	if err.StatusCode != 0 {
		return fmt.Sprintf("chat completion request failed with status %d: %s", err.StatusCode, err.Message)
	}
	return "chat completion request failed: " + err.Message
}

func (err *upstreamError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.APIError
}

func parseChatErrorBody(body []byte) (string, string, string) {
	var shaped struct {
		Type  string `json:"type"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &shaped); err != nil || shaped.Error == nil {
		return "", "", ""
	}
	return shaped.Error.Message, shaped.Error.Code, firstNonEmpty(shaped.Error.Type, shaped.Type)
}

func retryAfterHeader(header http.Header) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if moment, err := http.ParseTime(value); err == nil {
		return time.Until(moment)
	}
	return 0
}

func retryDelay(attempt int, hint time.Duration) time.Duration {
	if hint > 0 {
		return hint
	}
	backoff := 500 * time.Millisecond
	for index := 1; index < attempt; index++ {
		backoff = min(backoff*2, 10*time.Second)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err == nil {
		// Full jitter avoids synchronized retries without changing the cap.
		return time.Duration(binary.LittleEndian.Uint64(random[:]) % uint64(backoff+1))
	}
	return backoff
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

const idAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var (
	idMu      sync.Mutex
	idLast    int64
	idCounter uint64
)

// opencodeID generates identifiers with the exact layout of OpenCode's
// identifier.ts: 12 hex chars holding the low 48 bits of
// timestamp_ms*0x1000+counter (bitwise-inverted for descending IDs),
// followed by 14 random base62 chars. The Zen free-tier gate validates
// the embedded timestamp, so fully random IDs are rejected with
// FreeTierError.
func opencodeID(prefix string, descending bool) string {
	now := time.Now().UnixMilli()
	idMu.Lock()
	if now != idLast {
		idLast = now
		idCounter = 0
	}
	idCounter++
	counter := idCounter
	idMu.Unlock()
	const mask = uint64(0xFFFFFFFFFFFF)
	low48 := (uint64(now)*0x1000 + counter) & mask
	if descending {
		low48 ^= mask
	}
	raw := make([]byte, 14)
	if _, err := rand.Read(raw); err != nil {
		for index := range raw {
			raw[index] = byte(index*31 + 7)
		}
	}
	var builder strings.Builder
	builder.WriteString(prefix)
	builder.WriteString(fmt.Sprintf("%012x", low48))
	for _, value := range raw {
		builder.WriteByte(idAlphabet[int(value)%len(idAlphabet)])
	}
	return builder.String()
}
