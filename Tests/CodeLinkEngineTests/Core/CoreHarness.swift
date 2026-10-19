import Foundation
import HTTPTypes
import Hummingbird
import Logging
import NIOCore
import ServiceLifecycle
import Synchronization
import Testing
@testable import CodeLinkEngine

// MARK: - Stub Chat Completions backend

/// An in-process HTTP backend that records what the engine sends and answers
/// with scripted replies.
final class StubBackend: Sendable {
    struct Seen: Sendable {
        var path: String
        var method: String
        var headers: [String: String]
        var body: Data
        var json: JSON? { try? JSON.parse(body) }
    }
    indirect enum Reply: Sendable {
        case json(Int, String, headers: [String: String] = [:])
        /// Each string is one `data:` payload.
        case sse([String], done: Bool = true, every: Duration = .zero, after: Duration = .zero)
        /// Bytes as given, as an event stream.
        case rawStream(String)
        /// Waits, then answers.
        case delayed(Duration, Reply)
        /// Streams `count` chunks, one per `every`; records whether the
        /// client went away.
        case endless(every: Duration)
    }

    let port: Int
    private let seen = Mutex<[Seen]>([])
    private let disconnects = Mutex(0)
    private let group: ServiceGroup
    private let task: Task<Void, any Error>

    var baseURL: URL { URL(string: "http://127.0.0.1:\(port)/v1")! }
    var requests: [Seen] { seen.withLock { $0 } }
    var chatRequests: [Seen] { requests.filter { $0.path.hasSuffix("/chat/completions") } }
    var lastChat: JSON? { chatRequests.last?.json }
    var disconnected: Int { disconnects.withLock { $0 } }

    private init(port: Int, group: ServiceGroup, task: Task<Void, any Error>) {
        self.port = port; self.group = group; self.task = task
    }

    static func start(_ handler: @escaping @Sendable (Seen, Int) -> Reply) async throws -> StubBackend {
        let logger = Logger(label: "stub", factory: { _ in SwiftLogNoOpLogHandler() })
        let router = Router()
        let box = Mutex<StubBackend?>(nil)
        @Sendable func answer(_ request: Request) async throws -> Response {
            var headers: [String: String] = [:]
            for f in request.headers { headers[f.name.canonicalName] = f.value }
            let body = try await request.body.collect(upTo: 256 << 20)
            let s = Seen(path: request.uri.path, method: request.method.rawValue, headers: headers, body: Data(body.readableBytesView))
            guard let backend = box.withLock({ $0 }) else { return Response(status: .serviceUnavailable) }
            let n = backend.seen.withLock { $0.append(s); return $0.count - 1 }
            return await backend.respond(handler(s, n))
        }
        router.get("**") { request, _ in try await answer(request) }
        router.post("**") { request, _ in try await answer(request) }
        let (ports, cont) = AsyncStream.makeStream(of: Int.self)
        let app = Application(router: router, configuration: .init(address: .hostname("127.0.0.1", port: 0)),
                              onServerRunning: { cont.yield($0.localAddress?.port ?? 0); cont.finish() }, logger: logger)
        let group = ServiceGroup(configuration: .init(services: [app], gracefulShutdownSignals: [], logger: logger))
        let task = Task { try await group.run() }
        var it = ports.makeAsyncIterator()
        let port = await it.next() ?? 0
        let backend = StubBackend(port: port, group: group, task: task)
        box.withLock { $0 = backend }
        return backend
    }

    func stop() async {
        await group.triggerGracefulShutdown()
        let t = task
        let timer = Task { try await Task.sleep(for: .seconds(2)); t.cancel() }
        _ = await task.result
        timer.cancel()
    }

