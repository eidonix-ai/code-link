import Darwin
import Foundation
import Synchronization
import Testing
@testable import CodeLinkEngine

/// A port nothing listens on right now.
private func freePort() -> Int {
    let fd = socket(AF_INET, SOCK_STREAM, 0)
    defer { close(fd) }
    var addr = sockaddr_in()
    addr.sin_family = sa_family_t(AF_INET)
    addr.sin_addr.s_addr = inet_addr("127.0.0.1")
    addr.sin_port = 0
    var len = socklen_t(MemoryLayout<sockaddr_in>.size)
    _ = withUnsafePointer(to: &addr) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, len) } }
    _ = withUnsafeMutablePointer(to: &addr) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &len) } }
    return Int(UInt16(bigEndian: addr.sin_port))
}

/// Keeps the caller the engine handed to the hooks factory.
private final class CapturingFactory: EngineHooksFactory {
    let caller = Mutex<(any ModelCaller)?>(nil)
    func makeHooks(config: EngineConfig, caller: any ModelCaller, secrets: any SecretStore, log: any EventLog) -> EngineHooks {
        self.caller.withLock { $0 = caller }
        return EngineHooks()
    }
}

@Suite(.serialized) struct EngineLifecycleTests {
    @Test func applyReroutesTheNextRequestAndRebuildsHooks() async throws {
        let factory = CoreHooksFactory(EngineHooks())
        try await CoreHarness.run(factory: factory, backend: { _, _ in .json(200, Chunks.completion(content: "ok")) }) { h in
            _ = try await h.messages(messagesBody())
            #expect(h.backend.lastChat?["model"]?.string == "glm-upstream")
            var c = CoreHarness.defaultConfig(h.backend)
            c.models[0].upstreamModel = "glm-v2"
            c.roles.small = "GLM"
            await h.engine.apply(c)
            _ = try await h.messages(messagesBody())
            #expect(h.backend.lastChat?["model"]?.string == "glm-v2")
            _ = try await h.messages(messagesBody(model: "claude-haiku-4-5"))
            #expect(h.backend.lastChat?["model"]?.string == "glm-v2")
            #expect(factory.configs.count == 2)
            #expect(factory.configs.last?.models[0].upstreamModel == "glm-v2")
            guard case .running(let port) = await h.engine.state else { Issue.record("not running"); return }
            #expect(port == h.port) // same port: no restart
        }
    }

    @Test func aStreamInFlightSurvivesApply() async throws {
        let chunks = (0..<10).map { Chunks.content("w\($0) ") } + [Chunks.finish()]
        try await CoreHarness.run(backend: { _, _ in .sse(chunks, every: .milliseconds(40)) }) { h in
            let streaming = Task { try await h.messages(messagesBody(stream: true)) }
            try await Task.sleep(for: .milliseconds(120))
            var c = CoreHarness.defaultConfig(h.backend)
            c.models[0].upstreamModel = "changed"
            c.backends[0].baseURL = URL(string: "http://127.0.0.1:1/v1")!
            await h.engine.apply(c)
            let r = try await streaming.value
            #expect(r.events.last?.event == "message_stop")
            #expect(r.events.compactMap { $0.data?["delta"]?["text"]?.string }.joined() == (0..<10).map { "w\($0) " }.joined())
            // The next request uses the new (unreachable) backend.
            let next = try await h.messages(messagesBody())
            #expect(next.status == 502)
        }
    }

    @Test func applyWithANewPortMovesTheListener() async throws {
        try await CoreHarness.run(backend: { _, _ in .json(200, Chunks.completion(content: "ok")) }) { h in
            let newPort = freePort()
            var c = CoreHarness.defaultConfig(h.backend)
            c.port = newPort
            await h.engine.apply(c)
            #expect(await h.engine.state == .running(port: newPort))
            let health = try await h.request("GET", "/healthz", auth: false, port: newPort)
            #expect(health.status == 200)
            let r = try await h.request("POST", "/v1/messages", body: messagesBody(), port: newPort)
            #expect(r.status == 200)
            // The old listener drains and closes.
            let deadline = ContinuousClock.now + .seconds(5)
            var closed = false
            while !closed, ContinuousClock.now < deadline {
                do { _ = try await h.request("GET", "/healthz", auth: false) ; try await Task.sleep(for: .milliseconds(50)) }
                catch { closed = true }
            }
            #expect(closed)
        }
    }

