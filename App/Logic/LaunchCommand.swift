import Foundation
import CodeLinkEngine

enum CodingTool: String, Sendable, CaseIterable {
    case claudeCode, codex

    var displayName: String { self == .claudeCode ? "Claude Code" : "Codex" }
    /// The executable name looked up on the login shell's PATH.
    var executable: String { self == .claudeCode ? "claude" : "codex" }
    var installURL: URL {
        self == .claudeCode
            ? URL(string: "https://docs.anthropic.com/en/docs/claude-code/setup")!
            : URL(string: "https://github.com/openai/codex")!
    }
}

enum LaunchError: LocalizedError, Equatable {
    case noMainModel
    var errorDescription: String? {
        switch self {
        case .noMainModel: "No main model is configured. Add a backend and a model in Settings first."
        }
    }
}

/// Everything a launch command depends on.
struct LaunchContext: Sendable {
    var config: EngineConfig
    var port: Int
    var accessToken: String
    var claudeSettingsPath: String
    var permissionMode: String = "auto"
}

/// Renders the exact commands of docs/DESIGN.md §3. Only that process gets
/// the environment; `~/.claude` and `~/.codex` are never written.
enum LaunchCommand {
    static let codexProviderID = "code-link"
    static let codexTokenVariable = "CODE_LINK_TOKEN"

    static func render(_ tool: CodingTool, _ ctx: LaunchContext) throws -> String {
        switch tool {
        case .claudeCode: try claude(ctx)
        case .codex: try codex(ctx)
        }
    }

    static func claude(_ ctx: LaunchContext) throws -> String {
        let roles = ctx.config.roles
        guard !roles.main.isEmpty else { throw LaunchError.noMainModel }
        let main = roles.main
        let opus = nonEmpty(roles.opus) ?? main
        let small = nonEmpty(roles.small) ?? main
        var env: [(String, String)] = [
            ("ANTHROPIC_BASE_URL", "http://127.0.0.1:\(ctx.port)"),
            ("ANTHROPIC_AUTH_TOKEN", ctx.accessToken),
            ("ANTHROPIC_MODEL", main),
            ("ANTHROPIC_DEFAULT_SONNET_MODEL", main),
            ("ANTHROPIC_DEFAULT_SONNET_MODEL_NAME", main),
            ("ANTHROPIC_DEFAULT_OPUS_MODEL", opus),
            ("ANTHROPIC_DEFAULT_OPUS_MODEL_NAME", opus),
            // Fable is the tier above Opus; without this /model offers Anthropic's own.
            ("ANTHROPIC_DEFAULT_FABLE_MODEL", opus),
            ("ANTHROPIC_DEFAULT_FABLE_MODEL_NAME", opus),
            ("ANTHROPIC_DEFAULT_HAIKU_MODEL", small),
            ("ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME", small),
            ("ANTHROPIC_SMALL_FAST_MODEL", small),
            ("CLAUDE_CODE_AUTO_MODE_SERVER", "0"),
            ("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1"),
            ("API_TIMEOUT_MS", "900000"),
        ]
        if let model = ctx.config.models.first(where: { $0.id == main }) {
            if let window = model.contextWindow, window > 0 {
                env.append(("CLAUDE_CODE_MAX_CONTEXT_TOKENS", String(window)))
            }
            if let compact = model.effectiveAutoCompactAt, compact > 0 {
                env.append(("CLAUDE_CODE_AUTO_COMPACT_WINDOW", String(compact)))
            }
        }
        var lines = env.map { "\($0.0)=\(ShellQuote.quoteIfNeeded($0.1)) \\" }
        lines.append("claude --permission-mode \(ShellQuote.quoteIfNeeded(ctx.permissionMode)) --settings \(ShellQuote.quote(ctx.claudeSettingsPath))")
        return lines.joined(separator: "\n")
    }

    static func codex(_ ctx: LaunchContext) throws -> String {
        let roles = ctx.config.roles
        guard !roles.main.isEmpty else { throw LaunchError.noMainModel }
        let model = nonEmpty(roles.codex) ?? roles.main
        let id = codexProviderID
        var settings = [
            "model=\(ShellQuote.tomlString(model))",
            "model_provider=\(ShellQuote.tomlString(id))",
        ]
        if let window = ctx.config.models.first(where: { $0.id == model })?.contextWindow, window > 0 {
            settings.append("model_context_window=\(window)")
        }
        settings += [
            "model_providers.\(id).name=\(ShellQuote.tomlString(id))",
            "model_providers.\(id).base_url=\(ShellQuote.tomlString("http://127.0.0.1:\(ctx.port)/v1"))",
            "model_providers.\(id).wire_api=\(ShellQuote.tomlString("responses"))",
            "model_providers.\(id).env_key=\(ShellQuote.tomlString(codexTokenVariable))",
        ]
        var lines = ["\(codexTokenVariable)=\(ShellQuote.quoteIfNeeded(ctx.accessToken)) \\", "codex --approve-for-me \\"]
        for (i, s) in settings.enumerated() {
            lines.append("  -c \(ShellQuote.quote(s))" + (i < settings.count - 1 ? " \\" : ""))
        }
        return lines.joined(separator: "\n")
    }

    private static func nonEmpty(_ s: String?) -> String? {
        guard let s, !s.isEmpty else { return nil }
        return s
    }
}

/// The script handed to iTerm or Terminal. It runs in the user's interactive
/// login shell (so `~/.zshrc` PATH applies), deletes itself, `cd`s, runs the
/// command and keeps the window open when the command fails.
enum LaunchScript {
    static func render(shell: String, command: String, folder: String?) -> String {
        var s = "#!\(shell) -li\n"
        s += "rm -f -- \"$0\"\n"
        if let folder, !folder.isEmpty {
            s += "cd -- \(ShellQuote.quote(folder)) || exit 1\n"
        }
        s += command + "\n"
        s += "status=$?\n"
        s += "if [ \"$status\" -ne 0 ]; then printf '\\n[exited with status %s; press Return to close]' \"$status\"; read -r _; fi\n"
        return s
    }

    /// The user's login shell when it is a POSIX shell the script can run in, else zsh.
    static func loginShell(environment: [String: String] = ProcessInfo.processInfo.environment,
                           exists: (String) -> Bool = { FileManager.default.fileExists(atPath: $0) }) -> String {
        if let shell = environment["SHELL"], ["zsh", "bash", "ksh", "sh"].contains((shell as NSString).lastPathComponent), exists(shell) {
            return shell
        }
        return "/bin/zsh"
    }
}

/// The content of the `--settings` file passed to Claude Code.
enum ClaudeSettings {
    static let json = "{\"attribution\":{\"commit\":\"\",\"pr\":\"\"}}\n"
}
