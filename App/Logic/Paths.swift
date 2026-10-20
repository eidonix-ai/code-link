import Foundation

/// Where code-link keeps its files. Injectable so tests never touch the real ones.
struct AppPaths: Sendable, Equatable {
    var supportDirectory: URL
    var cacheDirectory: URL

    var configFile: URL { supportDirectory.appendingPathComponent("config.json") }
    var claudeSettingsFile: URL { supportDirectory.appendingPathComponent("claude-settings.json") }
    var appSettingsFile: URL { supportDirectory.appendingPathComponent("app-settings.json") }
    var accessTokenFile: URL { supportDirectory.appendingPathComponent("access-token") }
    var launchDirectory: URL { cacheDirectory.appendingPathComponent("launch", isDirectory: true) }

    static func live(fileManager: FileManager = .default) -> AppPaths {
        let home = fileManager.homeDirectoryForCurrentUser
        return AppPaths(
            supportDirectory: home.appendingPathComponent("Library/Application Support/CodeLink", isDirectory: true),
            cacheDirectory: home.appendingPathComponent("Library/Caches/CodeLink", isDirectory: true))
    }
}
