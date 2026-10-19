import Foundation
import Testing
@testable import CodeLinkEngine

private let okCompletion: @Sendable (StubBackend.Seen, Int) -> StubBackend.Reply = { _, _ in .json(200, Chunks.completion(content: "ok")) }

@Suite(.serialized) struct ServerTests {
    // MARK: auth, health, models

    @Test func healthNeedsNoAuthButTheAPIDoes() async throws {
        try await CoreHarness.run(backend: okCompletion) { h in
            let health = try await h.request("GET", "/healthz", auth: false)
            #expect(health.status == 200 && health.json?["status"]?.string == "ok")

            let noAuth = try await h.request("POST", "/v1/messages", body: messagesBody(), auth: false)
            #expect(noAuth.status == 401)
            #expect(noAuth.json?["type"]?.string == "error" && noAuth.json?["error"]?["type"]?.string == "authentication_error")

            let wrong = try await h.request("POST", "/v1/messages", body: messagesBody(), headers: ["Authorization": "Bearer nope"], auth: false)
            #expect(wrong.status == 401)
            let modelsNoAuth = try await h.request("GET", "/v1/models", auth: false)
            #expect(modelsNoAuth.status == 401)

            let bearer = try await h.messages(messagesBody())
            #expect(bearer.status == 200)
            let apiKey = try await h.request("POST", "/v1/messages", body: messagesBody(), headers: ["x-api-key": CoreHarness.token], auth: false)
            #expect(apiKey.status == 200)
            #expect(h.backend.chatRequests.count == 2)
        }
    }

    @Test func modelsListsConfiguredIDsAndRoleAliases() async throws {
        try await CoreHarness.run(backend: okCompletion) { h in
            let r = try await h.request("GET", "/v1/models")
            #expect(r.status == 200)
            let ids = (r.json?["data"]?.array ?? []).compactMap { $0["id"]?.string }
            #expect(ids == ["GLM", "Small", "Big", "sonnet", "opus", "haiku"])
            #expect(r.json?["data"]?[0]?["type"]?.string == "model")
            #expect(r.json?["has_more"] == false && r.json?["first_id"]?.string == "GLM")
            #expect(r.json?["data"]?[4]?["display_name"]?.string == "Big (opus)")
        }
    }

