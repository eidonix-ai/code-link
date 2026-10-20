import Foundation
import Testing
@testable import CodeLinkEngine

private let classifierBody = #"{"model":"claude-sonnet-5","max_tokens":64,"stop_sequences":["</severity>"],"system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents."}],"messages":[{"role":"user","content":"<transcript>rm -rf build</transcript>"}]}"#

private let classifierAnswer: JSONValue = .object([
    "id": .string("msg_x"), "type": .string("message"), "role": .string("assistant"), "model": .string("claude-sonnet-5"),
    "content": .array([.object(["type": .string("text"), "text": .string("<severity>12")])]),
    "stop_reason": .string("stop_sequence"), "stop_sequence": .string("</severity>"),
    "usage": .object(["input_tokens": .number(900), "output_tokens": .number(1)]),
])

private func userTexts(_ chat: JSON?) -> [String] {
    (chat?["messages"]?.array ?? []).filter { $0["role"]?.string == "user" }.flatMap { m -> [String] in
        if let s = m["content"]?.string { return [s] }
        return (m["content"]?.array ?? []).compactMap { $0["text"]?.string }
    }
}

private let pdfData = Data("%PDF-1.4 fake pdf body".utf8).base64EncodedString()

@Suite(.serialized) struct AutoModeHookTests {
    @Test func classifierRequestsAreAnsweredByTheResponder() async throws {
        let am = CoreAutoMode(answer: classifierAnswer, decisionModel: nil)
        try await CoreHarness.run(hooks: EngineHooks(autoMode: am), backend: { _, _ in .json(500, "{}") }) { h in
            let r = try await h.messages(classifierBody)
            #expect(r.status == 200)
            #expect(r.json?["content"]?[0]?["text"]?.string == "<severity>12")
            #expect(r.json?["stop_reason"]?.string == "stop_sequence" && r.json?["stop_sequence"]?.string == "</severity>")

            // stream: true gets the same answer as an event sequence.
            let s = try await h.messages(classifierBody.replacingOccurrences(of: #""max_tokens":64"#, with: #""max_tokens":64,"stream":true"#))
            let ev = s.events.withoutPings
            #expect(ev.names == ["message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"])
            #expect(ev[2].data?["delta"]?["text"]?.string == "<severity>12")
            #expect(ev[4].data?["delta"]?["stop_reason"]?.string == "stop_sequence")
            #expect(ev[4].data?["delta"]?["stop_sequence"]?.string == "</severity>")
            #expect(ev[0].data?["message"]?["content"] == [])
            #expect(am.responded == 2)
            #expect(h.backend.chatRequests.isEmpty)
        }
    }

    @Test func aMissRunsTheRequestOnTheDecisionModel() async throws {
        let decision = Model(id: "Small", backendID: "stub", upstreamModel: "small-upstream", contextWindow: 32_000)
        let am = CoreAutoMode(answer: nil, decisionModel: decision)
        try await CoreHarness.run(hooks: EngineHooks(autoMode: am), backend: { _, _ in
            .json(200, Chunks.completion(content: "\n</think>\n<severity>80", finish: "stop", stopReason: "</severity>"))
        }) { h in
            let r = try await h.messages(classifierBody)
            #expect(r.status == 200)
            #expect(h.backend.lastChat?["model"]?.string == "small-upstream")
            #expect(h.backend.lastChat?["stop"] == ["</severity>"])
            #expect(r.json?["content"]?[0]?["text"]?.string == "<severity>80")
            #expect(r.json?["stop_reason"]?.string == "stop_sequence")
            // Ordinary requests are untouched.
            _ = try await h.messages(messagesBody(model: "claude-sonnet-5"))
            #expect(h.backend.lastChat?["model"]?.string == "glm-upstream")
            #expect(am.responded == 1)
        }
    }
}

@Suite(.serialized) struct DocumentHookTests {
    @Test func documentsAreReadIntoText() async throws {
        let reader = CoreDocumentReader()
        try await CoreHarness.run(hooks: EngineHooks(documentReader: reader), backend: okReply) { h in
            let body = messagesBody(messages: #"[{"role":"user","content":[{"type":"document","title":"report.pdf","source":{"type":"base64","media_type":"application/pdf","data":"\#(pdfData)"}},{"type":"text","text":"summarise"}]}]"#)
            let status = try await h.messages(body).status
            #expect(status == 200)
            #expect(reader.inputs.count == 1)
            #expect(reader.inputs.first?.mediaType == "application/pdf" && reader.inputs.first?.title == "report.pdf")
            #expect(reader.inputs.first?.data == Data(base64Encoded: pdfData))
            let texts = userTexts(h.backend.lastChat)
            #expect(texts == ["[document: report.pdf]\nEXTRACTED 22 bytes of application/pdf", "summarise"])
            #expect(h.log.all.contains { if case .document(let n, 2, 1) = $0 { return n == "report.pdf" } else { return false } })
        }
    }

    @Test func documentsInToolResultsFollowTheToolMessages() async throws {
        let reader = CoreDocumentReader()
        try await CoreHarness.run(hooks: EngineHooks(documentReader: reader), backend: okReply) { h in
            let body = messagesBody(messages: #"[{"role":"user","content":"read a.pdf"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"a.pdf"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"\#(pdfData)"}}]},{"type":"text","text":"go on"}]}]"#)
            let status = try await h.messages(body).status
            #expect(status == 200)
            let m = h.backend.lastChat?["messages"]?.array ?? []
            #expect(m.compactMap { $0["role"]?.string } == ["user", "assistant", "tool", "user"])
            #expect(m[2]["content"]?.string?.contains("attachments") == true)
            let parts = m[3]["content"]?.array?.compactMap { $0["text"]?.string } ?? []
            #expect(parts.count == 3)
            #expect(parts[0].contains("toolu_1"))
            #expect(parts[1] == "[document: document]\nEXTRACTED 22 bytes of application/pdf")
            #expect(parts[2] == "go on")
        }
    }

    @Test func unreadableDocumentsBecomeNotes() async throws {
        let failing = CoreDocumentReader { _ in DocumentOutput(text: nil, error: "no text layer", via: "pdfkit") }
        let doc = #"{"type":"document","title":"scan.pdf","source":{"type":"base64","media_type":"application/pdf","data":"\#(pdfData)"}}"#
        try await CoreHarness.run(hooks: EngineHooks(documentReader: failing), backend: okReply) { h in
            _ = try await h.messages(messagesBody(messages: #"[{"role":"user","content":[\#(doc)]}]"#))
            #expect(userTexts(h.backend.lastChat) == ["[document: scan.pdf could not be read (no text layer)]"])
            _ = try await h.messages(messagesBody(messages: #"[{"role":"user","content":[{"type":"document","title":"bad.pdf","source":{"type":"base64","media_type":"application/pdf","data":"@@@"}}]}]"#))
            #expect(userTexts(h.backend.lastChat).first?.contains("could not be read") == true)
        }
        try await CoreHarness.run(backend: okReply) { h in
            _ = try await h.messages(messagesBody(messages: #"[{"role":"user","content":[\#(doc)]}]"#))
            #expect(userTexts(h.backend.lastChat) == ["[document: scan.pdf could not be read (document reading is not available)]"])
        }
    }

    @Test func textContentAndURLSources() async throws {
        let reader = CoreDocumentReader()
        try await CoreHarness.run(hooks: EngineHooks(documentReader: reader), backend: { seen, _ in
            if seen.method == "GET", seen.path == "/files/doc.pdf" { return .json(200, "%PDF-1.7 from url") }
            return .json(200, Chunks.completion(content: "ok"))
        }) { h in
            let txt = Data("plain notes".utf8).base64EncodedString()
            let body = messagesBody(messages: #"[{"role":"user","content":[{"type":"document","title":"a.txt","source":{"type":"text","media_type":"text/plain","data":"alpha"}},{"type":"document","title":"b","context":"from the wiki","source":{"type":"content","content":[{"type":"text","text":"beta"}]}},{"type":"document","title":"c.txt","source":{"type":"base64","media_type":"text/plain","data":"\#(txt)"}},{"type":"document","title":"d.pdf","source":{"type":"url","url":"http://127.0.0.1:\#(h.backend.port)/files/doc.pdf"}},{"type":"document","title":"e","source":{"type":"url","url":"ftp://x/y"}}]}]"#)
            let status = try await h.messages(body).status
            #expect(status == 200)
            let texts = userTexts(h.backend.lastChat)
            #expect(texts[0] == "[document: a.txt]\nalpha")
            #expect(texts[1] == "[document: b]\nfrom the wiki")
            #expect(texts[2] == "beta")
            #expect(texts[3] == "[document: c.txt]\nplain notes")
            #expect(texts[4] == "[document: d.pdf]\nEXTRACTED 17 bytes of application/json")
            #expect(texts[5] == "[document: e could not be read (only http and https URLs are fetched)]")
        }
    }
}

private let okReply: @Sendable (StubBackend.Seen, Int) -> StubBackend.Reply = { _, _ in .json(200, Chunks.completion(content: "ok")) }

@Suite(.serialized) struct ImageHookTests {
    let image = #"{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUg=="}}"#

    @Test func describedForAModelWithoutVision() async throws {
        let describer = CoreImageDescriber()
        try await CoreHarness.run(hooks: EngineHooks(imageDescriber: describer), configure: { $0.roles.vision = "Big"; $0.models[0].vision = false }, backend: okReply) { h in
            let body = messagesBody(messages: #"[{"role":"user","content":[{"type":"text","text":"what is it"},\#(image),\#(image)]}]"#)
            let status = try await h.messages(body).status
            #expect(status == 200)
            #expect(userTexts(h.backend.lastChat) == ["what is it", "[image: a cat on a mat]", "[image: a cat on a mat]"])
            #expect(describer.calls == 1) // the same image once
            #expect(!(h.backend.chatRequests.last.map { String(decoding: $0.body, as: UTF8.self) } ?? "").contains("image_url"))
        }
    }

    @Test func omittedWithoutAVisionRole() async throws {
        let describer = CoreImageDescriber()
        try await CoreHarness.run(hooks: EngineHooks(imageDescriber: describer), configure: { $0.models[0].vision = false }, backend: okReply) { h in
            _ = try await h.messages(messagesBody(messages: #"[{"role":"user","content":[\#(image)]}]"#))
            #expect(userTexts(h.backend.lastChat) == ["[image omitted: GLM has no vision and no vision model is configured]"])
            #expect(describer.calls == 0)
        }
    }

    @Test func passedToAVisionModel() async throws {
        try await CoreHarness.run(configure: { $0.models[0].vision = true }, backend: okReply) { h in
            _ = try await h.messages(messagesBody(messages: #"[{"role":"user","content":[\#(image)]},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Shot","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[\#(image)]}]}]"#))
            let m = h.backend.lastChat?["messages"]?.array ?? []
            #expect(m[0]["content"]?.array?.first?.type == "image_url")
            #expect(m.compactMap { $0["role"]?.string } == ["user", "assistant", "tool", "user"])
            #expect(m[3]["content"]?.array?.last?.type == "image_url")
        }
    }

    /// A model configured without `vision` is asked once, on the first
    /// request with an image: one that reads it gets images directly, one
    /// that refuses it gets descriptions.
    @Test func visionIsDetectedOnTheFirstImage() async throws {
        for reads in [true, false] {
            let describer = CoreImageDescriber()
            try await CoreHarness.run(hooks: EngineHooks(imageDescriber: describer), configure: { $0.roles.vision = "Big" }, backend: { seen, _ in
                if seen.json?["max_tokens"] == 1 { return reads ? .json(200, Chunks.completion(content: "H")) : .json(400, #"{"error":{"message":"not a multimodal model"}}"#) }
                return .json(200, Chunks.completion(content: "ok"))
            }) { h in
                _ = try await h.messages(messagesBody(messages: #"[{"role":"user","content":[{"type":"text","text":"hi"}]}]"#))
                #expect(h.backend.chatRequests.count == 1) // no image, no probe
                for _ in 0..<2 { _ = try await h.messages(messagesBody(messages: #"[{"role":"user","content":[\#(image)]}]"#)) }
                #expect(h.backend.chatRequests.count == 4) // probed once
                let first = h.backend.lastChat?["messages"]?[0]?["content"]?.array?.first
                #expect(reads ? first?.type == "image_url" : userTexts(h.backend.lastChat) == ["[image: a cat on a mat]"])
                #expect(describer.calls == (reads ? 0 : 2)) // one per request
            }
        }
    }

    @Test func describedInsideToolResults() async throws {
        let describer = CoreImageDescriber("a red button")
        try await CoreHarness.run(hooks: EngineHooks(imageDescriber: describer), configure: { $0.roles.vision = "Big"; $0.models[0].vision = false }, backend: okReply) { h in
            _ = try await h.messages(messagesBody(messages: #"[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Shot","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"shot taken"},\#(image)]}]}]"#))
            let m = h.backend.lastChat?["messages"]?.array ?? []
            #expect(m[1]["content"]?.string == "shot taken")
            #expect(m[2]["content"]?.array?.compactMap { $0["text"]?.string }.last == "[image: a red button]")
        }
    }
}

@Suite(.serialized) struct WebSearchLoopTests {
    static let searchBody = #"{"model":"claude-haiku-4-5","max_tokens":1000,"stream":true,"system":[{"type":"text","text":"You run a web search."}],"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":MAX}],"messages":[{"role":"user","content":"Perform a web search for the query: swift 6.4 release"}]}"#

    static func body(maxUses: Int = 8, stream: Bool = true) -> String {
        searchBody.replacingOccurrences(of: "MAX", with: "\(maxUses)").replacingOccurrences(of: #""stream":true"#, with: #""stream":\#(stream)"#)
    }

    static func searchCall(_ q: String, index: Int = 0, id: String = "call_s") -> String {
        Chunks.toolCall(index: index, id: id + "\(index)", name: "web_search", args: #"{"query":"\#(q)"}"#)
    }

    @Test func searchesStreamAcrossGenerations() async throws {
        let searcher = CoreSearcher()
        try await CoreHarness.run(hooks: EngineHooks(webSearcher: searcher), backend: { seen, n in
            n == 0 ? .sse([Chunks.reasoning("I should search."), Self.searchCall("swift 6.4 release"), Chunks.finish("tool_calls"), Chunks.usage(prompt: 100, completion: 10)])
                   : .sse([Chunks.content("Swift 6.4 is out."), Chunks.finish("stop"), Chunks.usage(prompt: 300, completion: 20, cached: 90)])
        }) { h in
            let r = try await h.messages(Self.body())
            let ev = r.events
            let blocks = checkBlocksBalanced(ev)
            #expect(blocks == [0: "thinking", 1: "server_tool_use", 2: "web_search_tool_result", 3: "text"])
            let stu = ev.first { $0.data?["content_block"]?.type == "server_tool_use" }?.data?["content_block"]
            #expect(stu?["name"]?.string == "web_search" && stu?["id"]?.string?.hasPrefix("srvtoolu_") == true)
            #expect(ev.contains { $0.data?["delta"]?["partial_json"]?.string == #"{"query":"swift 6.4 release"}"# })
            let result = ev.first { $0.data?["content_block"]?.type == "web_search_tool_result" }?.data?["content_block"]
            #expect(result?["tool_use_id"] == stu?["id"])
            let hit = result?["content"]?[0]
            #expect(hit?["type"]?.string == "web_search_result" && hit?["url"]?.string?.hasPrefix("https://example.com/") == true)
            #expect(hit?["title"]?.string == "Result for swift 6.4 release" && hit?["page_age"]?.string == "2026-01-01")
            #expect(hit?["encrypted_content"]?.string.flatMap { Data(base64Encoded: $0) }.map { String(decoding: $0, as: UTF8.self) } == "Snippet about swift 6.4 release")
            #expect(!ev.contains { $0.data?["content_block"]?.type == "tool_use" })
            let delta = ev.first { $0.event == "message_delta" }?.data
            #expect(delta?["delta"]?["stop_reason"]?.string == "end_turn")
            #expect(delta?["usage"]?["server_tool_use"]?["web_search_requests"] == 1)
            #expect(delta?["usage"]?["output_tokens"] == 30 && delta?["usage"]?["input_tokens"] == 310 && delta?["usage"]?["cache_read_input_tokens"] == 90)
            #expect(ev.last?.event == "message_stop")
            #expect(searcher.queries == ["swift 6.4 release"])

            let reqs = h.backend.chatRequests.compactMap(\.json)
            #expect(reqs.count == 2)
            #expect((reqs[0]["tools"]?.array ?? []).compactMap { $0["function"]?["name"]?.string } == ["web_search"])
            #expect(reqs[0]["model"]?.string == "small-upstream")
            let m = reqs[1]["messages"]?.array ?? []
            let assistant = m[m.count - 2], tool = m[m.count - 1]
            #expect(assistant["tool_calls"]?[0]?["function"]?["name"]?.string == "web_search")
            #expect(assistant["reasoning_content"]?.string == "I should search.")
            #expect(tool["role"]?.string == "tool" && tool["tool_call_id"]?.string == "call_s0")
            #expect(tool["content"]?.string?.contains("Snippet about swift 6.4 release") == true)
            #expect(h.log.all.contains { if case .webSearch("swift 6.4 release", 1) = $0 { return true } else { return false } })
        }
    }

    @Test func maxUsesAndUnavailableBecomeErrorResults() async throws {
        let searcher = CoreSearcher()
        try await CoreHarness.run(hooks: EngineHooks(webSearcher: searcher), backend: { _, n in
            n == 0 ? .sse([Self.searchCall("one"), Self.searchCall("two", index: 1), Chunks.finish("tool_calls")])
                   : .sse([Chunks.content("done"), Chunks.finish()])
        }) { h in
            let ev = try await h.messages(Self.body(maxUses: 1)).events
            let results = ev.compactMap { $0.data?["content_block"] }.filter { $0.type == "web_search_tool_result" }
            #expect(results.count == 2)
            #expect(results[0]["content"]?.array?.count == 1)
            #expect(results[1]["content"]?["type"]?.string == "web_search_tool_result_error")
            #expect(results[1]["content"]?["error_code"]?.string == "max_uses_exceeded")
            #expect(searcher.queries == ["one"])
            let tool = h.backend.chatRequests[1].json?["messages"]?.array?.last
            #expect(tool?["content"]?.string?.contains("search limit") == true)
        }
        try await CoreHarness.run(backend: { _, n in
            n == 0 ? .sse([Self.searchCall("x"), Chunks.finish("tool_calls")]) : .sse([Chunks.content("Could not search."), Chunks.finish()])
        }) { h in
            let ev = try await h.messages(Self.body()).events
            let result = ev.compactMap { $0.data?["content_block"] }.first { $0.type == "web_search_tool_result" }
            #expect(result?["content"]?["error_code"]?.string == "unavailable")
            #expect(ev.compactMap { $0.data?["delta"]?["text"]?.string }.joined() == "Could not search.")
            #expect(ev.first { $0.event == "message_delta" }?.data?["usage"]?["server_tool_use"]?["web_search_requests"] == 0)
        }
    }

    @Test func theLastRoundHasNoTools() async throws {
        let searcher = CoreSearcher()
        try await CoreHarness.run(hooks: EngineHooks(webSearcher: searcher), backend: { seen, _ in
            seen.json?["tools"] == nil ? .sse([Chunks.content("final"), Chunks.finish()])
                                       : .sse([Self.searchCall("again"), Chunks.finish("tool_calls")])
        }) { h in
            let ev = try await h.messages(Self.body(maxUses: 1)).events
            #expect(h.backend.chatRequests.count == 3) // search, told the limit, answer without tools
            #expect(searcher.queries == ["again"])
            #expect(ev.compactMap { $0.data?["delta"]?["text"]?.string }.joined() == "final")
            #expect(ev.last?.event == "message_stop")
        }
    }

    @Test func clientToolCallsEndTheLoop() async throws {
        try await CoreHarness.run(hooks: EngineHooks(webSearcher: CoreSearcher()), backend: { _, _ in
            .sse([Self.searchCall("q"), Chunks.toolCall(index: 1, id: "call_r", name: "Read", args: #"{"p":1}"#), Chunks.finish("tool_calls")])
        }) { h in
            let body = Self.body().replacingOccurrences(of: #""tools":["#, with: #""tools":[{"name":"Read","input_schema":{"type":"object"}},"#)
            let ev = try await h.messages(body).events
            let types = checkBlocksBalanced(ev)
            #expect(Set(types.values) == ["tool_use", "server_tool_use", "web_search_tool_result"])
            #expect(ev.first { $0.event == "message_delta" }?.data?["delta"]?["stop_reason"]?.string == "tool_use")
            #expect(h.backend.chatRequests.count == 1)
        }
    }

    @Test func nonStreamingSearch() async throws {
        let searcher = CoreSearcher()
        try await CoreHarness.run(hooks: EngineHooks(webSearcher: searcher), backend: { _, n in
            n == 0 ? .json(200, Chunks.completion(content: nil, toolCalls: #"[{"id":"c1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"q1\"}"}}]"#, finish: "tool_calls", prompt: 10, completion: 2))
                   : .json(200, Chunks.completion(content: "Found it.", prompt: 30, completion: 3))
        }) { h in
            let r = try await h.messages(Self.body(stream: false))
            #expect(r.status == 200)
            let types = r.json?["content"]?.array?.compactMap(\.type)
            #expect(types == ["server_tool_use", "web_search_tool_result", "text"])
            #expect(r.json?["stop_reason"]?.string == "end_turn")
            #expect(r.json?["usage"]?["server_tool_use"]?["web_search_requests"] == 1)
            #expect(r.json?["usage"]?["input_tokens"] == 40 && r.json?["usage"]?["output_tokens"] == 5)
            #expect(searcher.queries == ["q1"])
            let m = h.backend.chatRequests[1].json?["messages"]?.array ?? []
            #expect(m.last?["role"]?.string == "tool" && m.last?["tool_call_id"]?.string == "c1")
        }
    }

    @Test func searchUpstreamErrorBeforeTheStreamKeepsItsStatus() async throws {
        try await CoreHarness.run(hooks: EngineHooks(webSearcher: CoreSearcher()), backend: { _, _ in
            .json(503, #"{"error":{"message":"busy"}}"#)
        }) { h in
            let r = try await h.messages(Self.body())
            #expect(r.status == 503 && r.json?["error"]?["type"]?.string == "overloaded_error")
        }
    }

    @Test func aFailedLaterRoundIsAnInBandError() async throws {
        try await CoreHarness.run(hooks: EngineHooks(webSearcher: CoreSearcher()), backend: { _, n in
            n == 0 ? .sse([Self.searchCall("q"), Chunks.finish("tool_calls")]) : .json(500, #"{"error":{"message":"oops"}}"#)
        }) { h in
            let r = try await h.messages(Self.body())
            #expect(r.status == 200)
            #expect(r.events.last?.event == "error")
            #expect(r.events.last?.data?["error"]?["type"]?.string == "api_error")
        }
    }
}
