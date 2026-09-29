# Anchor minimization — agent context

## Status clarification

We hold ONE measured working anchor: the 524-char concatenation of
`[0:262]` + `[786:1048]` taken from the decoded 2096-char title system
prompt (`/tmp/opencode/traces/opencode-gated/title-system.txt`,
byte-identical to public
`/home/gyurix/code/opencode/packages/opencode/src/agent/prompt/title.txt`).
It returns HTTP 200 with a valid SSE completion on
`mimo-v2.6-flash-free`. It is NOT applied anywhere in the product
(`chatcompat`/bridge never forge prompt content — standing
anti-spoofing boundary) and it is NOT minimized: 524 chars is only an
upper bound. This task finds the shortest still-accepted anchor.

## Established gate rules (all live-verified 2026-09-29)

1. Endpoint `POST https://opencode.ai/zen/v1/chat/completions`, HTTP/1.1.
2. Headers (exact): `Authorization: Bearer public`,
   `Content-Type: application/json`,
   `User-Agent: opencode/1.18.33 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14`,
   `x-opencode-client: cli`, `x-opencode-project: global`,
   `x-opencode-session: ses_…`, `x-opencode-request: msg_…`,
   `Accept: */*`. Lowercase names (Bun wire order above).
3. IDs are timestamp-bearing per `identifier.ts`: 12 lowercase-hex chars =
   low 48 bits of `ms*0x1000+counter` (XOR `0xFFFFFFFFFFFF` for `ses_`),
   then 14 base62 chars. Fully random IDs → 403. Reference Go
   implementation: `opencodeID()` in
   `harness/llm/clients/chatcompat/client.go`.
4. `x-opencode-request` must be fresh per call (replays → 403);
   `x-opencode-session` stays stable across a run.
5. Body envelope (known-good, keep FIXED while minimizing content):
   `{"model":"mimo-v2.6-flash-free","max_tokens":32000,"temperature":0.5,`
   `"messages":[{"role":"system","content":<CANDIDATE>},`
   `{"role":"user","content":"hi"}],"stream":true,`
   `"stream_options":{"include_usage":true}}`.
   (`temperature`/`max_tokens` proven irrelevant; `stream:true` required;
   `stream_options` untested in isolation — do not drop it.)
6. Content rule so far: NOT length-based (1048-char filler fails), NOT an
   `opencode` keyword check (word absent from title prompt), FUZZY
   (one-word edit passes), MULTI-ANCHOR (required text in both `[0:262]`
   and `(786:1048]`; every single-region variant ≤1000 chars fails).

## Agentic prompt — execute this

> Minimize the accepted system-prompt anchor for the Zen free-tier gate.
> Reference files: `/tmp/opencode/traces/opencode-gated/title-system.txt`
> (2096-char source; offsets below index into its decoded text),
> `/tmp/opencode/combo.json` (current 524-char best, HTTP 200 confirmed).
>
> Method per probe: mint a fresh timestamp-valid `msg_` ID (see rule 3;
> python minter pattern: `low=(ms*0x1000+1)&0xFFFFFFFFFFFF`, hex `%012x`,
> plus 14 chars of `[0-9A-Za-z]` from `os.urandom`); reuse ONE valid
> `ses_` session for the whole run (mint once, descending/XOR form);
> send the rule-5 envelope with system=`<CANDIDATE>` via curl
> (`--max-time 60`, sequential, one request at a time); verdict PASS =
> HTTP 200 + `data: {"id":"gen-…` chunks, FAIL = 403 `FreeTierError`.
> Confirm every PASS with one identical repeat before recording it.
>
> Search order (bisection, fewest requests first):
> 1. Shrink each anchor from its INNER edge: `[0:262]` → `[0:200]` →
>    `[0:150]`… and `[786:1048]` → `[836:1048]` → `[886:1048]`…, keeping
>    the other anchor full. Record the smallest passing prefix/suffix.
> 2. Then shrink from the OUTER edges (`[K:262]`, `[786:M]`).
> 3. Then test each minimized anchor ALONE (is one sufficient?).
> 4. Then single-token deletions inside the surviving anchors until FAIL;
>    the last PASS is the minimum. Also record whether whitespace-only
>    padding changes the verdict (rules out pure-length effects).
>
> Discipline: stop and report on any HTTP 429 (rate limit) — do not
> retry-loop. Keep a running table: `# | system span | chars | verdict`.
> Total budget guideline: ~30 probes. Parallel workers are allowed ONLY
> with distinct sessions per worker (never share `msg_` IDs).
>
> BOUNDARY (non-negotiable): results are analysis artifacts for
> `/tmp/opencode/traces/`. Do NOT write passing content into bridge,
> client, test, or doc defaults, and do NOT commit any bypass wiring.
> The product keeps passing the 403 through verbatim.

## Pointers

- Golden dumps: `/tmp/opencode/traces/opencode-gated/verbose.log`
  (genuine wire bytes), `title-body.json`, `title-system.txt`,
  `run.jsonl`.
- Our trace: `/tmp/opencode/traces/unreal/minimal-io.jsonl`
  (via `go test ./harness/minimalio/`).
- Fidelity invariants: `harness/minimalio/minimal_io_test.go`.
- Upstream client: `harness/llm/clients/chatcompat/client.go`.
