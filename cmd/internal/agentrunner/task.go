package agentrunner

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

const (
	taskToolName                                     = "task"
	taskPlanType      operation.RemoteJobPlanType    = "local_subagent"
	taskPlanVersion   operation.RemoteJobPlanVersion = 1
	taskMaxPending                                   = 64
	taskMaxConcurrent                                = 10
)

type taskArguments struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
}

type taskRegistry struct {
	tool.Registry
	translator taskTranslator
}

func (r *taskRegistry) StaticDefinitions() []tool.Definition {
	return append(r.Registry.StaticDefinitions(), tool.Definition{Tool: llm.Tool{
		Type: llm.ToolFunction, Name: taskToolName,
		Description: "Run an independent subagent in the same workspace. Use a distinct name and a complete task prompt; subagents run concurrently when called together. The result includes its final answer and token usage.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"name":   map[string]any{"type": "string", "description": "Short subagent name, e.g. ag01."},
			"prompt": map[string]any{"type": "string", "description": "Self-contained work request for the subagent."},
		}, "required": []any{"name", "prompt"}, "additionalProperties": false},
	}})
}

func (r *taskRegistry) Resolve(name string) (tool.Translator, bool) {
	if name == taskToolName {
		return r.translator, true
	}
	return r.Registry.Resolve(name)
}

type taskTranslator struct{}

func (taskTranslator) Translate(ctx tool.Context, call llm.ToolCall) tool.CallStatus {
	if call.Name != "" && call.Name != taskToolName {
		return tool.CallStatus{Error: "task tool name mismatch"}
	}
	var args taskArguments
	if err := json.Unmarshal([]byte(call.Arguments), &args, json.RejectUnknownMembers(true)); err != nil {
		return tool.CallStatus{Error: fmt.Sprintf("decode task arguments: %v", err)}
	}
	args.Name = strings.TrimSpace(args.Name)
	if len(args.Name) == 0 || len(args.Name) > 64 || strings.ContainsAny(args.Name, "\r\n\x00") {
		return tool.CallStatus{Error: "task name must be 1-64 characters without control line breaks or NUL"}
	}
	if strings.TrimSpace(args.Prompt) == "" || len(args.Prompt) > 65536 {
		return tool.CallStatus{Error: "task prompt must be nonempty and at most 65536 bytes"}
	}
	data, err := json.Marshal(args)
	if err != nil {
		return tool.CallStatus{Error: fmt.Sprintf("encode task arguments: %v", err)}
	}
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: taskPlanType, Version: taskPlanVersion, Data: data})
	if err != nil {
		return tool.CallStatus{Error: fmt.Sprintf("create task: %v", err)}
	}
	return tool.CallStatus{WaitingFor: []operation.ID{ctx.Submit(spec)}}
}

func (taskTranslator) TranslateResult(callID string, status tool.CallStatus, operations []operation.Operation) (llm.ToolResult, error) {
	result := llm.ToolResult{CallID: callID}
	if status.Error != "" {
		result.Output = []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: status.Error}}
		return result, nil
	}
	if len(operations) != 1 {
		return result, fmt.Errorf("task call %q has %d operations, want 1", callID, len(operations))
	}
	state, err := operation.DecodeRemoteJobState(operations[0])
	if err != nil {
		return result, err
	}
	var value string
	switch operations[0].Status {
	case operation.StatusCompleted:
		value = state.TerminalResult
	case operation.StatusFailed, operation.StatusCanceled:
		value = "Task failed: " + state.TerminalError
	case operation.StatusReady, operation.StatusAwaiting, operation.StatusCanceling:
		value = "Task is running."
	default:
		return result, fmt.Errorf("task call %q has invalid status %q", callID, operations[0].Status)
	}
	result.Output = []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: value}}
	return result, nil
}

type taskRunnerConfig struct {
	Executable, Workspace, SessionDirectory, Provider, BaseURL, Model string
	MaxAttempts                                                       int
	Request                                                           Request
}

type taskStderr struct {
	bytes.Buffer
	truncated bool
}

func (output *taskStderr) Write(data []byte) (int, error) {
	original := len(data)
	remaining := 8192 - output.Len()
	if remaining < original {
		output.truncated = true
	}
	if remaining > 0 {
		_, _ = output.Buffer.Write(data[:min(remaining, original)])
	}
	return original, nil
}

func (output *taskStderr) Text() string {
	text := strings.TrimSpace(output.String())
	if output.truncated {
		text += " [stderr truncated]"
	}
	return text
}

type taskHandler struct {
	ctx     context.Context
	config  taskRunnerConfig
	updates chan operation.Operation
	slots   chan struct{}
	mu      sync.Mutex
	jobs    map[operation.ID]context.CancelFunc
	closed  bool
	workers sync.WaitGroup
}

func newTaskHandler(ctx context.Context, config taskRunnerConfig) *taskHandler {
	return &taskHandler{ctx: ctx, config: config, updates: make(chan operation.Operation, taskMaxPending*2), slots: make(chan struct{}, taskMaxConcurrent), jobs: make(map[operation.ID]context.CancelFunc)}
}

func (*taskHandler) RemoteJobPlanType() operation.RemoteJobPlanType       { return taskPlanType }
func (*taskHandler) RemoteJobPlanVersion() operation.RemoteJobPlanVersion { return taskPlanVersion }
func (h *taskHandler) RemoteJobUpdates() <-chan operation.Operation       { return h.updates }

