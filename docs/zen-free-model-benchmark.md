# Zen free-model and harness benchmark — 2026-09-29

## Method

The user prompt is the 661-byte `AgenticPrompt` in
`harness/minimalio/minimal_io_test.go` (SHA-256
`ddcf1a44d20518da8c695e290e4a5547e518a765e38587ede4155d1eda175fda`).
It asks for `/tmp/test-agentic-work`, a `London` answer file, ten parallel
checking/copying agents, five verification agents, and cleanup. The system
prompt is the exact 977-byte trimmed `harness/contextbuilder/prompts/preamble.md`.
The free-model inventory came from live `GET /zen/v1/models` (11 IDs containing
`free`). Each bridge probe sent this exact system and user text plus **real**
`bash`/`read` function definitions; no no-tool control was used in this survey.
All probes were keyless with `max_attempts=1`; timings are single-run wall
times, not latency distributions. Model output, usage, errors, and finish reason
were read from actual HTTP bodies. A 200 first turn proves acceptance, not task
completion. No 403 occurred in these tool-enabled probes.

The first survey imposed `max_tokens:512`, which artificially cut off the two
Muse models and Nemotron 3.5. Muse was then routed to its actual Zen Responses
endpoint and retested without that cap. An uncapped Nemotron 3.5 retry ended
with incomplete SSE and a bridge 502 after 56.86 seconds; a 1,024-token retry
returned 200 with a `bash` call. The table uses the corrected result.

| Model | Endpoint / first-turn result | Wall time | Input / output tokens | Tool result or issue |
| --- | --- | ---: | ---: | --- |
| `jev-1.13-free` | System One; Chat Completions probe 500 | 0.53 s | — | Not an agent/completions model; runner now rejects this use locally. |
| `deepseek-v4-flash-free` | Chat Completions 400 | 0.29 s | — | Upstream `Model is unavailable.` |
| `muse-spark-1.3-contributor-free` | Responses 200 | 12.21 s | 909 / 735 | `bash` call. Chat Completions incorrectly returned 500 before endpoint fix. |
| `muse-spark-1.2-contributor-free` | Responses 200 | 3.89 s | 909 / 514 | `bash` call. Chat Completions incorrectly returned 500 before endpoint fix. |
| `mimo-v2.6-flash-free` | Chat Completions 200 | 4.08 s | 502 / 178 | `bash` call. |
| `space-bunny-free` | Chat Completions 200 | 1.31 s | 786 / 79 | `bash` call. |
| `longcat-2.5-preview-free` | Chat Completions 200 | 4.42 s | 536 / 68 | `bash` call. |
| `mimo-v2.5-free` | Chat Completions 200 | 7.46 s | 665 / 201 | `bash` call. |
| `ling-3.0-flash-fin-free` | Chat Completions 400 | 0.33 s | — | Upstream `Endpoint is unavailable.` Genuine OpenCode also got 400. |
| `nemotron-3-ultra-free` | Chat Completions 200 | 31.18 s | 685 / 253 | `bash` call. |
| `nemotron-3.5-lightning-free` | Chat Completions 200 at 1,024 cap | 47.12 s | 626 / 543 | `bash` call; uncapped bridge 502 due incomplete SSE. |

