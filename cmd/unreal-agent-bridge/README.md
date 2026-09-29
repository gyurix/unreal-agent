# unreal-agent-bridge

An OpenAI-compatible Chat Completions server backed by one configured
harness LLM provider. It lets any independent OpenAI-compatible client use
the same authorized provider credentials the harness uses.

It does not create free-model access: the upstream provider, credentials,
quotas, and rate limits still apply. The bridge only translates protocols.

## Run

```sh
export UNREAL_HARNESS_LLM_PROVIDER="openrouter"
export OPENROUTER_API_KEY="..."
export BRIDGE_MODELS="z-ai/glm-4.5-air:free,openai/gpt-oss-20b:free"
export BRIDGE_API_KEY="local-secret"
export BRIDGE_ADDR="127.0.0.1:8080"
go run ./cmd/unreal-agent-bridge
```

Providers: `openai`, `openai-codex`, `openrouter`, `fireworks`, `ollama`,
plus `opencode-zen` (OpenAI Chat Completions upstream at
`https://opencode.ai/zen/v1`, keyless for its free tier; an optional
`UNREAL_HARNESS_LLM_API_KEY`/`OPENCODE_ZEN_API_KEY` is forwarded when set)
(see `cmd/unreal-agent-runner/README.md` for the other providers'
credentials).
`UNREAL_HARNESS_LLM_BASE_URL` and `UNREAL_HARNESS_LLM_MAX_ATTEMPTS`
behave as in the runner.

Keyless example (no API key needed):

```sh
export UNREAL_HARNESS_LLM_PROVIDER="opencode-zen"
export BRIDGE_MODELS="space-bunny-free"
export BRIDGE_ADDR="127.0.0.1:8080"
go run ./cmd/unreal-agent-bridge
```

## Zen request shape (live-probed 2026-09-29)

For `mimo-v2.6-flash-free`, a genuine OpenCode 1.18.33 CLI request and a
standalone bridge request both returned HTTP 200 with the exact 977-character
system prompt in `harness/contextbuilder/prompts/preamble.md`. OpenCode is
**not** needed to run the bridge. The upstream currently checks more than the
prompt: `stream: true`, timestamp-valid fresh `ses_`/`msg_` IDs, OpenCode
identity headers, and functional tool definitions named `bash` and `read`
were sufficient in controlled replay. With identical prompt and headers,
`stream: false`, no tools, or `bash` alone returned 403 `FreeTierError`.
Tool descriptions and parameter schemas could be short; `task` was not
required. This is an observed sufficiency result, not a guarantee the
provider's access policy will remain unchanged. The bridge does not insert
fake tools: the caller must supply and execute real `bash`/`read` tools.

The Zen adapter now sends streaming upstream and decodes SSE content, usage,
and fragmented tool calls. The bridge can still return ordinary JSON or SSE
to its own caller. A live two-turn bridge run returned a `bash` `pwd` call,
then accepted its tool result and produced the final answer, both HTTP 200.
An otherwise identical no-tools request returned the upstream 403 intact.

Run without OpenCode (Go is the only build dependency):

```sh
go build -o bin/unreal-agent-bridge ./cmd/unreal-agent-bridge
UNREAL_HARNESS_LLM_PROVIDER=opencode-zen \
  BRIDGE_MODELS=mimo-v2.6-flash-free \
  bin/unreal-agent-bridge
```

Send `preamble.md` as the system message, and supply actual `bash` and
`read` function tools. The bridge is a protocol adapter, not a tool executor;
the calling agent must run returned tool calls and submit tool results.
The gate investigation and exact prompt are in `docs/zen-prompt-gate.md`.

## Endpoints

- `GET /healthz` — unauthenticated probe, returns `ok`.
- `GET /v1/models` — lists `BRIDGE_MODELS`.
- `POST /v1/chat/completions` — Chat Completions, `stream: true` supported
  as SSE with a terminal `data: [DONE]`.

If `BRIDGE_API_KEY` is set, clients must send
`Authorization: Bearer $BRIDGE_API_KEY`. Upstream credentials are never
derived from it.

## Example

```sh
curl -s http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer $BRIDGE_API_KEY"
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $BRIDGE_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"z-ai/glm-4.5-air:free","messages":[{"role":"user","content":"hi"}]}'
```

## Supported

- System, user, assistant, tool, and function messages.
- Function tools with `tool_calls` round trips (IDs preserved).
- `max_tokens` / `max_completion_tokens` mapped to the upstream limit.
- `tool_choice: "none"` drops tools; other values keep them.
- Usage (`prompt_tokens`, `completion_tokens`, `total_tokens`).
- Upstream HTTP status passthrough with `Retry-After` on 429.
- Cancellation via client disconnect.

## Limitations

- The bridge buffers each upstream completion; downstream `stream: true`
  fans the completed response out as SSE chunks, not token-by-token.
- Only text content parts are supported; image/audio parts are rejected
  with 400 because `llm.Message` carries text only.
- Only function tools are supported; other tool types are rejected.
- Sampling fields (`temperature`, `top_p`, etc.) are accepted but ignored:
  the Responses backend does not expose them.
- Reasoning items have no Chat Completions equivalent and are omitted.