    @Test func responsesIsNotImplementedYet() async throws {
        try await CoreHarness.run(backend: okCompletion) { h in
            let r = try await h.request("POST", "/v1/responses", body: #"{"model":"GLM","input":"hi"}"#)
            #expect(r.status == 501)
            #expect(h.backend.chatRequests.isEmpty)
        }
    }

    // MARK: non-streaming

    @Test func nonStreamingTranslatesBothWays() async throws {
        try await CoreHarness.run(backend: { _, _ in
            .json(200, Chunks.completion(content: "<block>no", reasoning: "think", finish: "stop", stopReason: "</block>", prompt: 100, completion: 4, cached: 60))
        }) { h in
            let r = try await h.messages(messagesBody(#""stop_sequences":["</block>"],"system":"sys""#))
            #expect(r.status == 200)
            let up = try #require(h.backend.lastChat)
            #expect(up["model"]?.string == "glm-upstream")
            #expect(up["stream"] == false)
            #expect(up["stop"] == ["</block>"])
            #expect(up["messages"]?[0]?["role"]?.string == "system")
            #expect(h.backend.chatRequests.last?.headers["authorization"] == "Bearer sk-stub-secret")
            let msg = try #require(r.json)
            #expect(msg["type"]?.string == "message" && msg["role"]?.string == "assistant" && msg["model"]?.string == "GLM")
            #expect(msg["content"]?.array?.map { $0.type ?? "" } == ["thinking", "text"])
            #expect(msg["stop_reason"]?.string == "stop_sequence" && msg["stop_sequence"]?.string == "</block>")
            #expect(msg["usage"]?["input_tokens"] == 40 && msg["usage"]?["cache_read_input_tokens"] == 60)
            #expect(h.log.requests.last?.status == 200)
        }
    }

    @Test func claudeModelNamesRouteToRoles() async throws {
        try await CoreHarness.run(backend: okCompletion) { h in
            for (name, upstream) in [("claude-haiku-4-5-20251001", "small-upstream"), ("claude-opus-5[1m]", "big-upstream"),
                                     ("claude-sonnet-5", "glm-upstream"), ("Small", "small-upstream"), ("whatever", "glm-upstream")] {
                let r = try await h.messages(messagesBody(model: name))
                #expect(r.status == 200)
                #expect(h.backend.lastChat?["model"]?.string == upstream, "\(name)")
                #expect(r.json?["model"]?.string == name)
            }
        }
    }

    @Test func unknownModelWithoutMainIs404() async throws {
        try await CoreHarness.run(configure: { $0.roles = Roles(main: "") }, backend: okCompletion) { h in
            let r = try await h.messages(messagesBody(model: "claude-sonnet-5"))
            #expect(r.status == 404 && r.json?["error"]?["type"]?.string == "not_found_error")
            let bad = try await h.messages("{nope")
            #expect(bad.status == 400 && bad.json?["error"]?["type"]?.string == "invalid_request_error")
            let missing = try await h.messages(#"{"messages":[]}"#)
            #expect(missing.status == 400)
        }
    }

    // MARK: streaming

    @Test func streamingEventOrderAndBlockIndices() async throws {
        let chunks = [
            Chunks.content("hello"),
            Chunks.reasoning("hmm"),
            Chunks.toolCall(index: 0, id: "call_a", name: "A", args: ""),
            Chunks.toolCall(index: 0, id: "call_a", name: nil, args: #"{"x":"#),
            Chunks.toolCall(index: 0, id: "call_a", name: nil, args: "1}"),
            Chunks.toolCall(index: 1, id: "call_b", name: "B", args: "{}"),
            Chunks.finish("stop"),
            Chunks.usage(prompt: 50, completion: 7, cached: 20),
        ]
        try await CoreHarness.run(backend: { _, _ in .sse(chunks) }) { h in
            let r = try await h.messages(messagesBody(stream: true))
            #expect(r.status == 200 && r.headers["content-type"] == "text/event-stream")
            let ev = r.events
            #expect(ev.first?.event == "message_start")
            #expect(ev.first?.data?["message"]?["model"]?.string == "GLM")
            #expect(ev.dropFirst().first?.event == "ping")
            let blocks = checkBlocksBalanced(ev)
            #expect(blocks == [0: "text", 1: "thinking", 2: "tool_use", 3: "tool_use"])
            let deltas = ev.filter { $0.event == "content_block_delta" && $0.data?["index"] == 2 }
            #expect(deltas.compactMap { $0.data?["delta"]?["partial_json"]?.string }.joined() == #"{"x":1}"#)
            #expect(ev.contains { $0.data?["delta"]?["type"]?.string == "signature_delta" })
            let delta = try #require(ev.first { $0.event == "message_delta" })
            #expect(delta.data?["delta"]?["stop_reason"]?.string == "tool_use")
            #expect(delta.data?["usage"]?["input_tokens"] == 30 && delta.data?["usage"]?["cache_read_input_tokens"] == 20
                    && delta.data?["usage"]?["output_tokens"] == 7)
            #expect(ev.last?.event == "message_stop")
            let up = try #require(h.backend.lastChat)
            #expect(up["stream"] == true && up["stream_options"]?["include_usage"] == true)
            #expect(h.backend.chatRequests.last?.headers["accept"] == "text/event-stream")
        }
    }

    @Test func thinkTagsInContentBecomeThinking() async throws {
        let chunks = [Chunks.content("<thi"), Chunks.content("nk>let me see</th"), Chunks.content("ink>The answer"), Chunks.finish()]
        try await CoreHarness.run(backend: { _, _ in .sse(chunks) }) { h in
            let ev = try await h.messages(messagesBody(stream: true)).events
            #expect(checkBlocksBalanced(ev) == [0: "thinking", 1: "text"])
            let thinking = ev.compactMap { $0.data?["delta"]?["thinking"]?.string }.joined()
            let text = ev.compactMap { $0.data?["delta"]?["text"]?.string }.joined()
            #expect(thinking == "let me see" && text == "The answer")
            #expect(ev.first { $0.event == "message_delta" }?.data?["delta"]?["stop_reason"]?.string == "end_turn")
        }
    }

    @Test func streamingStopSequenceAndMaxTokens() async throws {
        try await CoreHarness.run(backend: { _, n in
            n == 0 ? .sse([Chunks.content("<severity>12"), Chunks.chunk("{}", finish: "stop", extra: #","stop_reason":"</severity>""#)])
                   : .sse([Chunks.content("long"), Chunks.finish("length")])
        }) { h in
            let a = try await h.messages(messagesBody(#""stop_sequences":["</severity>"]"#, stream: true)).events
            let d = a.first { $0.event == "message_delta" }?.data?["delta"]
            #expect(d?["stop_reason"]?.string == "stop_sequence" && d?["stop_sequence"]?.string == "</severity>")
            let b = try await h.messages(messagesBody(stream: true)).events
            #expect(b.first { $0.event == "message_delta" }?.data?["delta"]?["stop_reason"]?.string == "max_tokens")
        }
    }

    @Test func truncatedStreamsEndWithAnOverloadedError() async throws {
        let cases: [StubBackend.Reply] = [
            .sse([Chunks.content("par")], done: false),
            .sse([Chunks.content("par")], done: true),
            .sse([], done: false),
            .rawStream("data: " + Chunks.content("par") + "\n\ndata: {\"choi"),
        ]
        for (i, reply) in cases.enumerated() {
            try await CoreHarness.run(backend: { _, _ in reply }) { h in
                let r = try await h.messages(messagesBody(stream: true))
                let ev = r.events
                #expect(r.status == 200)
                #expect(ev.last?.event == "error", "case \(i): \(ev.names)")
                #expect(ev.last?.data?["error"]?["type"]?.string == "overloaded_error")
                #expect(!ev.names.contains("message_stop"), "case \(i)")
                #expect(!ev.names.contains("message_delta"), "case \(i)")
            }
        }
    }

    @Test func completeStreamWithoutDoneIsAccepted() async throws {
        try await CoreHarness.run(backend: { _, _ in .sse([Chunks.content("hi"), Chunks.finish()], done: false) }) { h in
            let ev = try await h.messages(messagesBody(stream: true)).events
            #expect(ev.last?.event == "message_stop")
        }
    }

    @Test func inBandErrorFrame() async throws {
        let frame = #"{"error":{"message":"This model's maximum context length is 1000 tokens. However, you requested 1500 tokens (1400 in the messages, 100 in the completion).","code":400}}"#
        try await CoreHarness.run(backend: { _, n in
            n == 0 ? .sse([Chunks.content("a"), frame], done: false) : .sse([#"{"error":{"message":"engine dead"}}"#], done: false)
        }) { h in
            let ev = try await h.messages(messagesBody(stream: true)).events
            #expect(ev.last?.event == "error")
            #expect(ev.last?.data?["error"]?["message"]?.string == "prompt is too long: 1400 tokens > 1000 maximum")
            #expect(checkBlocksBalanced(ev) == [0: "text"])
            let ev2 = try await h.messages(messagesBody(stream: true)).events
            #expect(ev2.last?.data?["error"]?["type"]?.string == "overloaded_error")
        }
    }

    @Test func pingsWhileTheBackendIsQuiet() async throws {
        var tuning = EngineTuning()
        tuning.pingInterval = .milliseconds(25)
        try await CoreHarness.run(tuning: tuning, backend: { _, _ in
            .sse([Chunks.content("late"), Chunks.finish()], after: .milliseconds(400))
        }) { h in
            let ev = try await h.messages(messagesBody(stream: true)).events
            #expect(ev.first?.event == "message_start")
            let before = ev.prefix { $0.event != "content_block_start" }.filter { $0.event == "ping" }.count
            #expect(before >= 5, "\(ev.names)")
            #expect(ev.last?.event == "message_stop")
        }
    }

    @Test func messageStartArrivesBeforeTheFirstToken() async throws {
        try await CoreHarness.run(backend: { _, _ in .sse([Chunks.content("x"), Chunks.finish()], after: .seconds(1)) }) { h in
            var r = URLRequest(url: h.url("/v1/messages"))
            r.httpMethod = "POST"
            r.httpBody = Data(messagesBody(stream: true).utf8)
            r.setValue("Bearer \(CoreHarness.token)", forHTTPHeaderField: "Authorization")
            let started = ContinuousClock.now
            let (bytes, _) = try await h.session.bytes(for: r)
            var first: String?
            for try await line in bytes.lines where line.hasPrefix("event:") { first = line; break }
            #expect(first == "event: message_start")
            #expect(ContinuousClock.now - started < .milliseconds(900))
        }
    }

    // MARK: errors before the stream

    @Test func upstreamErrorsKeepTheirRealStatus() async throws {
        let cases: [(Int, Int, String)] = [(400, 400, "invalid_request_error"), (401, 502, "api_error"), (404, 404, "not_found_error"),
                                           (429, 429, "rate_limit_error"), (500, 500, "api_error"), (503, 503, "overloaded_error"),
                                           (529, 529, "overloaded_error")]
        for (up, status, type) in cases {
            try await CoreHarness.run(backend: { _, _ in
                .json(up, #"{"error":{"message":"upstream said no at http://10.0.0.1:8000"}}"#, headers: ["Retry-After": "7", "X-Should-Retry": "true"])
            }) { h in
                for stream in [false, true] {
                    let r = try await h.messages(messagesBody(stream: stream))
                    #expect(r.status == status, "upstream \(up) stream \(stream)")
                    #expect(r.json?["error"]?["type"]?.string == type)
                    #expect(r.json?["error"]?["message"]?.string?.contains("10.0.0.1") == false)
                    #expect(r.headers["retry-after"] == "7" && r.headers["x-should-retry"] == "true")
                }
            }
        }
    }

    @Test func contextOverflowRewrittenOverHTTP() async throws {
        try await CoreHarness.run(backend: { _, _ in
            .json(400, #"{"error":{"message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens (35000 in the messages, 5000 in the completion).","type":"BadRequestError","code":400}}"#)
        }) { h in
            let r = try await h.messages(messagesBody(stream: true))
            #expect(r.status == 400)
            #expect(r.json?["error"]?["message"]?.string == "prompt is too long: 35000 tokens > 32768 maximum")
        }
    }

    @Test func headerWaitIsBounded() async throws {
        try await CoreHarness.run(configure: { $0.headerTimeoutSeconds = 0.3 }, backend: { _, _ in
            .delayed(.seconds(3), .json(200, Chunks.completion(content: "late")))
        }) { h in
            let started = ContinuousClock.now
            let r = try await h.messages(messagesBody(stream: true))
            #expect(r.status == 504)
            #expect(ContinuousClock.now - started < .seconds(2))
        }
    }

    @Test func aStartedStreamOutlivesTheHeaderWait() async throws {
        try await CoreHarness.run(configure: { $0.headerTimeoutSeconds = 0.3 }, backend: { _, _ in
            .sse([Chunks.content("still here"), Chunks.finish()], after: .milliseconds(800))
        }) { h in
            let ev = try await h.messages(messagesBody(stream: true)).events
            #expect(ev.compactMap { $0.data?["delta"]?["text"]?.string }.joined() == "still here")
            #expect(ev.last?.event == "message_stop")
        }
    }

    @Test func unreachableBackendIs502() async throws {
        try await CoreHarness.run(configure: { $0.backends[0].baseURL = URL(string: "http://127.0.0.1:1/v1")! }, backend: okCompletion) { h in
            let r = try await h.messages(messagesBody())
            #expect(r.status == 502 && r.json?["error"]?["type"]?.string == "api_error")
        }
    }

    // MARK: disconnects

    @Test func clientDisconnectCancelsTheUpstream() async throws {
        try await CoreHarness.run(backend: { _, _ in .endless(every: .milliseconds(20)) }) { h in
            var r = URLRequest(url: h.url("/v1/messages"))
            r.httpMethod = "POST"
            r.httpBody = Data(messagesBody(stream: true).utf8)
            r.setValue("Bearer \(CoreHarness.token)", forHTTPHeaderField: "Authorization")
            let reader = Task {
                let (bytes, _) = try await h.session.bytes(for: r)
                var n = 0
                for try await line in bytes.lines where line.contains("text_delta") {
                    n += 1
                    if n == 3 { break }
                }
                return n
            }
            let read = try await reader.value
            #expect(read == 3)
            h.session.invalidateAndCancel()
            let deadline = ContinuousClock.now + .seconds(5)
            while h.backend.disconnected == 0, ContinuousClock.now < deadline { try await Task.sleep(for: .milliseconds(20)) }
            #expect(h.backend.disconnected == 1)
        }
    }

    // MARK: count_tokens

    @Test func countTokensEstimates() async throws {
        try await CoreHarness.run(backend: okCompletion) { h in
            let text = String(repeating: "word ", count: 400)
            let r = try await h.request("POST", "/v1/messages/count_tokens", body: #"{"model":"claude-sonnet-5","messages":[{"role":"user","content":"\#(text)"}]}"#)
            #expect(r.status == 200)
            let n = r.json?["input_tokens"]?.int ?? 0
            #expect(n > 500 && n < 600)
            #expect(h.backend.chatRequests.isEmpty)
        }
    }

    // MARK: Anthropic backends

    @Test func anthropicBackendsPassThrough() async throws {
        try await CoreHarness.run(configure: { c in
            c.backends.append(Backend(id: "anth", kind: .anthropic, baseURL: c.backends[0].baseURL, apiKeyRef: "stub-key"))
            c.models.append(Model(id: "Claude", backendID: "anth", upstreamModel: "claude-real-1"))
        }, backend: { seen, _ in
            if seen.path.hasSuffix("/messages/count_tokens") { return .json(404, #"{"error":"nope"}"#) }
            if seen.json?["stream"] == true {
                return .rawStream("event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
            }
            if seen.json?["max_tokens"] == 1 {
                return .json(429, #"{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}"#, headers: ["Retry-After": "30"])
            }
            return .json(200, #"{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"native"}],"model":"claude-real-1","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}"#)
        }) { h in
            let body = #"{"model":"Claude","max_tokens":10,"tools":[{"name":"T","input_schema":{"type":"object","properties":{"b":{},"a":{}}}}],"messages":[{"role":"user","content":"hi"}]}"#
            let r = try await h.messages(body, headers: ["anthropic-beta": "x-1", "anthropic-version": "2023-06-01"])
            #expect(r.status == 200 && r.json?["content"]?[0]?["text"]?.string == "native")
            let seen = try #require(h.backend.requests.last)
            #expect(seen.path == "/v1/messages")
            #expect(seen.headers["x-api-key"] == "sk-stub-secret" && seen.headers["anthropic-beta"] == "x-1")
            #expect(seen.json?["model"]?.string == "claude-real-1")
            #expect(String(decoding: seen.body, as: UTF8.self).contains(#""properties":{"b":{},"a":{}}"#))

            let s = try await h.messages(#"{"model":"Claude","max_tokens":10,"stream":true,"messages":[]}"#)
            #expect(s.events.names == ["message_start", "message_stop"])

            let e = try await h.messages(#"{"model":"Claude","max_tokens":1,"messages":[]}"#)
            #expect(e.status == 429 && e.json?["error"]?["type"]?.string == "rate_limit_error" && e.headers["retry-after"] == "30")

            let c = try await h.request("POST", "/v1/messages/count_tokens", body: #"{"model":"Claude","messages":[{"role":"user","content":"hello hello"}]}"#)
            #expect(c.status == 200 && c.json?["input_tokens"] != nil)
        }
    }
}
