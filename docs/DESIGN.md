# code-link — design (v0.1)

A macOS menu-bar app that lets Claude Code and Codex run on your own models
(Flare, vLLM, SGLang) with a native-level experience: every Claude Code tool,
auto mode with a logprob decision head, PDFs and images, web search, MCP tool
search. It never touches your normal Claude Code or Codex setup: only the
sessions it launches are pointed at it.

Native macOS: Swift 6, SwiftUI `MenuBarExtra`, PDFKit, Vision, Keychain,
`SMAppService`. Behaviour is specified here; go-llm-proxy and Cletus are
references only (they prove each rule works), not code to copy.

## 1. Pieces

```
Menu bar app (CodeLink.app, SwiftUI, LSUIElement)
  ├─ menu: status · Launch Claude Code… · Launch Codex… · model · Copy command · Settings… · Quit
  ├─ Settings window: Backends · Models & roles · Documents · Web search · Launch · Activity
  ├─ Launcher: writes a login-shell script, opens it in iTerm or Terminal
  └─ CodeLinkEngine (Swift package, no UI)
       local HTTP server 127.0.0.1:<port>  ←  claude / codex sessions
         /v1/messages (+count_tokens), /v1/responses, /v1/chat/completions, /v1/models, /healthz
       → Chat Completions on the configured backends (SSE)
```

Layout:

```
Package.swift            CodeLinkEngine library + tests (swift test runs them)
Sources/CodeLinkEngine/  Server/, Translate/Messages, Translate/Responses,
                         AutoMode/, Documents/, WebSearch/, Backends/, Config/
Tests/CodeLinkEngineTests/
App/                     SwiftUI app sources (MenuBar, Settings, Launcher, Keychain)
project.yml              XcodeGen: CodeLink.app target depending on the package
scripts/                 build.sh (xcodegen + xcodebuild), conformance runner
docs/
```

HTTP server: Hummingbird 2 (SwiftNIO) — streaming responses, keep-alive,
no buffering. Everything else Apple frameworks or Foundation.

## 2. Engine public API (the contract between the app and the engine)

```swift
public struct EngineConfig: Codable, Sendable {      // persisted as JSON (§6)
    public var port: Int                              // default 18765
    public var backends: [Backend]
    public var models: [Model]
    public var roles: Roles
    public var documents: DocumentsConfig   // paddleOCR URL?, local: appleVision | textOnly | off
    public var webSearch: WebSearchConfig   // provider: tavily | brave | nil, keyRef
}
public struct Backend: Codable, Sendable, Identifiable {
    public var id: String                 // "cn3-flare"
    public var kind: BackendKind          // .openAICompatible (vLLM/SGLang/Flare) | .anthropic (native Messages)
    public var baseURL: URL               // http://192.168.13.33:8000/v1
    public var apiKeyRef: String?         // Keychain item name, never the key
}
public struct Model: Codable, Sendable, Identifiable {
    public var id: String                 // name Claude Code/Codex see, e.g. "GLM-5.3"
    public var backendID: String
    public var upstreamModel: String      // name the backend expects
    public var contextWindow: Int?        // nil = detect from /v1/models (max_model_len)
    public var autoCompactAt: Int?        // nil = 85% of contextWindow
    public var vision: Bool?              // nil = detect/assume false
    public var logprobs: Bool?            // nil = probe once (§4.3)
    public var thinkingOff: JSONValue?    // merged into the chat request when a client disables thinking
}
public struct Roles: Codable, Sendable {  // model ids
    public var main: String               // Claude Code default (Sonnet slot) and ANTHROPIC_MODEL
    public var opus: String?              // nil = main
    public var small: String?             // Haiku / background tasks; nil = main
    public var decision: String?          // auto-mode classifier; nil = main if it has logprobs
    public var vision: String?            // describes images for a non-vision main; nil = none
    public var codex: String?             // nil = main
}
public enum EngineState: Sendable { case stopped, starting, running(port: Int), failed(String) }
public final class Engine: Sendable {
    public init(config: EngineConfig, secrets: SecretStore, log: EventLog, accessToken: String)
    public func start() async throws
    public func stop() async
    public func apply(_ config: EngineConfig) async   // live reload, no restart for routing changes
    public var state: EngineState { get async }
    public let accessToken: String                    // random per install (app keeps it in the Keychain); the only credential sessions get
    public func probe(_ backend: Backend) async -> BackendProbe   // reachable, models, logprobs, vision hints
}
public protocol SecretStore: Sendable { func secret(_ ref: String) -> String? }   // app: Keychain
public protocol EventLog: Sendable { func record(_ e: EngineEvent) }             // app: Activity pane
```

