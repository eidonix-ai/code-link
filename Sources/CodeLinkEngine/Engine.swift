import Foundation
import Synchronization

public enum EngineState: Sendable, Equatable { case stopped, starting, running(port: Int), failed(String) }

/// What a backend probe found (Settings → Backends → Test).
public struct BackendProbe: Sendable, Equatable {
    public var reachable: Bool
    public var error: String?
    public var models: [ProbedModel]
    public var latencyMS: Int?
    public init(reachable: Bool, error: String? = nil, models: [ProbedModel] = [], latencyMS: Int? = nil) {
        self.reachable = reachable; self.error = error; self.models = models; self.latencyMS = latencyMS
    }
}
public struct ProbedModel: Sendable, Equatable {
    public var upstreamModel: String
    public var contextWindow: Int?
    public var logprobs: Bool?
    public init(upstreamModel: String, contextWindow: Int? = nil, logprobs: Bool? = nil) {
        self.upstreamModel = upstreamModel; self.contextWindow = contextWindow; self.logprobs = logprobs
    }
}

/// One line for the Activity pane. Never prompt text.
public enum EngineEvent: Sendable {
    case request(route: String, model: String, status: Int, latencyMS: Int)
    case decision(stage: Int, model: String, pBlock: Double, severity: Int, latencyMS: Int, fallback: String?)
    case webSearch(query: String, results: Int)
    case document(name: String, pages: Int, ocrPages: Int)
    case error(String)
}

public protocol SecretStore: Sendable { func secret(_ ref: String) -> String? }
public protocol EventLog: Sendable { func record(_ event: EngineEvent) }

/// The local proxy: Anthropic Messages and OpenAI Responses in, Chat
/// Completions to the backends out (docs/DESIGN.md §2–§5).
public final class Engine: Sendable {
    public let accessToken: String
    private let secrets: any SecretStore
    private let log: any EventLog
    private let hooksFactory: any EngineHooksFactory
    private let tuning: EngineTuning
    private let responses: any ResponsesHandler
    private let client = UpstreamClient()
    private let contextWindows: ContextWindows
    private let environment: EnvironmentBox

    private struct Life: Sendable {
        var config: EngineConfig
        var server: HTTPServer?
        /// The configured port the running server was started for.
        var serverConfigPort = 0
        var state: EngineState = .stopped
    }
    private let life: Mutex<Life>
    private let queue = Mutex<Task<Void, Never>?>(nil)

    /// `hooksFactory` builds the feature modules for each configuration; the
    /// default has none (documents and searches answer with notes).
    public convenience init(config: EngineConfig, secrets: SecretStore, log: EventLog, accessToken: String,
                            hooksFactory: any EngineHooksFactory = NoHooksFactory()) {
        self.init(config: config, secrets: secrets, log: log, accessToken: accessToken, hooksFactory: hooksFactory,
                  tuning: EngineTuning(), responses: NotImplementedResponses())
    }

    init(config: EngineConfig, secrets: any SecretStore, log: any EventLog, accessToken: String,
         hooksFactory: any EngineHooksFactory, tuning: EngineTuning, responses: any ResponsesHandler) {
        self.accessToken = accessToken
        self.secrets = secrets
        self.log = log
        self.hooksFactory = hooksFactory
        self.tuning = tuning
        self.responses = responses
        self.contextWindows = ContextWindows(client: client)
        self.life = Mutex(Life(config: config))
        self.environment = EnvironmentBox(RequestEnvironment(
            config: config, secrets: secrets, log: log, client: client, tuning: tuning,
            contextWindows: contextWindows, hooksFactory: hooksFactory, responses: responses))
    }

    /// Starts the server on 127.0.0.1:`config.port` (0 = any free port).
    public func start() async throws {
        let result: Result<Void, any Error> = await serialized { [self] in
            if life.withLock({ $0.server != nil }) { return .success(()) }
            let port = life.withLock { l in
                l.state = .starting
                return l.config.port
            }
            do {
                let server = try await HTTPServer.start(port: port, token: accessToken, environment: environment.get)
                life.withLock { l in
                    l.server = server; l.serverConfigPort = port; l.state = .running(port: server.port)
                }
                return .success(())
            } catch {
                life.withLock { $0.state = .failed("\(error)") }
                log.record(.error("engine: \(error)"))
                return .failure(error)
            }
        }
        try result.get()
    }

    /// Stops the server, letting requests in flight finish for a few seconds.
    public func stop() async {
        await serialized { [self] in
            let server = life.withLock { l in
                defer { l.server = nil; l.state = .stopped }
                return l.server
            }
            await server?.shutdown(grace: .seconds(5))
        }
    }

    /// Applies a new configuration. Routing, models, roles and hooks change
    /// for the next request without a restart; requests in flight finish on
    /// the configuration they started with. A new port starts a new listener
    /// and drains the old one.
    public func apply(_ config: EngineConfig) async {
        await serialized { [self] in
            environment.set(RequestEnvironment(
                config: config, secrets: secrets, log: log, client: client, tuning: tuning,
                contextWindows: contextWindows, hooksFactory: hooksFactory, responses: responses))
            let (old, oldPort) = life.withLock { l in
                l.config = config
                return (l.server, l.serverConfigPort)
            }
            guard let old, oldPort != config.port else { return }
            do {
                let server = try await HTTPServer.start(port: config.port, token: accessToken, environment: environment.get)
                life.withLock { l in
                    l.server = server; l.serverConfigPort = config.port; l.state = .running(port: server.port)
                }
                Task { await old.shutdown(grace: .seconds(900)) }
            } catch {
                log.record(.error("engine: could not move to port \(config.port): \(error)"))
            }
        }
    }

    public var state: EngineState { get async { life.withLock { $0.state } } }

    /// Reachability, model list (with context windows) and logprob support
    /// of a backend, and how long its model list took.
    public func probe(_ backend: Backend) async -> BackendProbe {
        await BackendProber.probe(backend, secrets: secrets, client: client)
    }

    /// Runs operations one at a time, in call order.
    private func serialized<T: Sendable>(_ op: @escaping @Sendable () async -> T) async -> T {
        let task: Task<T, Never> = queue.withLock { tail in
            let previous = tail
            let t = Task { await previous?.value; return await op() }
            tail = Task { _ = await t.value }
            return t
        }
        return await task.value
    }
}

/// The current RequestEnvironment, read by every request.
final class EnvironmentBox: Sendable {
    private let current: Mutex<RequestEnvironment>
    init(_ env: RequestEnvironment) { current = Mutex(env) }
    @Sendable func get() -> RequestEnvironment { current.withLock { $0 } }
    func set(_ env: RequestEnvironment) { current.withLock { $0 = env } }
}
