import SwiftUI
import AppKit
import ServiceManagement
import CodeLinkEngine

/// Where the app keeps its state; tests and previews pass in-memory ones.
struct AppEnvironment {
    var paths: AppPaths
    var secrets: any SecretBackend
    /// Start the engine and the polling loop at launch.
    var autostart: Bool

    static var live: AppEnvironment {
        AppEnvironment(paths: .live(), secrets: KeychainStore(), autostart: true)
    }

    static func isolated() -> AppEnvironment {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("code-link-\(UUID().uuidString)", isDirectory: true)
        return AppEnvironment(paths: AppPaths(supportDirectory: dir.appendingPathComponent("support"),
                                              cacheDirectory: dir.appendingPathComponent("cache")),
                              secrets: MemorySecretBackend(), autostart: false)
    }

    /// The real environment, except under XCTest (never touch the user's files or Keychain there).
    static var current: AppEnvironment {
        ProcessInfo.processInfo.environment["XCTestConfigurationFilePath"] != nil ? isolated() : live
    }
}

@MainActor @Observable
final class AppModel {
    let environment: AppEnvironment
    let configStore: ConfigStore
    let launchStore: LaunchSettingsStore
    let secrets: AppSecrets
    let activity = ActivityStore()
    let engine: Engine
    let accessToken: String

    /// What the engine is running with.
    private(set) var config: EngineConfig
    /// What Settings is editing.
    var draft: EngineConfig
    var launchSettings: LaunchSettings
    private(set) var engineState: EngineState = .stopped
    private(set) var backendReachable: Bool?
    private(set) var startError: String?
    private(set) var isLaunching = false
    private(set) var launchAtLogin = false

    /// Latest probe per provider id (reachable, models, logprobs, latency).
    private(set) var probes: [String: BackendProbe] = [:]
    private(set) var probing: Set<String> = []

    var saveMessage: SaveMessage?
    private var removedKeyRefs: Set<String> = []
    private var bootstrapped = false
    private let firstRun: Bool

    enum SaveMessage: Equatable { case saved, failed([String]) }

    init(environment: AppEnvironment = .current) {
        self.environment = environment
        configStore = ConfigStore(paths: environment.paths)
        launchStore = LaunchSettingsStore(paths: environment.paths)
        secrets = AppSecrets(backend: environment.secrets)
        accessToken = AccessToken.loadOrCreate(in: environment.secrets)
        let loaded = configStore.load()
        config = loaded.config
        draft = loaded.config
        firstRun = !loaded.config.isConfigured
        var settings = launchStore.load()
        settings.recentFolders = RecentFolders(paths: settings.recentFolders.existing())
        launchSettings = settings
        engine = Engine(config: loaded.config, secrets: secrets, log: ActivityLog(store: activity), accessToken: accessToken)
        startError = loaded.warning
        launchAtLogin = SMAppService.mainApp.status == .enabled
    }

    // MARK: Derived

    var status: EngineStatus { EngineStatus.make(config: config, state: engineState, backendReachable: backendReachable, startError: startError) }
    var hasUnsavedChanges: Bool { draft != config || secrets.hasPending || !removedKeyRefs.isEmpty }
    var runningPort: Int { if case .running(let p) = engineState { p } else { config.port } }
    var isRunning: Bool { if case .running = engineState { true } else { false } }

    // MARK: Lifecycle

    /// Called once when the menu bar item appears: start the engine, poll its
    /// state, and open Settings on a first run.
    func bootstrap() async {
        guard !bootstrapped else { return }
        bootstrapped = true
        guard environment.autostart else { return }
        if firstRun {
            try? await Task.sleep(for: .milliseconds(400))
            openSettings()
        }
        if config.isConfigured { await start() }
        var tick = 0
        while !Task.isCancelled {
            engineState = await engine.state
            if isRunning, tick % 10 == 0 { await checkMainBackend() }
            if !isRunning { backendReachable = nil }
            tick += 1
            try? await Task.sleep(for: .seconds(2))
        }
    }

    func start() async {
        guard config.isConfigured else { return }
        startError = nil
        engineState = .starting
        do { try await engine.start() } catch { startError = error.localizedDescription }
        engineState = await engine.state
        if isRunning { await checkMainBackend() }
    }