    @Test func stopAndStartAgain() async throws {
        try await CoreHarness.run(backend: { _, _ in .json(200, Chunks.completion(content: "ok")) }) { h in
            await h.engine.stop()
            #expect(await h.engine.state == .stopped)
            await h.engine.stop() // idempotent
            try await h.engine.start()
            guard case .running(let port) = await h.engine.state else { Issue.record("not running"); return }
            let r = try await h.request("POST", "/v1/messages", body: messagesBody(), port: port)
            #expect(r.status == 200)
        }
    }

    @Test func aBusyPortFailsToStart() async throws {
        try await CoreHarness.run(backend: { _, _ in .json(200, "{}") }) { h in
            var c = CoreHarness.defaultConfig(h.backend)
            c.port = h.port
            let other = Engine(config: c, secrets: CoreSecrets(), log: CoreEventLog(), accessToken: "x")
            await #expect(throws: (any Error).self) { try await other.start() }
            guard case .failed = await other.state else { Issue.record("expected failed"); return }
        }
    }

    @Test func probeListsModelsWindowsLogprobsAndVision() async throws {
        try await CoreHarness.run(backend: { seen, _ in
            let model = seen.json?["model"]?.string
            #expect(seen.json?["max_tokens"] == 1)
            if seen.json?["logprobs"] == nil { // the vision probe
                let part = seen.json?["messages"]?[0]?["content"]?[0]
                #expect(part?["image_url"]?["url"]?.string?.hasPrefix("data:image/png;base64,") == true)
                return model == "glm-upstream" ? .json(400, #"{"error":{"message":"not a multimodal model"}}"#)
                                               : .json(200, Chunks.completion(content: "H"))
            }
            #expect(seen.json?["logprobs"] == true && seen.json?["top_logprobs"] == 2)
            if model == "glm-upstream" {
                return .json(200, #"{"choices":[{"index":0,"message":{"role":"assistant","content":"H"},"logprobs":{"content":[{"token":"H","logprob":-0.1,"top_logprobs":[{"token":"H","logprob":-0.1},{"token":"Hi","logprob":-2.3}]}]},"finish_reason":"length"}]}"#)
            }
            return .json(200, #"{"choices":[{"index":0,"message":{"role":"assistant","content":"H"},"logprobs":null,"finish_reason":"length"}]}"#)
        }) { h in
            let p = await h.engine.probe(Backend(id: "stub", baseURL: h.backend.baseURL, apiKeyRef: "stub-key"))
            #expect(p.reachable && p.error == nil && p.latencyMS != nil)
            #expect(p.models == [ProbedModel(upstreamModel: "glm-upstream", contextWindow: 131072, logprobs: true, vision: false),
                                 ProbedModel(upstreamModel: "other", contextWindow: 8192, logprobs: false, vision: true)])
            #expect(h.backend.requests.first?.headers["authorization"] == "Bearer sk-stub-secret")
            let down = await h.engine.probe(Backend(id: "x", baseURL: URL(string: "http://127.0.0.1:1/v1")!))
            #expect(!down.reachable && down.error != nil)
        }
    }

    @Test func contextWindowDetectedFromTheBackend() async throws {
        try await CoreHarness.run(configure: { $0.models[0].contextWindow = nil }, backend: { _, _ in
            .json(200, Chunks.completion(content: "ok"))
        }) { h in
            _ = try await h.messages(messagesBody().replacingOccurrences(of: #""max_tokens":100"#, with: #""max_tokens":500000"#))
            let clamped = h.backend.lastChat?["max_tokens"]?.int ?? 0
            #expect(clamped > 100_000 && clamped < 131_072)
            _ = try await h.messages(messagesBody())
            #expect(h.backend.requests.filter { $0.method == "GET" }.count == 1) // looked up once
        }
    }

    @Test func theModelCallerReachesTheBackend() async throws {
        let factory = CapturingFactory()
        try await CoreHarness.run(factory: factory, backend: { _, _ in .json(200, Chunks.completion(content: "described")) }) { h in
            let caller = try #require(factory.caller.withLock { $0 })
            let model = CoreHarness.defaultConfig(h.backend).models[1]
            let r = try await caller.chat(model: model, body: ["messages": .array([.object(["role": .string("user"), "content": .string("x")])]),
                                                                "max_tokens": .number(1), "model": .string("ignored")])
            #expect(r.status == 200)
            #expect(JSON(r.json)["choices"]?[0]?["message"]?["content"]?.string == "described")
            let sent = try #require(h.backend.lastChat)
            #expect(sent["model"]?.string == "small-upstream" && sent["stream"] == false && sent["max_tokens"] == 1)
            await #expect(throws: (any Error).self) {
                _ = try await caller.chat(model: Model(id: "nope", backendID: "missing", upstreamModel: "n"), body: [:])
            }
        }
    }
}
