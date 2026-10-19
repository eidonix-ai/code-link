import Testing
import Foundation
import CodeLinkEngine
@testable import CodeLink

private func sample() -> EngineConfig {
    EngineConfig(port: 19000, backends: [Backend(id: "cn3", baseURL: URL(string: "http://h:8000/v1")!, apiKeyRef: "backend-ab12")],
                 models: [Model(id: "GLM", backendID: "cn3", upstreamModel: "glm", contextWindow: 1000)], roles: Roles(main: "GLM"))
}

@Test func configRoundTripsThroughDisk() throws {
    let env = AppEnvironment.isolated()
    defer { try? FileManager.default.removeItem(at: env.paths.supportDirectory.deletingLastPathComponent()) }
    let store = ConfigStore(paths: env.paths)
    #expect(!store.load().existed)
    try store.save(sample())
    let loaded = store.load()
    #expect(loaded.existed && loaded.config == sample() && loaded.warning == nil)
    let text = try String(contentsOf: env.paths.configFile, encoding: .utf8)
    #expect(text.contains("backend-ab12"))   // the reference, never a key
    let perms = try FileManager.default.attributesOfItem(atPath: env.paths.configFile.path)[.posixPermissions] as? Int
    #expect(perms == 0o600)
}

@Test func saveWritesClaudeSettingsNextToConfig() throws {
    let env = AppEnvironment.isolated()
    defer { try? FileManager.default.removeItem(at: env.paths.supportDirectory.deletingLastPathComponent()) }
    try ConfigStore(paths: env.paths).save(sample())
    let data = try Data(contentsOf: env.paths.claudeSettingsFile)
    let json = try JSONSerialization.jsonObject(with: data) as? [String: [String: String]]
    #expect(json == ["attribution": ["commit": "", "pr": ""]])
    #expect(env.paths.claudeSettingsFile.deletingLastPathComponent() == env.paths.configFile.deletingLastPathComponent())
}

@Test func unreadableConfigIsSetAsideNotOverwritten() throws {
    let env = AppEnvironment.isolated()
    defer { try? FileManager.default.removeItem(at: env.paths.supportDirectory.deletingLastPathComponent()) }
    try FileManager.default.createDirectory(at: env.paths.supportDirectory, withIntermediateDirectories: true)
    try Data("{nope".utf8).write(to: env.paths.configFile)
    let loaded = ConfigStore(paths: env.paths).load()
    #expect(loaded.config == EngineConfig() && loaded.warning != nil)
    #expect(FileManager.default.fileExists(atPath: env.paths.supportDirectory.appendingPathComponent("config.unreadable.json").path))
}

@Test func launchSettingsPersistAndDefaultOldFiles() throws {
    let env = AppEnvironment.isolated()
    defer { try? FileManager.default.removeItem(at: env.paths.supportDirectory.deletingLastPathComponent()) }
    let store = LaunchSettingsStore(paths: env.paths)
    #expect(store.load() == LaunchSettings())
    #expect(LaunchSettings().permissionMode == "auto")
    var s = LaunchSettings()
    s.terminal = .terminal; s.permissionMode = "plan"; s.recentFolders.add("/tmp/a")
    try store.save(s)
    #expect(store.load() == s)
    try Data(#"{"terminal":"iTerm"}"#.utf8).write(to: env.paths.appSettingsFile)
    let partial = store.load()
    #expect(partial.terminal == .iTerm && partial.permissionMode == "auto" && partial.recentFolders.paths.isEmpty)
}

@Test func recentFoldersAreNewestFirstDedupedAndCapped() {
    var r = RecentFolders()
    for i in 0..<12 { r.add("/tmp/f\(i)") }
    #expect(r.paths.count == RecentFolders.limit && r.paths.first == "/tmp/f11")
    r.add("/tmp/f9/")
    #expect(r.paths.first == "/tmp/f9" && r.paths.filter { $0 == "/tmp/f9" }.count == 1)
    r.add("")
    #expect(r.paths.count == RecentFolders.limit)
    #expect(r.existing { $0 == "/tmp/f9" } == ["/tmp/f9"])
    r.clear()
    #expect(r.paths.isEmpty)
}

@Test func recentFoldersEncodeAsAPlainArray() throws {
    var r = RecentFolders(); r.add("/a"); r.add("/b")
    #expect(String(data: try JSONEncoder().encode(r), encoding: .utf8) == #"["\/b","\/a"]"#)
    #expect(try JSONDecoder().decode(RecentFolders.self, from: Data(#"["/x","/x","/y"]"#.utf8)).paths == ["/x", "/y"])
    #expect(RecentFolders.title(for: "/Users/d/code/app", home: "/Users/d") == "app — ~/code/app")
}

@Test func secretsStayPendingUntilCommitted() throws {
    let backend = MemorySecretBackend()
    let secrets = AppSecrets(backend: backend)
    try backend.set("old", account: "r1")
    #expect(secrets.secret("r1") == "old")
    secrets.setPending("new", ref: "r1")
    #expect(secrets.secret("r1") == "new" && backend.get("r1") == "old" && secrets.hasPending)
    secrets.discardPending()
    #expect(secrets.secret("r1") == "old" && !secrets.hasPending)
    secrets.setPending("", ref: "r1")
    #expect(secrets.secret("r1") == nil)
    secrets.setPending("k2", ref: "r2")
    try secrets.commit()
    #expect(backend.get("r1") == nil && backend.get("r2") == "k2" && !secrets.hasPending)
}

@Test func accessTokenIsGeneratedOnceAsBase64URL() {
    let backend = MemorySecretBackend()
    let t = AccessToken.loadOrCreate(in: backend)
    #expect(t.count == 43)
    #expect(t.allSatisfy { $0.isLetter || $0.isNumber || $0 == "-" || $0 == "_" })
    #expect(AccessToken.loadOrCreate(in: backend) == t)
    #expect(AccessToken.make() != AccessToken.make())
}

@Test func keychainRoundTrip() throws {
    let store = KeychainStore(service: "code-link-tests-\(UUID().uuidString)")
    #expect(store.get("a") == nil)
    try store.set("one", account: "a")
    #expect(store.get("a") == "one")
    try store.set("two", account: "a")
    #expect(store.get("a") == "two")
    store.delete("a")
    #expect(store.get("a") == nil)
}