Requests must carry `Authorization: Bearer <accessToken>` (or `x-api-key`);
anything else gets 401. The server binds 127.0.0.1 only.

## 3. Launching (never disturbs the normal setup)

Launch = a script in `~/Library/Caches/CodeLink/launch/` run by the user's
login shell (`#!$SHELL -li`, so `~/.zshrc` PATH applies) in iTerm (session
command = script path) or Terminal (`do script`), in a folder the user picks
(remembered). The script `cd`s, runs the tool, deletes itself, and keeps the
window open on a non-zero exit. The same command is offered as "Copy command".

Claude Code environment — only for that process; `~/.claude` (settings,
MCP servers, skills, plugins, history, subscription login) is never written:

```
ANTHROPIC_BASE_URL=http://127.0.0.1:<port>
ANTHROPIC_AUTH_TOKEN=<accessToken>
ANTHROPIC_MODEL=<main>  ANTHROPIC_DEFAULT_SONNET_MODEL=<main>
ANTHROPIC_DEFAULT_OPUS_MODEL=<opus>  ANTHROPIC_DEFAULT_FABLE_MODEL=<opus>  ANTHROPIC_DEFAULT_HAIKU_MODEL=<small>  ANTHROPIC_SMALL_FAST_MODEL=<small>
ANTHROPIC_DEFAULT_{SONNET,OPUS,FABLE,HAIKU}_MODEL_NAME=<the same ids, so /model shows them>
CLAUDE_CODE_AUTO_MODE_SERVER=0  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1  API_TIMEOUT_MS=900000
CLAUDE_CODE_MAX_CONTEXT_TOKENS=<window>  CLAUDE_CODE_AUTO_COMPACT_WINDOW=<autoCompactAt or 85%>
claude --permission-mode auto --settings ~/Library/Application\ Support/CodeLink/claude-settings.json
```

`claude-settings.json` = `{"attribution":{"commit":"","pr":""}}` (passed by
path). Not set: `DISABLE_PROMPT_CACHING`, `CLAUDE_CODE_DISABLE_1M_CONTEXT`
(Claude-only; they make Claude Code warn).

Codex — `-c` overrides only, `~/.codex/config.toml` untouched:
`codex --approve-for-me -c model="<codex>" -c model_provider="code-link"
-c model_context_window=<window> -c model_providers.code-link.{name="code-link",
base_url="http://127.0.0.1:<port>/v1", wire_api="responses",
env_key="CODE_LINK_TOKEN"}` with `CODE_LINK_TOKEN=<accessToken>` in the env.

