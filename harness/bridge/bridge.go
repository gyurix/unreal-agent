package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
)

type Config struct {
	Adapter      llm.Adapter
	Models       []string
	APIKey       string
	DefaultModel string
}

type Handler struct {
	adapter      llm.Adapter
	models       []string
	apiKey       string
	defaultModel string
}

func NewHandler(config Config) (*Handler, error) {
	if config.Adapter == nil {
		return nil, errors.New("bridge adapter must be set")
	}
	if len(config.Models) == 0 && strings.TrimSpace(config.DefaultModel) == "" {
		return nil, errors.New("bridge models or default model must be set")
	}
	models := append([]string(nil), config.Models...)
	if len(models) == 0 {
		models = []string{config.DefaultModel}
	}
	return &Handler{
		adapter:      config.Adapter,
		models:       models,
		apiKey:       config.APIKey,
		defaultModel: strings.TrimSpace(config.DefaultModel),
	}, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/healthz":
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok"))
		return
	case request.Method == http.MethodGet && request.URL.Path == "/v1/models":
		if !handler.authorize(writer, request) {
			return
		}
		handler.serveModels(writer)
		return
	case request.Method == http.MethodPost && request.URL.Path == "/v1/chat/completions":
		if !handler.authorize(writer, request) {
			return
		}
		handler.serveChatCompletions(writer, request)
		return
	default:
		writeError(writer, http.StatusNotFound, "unknown endpoint", "invalid_request_error", "not_found")
		return
	}
}

func (handler *Handler) authorize(writer http.ResponseWriter, request *http.Request) bool {
	if handler.apiKey == "" {
		return true
	}
	header := strings.TrimSpace(request.Header.Get("Authorization"))
	if !strings.HasPrefix(header, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")) != handler.apiKey {
		writeError(writer, http.StatusUnauthorized, "invalid bearer token", "invalid_request_error", "invalid_api_key")
		return false
	}
	return true
}

type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (handler *Handler) serveModels(writer http.ResponseWriter) {
	entries := make([]modelEntry, 0, len(handler.models))
	for _, id := range handler.models {
		entries = append(entries, modelEntry{ID: id, Object: "model", Created: time.Now().Unix(), OwnedBy: "bridge"})
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"object": "list", "data": entries})
}

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	Tools               []chatTool    `json:"tools"`
	ToolChoice          any           `json:"tool_choice"`
	Stream              bool          `json:"stream"`
	MaxTokens           *int64        `json:"max_tokens"`
	MaxCompletionTokens *int64        `json:"max_completion_tokens"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls"`
	ToolCallID string         `json:"tool_call_id"`
	Name       string         `json:"name"`
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
	Type     string           `json:"type"`
	Function *chatFunctionDef `json:"function"`
}

type chatFunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

func (handler *Handler) serveChatCompletions(writer http.ResponseWriter, request *http.Request) {
	var parsed chatRequest
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&parsed); err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err), "invalid_request_error", "invalid_request")
		return
	}
	model := strings.TrimSpace(parsed.Model)
	if model == "" {
		model = handler.defaultModel
	}
	if model == "" {
		writeError(writer, http.StatusBadRequest, "model must be set", "invalid_request_error", "invalid_request")
		return
	}
	if !handler.knowsModel(model) {
		writeError(writer, http.StatusNotFound, fmt.Sprintf("model %q not served by this bridge", model), "invalid_request_error", "model_not_found")
		return
	}
	llmRequest, err := chatToLLM(model, parsed)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_request")
		return
	}
	response, err := handler.adapter.Respond(request.Context(), llmRequest, llm.RequestOptions{})
	if err != nil {
		writeUpstreamError(writer, err)
		return
	}
	if response.Failure != nil {
		writeUpstreamError(writer, responsesapi.FailureError(*response.Failure))
		return
	}
	if parsed.Stream {
		handler.serveStream(writer, request.Context(), model, response)
		return
	}
	serveCompletion(writer, model, response)
}

func (handler *Handler) knowsModel(model string) bool {
	for _, id := range handler.models {
		if id == model {
			return true
		}
	}
	return false
}

