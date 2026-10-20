# code-link in Go — porting brief

code-link is being rebuilt from scratch in Go + Wails v3 so one codebase runs on
macOS and Windows (most users are on Windows) as a portable app. The Swift app
(branch `main`, checkout `~/src/code-link`) is the **specification**: its
behaviour and its tests are what the Go build must match. `docs/DESIGN.md` is
the behaviour spec (written for Swift; where it names Apple frameworks or the
Keychain, this brief overrides it).

## Ground rules

- **Port behaviour, not syntax.** Read the Swift source and its tests, then
  write idiomatic Go. Every Swift test of your area gets a Go test (same cases,
  same expectations), unless it tests something that no longer exists (Keychain,
  SwiftUI). Say which ones you dropped and why.
- **No regressions from the Swift build's late fixes:** no `temperature` in
  probe/decision/describe calls (Flare refuses 0); the decision head's "locate"
  fallback when a server refuses a prefilled assistant message; vision probed on
  the first image; Fable/Mythos route to the Opus role.
- Simple code that just works. No speculative abstractions.
- **Never write to `~/.claude`, `~/.codex`, or the user's real code-link
  settings.** Tests use temp dirs.
- Stay in your package(s). Shared packages (`config`, `core`, `hooks`,
  `routing`, `upstream`, `apierr`, `httpx`, `jsonx`, `testkit`, `media` API)
  belong to the integrator: if you need a change there, make the smallest one
  and call it out in your report.
- Commits: author `derek <derek.yates@live.com>`, no Co-Authored-By or any AI
  attribution, ever. Commit on your own branch only; never merge or push.
- `GOWORK=off go test ./...` and `GOWORK=off go vet ./...` must pass.

## Conventions

- JSON: `internal/jsonx` — requests decoded to `map[string]any` / `[]any` /
  `json.Number` (numbers keep their spelling), written with `jsonx.Marshal`
  (no HTML escaping; keys sorted, so output is byte-stable for prefix caches).
  Helpers: `jsonx.Get(v, "choices", 0, "message")`, `Str`, `Int`, `Arr`, `Obj`.
- Handlers: `func Handle(env *core.Env, w http.ResponseWriter, r *http.Request, body []byte)`.
  `r.Context()` is cancelled when the client goes: pass it to every upstream
  call so the backend stops generating.
- Upstream: `env.Client.Send(ctx, target.ChatRequest(body, stream), env.HeaderTimeout())`;
  stream lines with `resp.Lines()`. Errors → `apierr` (Anthropic shape for
  Messages, OpenAI shape for Responses / Chat Completions).
- Streams: `httpx.StartStream(w, status, header, env.Tuning.PingInterval, commentPings)`;
  `Event(name, data)` for Anthropic/Responses events, `Data(v)` for Chat
  Completions chunks, `Close()` at the end.
- Per-model facts: `env.Facts.ContextWindow(ctx, target)`, `env.Facts.HasVision(ctx, target, request)`.
- Hooks (`internal/hooks`): `env.Hooks.Documents/Images/WebSearch/AutoMode`,
  nil = off. Media rewriting: `internal/media` (`ProcessMessages`,
  `ProcessChatMessages`, `ProcessResponsesInput`) — call it even while it is a
  placeholder.
- Activity: `env.Log.Record(hooks.Event{Kind: "request", Route:, Model:, Status:, LatencyMS:})`. Never prompt text.
- Tests: `internal/testkit` (stub backend, chunk builders, SSE parsing, hook
  fakes) and `internal/testkit/harness` (engine on a free port against the
  stub). Tests that run the engine live in external test packages
  (`package messages_test`).

## Packages

| Package | Swift source (spec) | Swift tests |
|---|---|---|
| `messages` | Translate/Messages/* except MediaPreprocessor | Core/TranslationTests, Core/ServerTests, Core/HookTests (Messages parts) |
| `media` | Translate/Messages/MediaPreprocessor, Translate/OpenAIMedia | Core/HookTests (documents/images), media cases in Responses/Chat tests |
| `responses` | Translate/Responses/* | Responses/ResponsesTests |
| `chat` | Translate/Chat/* | Responses/ChatCompletionsTests |
| `automode`, `documents`, `images`, `websearch`, `features` | Features/* | Features/* |
| app: `main.go`, `internal/app`, `internal/launch`, `frontend/` | App/* | CodeLinkTests/* |

Platform: Windows and macOS (Linux should build). Local PDF reading is pure Go
(text layer); OCR of scanned pages is per platform later, behind
`documents`' interface. The access token and API keys live in the one settings
file (`internal/config`), not a keychain.

Port 18775 by default; settings in `Eidonix/code-link/code-link.json` under the
user config dir (or next to the exe in portable mode) — so it runs alongside
the Swift app (18765, `Application Support/CodeLink`).