Missing CLI → the menu says so with the install link (Claude Code:
https://docs.anthropic.com/en/docs/claude-code/setup ; Codex:
https://github.com/openai/codex). Model names Claude Code sends that aren't
configured (e.g. `claude-sonnet-5`, `claude-haiku-*`, `[1m]` suffixes) map to
the matching role (opus/haiku/sonnet → opus/small/main), else main.

## 4. `/v1/messages` — Anthropic Messages on a Chat Completions backend

Anthropic-kind backends get the request passed through (model name
rewritten). Everything else is translated. Rules (each proven in
go-llm-proxy `internal/handler/messages*.go` / Cletus
`dataproxy/internal/chat/messages*.go`):

### 4.1 Request
- System blocks joined in order into one system message (keep the first
  attribution block as-is; byte-stable for prefix caching). Mid-conversation
  `system` messages → `<system-reminder>` text.
- Per message: text, images (base64/url → `image_url`), `tool_use` →
  assistant `tool_calls`, `tool_result` → `role: tool` messages emitted
  FIRST, then the rest of that user message. Images/documents inside a
  `tool_result` go into a user message right after the tool messages.
  `is_error` → prefix the tool text.
- Assistant `thinking` blocks → `reasoning_content` (and `reasoning`) on the
  assistant message; `redacted_thinking` dropped.
- Tools: function tools translated; `disable_parallel_tool_use` →
  `parallel_tool_calls: false`; `tool_choice` mapped.
- **Tool search:** a tool with `defer_loading: true` is sent only after a
  `tool_reference` block in some `tool_result` names it; never send
  `DeferredToolPlaceholder`; a `tool_reference` becomes the text
  `Tool <name> is now loaded and can be called.` (Without this, MCP tools
  behind tool search can never be called.)
- `thinking: {type: disabled}` → the model's `thinkingOff` object (none by
  default — on GLM-5.3 with vLLM's reasoning parser `enable_thinking:false`
  BREAKS output; `reasoning_effort: low` works). `output_config.effort` →
  `reasoning_effort` (max → xhigh). `output_config.format` / `output_format`
  json_schema → `response_format: {type: json_schema, json_schema: {name:
  "output", schema, strict: true}}`. `stop_sequences` → `stop`.
- `max_tokens` clamped to fit the context window when it is known.
- Server tools: `web_search_*` → §4.5; others dropped.
- Documents → §4.4.

### 4.2 Response
- Non-streaming: content blocks (thinking from `reasoning_content` /
  `reasoning`, text, tool_use), `stop_reason` (`tool_use` whenever tool
  calls were made; `stop_sequence` + `stop_sequence` when the backend says
  which matched; `max_tokens`; `end_turn`), usage with
  `cache_read_input_tokens` from `prompt_tokens_details.cached_tokens`
  (subtracted from input).
- Streaming: `message_start` immediately; `ping` every 15 s until output
  and while idle; correct block indices (close text before a thinking block
  opens, one block per tool call keyed by index); `message_delta` +
  `message_stop` only on a real finish. A stream that ends without a finish
  reason, `[DONE]`, or on a read error → `event: error`
  (`overloaded_error`) so Claude Code retries. No line-length limit below
  16 MiB.
- Strip `<think>…</think>` that a backend puts in content, and a lone
  leading `</think>`.
- Errors: Anthropic shape `{type:"error",error:{type,message}}`; 400
  `invalid_request_error`, 401/403 from upstream → 502, 404
  `not_found_error`, 413 `request_too_large`, 429 `rate_limit_error`
  (+ `retry-after`), 503/529 `overloaded_error`; context-length errors
  rewritten to `prompt is too long: N tokens > M maximum`; if nothing was
  streamed yet, return the real status, not 200 + error event.
- `count_tokens`: estimate (~4 chars/token, media counted separately, not
  by base64 length).

### 4.3 Auto mode (decision head)
Claude Code's own classifier requests (it can't use server-side review
through a gateway) are non-streaming Messages calls, first for
`claude-sonnet-5`, then the session model. Detect: system text contains
`You are a security monitor for autonomous AI coding agents` AND
`stop_sequences` contain `</severity>` / `</block>` or the system text
contains `<severity>N</severity>` / `<block>`. Stage 1 = has stop
sequences, stage 2 = not. Answer on the **decision** model regardless of
the requested name:

```json
{"model": <upstream>, "max_tokens": 1, "temperature": 0, "logprobs": true, "top_logprobs": 20,
 "continue_final_message": true, "add_generation_prompt": false,
 "messages": [system, user (verbatim), {"role":"assistant","content":"<severity>"}]}
```

