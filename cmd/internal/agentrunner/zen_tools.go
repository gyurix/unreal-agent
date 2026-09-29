package agentrunner

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// Zen's free tool-using route expects the conventional lowercase bash/read
// names. These aliases are real harness tools, not decorative gate markers.
type zenTools struct {
	tool.Registry
	bash tool.Translator
}

func newZenTools(base tool.Registry) (tool.Registry, error) {
	bash, ok := base.Resolve(tool.BashName)
	if !ok {
		return nil, errors.New("Zen requires the Bash tool")
	}
	return &zenTools{Registry: base, bash: bash}, nil
}

func (current *zenTools) StaticDefinitions() []tool.Definition {
	base := current.Registry.StaticDefinitions()
	result := make([]tool.Definition, 0, len(base)+1)
	for _, definition := range base {
		if definition.Tool.Name == tool.BashName {
			definition.Tool.Name = "bash"
			result = append(result, definition, tool.Definition{Tool: llm.Tool{
				Type: llm.ToolFunction, Name: "read",
				Description: "Read a UTF-8 file from the workspace or an absolute path.",
				Parameters: map[string]any{
					"type": "object", "properties": map[string]any{
						"path": map[string]any{"type": "string", "description": "File path."},
					}, "required": []any{"path"},
				},
			}})
			continue
		}
		result = append(result, definition)
	}
	return result
}

func (current *zenTools) Resolve(name string) (tool.Translator, bool) {
	switch name {
	case "bash":
		return current.bash, true
	case "read":
		return zenRead{bash: current.bash}, true
	case tool.BashName:
		// Older sessions may contain the canonical tool name.
		return current.bash, true
	default:
		return current.Registry.Resolve(name)
	}
}

type zenRead struct{ bash tool.Translator }

func (current zenRead) Translate(ctx tool.Context, call llm.ToolCall) tool.CallStatus {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &args, json.RejectUnknownMembers(true)); err != nil {
		return tool.CallStatus{Error: fmt.Sprintf("decode read arguments: %v", err)}
	}
	if strings.TrimSpace(args.Path) == "" || strings.ContainsRune(args.Path, 0) {
		return tool.CallStatus{Error: "read path must be a nonempty path without NUL"}
	}
	// POSIX single-quoting and -- make paths with quotes or leading dashes safe.
	command := "cat -- '" + strings.ReplaceAll(args.Path, "'", "'\\''") + "'"
	encoded, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		return tool.CallStatus{Error: fmt.Sprintf("encode read command: %v", err)}
	}
	call.Name, call.Arguments = tool.BashName, string(encoded)
	return current.bash.Translate(ctx, call)
}

func (current zenRead) TranslateResult(callID string, status tool.CallStatus, operations []operation.Operation) (llm.ToolResult, error) {
	return current.bash.TranslateResult(callID, status, operations)
}
