import Foundation
import Hummingbird
import NIOCore

/// Timings tests shorten.
struct EngineTuning: Sendable {
    var pingInterval: Duration = .seconds(15)
    /// Wait for a backend's response headers when the config doesn't set one.
    var defaultHeaderTimeout: Duration = .seconds(600)
    var maxRequestBody = 128 << 20
}

/// Everything one request runs with: a snapshot of the configuration and the
/// hooks built for it. `apply` swaps the engine's environment; a request in
/// flight keeps the one it started with.
final class RequestEnvironment: Sendable {
    let config: EngineConfig
    let routing: Routing
    let hooks: EngineHooks
    let caller: EngineModelCaller
    let client: UpstreamClient
    let log: any EventLog
    let tuning: EngineTuning
    let contextWindows: ContextWindows
    let responses: any ResponsesHandler

    init(config: EngineConfig, secrets: any SecretStore, log: any EventLog, client: UpstreamClient, tuning: EngineTuning,
         contextWindows: ContextWindows, hooksFactory: any EngineHooksFactory, responses: any ResponsesHandler) {
        self.config = config
        self.routing = Routing(config: config, secrets: secrets)
        self.client = client
        self.log = log
        self.tuning = tuning
        self.contextWindows = contextWindows
        self.responses = responses
        let caller = EngineModelCaller(routing: routing, client: client, headerTimeout: tuning.headerTimeout(config))
        self.caller = caller
        self.hooks = hooksFactory.makeHooks(config: config, caller: caller, secrets: secrets, log: log)
    }

    var headerTimeout: Duration { tuning.headerTimeout(config) }
}

extension EngineTuning {
    func headerTimeout(_ config: EngineConfig) -> Duration {
        if let s = config.headerTimeoutSeconds, s > 0 { return .milliseconds(Int64(s * 1000)) }
        return defaultHeaderTimeout
    }
}

/// What a handler needs from the HTTP layer.
struct IncomingRequest: Sendable {
    var body: Data
    /// Lower-cased header names.
    var headers: [String: String]
    var channel: (any Channel)?
}

/// `/v1/responses` (Codex, docs/DESIGN.md §5). Until it is built the engine
/// answers 501; the Responses team supplies a conforming handler.
protocol ResponsesHandler: Sendable {
    func respond(_ request: IncomingRequest, env: RequestEnvironment) async -> Response
}

struct NotImplementedResponses: ResponsesHandler {
    func respond(_ request: IncomingRequest, env: RequestEnvironment) async -> Response {
        .json(["error": ["type": "not_implemented", "code": "not_implemented",
                         "message": "the Responses API (Codex) is not available in this version of code-link"]],
              status: .notImplemented)
    }
}