    private func respond(_ reply: Reply) async -> Response {
        switch reply {
        case .json(let status, let body, let extra):
            var h: HTTPFields = [.contentType: "application/json"]
            for (k, v) in extra { if let n = HTTPField.Name(k) { h[n] = v } }
            return Response(status: .init(code: status), headers: h, body: .init(byteBuffer: ByteBuffer(string: body)))
        case .delayed(let d, let r):
            try? await Task.sleep(for: d)
            return await respond(r)
        case .rawStream(let s):
            return Response(status: .ok, headers: [.contentType: "text/event-stream"], body: .init(byteBuffer: ByteBuffer(string: s)))
        case .sse(let chunks, let done, let every, let after):
            return Response(status: .ok, headers: [.contentType: "text/event-stream"], body: ResponseBody { writer in
                if after > .zero {
                    try await writer.write(ByteBuffer(string: ": hi\n\n"))
                    try await Task.sleep(for: after)
                }
                for c in chunks {
                    if every > .zero { try await Task.sleep(for: every) }
                    try await writer.write(ByteBuffer(string: "data: \(c)\n\n"))
                }
                if done { try await writer.write(ByteBuffer(string: "data: [DONE]\n\n")) }
                try await writer.finish(nil)
            })
        case .endless(let every):
            return Response(status: .ok, headers: [.contentType: "text/event-stream"], body: ResponseBody { [self] writer in
                do {
                    for i in 0..<10_000 {
                        try await writer.write(ByteBuffer(string: "data: \(Chunks.content("t\(i) "))\n\n"))
                        try await Task.sleep(for: every)
                    }
                    try await writer.finish(nil)
                } catch {
                    disconnects.withLock { $0 += 1 }
                    throw error
                }
            })
        }
    }
}