    func stop() async {
        await engine.stop()
        engineState = await engine.state
        backendReachable = nil
    }

    func toggleEngine() async { if isRunning { await stop() } else { await start() } }

    private func checkMainBackend() async {
        guard let id = config.mainModel?.backendID, let b = config.backends.first(where: { $0.id == id }) else { return }
        let result = await engine.probe(b)
        probes[id] = result
        backendReachable = result.reachable
    }

    // MARK: Settings

    func openSettings() {
        NSApp.activate()
        NSApp.sendAction(Selector(("showSettingsWindow:")), to: nil, from: nil)
    }

    func save() async {
        let problems = draft.problems()
        guard problems.isEmpty else { saveMessage = .failed(problems); return }
        draft.pruneModels()
        let portChanged = draft.port != config.port
        do {
            try configStore.save(draft)
            try secrets.commit()
            for ref in removedKeyRefs { secrets.delete(ref) }
            removedKeyRefs = []
        } catch {
            saveMessage = .failed([error.localizedDescription]); return
        }
        config = draft
        await engine.apply(config)
        if config.isConfigured {
            if isRunning, portChanged { await stop() }
            if !isRunning { await start() }
        } else if isRunning {
            await stop()
        }
        saveMessage = .saved
    }

    func revert() {
        draft = config
        secrets.discardPending()
        removedKeyRefs = []
        saveMessage = nil
    }

    // MARK: Providers

    func addProvider() {
        var n = draft.backends.count + 1
        while draft.backends.contains(where: { $0.id == "provider-\(n)" }) { n += 1 }
        draft.backends.append(Backend(id: "provider-\(n)", kind: .openAICompatible, baseURL: URL(string: "http://localhost:8000/v1")!))
    }

    func removeProvider(_ id: String) {
        if let ref = draft.backends.first(where: { $0.id == id })?.apiKeyRef { removedKeyRefs.insert(ref); secrets.setPending("", ref: ref) }
        draft.removeBackend(id)
        probes[id] = nil
    }

    /// Stores a typed API key (pending until Save); empty removes it.
    func setAPIKey(_ value: String, forProvider id: String) {
        guard let i = draft.backends.firstIndex(where: { $0.id == id }) else { return }
        let ref = draft.backends[i].apiKeyRef ?? "backend-\(UUID().uuidString.prefix(8).lowercased())"
        draft.backends[i].apiKeyRef = value.isEmpty ? nil : ref
        if value.isEmpty { if let old = config.backends.first(where: { $0.id == id })?.apiKeyRef { removedKeyRefs.insert(old) }; secrets.setPending("", ref: ref) }
        else { removedKeyRefs.remove(ref); secrets.setPending(value, ref: ref) }
    }

    func hasAPIKey(forProvider id: String) -> Bool {
        guard let ref = draft.backends.first(where: { $0.id == id })?.apiKeyRef else { return false }
        return secrets.hasSecret(ref)
    }

    func setSearchKey(_ value: String) {
        guard let provider = draft.webSearch.provider else { return }
        let ref = "websearch-\(provider.rawValue)"
        draft.webSearch.keyRef = ref
        secrets.setPending(value, ref: ref)
    }

    func hasSearchKey() -> Bool { draft.webSearch.keyRef.map(secrets.hasSecret) ?? false }

    func probe(_ id: String) async {
        guard let b = draft.backends.first(where: { $0.id == id }), !probing.contains(id) else { return }
        probing.insert(id)
        let result = await engine.probe(b)
        probing.remove(id)
        probes[id] = result
    }

    /// Probes a provider the first time its models are needed.
    func probeIfNeeded(_ id: String) async { if probes[id] == nil { await probe(id) } }

    func probedModel(_ choice: ModelChoice) -> ProbedModel? {
        probes[choice.backendID]?.models.first { $0.upstreamModel == choice.upstream }
    }

