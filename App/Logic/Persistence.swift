import Foundation
import CodeLinkEngine

/// config.json (EngineConfig, never secrets) and claude-settings.json.
struct ConfigStore: Sendable {
    var paths: AppPaths

    /// What loading found: a config (default when the file is absent) and a
    /// note when an unreadable file was set aside.
    struct Loaded: Sendable { var config: EngineConfig; var existed: Bool; var warning: String? }

    func load() -> Loaded {
        guard let data = try? Data(contentsOf: paths.configFile) else {
            return Loaded(config: EngineConfig(), existed: false, warning: nil)
        }
        do {
            return Loaded(config: try JSONDecoder().decode(EngineConfig.self, from: data), existed: true, warning: nil)
        } catch {
            let aside = paths.supportDirectory.appendingPathComponent("config.unreadable.json")
            try? FileManager.default.removeItem(at: aside)
            try? FileManager.default.moveItem(at: paths.configFile, to: aside)
            return Loaded(config: EngineConfig(), existed: false,
                          warning: "config.json could not be read (\(error.localizedDescription)); it was moved to config.unreadable.json.")
        }
    }

    func save(_ config: EngineConfig) throws {
        let enc = JSONEncoder()
        enc.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        try write(try enc.encode(config), to: paths.configFile)
        try ensureClaudeSettings()
    }

    /// Writes claude-settings.json (only when its content differs).
    @discardableResult
    func ensureClaudeSettings() throws -> URL {
        let url = paths.claudeSettingsFile
        if let existing = try? String(contentsOf: url, encoding: .utf8), existing == ClaudeSettings.json { return url }
        try write(Data(ClaudeSettings.json.utf8), to: url)
        return url
    }

    private func write(_ data: Data, to url: URL) throws {
        try FileManager.default.createDirectory(at: paths.supportDirectory, withIntermediateDirectories: true,
                                                attributes: [.posixPermissions: 0o700])
        try data.write(to: url, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
    }
}

enum TerminalPreference: String, Codable, Sendable, CaseIterable {
    case automatic, iTerm, terminal
    var label: String {
        switch self {
        case .automatic: "iTerm if installed, otherwise Terminal"
        case .iTerm: "iTerm"
        case .terminal: "Terminal"
        }
    }
}

/// Claude Code permission modes offered in Settings.
enum PermissionMode {
    static let all = ["auto", "default", "acceptEdits", "plan", "bypassPermissions"]
}

struct LaunchSettings: Codable, Sendable, Equatable {
    var terminal: TerminalPreference = .automatic
    var permissionMode: String = "auto"
    var recentFolders = RecentFolders()

    init() {}
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: Keys.self)
        terminal = try c.decodeIfPresent(TerminalPreference.self, forKey: .terminal) ?? .automatic
        permissionMode = try c.decodeIfPresent(String.self, forKey: .permissionMode) ?? "auto"
        recentFolders = try c.decodeIfPresent(RecentFolders.self, forKey: .recentFolders) ?? RecentFolders()
    }
    private enum Keys: String, CodingKey { case terminal, permissionMode, recentFolders }
}

struct LaunchSettingsStore: Sendable {
    var paths: AppPaths

    func load() -> LaunchSettings {
        guard let data = try? Data(contentsOf: paths.appSettingsFile),
              let s = try? JSONDecoder().decode(LaunchSettings.self, from: data) else { return LaunchSettings() }
        return s
    }

    func save(_ settings: LaunchSettings) throws {
        let enc = JSONEncoder()
        enc.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        try FileManager.default.createDirectory(at: paths.supportDirectory, withIntermediateDirectories: true,
                                                attributes: [.posixPermissions: 0o700])
        try enc.encode(settings).write(to: paths.appSettingsFile, options: .atomic)
    }
}

/// The folders sessions were launched in, newest first.
struct RecentFolders: Codable, Sendable, Equatable {
    static let limit = 8
    private(set) var paths: [String] = []

    init() {}
    init(paths: [String]) { self.paths = Array(Self.dedupe(paths).prefix(Self.limit)) }

    mutating func add(_ path: String) {
        let p = Self.normalize(path)
        guard !p.isEmpty else { return }
        paths = Array(([p] + paths.filter { $0 != p }).prefix(Self.limit))
    }

    mutating func clear() { paths = [] }

    /// Drops folders that no longer exist.
    func existing(_ exists: (String) -> Bool = { var d: ObjCBool = false; return FileManager.default.fileExists(atPath: $0, isDirectory: &d) && d.boolValue }) -> [String] {
        paths.filter(exists)
    }

    static func normalize(_ path: String) -> String {
        var p = (path as NSString).standardizingPath
        while p.count > 1 && p.hasSuffix("/") { p.removeLast() }
        return p
    }

    private static func dedupe(_ list: [String]) -> [String] {
        var seen = Set<String>()
        return list.map(normalize).filter { !$0.isEmpty && seen.insert($0).inserted }
    }

    // Encoded as a plain array of strings.
    init(from decoder: Decoder) throws { self.init(paths: try decoder.singleValueContainer().decode([String].self)) }
    func encode(to encoder: Encoder) throws { var c = encoder.singleValueContainer(); try c.encode(paths) }

    /// "folder — ~/path/to" for menus.
    static func title(for path: String, home: String = NSHomeDirectory()) -> String {
        let name = (path as NSString).lastPathComponent
        var shown = path
        if shown == home { shown = "~" } else if shown.hasPrefix(home + "/") { shown = "~" + shown.dropFirst(home.count) }
        return name.isEmpty ? shown : "\(name) — \(shown)"
    }
}
