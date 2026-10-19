import Foundation

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
    public init(config: EngineConfig, secrets: SecretStore, log: EventLog, accessToken: String) {
        self.accessToken = accessToken
        // Implementation: engine team (docs/DESIGN.md).
    }
    public func start() async throws {}
    public func stop() async {}
    public func apply(_ config: EngineConfig) async {}
    public var state: EngineState { get async { .stopped } }
    public func probe(_ backend: Backend) async -> BackendProbe { BackendProbe(reachable: false, error: "not implemented") }
}
