import Testing
import Foundation
import CodeLinkEngine
@testable import CodeLink

private func config(main: String = "GLM-5.3", opus: String? = nil, small: String? = nil, codex: String? = nil,
                    window: Int? = 1_048_576, compact: Int? = nil) -> EngineConfig {
    var c = EngineConfig(backends: [Backend(id: "cn3", baseURL: URL(string: "http://h:8000/v1")!)],
                         models: [Model(id: main, backendID: "cn3", upstreamModel: main, contextWindow: window, autoCompactAt: compact)],
                         roles: Roles(main: main, opus: opus, small: small, codex: codex))
    for id in [opus, small, codex].compactMap({ $0 }) where !c.models.contains(where: { $0.id == id }) {
        c.models.append(Model(id: id, backendID: "cn3", upstreamModel: id, contextWindow: 262_144))
    }
    return c
}

private func ctx(_ c: EngineConfig, mode: String = "auto") -> LaunchContext {
    LaunchContext(config: c, port: 18765, accessToken: "tok_en-1", claudeSettingsPath: "/Users/d/Library/Application Support/CodeLink/claude-settings.json", permissionMode: mode)
}

@Test func claudeCommandMatchesDesign() throws {
    let cmd = try LaunchCommand.claude(ctx(config(opus: "Big", small: "Tiny")))
    #expect(cmd == """
    ANTHROPIC_BASE_URL=http://127.0.0.1:18765 \\
    ANTHROPIC_AUTH_TOKEN=tok_en-1 \\
    ANTHROPIC_MODEL=GLM-5.3 \\
    ANTHROPIC_DEFAULT_SONNET_MODEL=GLM-5.3 \\
    ANTHROPIC_DEFAULT_OPUS_MODEL=Big \\
    ANTHROPIC_DEFAULT_HAIKU_MODEL=Tiny \\
    ANTHROPIC_SMALL_FAST_MODEL=Tiny \\
    CLAUDE_CODE_AUTO_MODE_SERVER=0 \\
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \\
    API_TIMEOUT_MS=900000 \\
    CLAUDE_CODE_MAX_CONTEXT_TOKENS=1048576 \\
    CLAUDE_CODE_AUTO_COMPACT_WINDOW=891289 \\
    claude --permission-mode auto --settings '/Users/d/Library/Application Support/CodeLink/claude-settings.json'
    """)
}

@Test func claudeAliasesDefaultToMainAndNeverSetClaudeOnlyFlags() throws {
    let cmd = try LaunchCommand.claude(ctx(config()))
    for name in ["ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_SMALL_FAST_MODEL"] {
        #expect(cmd.contains("\(name)=GLM-5.3 "))
    }
    #expect(!cmd.contains("DISABLE_PROMPT_CACHING"))
    #expect(!cmd.contains("DISABLE_1M_CONTEXT"))
}

@Test func claudeOmitsWindowVariablesWhenUnknownAndHonoursAutoCompactAt() throws {
    let unknown = try LaunchCommand.claude(ctx(config(window: nil)))
    #expect(!unknown.contains("CLAUDE_CODE_MAX_CONTEXT_TOKENS"))
    #expect(!unknown.contains("CLAUDE_CODE_AUTO_COMPACT_WINDOW"))
    let explicit = try LaunchCommand.claude(ctx(config(window: 200_000, compact: 150_000)))
    #expect(explicit.contains("CLAUDE_CODE_MAX_CONTEXT_TOKENS=200000 "))
    #expect(explicit.contains("CLAUDE_CODE_AUTO_COMPACT_WINDOW=150000 "))
}

@Test func claudeUsesPermissionModeAndQuotesAwkwardModelNames() throws {
    let cmd = try LaunchCommand.claude(ctx(config(main: "my model's [1m]"), mode: "acceptEdits"))
    #expect(cmd.contains(#"ANTHROPIC_MODEL='my model'"'"'s [1m]' \"#))
    #expect(cmd.hasSuffix("claude --permission-mode acceptEdits --settings '/Users/d/Library/Application Support/CodeLink/claude-settings.json'"))
}

@Test func commandsNeedAMainModel() {
    #expect(throws: LaunchError.noMainModel) { try LaunchCommand.claude(ctx(EngineConfig())) }
    #expect(throws: LaunchError.noMainModel) { try LaunchCommand.codex(ctx(EngineConfig())) }
}

@Test func codexCommandMatchesDesign() throws {
    let cmd = try LaunchCommand.codex(ctx(config(codex: "Coder")))
    #expect(cmd == """
    CODE_LINK_TOKEN=tok_en-1 \\
    codex --approve-for-me \\
      -c 'model="Coder"' \\
      -c 'model_provider="code-link"' \\
      -c 'model_context_window=262144' \\
      -c 'model_providers.code-link.name="code-link"' \\
      -c 'model_providers.code-link.base_url="http://127.0.0.1:18765/v1"' \\
      -c 'model_providers.code-link.wire_api="responses"' \\
      -c 'model_providers.code-link.env_key="CODE_LINK_TOKEN"'
    """)
}

@Test func codexDefaultsToMainAndSkipsUnknownWindow() throws {
    let cmd = try LaunchCommand.codex(ctx(config(window: nil)))
    #expect(cmd.contains(#"-c 'model="GLM-5.3"'"#))
    #expect(!cmd.contains("model_context_window"))
}

@Test func shellQuoting() {
    #expect(ShellQuote.quote("it's") == #"'it'"'"'s'"#)
    #expect(ShellQuote.quoteIfNeeded("GLM-5.3") == "GLM-5.3")
    #expect(ShellQuote.quoteIfNeeded("a b") == "'a b'")
    #expect(ShellQuote.quoteIfNeeded("") == "''")
    #expect(ShellQuote.tomlString(#"a"b\c"#) == #""a\"b\\c""#)
}

@Test func launchScriptText() {
    let s = LaunchScript.render(shell: "/bin/zsh", command: "claude --version", folder: "/Users/d/my project")
    #expect(s == """
    #!/bin/zsh -li
    rm -f -- "$0"
    cd -- '/Users/d/my project' || exit 1
    claude --version
    status=$?
    if [ "$status" -ne 0 ]; then printf '\\n[exited with status %s; press Return to close]' "$status"; read -r _; fi

    """)
    #expect(!LaunchScript.render(shell: "/bin/zsh", command: "x", folder: nil).contains("cd --"))
}

@Test func loginShellFallsBackToZsh() {
    #expect(LaunchScript.loginShell(environment: ["SHELL": "/bin/bash"], exists: { _ in true }) == "/bin/bash")
    #expect(LaunchScript.loginShell(environment: ["SHELL": "/opt/homebrew/bin/fish"], exists: { _ in true }) == "/bin/zsh")
    #expect(LaunchScript.loginShell(environment: ["SHELL": "/bin/zsh"], exists: { _ in false }) == "/bin/zsh")
    #expect(LaunchScript.loginShell(environment: [:], exists: { _ in true }) == "/bin/zsh")
}

@Test func itermIsUsedOnlyWhenInstalledAndThePathIsPlain() {
    #expect(Launcher.useITerm(.automatic, itermInstalled: true, scriptPath: "/Users/d/Library/Caches/CodeLink/launch/launch-1.sh"))
    #expect(!Launcher.useITerm(.automatic, itermInstalled: false, scriptPath: "/a/b.sh"))
    #expect(!Launcher.useITerm(.terminal, itermInstalled: true, scriptPath: "/a/b.sh"))
    #expect(Launcher.useITerm(.iTerm, itermInstalled: true, scriptPath: "/a/b.sh"))
    #expect(!Launcher.useITerm(.automatic, itermInstalled: true, scriptPath: "/Users/d/My Mac/b.sh"))
    #expect(!Launcher.useITerm(.automatic, itermInstalled: true, scriptPath: "/Users/d/it's/b.sh"))
}

@Test func writesAnExecutableScriptInTheLaunchDirectory() throws {
    let env = AppEnvironment.isolated()
    let path = try Launcher(paths: env.paths).writeScript(command: "echo hi", folder: "/tmp", shell: "/bin/zsh")
    defer { try? FileManager.default.removeItem(at: env.paths.cacheDirectory) }
    #expect(path.hasPrefix(env.paths.launchDirectory.path + "/launch-"))
    #expect(path.hasSuffix(".sh") && !path.contains(" "))
    let attrs = try FileManager.default.attributesOfItem(atPath: path)
    #expect((attrs[.posixPermissions] as? Int) == 0o700)
    #expect(try String(contentsOfFile: path, encoding: .utf8).hasPrefix("#!/bin/zsh -li\n"))
}
