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
pasting the key is the whole setup (the gateway's models are synced in, its
recommendations become the defaults, and web search and documents go through
it). Models are kept in step with the providers automatically; the model is
chosen per launch, in the small window "Launch Claude Code…" / "Launch
Codex…" opens from the tray, which also installs and updates the two CLIs. "Share settings" exports a `.codelink` file — providers, models, roles,
documents and web search, never keys, the token, the port or launch
settings — and imports one, asking for the keys it needs.

## Building

Needs Go 1.27, Node 20+ and the Wails 3 CLI
(`go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.33`).

```
scripts/build.sh            # frontend, then the macOS app and the Windows exe
scripts/build.sh mac        # bin/code-link.app (arm64) + bin/code-link-<version>-macos-arm64.dmg (download) and .zip (update file)
scripts/build.sh windows    # bin/code-link.exe (amd64) + the same as bin/code-link-<version>-windows-amd64.exe
scripts/build.sh frontend   # regenerate bindings and rebuild frontend/dist only
scripts/build.sh icons      # redraw build/appicon.png and regenerate the .icns / .ico
```

The macOS app is built on a Mac (cgo) and signed with the "Apple Development"
identity (team 342KG4SM2D) when it is in the keychain, ad hoc otherwise
(`SIGN_IDENTITY=-` forces ad hoc). The Windows exe cross-compiles from any OS
(no cgo); it is a single portable file — users download it and run it, choosing
"More info → Run anyway" the first time, since it isn't code-signed. It needs
the WebView2 runtime, which Windows 10 and 11 have.

`VERSION` (default 0.2.0, stamped into the app with `-X main.version`) and
`BUNDLE_ID` (default `com.eidonix.code-link-go`, so it runs beside the Swift
app) are environment variables of the script; a plain `go build` reports
version "dev" and doesn't update itself.

## Updates and publishing

code-link updates itself (`internal/selfupdate`): it checks
`https://codelink.eidonix.com/update/manifest.json` and its detached Ed25519
signature `manifest.json.sig` at start and every 6 hours, and the tray offers
"Update code-link to x.y.z…" when a newer version is out. The format and the
replace-and-restart steps are described in the package. The public key is
embedded from `internal/selfupdate/update-public-key.txt`; the private key is
`~/.code-link/update-signing.key` on the publishing Mac. Make them once, from
the repository root, and commit the public key:

```
GOWORK=off go run ./cmd/updatekeys keygen
```

`scripts/publish.sh` uploads the site, the downloads
(`files/code-link-macos-arm64.dmg`, `files/code-link-windows-amd64.exe` and
versioned copies), the update files and the signed manifest; it refuses to
publish without the key, or builds that don't embed it. `NOTES="…"` sets the
release notes (default: the last commit's subject). To try an update against
another feed, run the app with `CODE_LINK_UPDATE_URL` set to its manifest
(it must still be signed with the key).

Tests and checks (no frontend build needed: `frontend/dist/.keep` satisfies
the embed):

```
GOWORK=off go vet ./... && GOWORK=off go test ./...
GOOS=windows GOARCH=amd64 GOWORK=off go build ./...
cd frontend && npm ci && npm run check
```

For a manual run, keep it away from your real settings and `~/.claude`:
`HOME=$(mktemp -d) bin/code-link.app/Contents/MacOS/code-link`.