func chatToLLM(model string, parsed chatRequest) (llm.Request, error) {
	if len(parsed.Messages) == 0 {
		return llm.Request{}, errors.New("messages must not be empty")
	}
	dropTools := false
	if parsed.ToolChoice != nil {
		if name, ok := toolChoiceName(parsed.ToolChoice); ok && name == "none" {
			dropTools = true
		}
	}
	request := llm.Request{Model: llm.Model{ID: model}}
	if parsed.MaxCompletionTokens != nil {
		request.Model.MaxOutputTokens = parsed.MaxCompletionTokens
	} else if parsed.MaxTokens != nil {
		request.Model.MaxOutputTokens = parsed.MaxTokens
	}
	if request.Model.MaxOutputTokens != nil && *request.Model.MaxOutputTokens <= 0 {
		return llm.Request{}, errors.New("max_tokens must be positive")
	}
	if !dropTools {
		for index, tool := range parsed.Tools {
			if tool.Type != "" && tool.Type != "function" {
				return llm.Request{}, fmt.Errorf("tools[%d].type %q is not supported; only function tools are supported", index, tool.Type)
			}
			if tool.Function == nil || strings.TrimSpace(tool.Function.Name) == "" {
				return llm.Request{}, fmt.Errorf("tools[%d].function.name must be set", index)
			}
			request.Tools = append(request.Tools, llm.Tool{
				Type:        llm.ToolFunction,
				Name:        tool.Function.Name,
				Description: tool.Function.Description,
				Parameters:  tool.Function.Parameters,
			})
		}
	}
	for index, message := range parsed.Messages {
		items, err := chatMessageToItems(message)
		if err != nil {
			return llm.Request{}, fmt.Errorf("messages[%d]: %w", index, err)
		}
		request.Input = append(request.Input, items...)
	}
	return request, nil
}

func toolChoiceName(value any) (string, bool) {
	switch choice := value.(type) {
	case string:
		return choice, true
	case map[string]any:
		if typ, ok := choice["type"].(string); ok {
			return typ, true
		}
	}
	return "", false
}

func chatMessageToItems(message chatMessage) ([]llm.Item, error) {
	switch message.Role {
	case "system", "developer":
		text, err := chatContentText(message.Content)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(text) == "" {
			return nil, errors.New("content must not be empty")
		}
		return []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: text}}}, nil
	case "user":
		text, err := chatContentText(message.Content)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(text) == "" {
			return nil, errors.New("content must not be empty")
		}
		return []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: text}}}, nil
	case "assistant":
		var items []llm.Item
		if message.Content != nil {
			text, err := chatContentText(message.Content)
			if err != nil {
				return nil, err
			}
			if text != "" {
				items = append(items, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}})
			}
		}
		for _, call := range message.ToolCalls {
			if strings.TrimSpace(call.Function.Name) == "" || strings.TrimSpace(call.ID) == "" {
				return nil, errors.New("assistant tool_calls entries must carry id and function.name")
			}
			args := call.Function.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			items = append(items, llm.Item{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: call.ID, Name: call.Function.Name, Arguments: args}})
		}
		if len(items) == 0 {
			return nil, errors.New("assistant message must carry content or tool_calls")
		}
		return items, nil
	case "tool":
		if strings.TrimSpace(message.ToolCallID) == "" {
			return nil, errors.New("tool message must carry tool_call_id")
		}
		text, err := chatContentText(message.Content)
		if err != nil {
			return nil, err
		}
		return []llm.Item{{Type: llm.ItemToolResult, Data: llm.ToolResult{
			CallID: message.ToolCallID,
			Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}},
		}}}, nil
	case "function":
		if strings.TrimSpace(message.Name) == "" {
			return nil, errors.New("function message must carry name")
		}
		text, err := chatContentText(message.Content)
		if err != nil {
			return nil, err
		}
		return []llm.Item{{Type: llm.ItemToolResult, Data: llm.ToolResult{
			CallID: message.Name,
			Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}},
		}}}, nil
	default:
		return nil, fmt.Errorf("role %q is not supported", message.Role)
	}
}

func chatContentText(content any) (string, error) {
	if content == nil {
		return "", nil
	}
	switch value := content.(type) {
	case string:
		return value, nil
	case []any:
		var builder strings.Builder
		for index, part := range value {
			obj, ok := part.(map[string]any)
			if !ok {
				return "", fmt.Errorf("content part %d must be an object", index)
			}
			typ, _ := obj["type"].(string)
			switch typ {
			case "text":
				if text, ok := obj["text"].(string); ok {
					builder.WriteString(text)
				} else if nested, ok := obj["text"].(map[string]any); ok {
					if text, ok := nested["value"].(string); ok {
						builder.WriteString(text)
					}
				}
			case "input_text":
				if text, ok := obj["text"].(string); ok {
					builder.WriteString(text)
				}
			case "refusal":
				if text, ok := obj["refusal"].(string); ok {
					builder.WriteString(text)
				}
			default:
				return "", fmt.Errorf("content part %d of type %q is not supported; only text parts are supported", index, typ)
			}
		}
		return builder.String(), nil
	default:
		return "", errors.New("content must be a string or an array of text parts")
	}
}

type completionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   completionUsage    `json:"usage"`
}

type completionChoice struct {
	Index        int               `json:"index"`
	Message      completionMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type completionMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

type completionUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func llmToCompletion(model string, response llm.Response) completionResponse {
	var text strings.Builder
	var calls []chatToolCall
	for _, item := range response.Output {
		switch item.Type {
		case llm.ItemMessage:
			if message, ok := item.Data.(llm.Message); ok {
				text.WriteString(message.Text)
			}
		case llm.ItemToolCall:
			if call, ok := item.Data.(llm.ToolCall); ok {
				calls = append(calls, chatToolCall{
					ID:       call.CallID,
					Type:     "function",
					Function: chatFunctionCall{Name: call.Name, Arguments: call.Arguments},
				})
			}
		}
	}
	finish := "stop"
	switch {
	case len(calls) != 0:
		finish = "tool_calls"
	case response.Stop == llm.StopMaxOutputTokens:
		finish = "length"
	}
	id := response.ID
	if strings.TrimSpace(id) == "" {
		id = "chatcmpl-bridge"
	}
	return completionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []completionChoice{{Index: 0, Message: completionMessage{Role: "assistant", Content: text.String(), ToolCalls: calls}, FinishReason: finish}},
		Usage: completionUsage{
			PromptTokens:     response.Usage.InputTokens,
			CompletionTokens: response.Usage.OutputTokens,
			TotalTokens:      response.Usage.InputTokens + response.Usage.OutputTokens,
		},
	}
}

func serveCompletion(writer http.ResponseWriter, model string, response llm.Response) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(llmToCompletion(model, response))
}

type streamChunk struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []streamChoice   `json:"choices"`
	Usage   *completionUsage `json:"usage,omitempty"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamDelta struct {
	Role      string           `json:"role,omitempty"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []streamToolCall `json:"tool_calls,omitempty"`
}

type streamToolCall struct {
	Index    int            `json:"index"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function streamFunction `json:"function"`
}

type streamFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

func (handler *Handler) serveStream(writer http.ResponseWriter, ctx context.Context, model string, response llm.Response) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		serveCompletion(writer, model, response)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	completed := llmToCompletion(model, response)
	choice := completed.Choices[0]
	writeChunk := func(chunk streamChunk) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return false
		}
		_, _ = fmt.Fprintf(writer, "data: %s\n\n", encoded)
		flusher.Flush()
		return true
	}
	id, created := completed.ID, completed.Created
	base := func() streamChunk {
		return streamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model, Choices: []streamChoice{{Index: 0}}}
	}
	first := base()
	first.Choices[0].Delta = streamDelta{Role: "assistant"}
	if !writeChunk(first) {
		return
	}
	if choice.Message.Content != "" {
		content := base()
		content.Choices[0].Delta = streamDelta{Content: choice.Message.Content}
		if !writeChunk(content) {
			return
		}
	}
	for index, call := range choice.Message.ToolCalls {
		delta := base()
		delta.Choices[0].Delta = streamDelta{ToolCalls: []streamToolCall{{Index: index, ID: call.ID, Type: "function", Function: streamFunction{Name: call.Function.Name, Arguments: call.Function.Arguments}}}}
		if !writeChunk(delta) {
			return
		}
	}
	last := base()
	last.Choices[0].FinishReason = &choice.FinishReason
	usage := completed.Usage
	last.Usage = &usage
	_ = writeChunk(last)
	_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
	flusher.Flush()
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

func writeError(writer http.ResponseWriter, status int, message, errType, code string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(errorEnvelope{Error: errorBody{Message: message, Type: errType, Code: code}})
}

func writeUpstreamError(writer http.ResponseWriter, err error) {
	var apiErr *responsesapi.APIError
	if errors.As(err, &apiErr) {
		status := apiErr.StatusCode
		if status < 400 || status >= 600 {
			status = http.StatusBadGateway
		}
		if apiErr.RetryAfter > 0 {
			writer.Header().Set("Retry-After", fmt.Sprintf("%d", int64((apiErr.RetryAfter+time.Second/2)/time.Second)))
		}
		code := apiErr.Code
		if code == "" {
			code = apiErr.Type
		}
		message := apiErr.Message
		if message == "" {
			message = http.StatusText(status)
		}
		writeError(writer, status, message, "server_error", code)
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(writer, http.StatusGatewayTimeout, "upstream request timed out", "server_error", "timeout")
		return
	}
	writeError(writer, http.StatusBadGateway, err.Error(), "server_error", "upstream_error")
}
