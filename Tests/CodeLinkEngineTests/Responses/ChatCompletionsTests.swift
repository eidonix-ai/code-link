import Foundation
import Testing
@testable import CodeLinkEngine

private func chatBody(_ extra: String = "", model: String = "GLM", stream: Bool = false,
                      messages: String = #"[{"role":"user","content":"hi"}]"#) -> String {
    #"{"model":"\#(model)","stream":\#(stream),"messages":\#(messages)\#(extra.isEmpty ? "" : "," + extra)}"#
}

private let okChat: @Sendable (StubBackend.Seen, Int) -> StubBackend.Reply = { _, _ in
    .json(200, Chunks.completion(content: "ok"))
}

/// Anthropic Messages stream events.
private enum A {
    static func start(input: Int = 25, cacheRead: Int = 0) -> String {
        #"{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-x","usage":{"input_tokens":\#(input),"output_tokens":1,"cache_read_input_tokens":\#(cacheRead)}}}"#
    }
    static func block(_ i: Int, _ b: String) -> String { #"{"type":"content_block_start","index":\#(i),"content_block":\#(b)}"# }
    static func delta(_ i: Int, _ d: String) -> String { #"{"type":"content_block_delta","index":\#(i),"delta":\#(d)}"# }
    static func stop(_ i: Int) -> String { #"{"type":"content_block_stop","index":\#(i)}"# }
    static func text(_ i: Int, _ t: String) -> String { delta(i, #"{"type":"text_delta","text":\#(JSON.string(t).text)}"#) }
    static func end(_ reason: String, output: Int = 9) -> String {
        #"{"type":"message_delta","delta":{"stop_reason":"\#(reason)"},"usage":{"output_tokens":\#(output)}}"#
    }
    static let messageStop = #"{"type":"message_stop"}"#
    static let ping = #"{"type":"ping"}"#
}

@Suite(.serialized) struct ChatCompletionsTests {
    // MARK: OpenAI-compatible backends

    @Test func streamPassesThroughVerbatim() async throws {
        let payload = [
            "data: " + Chunks.chunk(#"{"role":"assistant"}"#),
            ": a comment the backend sent",
            "data: " + Chunks.content("Hel"),
            "data: " + Chunks.reasoning("hmm"),
            "data: " + Chunks.content("lo"),
            "data: " + Chunks.finish("stop"),
            "data: " + Chunks.usage(prompt: 9, completion: 3),
            "data: [DONE]",
        ].joined(separator: "\n\n") + "\n\n"
        try await CoreHarness.run(backend: { _, _ in .rawStream(payload) }) { h in
            let r = try await h.chat(chatBody(#""temperature":0.2,"stream_options":{"include_usage":true}"#, stream: true))
            #expect(r.status == 200 && r.headers["content-type"]?.contains("text/event-stream") == true)
            #expect(r.text == payload)
            let up = try #require(h.backend.lastChat)
            #expect(up["model"]?.string == "glm-upstream" && up["stream"] == true && up["temperature"] == 0.2)
            #expect(up["stream_options"]?["include_usage"] == true)
            #expect(h.backend.chatRequests.last?.headers["authorization"] == "Bearer sk-stub-secret")
            #expect(h.backend.chatRequests.last?.headers["accept"] == "text/event-stream")
            #expect(h.log.requests.last?.route == "/v1/chat/completions" && h.log.requests.last?.status == 200)
        }
    }

    @Test func chunksArriveAsTheBackendSendsThem() async throws {
        try await CoreHarness.run(backend: { _, _ in
            .sse([Chunks.content("one"), Chunks.content("two"), Chunks.finish()], every: .milliseconds(200))
        }) { h in
            var r = URLRequest(url: h.url("/v1/chat/completions"))
            r.httpMethod = "POST"
            r.httpBody = Data(chatBody(stream: true).utf8)
            r.setValue("Bearer \(CoreHarness.token)", forHTTPHeaderField: "Authorization")
            let (bytes, _) = try await h.session.bytes(for: r)
            var stamps: [ContinuousClock.Instant] = []
            for try await line in bytes.lines where line.hasPrefix("data: {") { stamps.append(.now) }
            #expect(stamps.count == 3)
            // Not buffered into one write: the first chunk is well ahead of the last.
            #expect(stamps[2] - stamps[0] > .milliseconds(250))
        }
    }

    @Test func nonStreamingAnswerIsForwardedAsIs() async throws {
        let body = Chunks.completion(content: "plain", reasoning: "why", prompt: 5, completion: 2)
        try await CoreHarness.run(backend: { _, _ in .json(200, body) }) { h in
            let r = try await h.chat(chatBody(#""max_tokens":50,"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]"#))
            #expect(r.status == 200 && r.text == body)
            let up = try #require(h.backend.lastChat)
            #expect(up["stream"] == false && up["max_tokens"] == 50 && up["tools"]?[0]?["function"]?["name"]?.string == "f")
            #expect(up["messages"] == [["role": "user", "content": "hi"]])
        }
    }

    @Test func modelNamesMapLikeTheMessagesRoute() async throws {
        try await CoreHarness.run(backend: okChat) { h in
            for (name, upstream) in [("GLM", "glm-upstream"), ("Small", "small-upstream"), ("opus", "big-upstream"),
                                     ("sonnet", "glm-upstream"), ("haiku", "small-upstream"), ("claude-haiku-4-5-20251001", "small-upstream"),
                                     ("claude-opus-5[1m]", "big-upstream"), ("claude-sonnet-5", "glm-upstream"), ("gpt-4o", "glm-upstream")] {
                let r = try await h.chat(chatBody(model: name))
                #expect(r.status == 200)
                #expect(h.backend.lastChat?["model"]?.string == upstream, "\(name)")
            }
        }
        try await CoreHarness.run(configure: { $0.roles = Roles(main: "") }, backend: okChat) { h in
            let r = try await h.chat(chatBody(model: "gpt-4o"))
            #expect(r.status == 404 && r.json?["error"]?["code"]?.string == "model_not_found")
        }
    }

    @Test func badRequestsAreOpenAIShapedErrors() async throws {
        try await CoreHarness.run(backend: okChat) { h in
            for body in ["{nope", #"{"messages":[{"role":"user","content":"x"}]}"#, #"{"model":"GLM"}"#, #"{"model":"GLM","messages":[]}"#] {
                let r = try await h.chat(body)
                #expect(r.status == 400 && r.json?["error"]?["type"]?.string == "invalid_request_error")
                #expect(r.json?["error"]?["message"]?.string?.isEmpty == false)
            }
            let noAuth = try await h.request("POST", "/v1/chat/completions", body: chatBody(), auth: false)
            #expect(noAuth.status == 401)
            #expect(h.backend.chatRequests.isEmpty)
        }
    }

    @Test func backendErrorsKeepTheirStatus() async throws {
        let cases: [(Int, String, [String: String], Int)] = [
            (400, #"{"error":{"message":"bad schema","type":"BadRequestError","param":"tools","code":400}}"#, [:], 400),
            (404, #"{"error":{"message":"The model `x` does not exist.","type":"NotFoundError","code":404}}"#, [:], 404),
            (429, #"{"error":{"message":"slow down","type":"rate_limit"}}"#, ["retry-after": "11"], 429),
            (503, #"{"error":{"message":"overloaded"}}"#, [:], 503),
            (500, #"{"error":{"message":"boom"}}"#, [:], 500),
        ]
        for (status, body, headers, want) in cases {
            try await CoreHarness.run(backend: { _, _ in .json(status, body, headers: headers) }) { h in
                for stream in [false, true] {
                    let r = try await h.chat(chatBody(stream: stream))
                    #expect(r.status == want && r.text == body, "\(status) stream=\(stream)")
                    if let retry = headers["retry-after"] { #expect(r.headers["retry-after"] == retry) }
                    #expect(h.log.requests.last?.status == want)
                }
            }
        }
        // The engine's own credentials failing is a gateway problem, not the client's.
        try await CoreHarness.run(backend: { _, _ in .json(401, #"{"error":{"message":"Incorrect API key sk-live-abcdef123456"}}"#) }) { h in
            let r = try await h.chat(chatBody())
            #expect(r.status == 502 && r.json?["error"]?["code"]?.string == "upstream_auth_error")
            #expect(!r.text.contains("abcdef"))
        }
        // A body that is not an OpenAI error is wrapped, and scrubbed.
        try await CoreHarness.run(backend: { _, _ in .json(500, "engine died at 10.1.2.3") }) { h in
            let r = try await h.chat(chatBody())
            #expect(r.status == 500 && r.json?["error"]?["type"]?.string == "server_error")
            #expect(!r.text.contains("10.1.2.3"))
        }
        try await CoreHarness.run(configure: { $0.backends[0].baseURL = URL(string: "http://127.0.0.1:1/v1")! }, backend: okChat) { h in
            let r = try await h.chat(chatBody())
            #expect(r.status == 502 && r.json?["error"]?["code"]?.string == "upstream_unreachable")
        }
    }

    @Test func clientDisconnectCancelsTheUpstream() async throws {
        try await CoreHarness.run(backend: { _, _ in .endless(every: .milliseconds(20)) }) { h in
            var r = URLRequest(url: h.url("/v1/chat/completions"))
            r.httpMethod = "POST"
            r.httpBody = Data(chatBody(stream: true).utf8)
            r.setValue("Bearer \(CoreHarness.token)", forHTTPHeaderField: "Authorization")
            let reader = Task {
                let (bytes, _) = try await h.session.bytes(for: r)
                var n = 0
                for try await line in bytes.lines where line.hasPrefix("data: {") {
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

    // MARK: documents and images

    @Test func imagesForAModelWithoutVisionAreDescribedOrNoted() async throws {
        let png = Data(repeating: 3, count: 64).base64EncodedString()
        let messages = #"""
        [{"role":"user","content":[{"type":"text","text":"what is this"},
           {"type":"image_url","image_url":{"url":"data:image/png;base64,\#(png)","detail":"low"}}]}]
        """#
        let describer = CoreImageDescriber()
        try await CoreHarness.run(hooks: EngineHooks(imageDescriber: describer), configure: { $0.roles.vision = "Big"; $0.models[0].vision = false }, backend: okChat) { h in
            _ = try await h.chat(chatBody(messages: messages))
            let parts = h.backend.lastChat?["messages"]?[0]?["content"]?.array ?? []
            #expect(parts.map { $0.type ?? "" } == ["text", "text"])
            #expect(parts[1]["text"]?.string == "[image: a cat on a mat]")
            #expect(describer.calls == 1)
        }
        try await CoreHarness.run(configure: { $0.models[0].vision = false }, backend: okChat) { h in
            _ = try await h.chat(chatBody(messages: messages))
            #expect(h.backend.lastChat?["messages"]?[0]?["content"]?[1]?["text"]?.string
                    == "[image omitted: GLM has no vision and no vision model is configured]")
        }
        // A vision model keeps its images untouched.
        try await CoreHarness.run(configure: { $0.models[0].vision = true }, backend: okChat) { h in
            _ = try await h.chat(chatBody(messages: messages))
            let part = h.backend.lastChat?["messages"]?[0]?["content"]?[1]
            #expect(part?.type == "image_url" && part?["image_url"]?["url"]?.string == "data:image/png;base64,\(png)")
            #expect(part?["image_url"]?["detail"]?.string == "low")
        }
    }

    @Test func pdfFilePartsAreReadAsDocuments() async throws {
        let reader = CoreDocumentReader()
        let messages = #"""
        [{"role":"user","content":[{"type":"text","text":"summarise"},
           {"type":"file","file":{"filename":"report.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"}},
           {"type":"file","file":{"file_id":"file-9"}},
           {"type":"image_url","image_url":{"url":"data:application/pdf;base64,JVBERi0xLjQK"}}]}]
        """#
        try await CoreHarness.run(hooks: EngineHooks(documentReader: reader), configure: { $0.models[0].vision = true }, backend: okChat) { h in
            let r = try await h.chat(chatBody(messages: messages))
            #expect(r.status == 200)
            let parts = h.backend.lastChat?["messages"]?[0]?["content"]?.array ?? []
            #expect(parts.allSatisfy { $0.type == "text" })
            #expect(parts.map { $0["text"]?.string ?? "" }[1] == "[document: report.pdf]\nEXTRACTED 9 bytes of application/pdf")
            #expect(parts[2]["text"]?.string?.contains("could not be read") == true)
            #expect(parts[3]["text"]?.string?.hasPrefix("[document: document]") == true)
            #expect(reader.inputs.count == 2 && reader.inputs.contains { $0.title == "report.pdf" })
        }
    }

    // MARK: models

    @Test func modelsAnswerInBothShapes() async throws {
        try await CoreHarness.run(backend: okChat) { h in
            let openai = try await h.request("GET", "/v1/models")
            #expect(openai.status == 200 && openai.json?["object"]?.string == "list")
            let ids = (openai.json?["data"]?.array ?? []).compactMap { $0["id"]?.string }
            #expect(ids == ["GLM", "Small", "Big", "sonnet", "opus", "haiku"])
            #expect(openai.json?["data"]?[0]?["object"]?.string == "model" && openai.json?["data"]?[0]?["owned_by"]?.string == "code-link")
            #expect(openai.json?["data"]?[0]?["created"]?.int != nil)
            #expect(openai.json?["has_more"] == nil)

            let anthropic = try await h.request("GET", "/v1/models", headers: ["anthropic-version": "2023-06-01"])
            #expect(anthropic.json?["data"]?[0]?["type"]?.string == "model" && anthropic.json?["has_more"] == false)
            #expect(anthropic.json?["object"] == nil)
            #expect((anthropic.json?["data"]?.array ?? []).compactMap { $0["id"]?.string } == ids)
        }
    }

    // MARK: Anthropic backends

    private static func anthropicConfig(_ c: inout EngineConfig) { c.backends[0].kind = .anthropic }

    @Test func anthropicBackendGetsATranslatedRequest() async throws {
        let messages = #"""
        [{"role":"system","content":"be brief"},{"role":"developer","content":[{"type":"text","text":"and kind"}]},
         {"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
         {"role":"assistant","content":"checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"ls","arguments":"{\"dir\":\"/\"}"}},
                                                               {"id":"call_2","type":"function","function":{"name":"pwd","arguments":""}}]},
         {"role":"tool","tool_call_id":"call_1","content":"a b c"},
         {"role":"tool","tool_call_id":"call_2","content":[{"type":"text","text":"/home"}]},
         {"role":"user","content":"thanks"}]
        """#
        let extra = #"""
        "max_tokens":77,"temperature":0.5,"top_p":0.8,"stop":["END"],"user":"u7","parallel_tool_calls":false,"tool_choice":"required",
        "tools":[{"type":"function","function":{"name":"ls","description":"list","parameters":{"type":"object","properties":{"dir":{"type":"string"}}}}}]
        """#
        try await CoreHarness.run(configure: Self.anthropicConfig, backend: { _, _ in
            .json(200, #"{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}"#)
        }) { h in
            let r = try await h.chat(chatBody(extra, messages: messages))
            #expect(r.status == 200)
            let seen = try #require(h.backend.requests.last)
            #expect(seen.path == "/v1/messages")
            #expect(seen.headers["x-api-key"] == "sk-stub-secret" && seen.headers["anthropic-version"] == "2023-06-01")
            let up = try #require(seen.json)
            #expect(up["model"]?.string == "glm-upstream" && up["max_tokens"] == 77 && up["stream"] == false)
            #expect(up["system"]?.string == "be brief\nand kind")
            #expect(up["temperature"] == 0.5 && up["top_p"] == 0.8 && up["stop_sequences"] == ["END"] && up["metadata"]?["user_id"]?.string == "u7")
            #expect(up["tool_choice"] == ["type": "any", "disable_parallel_tool_use": true])
            #expect(up["tools"]?[0]?["name"]?.string == "ls" && up["tools"]?[0]?["input_schema"]?["properties"]?["dir"] != nil)
            let msgs = up["messages"]?.array ?? []
            #expect(msgs.map { $0["role"]?.string ?? "" } == ["user", "assistant", "user", "user"])
            #expect(msgs[0]["content"]?[1]?["source"]?["type"]?.string == "base64" && msgs[0]["content"]?[1]?["source"]?["media_type"]?.string == "image/png")
            #expect(msgs[1]["content"]?.array?.map { $0.type ?? "" } == ["text", "tool_use", "tool_use"])
            #expect(msgs[1]["content"]?[1]?["input"]?["dir"]?.string == "/" && msgs[1]["content"]?[2]?["input"] == [:])
            // Both tool results ride in one user turn.
            #expect(msgs[2]["content"]?.array?.map { $0.type ?? "" } == ["tool_result", "tool_result"])
            #expect(msgs[2]["content"]?[0]?["tool_use_id"]?.string == "call_1" && msgs[2]["content"]?[1]?["content"]?.string == "/home")
            #expect(msgs[3]["content"]?[0]?["text"]?.string == "thanks")
        }
    }

    @Test func anthropicDefaultsAndRefusals() async throws {
        try await CoreHarness.run(configure: Self.anthropicConfig, backend: { _, _ in .json(200, #"{"type":"message","content":[],"stop_reason":"end_turn","usage":{}}"#) }) { h in
            _ = try await h.chat(chatBody())
            #expect(h.backend.requests.last?.json?["max_tokens"] == 4096 && h.backend.requests.last?.json?["system"] == nil)
            let none = try await h.chat(#"{"model":"GLM","messages":[{"role":"system","content":"only"}]}"#)
            #expect(none.status == 400)
        }
    }

    @Test func anthropicMessageBecomesAChatCompletion() async throws {
        let reply = #"""
        {"id":"msg_9","type":"message","role":"assistant","model":"claude-x","stop_reason":"tool_use",
         "content":[{"type":"thinking","thinking":"hmm","signature":"s"},{"type":"text","text":"Let me "},{"type":"text","text":"check."},
                    {"type":"tool_use","id":"toolu_1","name":"ls","input":{"dir":"/"}}],
         "usage":{"input_tokens":20,"cache_creation_input_tokens":5,"cache_read_input_tokens":75,"output_tokens":11}}
        """#
        try await CoreHarness.run(configure: Self.anthropicConfig, backend: { _, _ in .json(200, reply) }) { h in
            let r = try await h.chat(chatBody())
            let j = try #require(r.json)
            #expect(j["object"]?.string == "chat.completion" && j["model"]?.string == "GLM" && j["id"]?.string?.hasPrefix("chatcmpl-") == true)
            let msg = j["choices"]?[0]?["message"]
            #expect(msg?["role"]?.string == "assistant" && msg?["content"]?.string == "Let me check." && msg?["reasoning_content"]?.string == "hmm")
            #expect(msg?["tool_calls"]?[0]?["id"]?.string == "toolu_1" && msg?["tool_calls"]?[0]?["function"]?["name"]?.string == "ls")
            #expect(msg?["tool_calls"]?[0]?["function"]?["arguments"]?.string == #"{"dir":"/"}"#)
            #expect(j["choices"]?[0]?["finish_reason"]?.string == "tool_calls")
            #expect(j["usage"]?["prompt_tokens"] == 100 && j["usage"]?["completion_tokens"] == 11 && j["usage"]?["total_tokens"] == 111)
            #expect(j["usage"]?["prompt_tokens_details"]?["cached_tokens"] == 75)
        }
        for (stop, finish) in [("max_tokens", "length"), ("end_turn", "stop"), ("stop_sequence", "stop"), ("refusal", "content_filter")] {
            #expect(ChatToAnthropic.finishReason(stop) == finish)
        }
    }

    @Test func anthropicStreamBecomesChatChunks() async throws {
        let events = [
            A.start(input: 25, cacheRead: 10), A.ping,
            A.block(0, #"{"type":"thinking","thinking":""}"#), A.delta(0, #"{"type":"thinking_delta","thinking":"hmm"}"#),
            A.delta(0, #"{"type":"signature_delta","signature":"s"}"#), A.stop(0),
            A.block(1, #"{"type":"text","text":""}"#), A.text(1, "Hel"), A.text(1, "lo"), A.stop(1),
            A.block(2, #"{"type":"tool_use","id":"toolu_1","name":"ls","input":{}}"#),
            A.delta(2, #"{"type":"input_json_delta","partial_json":"{\"dir\""}"#), A.delta(2, #"{"type":"input_json_delta","partial_json":":\"/\"}"}"#), A.stop(2),
            A.end("tool_use", output: 12), A.messageStop,
        ]
        try await CoreHarness.run(configure: Self.anthropicConfig, backend: { _, _ in .sse(events, done: false) }) { h in
            let r = try await h.chat(chatBody(#""stream_options":{"include_usage":true}"#, stream: true))
            #expect(r.status == 200 && r.headers["content-type"]?.contains("text/event-stream") == true)
            #expect(h.backend.requests.last?.json?["stream"] == true && h.backend.requests.last?.headers["accept"] == "text/event-stream")
            let lines = r.text.components(separatedBy: "\n\n").filter { !$0.isEmpty }
            #expect(lines.last == "data: [DONE]")
            let chunks = try lines.dropLast().map { try JSON.parse(String($0.dropFirst(6))) }
            #expect(chunks.allSatisfy { $0["object"]?.string == "chat.completion.chunk" && $0["model"]?.string == "GLM" })
            #expect(Set(chunks.compactMap { $0["id"]?.string }).count == 1)
            #expect(chunks[0]["choices"]?[0]?["delta"]?["role"]?.string == "assistant")
            func joined(_ key: String) -> String { chunks.compactMap { $0["choices"]?[0]?["delta"]?[key]?.string }.joined() }
            #expect(joined("content") == "Hello" && joined("reasoning_content") == "hmm")
            let tools = chunks.compactMap { $0["choices"]?[0]?["delta"]?["tool_calls"]?[0] }
            #expect(tools[0]["id"]?.string == "toolu_1" && tools[0]["function"]?["name"]?.string == "ls" && tools[0]["index"] == 0)
            #expect(tools.dropFirst().compactMap { $0["function"]?["arguments"]?.string }.joined() == #"{"dir":"/"}"#)
            let finish = try #require(chunks.first { $0["choices"]?[0]?["finish_reason"]?.string != nil })
            #expect(finish["choices"]?[0]?["finish_reason"]?.string == "tool_calls")
            let usage = try #require(chunks.last)
            #expect(usage["choices"]?.array?.isEmpty == true)
            #expect(usage["usage"]?["prompt_tokens"] == 35 && usage["usage"]?["completion_tokens"] == 12 && usage["usage"]?["total_tokens"] == 47)
            #expect(usage["usage"]?["prompt_tokens_details"]?["cached_tokens"] == 10)
        }
        // No usage chunk unless asked for.
        try await CoreHarness.run(configure: Self.anthropicConfig, backend: { _, _ in
            .sse([A.start(), A.block(0, #"{"type":"text","text":""}"#), A.text(0, "x"), A.stop(0), A.end("end_turn"), A.messageStop])
        }) { h in
            let r = try await h.chat(chatBody(stream: true))
            #expect(!r.text.contains("\"usage\"") && r.text.hasSuffix("data: [DONE]\n\n"))
            #expect(r.text.contains(#""finish_reason":"stop""#))
        }
    }

    @Test func anthropicStreamsThatBreakEndWithAnErrorFrame() async throws {
        let cases: [(String, StubBackend.Reply)] = [
            ("cut", .sse([A.start(), A.block(0, #"{"type":"text","text":""}"#), A.text(0, "par")], done: false)),
            ("error event", .sse([A.start(), #"{"type":"error","error":{"type":"overloaded_error","message":"Overloaded at 10.0.0.9"}}"#])),
        ]
        for (name, reply) in cases {
            try await CoreHarness.run(configure: Self.anthropicConfig, backend: { _, _ in reply }) { h in
                let r = try await h.chat(chatBody(stream: true))
                #expect(r.status == 200)
                #expect(r.text.contains(#""error":{"#), "\(name)")
                #expect(!r.text.contains("finish_reason\":\"stop"), "\(name)")
                #expect(r.text.hasSuffix("data: [DONE]\n\n"), "\(name)")
                #expect(!r.text.contains("10.0.0.9"))
                #expect(h.log.requests.last?.status == 502)
            }
        }
    }

    @Test func anthropicErrorsBecomeOpenAIErrors() async throws {
        let cases: [(Int, String, Int, String?)] = [
            (400, #"{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: required"}}"#, 400, nil),
            (429, #"{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}"#, 429, "rate_limit_exceeded"),
            (529, #"{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}"#, 529, "server_is_overloaded"),
            (401, #"{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}"#, 502, nil),
        ]
        for (status, body, want, code) in cases {
            try await CoreHarness.run(configure: Self.anthropicConfig, backend: { _, _ in .json(status, body, headers: ["retry-after": "4"]) }) { h in
                let r = try await h.chat(chatBody())
                #expect(r.status == want, "\(status)")
                #expect(r.json?["error"]?["message"]?.string?.isEmpty == false && r.json?["type"] == nil)
                #expect(r.json?["error"]?["code"]?.string == code, "\(status): \(r.text)")
                if status == 429 { #expect(r.headers["retry-after"] == "4") }
            }
        }
    }

    @Test func imagesGoToAnAnthropicModelAndDocumentsAreRead() async throws {
        let reader = CoreDocumentReader()
        let messages = #"""
        [{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,/9j/"}},
          {"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"}}]}]
        """#
        try await CoreHarness.run(hooks: EngineHooks(documentReader: reader), configure: Self.anthropicConfig, backend: { _, _ in
            .json(200, #"{"type":"message","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}"#)
        }) { h in
            _ = try await h.chat(chatBody(messages: messages))
            let content = h.backend.requests.last?.json?["messages"]?[0]?["content"]?.array ?? []
            #expect(content.map { $0.type ?? "" } == ["image", "text"])
            #expect(content[1]["text"]?.string == "[document: a.pdf]\nEXTRACTED 9 bytes of application/pdf")
        }
    }
}