func (h *taskHandler) AddRemoteJob(current operation.Operation) error {
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		return err
	}
	var args taskArguments
	if err := json.Unmarshal(state.Plan.Data, &args); err != nil {
		return fmt.Errorf("decode task plan: %w", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("task handler closed")
	}
	if _, ok := h.jobs[current.ID]; ok {
		return nil
	}
	if len(h.jobs) >= taskMaxPending {
		return fmt.Errorf("task queue full (%d pending)", taskMaxPending)
	}
	ctx, cancel := context.WithCancel(h.ctx)
	h.jobs[current.ID] = cancel
	h.workers.Add(1)
	go h.run(ctx, current, state, args)
	return nil
}

func (h *taskHandler) CancelRemoteJob(id operation.ID, _ string) error {
	h.mu.Lock()
	cancel := h.jobs[id]
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (h *taskHandler) Close() {
	h.mu.Lock()
	h.closed = true
	for _, cancel := range h.jobs {
		cancel()
	}
	h.mu.Unlock()
	h.workers.Wait()
	close(h.updates)
}

func (h *taskHandler) emit(_ context.Context, current operation.Operation) {
	select {
	case h.updates <- current:
	case <-h.ctx.Done():
	}
}

func (h *taskHandler) run(ctx context.Context, current operation.Operation, state operation.RemoteJobState, args taskArguments) {
	defer h.workers.Done()
	defer func() { h.mu.Lock(); delete(h.jobs, current.ID); h.mu.Unlock() }()
	childID := "task-" + string(current.ID)
	handle, _ := json.Marshal(childID)
	state.Handle = handle
	step, err := operation.UpdateRemoteJob(current, state, operation.StatusAwaiting)
	if err != nil {
		h.fail(ctx, current, err)
		return
	}
	current = *step.Operation
	h.emit(ctx, current)
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	case <-ctx.Done():
		h.cancel(ctx, current)
		return
	}
	result, err := h.execute(ctx, childID, current.ID, args)
	if ctx.Err() != nil {
		h.cancel(ctx, current)
		return
	}
	if err != nil {
		h.fail(ctx, current, err)
		return
	}
	state.TerminalResult = result
	step, err = operation.UpdateRemoteJob(current, state, operation.StatusCompleted)
	if err != nil {
		h.fail(ctx, current, err)
		return
	}
	h.emit(ctx, *step.Operation)
}

func (h *taskHandler) fail(ctx context.Context, current operation.Operation, err error) {
	step, stepErr := operation.FailRemoteJob(current, err)
	if stepErr == nil {
		h.emit(ctx, *step.Operation)
	}
}

func (h *taskHandler) cancel(ctx context.Context, current operation.Operation) {
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		return
	}
	state.TerminalError = "task canceled"
	step, err := operation.UpdateRemoteJob(current, state, operation.StatusCanceled)
	if err == nil {
		h.emit(ctx, *step.Operation)
	}
}

func (h *taskHandler) execute(ctx context.Context, childID string, id operation.ID, args taskArguments) (string, error) {
	messageID := string(id)
	request := Request{Messages: []RequestMessage{{Role: "user", Content: args.Prompt, MessageID: &messageID}},
		Model: h.config.Model, SessionID: &childID, DisallowedTools: []string{taskToolName},
		MaxAttempts: &h.config.MaxAttempts, ThinkingLevel: h.config.Request.ThinkingLevel,
		SystemPrompt: h.config.Request.SystemPrompt, SystemPromptAppend: h.config.Request.SystemPromptAppend,
		Preamble: h.config.Request.Preamble}
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encode child request: %w", err)
	}
	cmd := exec.CommandContext(ctx, h.config.Executable, "-workspace", h.config.Workspace, "-session-directory", h.config.SessionDirectory)
	cmd.Dir = h.config.Workspace
	cmd.Env = append(os.Environ(), llmProviderEnvironment+"="+h.config.Provider, llmBaseURLEnvironment+"="+h.config.BaseURL, llmModelEnvironment+"="+h.config.Model, "UNREAL_AGENT_TASK_DEPTH=1")
	cmd.Stdin = bytes.NewReader(encoded)
	cmd.Stdout = io.Discard
	var stderr taskStderr
	cmd.Stderr = &stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("subagent %q session %q: %w: %s", args.Name, childID, err, stderr.Text())
	}
	store, err := localfile.New(h.config.SessionDirectory)
	if err != nil {
		return "", fmt.Errorf("open child session: %w", err)
	}
	var answer string
	var input, output int64
	var cursor sessionstore.Sequence
	for {
		page, err := store.Items(ctx, session.ID(childID), cursor, 256)
		if err != nil {
			return "", fmt.Errorf("inspect child session %q: %w", childID, err)
		}
		for _, item := range page.Items {
			if item.Kind != sessionstore.ItemModelResponse {
				continue
			}
			response := item.Data.(sessionstore.ModelResponse).Response
			input += response.Usage.InputTokens
			output += response.Usage.OutputTokens
			if response.Failure != nil {
				return "", fmt.Errorf("subagent %q: %s: %s", args.Name, response.Failure.Code, response.Failure.Message)
			}
			answer = ""
			var final strings.Builder
			hasToolCall := false
			for _, part := range response.Output {
				if part.Type == llm.ItemToolCall {
					hasToolCall = true
				}
				if part.Type != llm.ItemMessage {
					continue
				}
				message, ok := part.Data.(llm.Message)
				if ok && message.Role == llm.RoleAssistant && message.Phase != "commentary" {
					final.WriteString(message.Text)
				}
			}
			if !hasToolCall && response.Stop == llm.StopComplete {
				answer = final.String()
			}
		}
		if !page.More {
			break
		}
		cursor = page.NextAfter
	}
	if strings.TrimSpace(answer) == "" {
		return "", fmt.Errorf("subagent %q session %q finished without a final answer", args.Name, childID)
	}
	return fmt.Sprintf("Subagent %s (session %s; input_tokens=%d output_tokens=%d):\n%s", args.Name, childID, input, output, answer), nil
}
