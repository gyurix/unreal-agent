# Zen prompt-gate probe (2026-09-29)

Scope: genuine OpenCode 1.18.33 CLI, `opencode/mimo-v2.6-flash-free`, and a temporary `experimental.chat.system.transform` plugin replacing the system string. Runs used isolated OpenCode configuration, a fresh session per probe, and `BUN_CONFIG_VERBOSE_FETCH=curl` to inspect upstream HTTP status. No product client, bridge, or default prompt was changed.

## Exact passing system payloads

**Title agent, no tools:** shortest observed two-span reduction of OpenCode's title prompt is **74 characters**:

```text
You are a title generator. You output ONLY a thread title. Never use tools
```

The genuine OpenCode request body captured for this case was:

```json
{"model":"mimo-v2.6-flash-free","max_tokens":32000,"temperature":0.5,"messages":[{"role":"system","content":"You are a title generator. You output ONLY a thread title. Never use tools"},{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}
```

It returned HTTP 200 twice. Removing its final `s` or the character before `Never` yielded HTTP 403; removing `a`, `You output ONLY a thread title.`, `Never`, or `use tools` also failed. This is a **shortest observed** payload under the recorded reductions, not a proof of global minimality across arbitrary text.

**Build agent, tools present:** the shortest observed system payload is the **empty string** (`"content":""`), with user content `"hi"` and OpenCode's ordinary tool definitions in the request body. It returned HTTP 200. The existing Unreal Agent Harness preamble also returned HTTP 200. Conversely, inserting the 74-character title payload into the build-agent request returned HTTP 403. Thus the title marker is context-specific and must not be treated as a universal gate token.

## Harness-taste candidate, not wired

OpenCode-only analysis of the existing preamble highlighted its second-person, engineering-literal voice; turn persistence; batching of independent asynchronous calls; placeholder and heartbeat semantics; and workspace/data-size rules. A separate OpenCode-only critique identified dropped causal explanations and sleep/termination semantics in an earlier draft. The resulting tool-using candidate is:

```text
You are a tool-using engineering agent on Unreal Agent Harness in an isolated sandbox. "Unreal" names the harness; it does not imply an Unreal Engine project.

Work in turns: each turn reads the full conversation and replies with text, tool calls, or both. The full conversation is re-sent next turn, so batch independent tool calls now rather than serializing them across turns. Issuing a call never blocks you; many can run at once. Results arrive asynchronously and wake the next turn, while unfinished calls show placeholders. Do not repeat pending calls. With calls running, end the turn to sleep until a result arrives; a ten-minute heartbeat is a chance to check health, not proof of progress. With no calls running, ending the turn ends the session, so keep working until the goal is met.

Use tools when the task needs them. Verify concrete outcomes before claiming success. Save output files to the workspace root. Sample large datasets before processing all of them.
```

This 978-character system payload returned HTTP 200 in two build-agent calls. A separate real `bash` tool exercise (`pwd`) completed with two HTTP 200 model turns and returned the workspace path. It did not include the title marker: an earlier candidate that quoted the marker passed in title-agent requests but failed with HTTP 403 in build-agent requests.

These observations apply to genuine OpenCode requests with its native request envelope and tools. They do **not** establish that another client's request will pass. No spoofing or bypass was added to this repository.
