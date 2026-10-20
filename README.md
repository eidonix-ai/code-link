# code-link

Claude Code and Codex on your own models: a small tray app (menu bar on
macOS) with one Settings page, and a local engine that translates their APIs
to vLLM, SGLang, Flare or an Anthropic endpoint. `docs/DESIGN.md` is the
behaviour spec; `docs/GO-PORT.md` the rules of the Go port.

- `main.go` — `code-link` runs the tray app; `code-link serve [config.json]`
  runs the engine alone in the foreground (prints the port; Ctrl-C stops it;
  `CODE_LINK_TOKEN` overrides the access token).
- `internal/app` — state behind the UI: settings, engine lifecycle (next free
  port when busy), status, routing and role edits, launches, activity.
- `internal/launch` — the Claude Code and Codex commands (bash / PowerShell),
  CLI detection, and opening iTerm, Terminal, Windows Terminal or PowerShell.
- `internal/loginitem` — open at login: a LaunchAgent (macOS), an HKCU Run
  value (Windows).
- `internal/gui` — the Wails v3 shell: tray, Settings window, dialogs.
- `frontend/` — the Settings page (Svelte 5 + TypeScript + Vite).

Settings live in one file, `code-link.json`: next to the program when one is
there (portable mode), else `~/Library/Application Support/Eidonix/code-link`
on macOS and `%AppData%\Eidonix\code-link` on Windows.

A fresh install ships one provider, the Go-LLM-Proxy gateway
`llm.eidonix.com`, without a key: Settings opens on "Get started", where
pasting the key is the whole setup (its models are enabled, the gateway's
recommended models fill the roles, and web search and documents go through
it). "Share settings" exports a `.codelink` file — providers, models, roles,
documents and web search, never keys, the token, the port or launch
settings — and imports one, asking for the keys it needs.

## Building

Needs Go 1.27, Node 20+ and the Wails 3 CLI
(`go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.33`).

```
scripts/build.sh            # frontend, then the macOS app and the Windows exe
scripts/build.sh mac        # bin/code-link.app (arm64) + bin/code-link-<version>-macos-arm64.zip
scripts/build.sh windows    # bin/code-link.exe (amd64) + bin/code-link-<version>-windows-amd64.zip
scripts/build.sh frontend   # regenerate bindings and rebuild frontend/dist only
scripts/build.sh icons      # redraw build/appicon.png and regenerate the .icns / .ico
```

The macOS app is built on a Mac (cgo) and signed with the "Apple Development"
identity (team 342KG4SM2D) when it is in the keychain, ad hoc otherwise
(`SIGN_IDENTITY=-` forces ad hoc). The Windows exe cross-compiles from any OS
(no cgo); it is a single portable file — users unzip it and run it, choosing
"More info → Run anyway" the first time, since it isn't code-signed. It needs
the WebView2 runtime, which Windows 10 and 11 have.

`VERSION` (default 0.1.0) and `BUNDLE_ID` (default `com.eidonix.code-link-go`,
so it runs beside the Swift app) are environment variables of the script.

Tests and checks (no frontend build needed: `frontend/dist/.keep` satisfies
the embed):

```
GOWORK=off go vet ./... && GOWORK=off go test ./...
GOOS=windows GOARCH=amd64 GOWORK=off go build ./...
cd frontend && npm ci && npm run check
```

For a manual run, keep it away from your real settings and `~/.claude`:
`HOME=$(mktemp -d) bin/code-link.app/Contents/MacOS/code-link`.
