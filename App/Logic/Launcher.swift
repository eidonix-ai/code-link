import Foundation
import AppKit

/// Writes the launch script and opens it in iTerm or Terminal (docs/DESIGN.md §3).
struct Launcher {
    var paths: AppPaths

    /// iTerm takes the script path as the session command, so a path with
    /// spaces or quotes can't be used there.
    static func useITerm(_ preference: TerminalPreference, itermInstalled: Bool, scriptPath: String) -> Bool {
        guard preference != .terminal, itermInstalled else { return false }
        return !scriptPath.contains(where: { " \t'\"".contains($0) })
    }

    /// Creates `<launch dir>/launch-<id>.sh` (0700) and returns its path.
    func writeScript(command: String, folder: String?, shell: String = LaunchScript.loginShell()) throws -> String {
        try FileManager.default.createDirectory(at: paths.launchDirectory, withIntermediateDirectories: true,
                                                attributes: [.posixPermissions: 0o700])
        let url = paths.launchDirectory.appendingPathComponent("launch-\(UUID().uuidString.prefix(8).lowercased()).sh")
        try Data(LaunchScript.render(shell: shell, command: command, folder: folder).utf8).write(to: url)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: url.path)
        return url.path
    }

    static var itermInstalled: Bool {
        NSWorkspace.shared.urlForApplication(withBundleIdentifier: "com.googlecode.iterm2") != nil
    }

    /// Off the main thread: osascript may wait on the Automation permission prompt.
    func open(command: String, folder: String?, preference: TerminalPreference) async throws {
        let launcher = self
        try await Task.detached { try launcher.openSync(command: command, folder: folder, preference: preference) }.value
    }

    func openSync(command: String, folder: String?, preference: TerminalPreference) throws {
        let script = try writeScript(command: command, folder: folder)
        let plans = Self.plans(preference, itermInstalled: Self.itermInstalled, scriptPath: script)
        var lastError: Error?
        for plan in plans {
            do { try Self.osascript(plan.appleScript, plan.argument); return } catch { lastError = error }
        }
        try? FileManager.default.removeItem(atPath: script)
        throw lastError ?? ScriptError(errorDescription: "No terminal could be opened.")
    }

    /// One way of opening the script: the AppleScript and its single argument.
    struct TerminalPlan: Equatable {
        enum App: Equatable { case iTerm, terminal }
        var app: App
        var appleScript: String
        var argument: String
    }

    /// The plans to try, in order. iTerm (when wanted, installed and the path
    /// is plain) comes first with Terminal as the fallback; iTerm takes the
    /// bare path as the session command, Terminal gets it shell-quoted.
    static func plans(_ preference: TerminalPreference, itermInstalled: Bool, scriptPath: String) -> [TerminalPlan] {
        var out: [TerminalPlan] = []
        if useITerm(preference, itermInstalled: itermInstalled, scriptPath: scriptPath) {
            out.append(TerminalPlan(app: .iTerm, appleScript: iTermScript, argument: scriptPath))
        }
        out.append(TerminalPlan(app: .terminal, appleScript: terminalScript, argument: ShellQuote.quote(scriptPath)))
        return out
    }

    /// A new tab in iTerm's current window when there is one, else a new window.
    static let iTermScript = """
    on run argv
      tell application "iTerm2"
        activate
        if (count of windows) = 0 then
          create window with default profile command (item 1 of argv)
        else
          tell current window to create tab with default profile command (item 1 of argv)
        end if
      end tell
    end run
    """

    /// A new Terminal window running the script.
    static let terminalScript = """
    on run argv
      tell application "Terminal"
        activate
        do script (item 1 of argv)
      end tell
    end run
    """

    struct ScriptError: LocalizedError { var errorDescription: String? }

    @discardableResult
    private static func osascript(_ script: String, _ arg: String) throws -> Bool {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
        p.arguments = ["-e", script, arg]
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        try p.run()
        p.waitUntilExit()
        guard p.terminationStatus == 0 else {
            let out = String(data: pipe.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8)?
                .trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
            throw ScriptError(errorDescription: out.isEmpty ? "osascript failed (\(p.terminationStatus))" : out)
        }
        return true
    }
}