/// Chat Completions response fixtures.
enum Chunks {
    static func chunk(_ delta: String, finish: String? = nil, extra: String = "") -> String {
        let fr = finish.map { "\"\($0)\"" } ?? "null"
        return #"{"id":"c","object":"chat.completion.chunk","model":"glm-upstream","choices":[{"index":0,"delta":\#(delta),"finish_reason":\#(fr)\#(extra)}]}"#
    }
    static func content(_ s: String) -> String { chunk(#"{"content":\#(JSON.string(s).text)}"#) }
    static func reasoning(_ s: String) -> String { chunk(#"{"reasoning_content":\#(JSON.string(s).text)}"#) }
    static func finish(_ reason: String = "stop") -> String { chunk("{}", finish: reason) }
    static func usage(prompt: Int, completion: Int, cached: Int = 0) -> String {
        #"{"id":"c","choices":[],"usage":{"prompt_tokens":\#(prompt),"completion_tokens":\#(completion),"prompt_tokens_details":{"cached_tokens":\#(cached)}}}"#
    }
    static func toolCall(index: Int, id: String?, name: String?, args: String) -> String {
        var fn = #""arguments":\#(JSON.string(args).text)"#
        if let name { fn = #""name":"\#(name)","# + fn }
        let idPart = id.map { #""id":"\#($0)","type":"function","# } ?? ""
        return chunk(#"{"tool_calls":[{"index":\#(index),\#(idPart)"function":{\#(fn)}}]}"#)
    }
    static func completion(content: String?, reasoning: String? = nil, toolCalls: String? = nil, finish: String = "stop",
                           stopReason: String? = nil, prompt: Int = 10, completion: Int = 5, cached: Int = 0) -> String {
        var msg = #""role":"assistant","content":\#(content.map { JSON.string($0).text } ?? "null")"#
        if let reasoning { msg += #","reasoning_content":\#(JSON.string(reasoning).text)"# }
        if let toolCalls { msg += #","tool_calls":\#(toolCalls)"# }
        let sr = stopReason.map { #","stop_reason":"\#($0)""# } ?? ""
        return #"{"id":"c","object":"chat.completion","model":"glm-upstream","choices":[{"index":0,"message":{\#(msg)},"finish_reason":"\#(finish)"\#(sr)}],"usage":{"prompt_tokens":\#(prompt),"completion_tokens":\#(completion),"total_tokens":\#(prompt + completion),"prompt_tokens_details":{"cached_tokens":\#(cached)}}}"#
    }
}

// MARK: - Fakes

final class CoreEventLog: EventLog {
    private let events = Mutex<[EngineEvent]>([])
    func record(_ event: EngineEvent) { events.withLock { $0.append(event) } }
    var all: [EngineEvent] { events.withLock { $0 } }
    var requests: [(route: String, model: String, status: Int)] {
        all.compactMap { if case .request(let r, let m, let s, _) = $0 { return (r, m, s) } else { return nil } }
    }
}

struct CoreSecrets: SecretStore {
    var values: [String: String] = ["stub-key": "sk-stub-secret"]
    func secret(_ ref: String) -> String? { values[ref] }
}

final class CoreDocumentReader: DocumentReader {
    private let seen = Mutex<[DocumentInput]>([])
    let output: @Sendable (DocumentInput) -> DocumentOutput
    init(_ output: @escaping @Sendable (DocumentInput) -> DocumentOutput = { d in
        DocumentOutput(text: "EXTRACTED \(d.data.count) bytes of \(d.mediaType)", pages: 2, ocrPages: 1, via: "fake")
    }) { self.output = output }
    func read(_ document: DocumentInput) async -> DocumentOutput {
        seen.withLock { $0.append(document) }
        return output(document)
    }
    var inputs: [DocumentInput] { seen.withLock { $0 } }
}

final class CoreImageDescriber: ImageDescriber {
    private let count = Mutex(0)
    let answer: String?
    init(_ answer: String? = "a cat on a mat") { self.answer = answer }
    func describe(_ image: Data, mediaType: String) async -> String? {
        count.withLock { $0 += 1 }
        return answer
    }
    var calls: Int { count.withLock { $0 } }
}

final class CoreSearcher: WebSearcher {
    private let seen = Mutex<[String]>([])
    let result: @Sendable (String) throws -> [SearchHit]
    init(_ result: @escaping @Sendable (String) throws -> [SearchHit] = { q in
        [SearchHit(title: "Result for \(q)", url: "https://example.com/\(q.count)", snippet: "Snippet about \(q)", pageAge: "2026-01-01")]
    }) { self.result = result }
    func search(query: String, allowedDomains: [String]?, blockedDomains: [String]?) async throws -> [SearchHit] {
        seen.withLock { $0.append(query) }
        return try result(query)
    }
    var queries: [String] { seen.withLock { $0 } }
}

final class CoreAutoMode: AutoModeResponder {
    static let marker = "You are a security monitor for autonomous AI coding agents"
    let answer: JSONValue?
    let decisionModel: Model?
    private let calls = Mutex(0)
    init(answer: JSONValue?, decisionModel: Model?) { self.answer = answer; self.decisionModel = decisionModel }
    func isClassifierRequest(_ request: JSONValue) -> Bool {
        RequestTranslator.system(JSON(request)["system"]).contains(Self.marker)
    }
    func respond(to request: JSONValue) async -> JSONValue? {
        calls.withLock { $0 += 1 }
        return answer
    }
    var responded: Int { calls.withLock { $0 } }
}

/// Returns fixed hooks and counts how often it was asked.
final class CoreHooksFactory: EngineHooksFactory {
    let hooks: EngineHooks
    private let made = Mutex<[EngineConfig]>([])
    init(_ hooks: EngineHooks) { self.hooks = hooks }
    func makeHooks(config: EngineConfig, caller: any ModelCaller, secrets: any SecretStore, log: any EventLog) -> EngineHooks {
        made.withLock { $0.append(config) }
        return hooks
    }
    var configs: [EngineConfig] { made.withLock { $0 } }
}

// MARK: - Harness

struct HTTPReply {
    var status: Int
    var headers: [String: String]
    var body: Data
    var text: String { String(decoding: body, as: UTF8.self) }
    var json: JSON? { try? JSON.parse(body) }
    var events: [SSEEvent] { SSEEvent.parse(text) }
}

struct SSEEvent {
    var event: String
    var data: JSON?
    static func parse(_ s: String) -> [SSEEvent] {
        s.components(separatedBy: "\n\n").compactMap { block in
            var name = "", data = ""
            for line in block.split(separator: "\n") {
                if line.hasPrefix("event: ") { name = String(line.dropFirst(7)) }
                if line.hasPrefix("data: ") { data = String(line.dropFirst(6)) }
            }
            return name.isEmpty ? nil : SSEEvent(event: name, data: try? JSON.parse(data))
        }
    }
}

extension Array where Element == SSEEvent {
    var names: [String] { map(\.event) }
    var withoutPings: [SSEEvent] { filter { $0.event != "ping" } }
}

struct CoreHarness {
    static let token = "test-token-123"
    let engine: Engine
    let backend: StubBackend
    let log: CoreEventLog
    let port: Int
    let session: URLSession

    static func defaultConfig(_ backend: StubBackend) -> EngineConfig {
        EngineConfig(port: 0,
                     backends: [Backend(id: "stub", baseURL: backend.baseURL, apiKeyRef: "stub-key")],
                     models: [Model(id: "GLM", backendID: "stub", upstreamModel: "glm-upstream", contextWindow: 200_000),
                              Model(id: "Small", backendID: "stub", upstreamModel: "small-upstream", contextWindow: 32_000),
                              Model(id: "Big", backendID: "stub", upstreamModel: "big-upstream", contextWindow: 400_000)],
                     roles: Roles(main: "GLM", opus: "Big", small: "Small"))
    }

    static func run(hooks: EngineHooks = EngineHooks(), factory: (any EngineHooksFactory)? = nil,
                    tuning: EngineTuning = EngineTuning(),
                    configure: (inout EngineConfig) -> Void = { _ in },
                    backend handler: @escaping @Sendable (StubBackend.Seen, Int) -> StubBackend.Reply,
                    _ body: (CoreHarness) async throws -> Void) async throws {
        let backend = try await StubBackend.start { seen, n in
            if seen.method == "GET", seen.path.hasSuffix("/models") {
                return .json(200, #"{"object":"list","data":[{"id":"glm-upstream","max_model_len":131072},{"id":"other","max_model_len":8192}]}"#)
            }
            return handler(seen, n)
        }
        var config = defaultConfig(backend)
        configure(&config)
        let log = CoreEventLog()
        let engine = Engine(config: config, secrets: CoreSecrets(), log: log, accessToken: token,
                            hooksFactory: factory ?? CoreHooksFactory(hooks), tuning: tuning, responses: CodexResponses())
        try await engine.start()
        guard case .running(let port) = await engine.state else {
            Issue.record("engine not running")
            return
        }
        let c = URLSessionConfiguration.ephemeral
        c.timeoutIntervalForRequest = 30
        let h = CoreHarness(engine: engine, backend: backend, log: log, port: port, session: URLSession(configuration: c))
        do {
            try await body(h)
        } catch {
            await engine.stop(); await backend.stop()
            throw error
        }
        await engine.stop()
        await backend.stop()
    }

    func url(_ path: String, port: Int? = nil) -> URL { URL(string: "http://127.0.0.1:\(port ?? self.port)\(path)")! }

    func request(_ method: String, _ path: String, body: String? = nil, headers: [String: String] = [:],
                 auth: Bool = true, port: Int? = nil) async throws -> HTTPReply {
        var r = URLRequest(url: url(path, port: port))
        r.httpMethod = method
        if let body { r.httpBody = Data(body.utf8); r.setValue("application/json", forHTTPHeaderField: "Content-Type") }
        if auth { r.setValue("Bearer \(Self.token)", forHTTPHeaderField: "Authorization") }
        for (k, v) in headers { r.setValue(v, forHTTPHeaderField: k) }
        let (data, resp) = try await session.data(for: r)
        let http = resp as! HTTPURLResponse
        var h: [String: String] = [:]
        for (k, v) in http.allHeaderFields { if let k = k as? String { h[k.lowercased()] = "\(v)" } }
        return HTTPReply(status: http.statusCode, headers: h, body: data)
    }

    func messages(_ body: String, headers: [String: String] = [:]) async throws -> HTTPReply {
        try await request("POST", "/v1/messages", body: body, headers: headers)
    }
}

/// A minimal Messages request.
func messagesBody(_ extra: String = "", model: String = "GLM", stream: Bool = false,
                  messages: String = #"[{"role":"user","content":"hi"}]"#) -> String {
    #"{"model":"\#(model)","max_tokens":100,"stream":\#(stream),"messages":\#(messages)\#(extra.isEmpty ? "" : "," + extra)}"#
}

/// Every content_block_start has a unique index and exactly one stop.
func checkBlocksBalanced(_ events: [SSEEvent], sourceLocation: SourceLocation = #_sourceLocation) -> [Int: String] {
    var started: [Int: String] = [:]
    var stopped = Set<Int>()
    for e in events {
        guard let idx = e.data?["index"]?.int else { continue }
        if e.event == "content_block_start" {
            #expect(started[idx] == nil, "duplicate block index \(idx)", sourceLocation: sourceLocation)
            started[idx] = e.data?["content_block"]?.type ?? "?"
        } else if e.event == "content_block_stop" {
            #expect(started[idx] != nil && !stopped.contains(idx), "unbalanced stop \(idx)", sourceLocation: sourceLocation)
            stopped.insert(idx)
        } else if e.event == "content_block_delta" {
            #expect(started[idx] != nil && !stopped.contains(idx), "delta outside block \(idx)", sourceLocation: sourceLocation)
        }
    }
    #expect(Set(started.keys) == stopped, "blocks left open", sourceLocation: sourceLocation)
    return started
}