    /// Every model a picker can offer: probed ones, plus those already configured.
    func choices(for backendID: String? = nil) -> [ModelChoice] {
        var out: [ModelChoice] = []
        func add(_ c: ModelChoice) { if !out.contains(c), backendID == nil || c.backendID == backendID { out.append(c) } }
        for b in draft.backends { for m in probes[b.id]?.models ?? [] { add(ModelChoice(backendID: b.id, upstream: m.upstreamModel)) } }
        for m in draft.models { add(ModelChoice(backendID: m.backendID, upstream: m.upstreamModel)) }
        return out
    }

    func setRole(_ slot: RoleSlot, to choice: ModelChoice?) {
        draft.setRole(slot, to: choice, probed: choice.flatMap(probedModel))
    }

    // MARK: Menu actions

    /// Quick switch of the Sonnet (main) model among the configured ones.
    func switchMain(to id: String) async {
        guard config.model(id) != nil, config.roles.main != id else { return }
        draft = config
        draft.roles.main = id
        draft.pruneModels()
        await save()
    }

    func chooseFolderAndLaunch(_ tool: CodingTool) {
        NSApp.activate()
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.prompt = "Launch"
        panel.message = "Choose the folder to start \(tool.displayName) in"
        if let last = launchSettings.recentFolders.paths.first { panel.directoryURL = URL(fileURLWithPath: last) }
        guard panel.runModal() == .OK, let url = panel.url else { return }
        Task { await launch(tool, folder: url.path) }
    }

    func clearRecentFolders() {
        launchSettings.recentFolders.clear()
        saveLaunchSettings()
    }

    func saveLaunchSettings() { try? launchStore.save(launchSettings) }

    private func context(port: Int) throws -> LaunchContext {
        LaunchContext(config: config, port: port, accessToken: accessToken,
                      claudeSettingsPath: try configStore.ensureClaudeSettings().path,
                      permissionMode: launchSettings.permissionMode)
    }

    func copyCommand(_ tool: CodingTool) {
        do {
            let command = try LaunchCommand.render(tool, context(port: runningPort))
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(command, forType: .string)
        } catch {
            Alerts.show("Can't copy the \(tool.displayName) command", detail: error.localizedDescription)
        }
    }

    func launch(_ tool: CodingTool, folder: String) async {
        guard !isLaunching else { return }
        guard config.isConfigured else {
            Alerts.show("Nothing to launch yet", detail: LaunchError.noMainModel.localizedDescription)
            openSettings(); return
        }
        isLaunching = true
        defer { isLaunching = false }
        guard await CLICheck.isAvailable(tool) else { Alerts.missingCLI(tool); return }
        if !isRunning { await start() }
        guard isRunning else {
            Alerts.show("The code-link engine isn't running", detail: startError ?? "It could not be started.")
            return
        }
        do {
            let command = try LaunchCommand.render(tool, context(port: runningPort))
            try await Launcher(paths: environment.paths).open(command: command, folder: folder, preference: launchSettings.terminal)
            launchSettings.recentFolders.add(folder)
            saveLaunchSettings()
        } catch {
            Alerts.show("Couldn't open a terminal", detail: error.localizedDescription + "\n\nUse “Copy \(tool.displayName) command” and run it yourself.")
        }
    }

    // MARK: Launch at login

    func setLaunchAtLogin(_ on: Bool) {
        do {
            if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
        } catch {
            Alerts.show("Couldn't change Launch at login", detail: error.localizedDescription)
        }
        launchAtLogin = SMAppService.mainApp.status == .enabled
    }
}

@MainActor
enum Alerts {
    static func show(_ title: String, detail: String) {
        NSApp.activate()
        let a = NSAlert()
        a.messageText = title
        a.informativeText = detail
        a.runModal()
    }

    static func missingCLI(_ tool: CodingTool) {
        NSApp.activate()
        let a = NSAlert()
        a.messageText = "\(tool.displayName) isn't installed"
        a.informativeText = "`\(tool.executable)` wasn't found on your login shell's PATH. Install \(tool.displayName), then launch again."
        a.addButton(withTitle: "Open install page")
        a.addButton(withTitle: "Cancel")
        if a.runModal() == .alertFirstButtonReturn { NSWorkspace.shared.open(tool.installURL) }
    }
}