The three 400/500 cases are **not** evidence of a prompt gate. [Official Zen
documentation](https://opencode.ai/docs/zen/) assigns Jev to
`/zen/v1/systemone` rather than an agent endpoint.
The two unavailable models returned provider errors; the bridge did not convert
them to success. A first client timeout at 90 seconds for Nemotron 3.5 was
followed by a capped 200 response at 37.99 seconds, then the uncapped 502 and
a capped 200 response with a tool call at 47.12 seconds. The provider is
variable; these observations can change with availability.

## End-to-end execution and token comparison

All three runs used `mimo-v2.6-flash-free` and the same 661-byte user prompt.
The custom OpenCode run used an isolated `experimental.chat.system.transform`
plugin replacing the system text with the 977-byte harness prompt. The default
OpenCode run used its built-in system prompt. The Go runner did not invoke
OpenCode. OpenCode's `step_finish.tokens` and the runner's persisted
`model_response.Usage` are provider-reported; OpenCode input is
`input + cache.read + cache.write`, and output includes reasoning. Tool lists,
cache state, working-directory permissions, and orchestration differ, so these
are **not** controlled model-quality or cost rankings.
OpenCode's CLI additionally wraps the supplied argument in a quoted user
content part; the runner sends the string directly.

| Run | Outcome | Wall time | Model turns | Reported model input | Cached input | Reported output | Calls |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Go runner + harness prompt | Completed file/copy/verification/cleanup sequence | 49.86 s | 8 | 27,884 | not captured in that run | 2,814 | 20 `bash` |
| OpenCode + harness prompt | Completed exact task with real Task subagents | 121.26 s | 8 | 86,611 | 73,344 | 3,679 | 15 `task`, 3 `bash`, 1 `write` |
| OpenCode + default prompt | Incomplete: timed out after 190 s | ≥190 s | 3 completed | 24,675 so far | 17,600 so far | 1,360 so far | 8 completed `task`, 1 `bash`, 1 `write` |

The OpenCode totals above are **main-agent model steps only**; Task subagent
tokens are not included. The timed-out default run has no valid whole-task token
total. On the *first* model turn, reported input was 727 tokens (Go runner),
5,997 (OpenCode + harness prompt), and 8,095 (OpenCode default). Thus the
OpenCode custom and default envelopes sent roughly 8.2× and 11.1× as many
first-turn input tokens as this runner. This primarily reflects tool schemas
and built-in context, not just the system prompt; cache treatment differs.

In the pre-Task runner benchmark, the first two independent `bash` calls raced: the write ran before
`mkdir` and failed with exit code 1. The model noticed and repaired step 2.
All ten agent-named folders and copies were made and later removed, but the
runner launched **zero actual subagents**. It used parallel shell commands and
then falsely described them as agents in its final answer. This is a real
capability/correctness gap. OpenCode's custom run used ten actual Task calls and
five verification Task calls, then removed the folder. The default run timed
out after eight Task calls; its test-generated folder was removed afterward.

Separate real runner checks: `mimo-v2.6-flash-free` called `read` on a local
file and used the result on its next model turn (both 200); Muse Spark 1.3 did
the same through `/responses`. This proves tool execution and continuation,
not merely accepted tool schemas. The full Go test suite, race suite, vet,
format check, and build passed after integration.

## Native Task implementation — 2026-09-29

The CLI now exposes a real `task` tool backed by isolated child runner
processes and durable `task-<operation-id>` sessions; no OpenCode installation
is involved. The tool is omitted from child requests, so nesting cannot run
away. A process semaphore allows ten concurrent children, with at most 64
pending operations. Parent operations record child handles, final answers,
provider-reported child token usage, and concrete failures. Parent shutdown
interrupts active children.

Direct execution checks after this change:

| Check | Observed result |
| --- | --- |
| Local SSE provider, ten `task` calls | Parent exit 0; ten separate child session files; peak ten concurrent child HTTP requests; `task` absent from child tool schemas. |
| Local SSE provider, child HTTP 400 | Child failure appeared in parent tool result; parent exit 0 after handling it. No fake success. |
| Live Zen `mimo-v2.6-flash-free`, one Task | Parent exit 0; one persisted child session; final answer quoted child `Ready.`; no 403. |
| Exact 661-byte prompt, live Zen | Fifteen child session files created; timed out at 210 seconds. The requested `/tmp/test-agentic-work` path was absent afterward. No whole-task success, speed, token, or absence-of-403 claim can be made for this run. |

The previous table remains **pre-Task** baseline evidence and must not be
read as a speed/token comparison for the new implementation. The follow-up
below includes root and child usage but remains single-run evidence.

## Native Task performance follow-up — 2026-09-29

The performance bottleneck was not process startup: a local ten-child SSE run
finished in 0.36 seconds including 0.25 seconds of simulated child inference.
The live trace instead showed 12 root model turns, often one per asynchronously
completed child, and 45 child model turns. Some children made redundant reads
and checks. The task-specific fix adds `task_batch`, which submits separate
durable child operations but delivers one result only after the entire batch
finishes. The child instruction now encourages safe command batching and concise
reports; the root preamble discourages polling. Batch results are bounded per
child with an explicit truncation marker, while complete child histories remain
in their session files.

Three **single-run**, keyless `mimo-v2.6-flash-free` executions used the same
661-byte prompt, `max_attempts=1`, and 15 real subagents. The first is the
previous native-Task implementation; the second adds `task_batch`; the third
also adds the child instruction. All three exited 0, persisted 15 completed
child operations, and removed `/tmp/test-agentic-work`; stderr and persisted
model failure counts were zero. No 403 occurred in these completed runs.

| Harness version | Wall time | Root turns | Root input / output tokens | Child turns | Child input / output tokens | Total input / output tokens |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Individual `task` calls | 123.26 s | 12 | 64,579 / 3,013 | 45 | 48,439 / 9,192 | 113,018 / 12,205 |
| `task_batch` | 127.79 s | 5 | 28,296 / 2,819 | 43 | 50,607 / 9,474 | 78,903 / 12,293 |
| `task_batch` + concise child guidance | 100.93 s | 5 | 20,616 / 2,515 | 38 | 40,081 / 5,655 | 60,697 / 8,170 |

The final run used **46.3% fewer reported input tokens** and **33.1% fewer
reported output tokens** than the individual-Task run, and its wall time was
18.1% lower. The batch-only run was slower despite fewer root turns: free-model
latency varied. These are observations, **not** repeat-sample latency estimates
or guaranteed savings. The original 49.86-second shell-only run remains
non-comparable because it did not run real agents. The previous 210-second
timeout remains a reliability warning.

## Next actions

1. Make tool-dependency scheduling explicit. `mkdir` and file write must not
   be parallel; the observed first-turn race cost a repair turn.
2. Keep per-model endpoint routing and a current free-model availability
   probe. Do not retry Jev as chat or treat upstream 400 as a gate failure.
3. Run repeated, isolated benchmarks with warm/cold cache separation; report
   tail latency and all child usage, not root tokens alone.
4. Investigate the earlier 210-second exact-prompt timeout and add a bounded,
   configurable per-task wall time before claiming reliable completion.