Scores 0–100 are single tokens on GLM-5.3: from
`choices[0].logprobs.content[0].top_logprobs` keep integer tokens 0–100,
normalise; p_block = Σp(v>50); N = round(E[v]) forced to the p_block side
(≥ threshold 0.5 → ≥51, else ≤49; never 50). Read logprobs, never
`message.content` (reasoning parsers file the token under `reasoning`).
Stage 2 with N > 50: a second short call (prefill
`<severity>N</severity><category>`, 16 tokens, stop `</category>`) for the
category. Reply: stage 1 text `<severity>N` with `stop_reason:
stop_sequence`, `stop_sequence: "</severity>"`; stage 2
`<severity>N</severity>[<category>X</category>]`, `end_turn`. Block format
(older Claude Code): prefill `<block>`, tokens yes/no. No logprobs /
numeric mass < 0.5 / error → answer the same request generatively on the
decision model. Logprob support is probed once per model (a 1-token
request with `logprobs`) and cached; Flare currently has none (decision
role should then point at a vLLM/SGLang model). Measured with
GLM-5.3-Flash: 12/12 on the eval set, ~0.3 s per decision.

### 4.4 Documents and images
`document` blocks (top level and inside `tool_result`):
- **PaddleOCR first.** When `documents.paddleOCR` (a PaddleOCR(-VL)
  `/layout-parsing` endpoint) is set and reachable, the whole document
  (PDF, or an image Claude Code sends as a document) goes there: base64 file
  in, page-ordered markdown/text out (per-request options as Cletus uses:
  layout detection on for documents). Reachability is checked with a cheap
  probe and cached for a minute, so an offline server doesn't stall reads.
- **Local fallback** (PaddleOCR unset, unreachable or failing), per
  `documents.local`: `appleVision` = PDFKit text per page, and pages without
  a text layer rendered and read with Vision (`VNRecognizeTextRequest`,
  accurate); `textOnly` = PDFKit only; `off` = a note.
- Result → text block `[document: <title>]\n…` (cap ~600 KB of text;
  in-memory cache by SHA-256 so resent history isn't re-extracted). Failure
  → `[document: <title> could not be read (<reason>)]`.
- `text` / `content` sources → text; `url` sources fetched (http/https,
  size-capped, no redirects to private addresses).

Images (`image` blocks, top level and in `tool_result`):
- Model with vision (`vision: true`, or detected) → `image_url` parts.
- Otherwise, if the `vision` role names a vision model → that model
  describes each image (one short call per image, cached by SHA-256) and the
  description replaces the image: `[image: <description>]`.
- Neither → `[image omitted: <model> has no vision and no vision model is
  configured]`, so the model says so instead of guessing.

### 4.5 Web search
Claude Code's WebSearch is a separate streaming request whose only tool is
`web_search_20250305`. Offer the model a `web_search(query)` function; on a
call, search with the configured provider — Tavily (`POST https://api.tavily.com/search`) or Brave (`GET https://api.search.brave.com/res/v1/web/search`, header `X-Subscription-Token`); key in the Keychain; ~5 results —, stream `server_tool_use`
(`srvtoolu_…`, name `web_search`) + `web_search_tool_result` (results:
`{type: web_search_result, url, title, page_age?, encrypted_content}`), feed
results back to the model and continue; respect `max_uses` (default 5);
usage `server_tool_use.web_search_requests`. No provider/key / error →
`web_search_tool_result_error` (`unavailable` / `too_many_requests` /
`max_uses_exceeded`) and the model answers anyway. Replay earlier
`server_tool_use` + results in history as function call + tool result.

## 5. `/v1/responses` — Codex on a Chat Completions backend

Rules from go-llm-proxy `internal/handler/responses*.go` (Codex 0.163):
- `instructions` + the LEADING run of `system`/`developer` messages → ONE
  system message (joined with blank lines); later developer messages →
  user text `<developer>\n…\n</developer>`.
- Tools: functions; `namespace` tools flattened to `NS__NAME` (collision-safe,
  ≤64 chars) and mapped back with `namespace` on `function_call` items;
  `custom`/freeform tools (apply_patch, exec) → a function with one string
  `input`, returned as `custom_tool_call` items with
  `response.custom_tool_call_input.delta/.done`; `additional_tools` input
  items are tool definitions, not messages (Codex auto-review needs this);
  `web_search` → §4.5's search, emitted as a `web_search_call` item.
- `text.format` json_schema → `response_format`; `reasoning.effort` →
  `reasoning_effort`; reasoning items in input → `reasoning_content` on the
  next assistant message; reasoning streamed as `reasoning` items with
  `reasoning_text` + summary.
- `input_file` / PDF `input_image` → §4.4.
- Streaming events in Codex's order (`response.created`,
  `output_item.added`, deltas, `output_item.done`, `response.completed` with
  usage incl. cached/reasoning tokens). Truncated stream / read error →
  `response.failed`. Errors before any output → real HTTP status with code
  `context_length_exceeded` / `invalid_prompt` / `rate_limit_exceeded` /
  `server_is_overloaded`. `previous_response_id` / `store: true` → 400.
- Codex auto-review requests use model `codex-auto-review`: route to the
  decision model (generative; it wants a JSON outcome).

## 5a. `/v1/chat/completions` — for every other client

For tools that speak Chat Completions (opencode, qwen-code, aider, editor
plugins, scripts) with the same access token:
- Model name → configured model (or tier name: `opus`, `sonnet`, `haiku`,
  and claude-* names as in §3), else main.
- OpenAI-compatible backend: forward the request with the upstream model
  name; stream passes through unchanged (flush per chunk, cancel upstream on
  disconnect), errors pass through with their status.
- Images on a model without vision → §4.4 (vision-role description, else the
  "image omitted" note); `file` parts with PDFs → §4.4 documents.
- Anthropic backend: translate Chat Completions → Messages and the response
  (incl. streaming chunks, tool calls, usage) back — reference go-llm-proxy
  `internal/handler/anthropic_oai_translate.go` / `anthropic_oai_streaming.go`.
- `GET /v1/models` also answers in OpenAI list shape when the client asks
  for it (no `anthropic-version` header).

## 6. Settings and state

- `~/Library/Application Support/CodeLink/config.json` (EngineConfig, no
  secrets), `claude-settings.json`; secrets in the Keychain (service
  `code-link`); last launch folders; launch at login via `SMAppService`.
- **One configuration page** (Derek, 2026-10-19: small app — menu bar plus
  a single compact settings page, simple but powerful), in order:
  - **Providers:** name, kind (OpenAI-compatible = vLLM/SGLang/Flare, or
    Anthropic), base URL, API key (Keychain), Test (probe: reachable, model
    list, logprobs, latency).
  - **Models — two tiers:** Opus tier and Sonnet tier, each a provider +
    a model on it (the same provider or different ones); context window
    filled from the probe. Sonnet = `roles.main` (default, also haiku/small
    and Codex unless overridden); Opus = `roles.opus`. Advanced: small/haiku,
    decision (needs logprobs), vision (only if the tier model lacks vision),
    Codex overrides.
  - **Auto mode:** decision model and its logprob status.
  - **Documents:** PaddleOCR endpoint (Test) and the local fallback
    (Apple Vision / text only / off).
  - **Web search:** Tavily / Brave / off and its key (Keychain), Test.
  - **Launch:** terminal (iTerm/Terminal), Claude Code permission mode
    (auto default), launch at login.
  - Recent activity (requests, decisions — no prompt text) tucked away at
    the bottom.
- **Style:** Eidonix teal `#00C2C7` as the accent (deeper `#009EA3` where
  light-mode contrast needs it), system backgrounds and labels, correct in
  light and dark mode; status glyphs green/amber/red only.
- Menu bar icon reflects engine state (running / backend unreachable /
  stopped).

## 7. Testing

- `swift test`: translation fixtures for every rule in §4–§5 (stub backend
  served in-process), decision-head math and response shapes, documents
  (PDFKit on a generated PDF; OCR behind a protocol), launcher script and
  env rendering.
- Conformance (manual, `scripts/conformance.sh`): runs
  `~/cletus-worktrees/cutover/cc-conformance.py` and `automode-eval.py`
  against `http://127.0.0.1:<port>` — files, image, PDF, notebook,
  background bash, subagent, WebSearch, WebFetch, MCP tool search, and the
  12-case auto-mode eval.
