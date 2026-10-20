import Foundation
import Testing
@testable import CodeLinkEngine

private let glm = Model(id: "GLM", backendID: "b", upstreamModel: "glm", contextWindow: nil)

private func translate(_ body: String, model: Model = glm, window: Int? = nil) throws -> JSON {
    let req = try JSON.parse(body)
    return RequestTranslator.chatRequest(req, model: model, contextWindow: window, webSearch: WebSearchTool.find(in: req["tools"]?.array))
}

private func msgs(_ body: String) throws -> [JSON] {
    try translate(body)["messages"]?.array ?? []
}

private func roles(_ m: [JSON]) -> [String] { m.compactMap { $0["role"]?.string } }

private func toolNames(_ chat: JSON) -> [String] {
    (chat["tools"]?.array ?? []).compactMap { $0["function"]?["name"]?.string }
}

@Suite struct JSONTests {
    @Test func roundTripKeepsOrderAndNumbers() throws {
        let src = #"{"z":1,"a":{"y":2.50,"b":[true,false,null,1e-3]},"s":"q\"\\/\n\té😀"}"#
        let j = try JSON.parse(src)
        #expect(j.object?.keys == ["z", "a", "s"])
        #expect(j["a"]?.object?.keys == ["y", "b"])
        #expect(j["a"]?["y"] == .number("2.50"))
        #expect(j["s"]?.string == "q\"\\/\n\té😀")
        #expect(j.text == #"{"z":1,"a":{"y":2.50,"b":[true,false,null,1e-3]},"s":"q\"\\/\n\té😀"}"#)
        #expect(try JSON.parse(j.text) == j)
    }

    @Test func rejectsInvalid() {
        for bad in ["{", #"{"a":}"#, "[1,]", #""unterminated"#, "tru", "{} x", "-"] {
            #expect(throws: JSONParseError.self) { try JSON.parse(bad) }
        }
    }

    @Test func controlCharactersEscaped() {
        #expect(JSON.string("a\u{01}b").text == #""a\u0001b""#)
    }

    @Test func bridgesJSONValue() {
        let v: JSONValue = .object(["n": .number(64), "f": .number(0.5), "a": .array([.bool(true), .null])])
        let j = JSON(v)
        #expect(j["n"] == .number("64"))
        #expect(j["f"] == .number("0.5"))
        #expect(j.jsonValue == v)
    }
}

@Suite struct RequestTranslationTests {
    @Test func systemBlocksJoinedInOrderAndMidConversationSystemBecomesReminder() throws {
        let m = try msgs(#"{"model":"m","max_tokens":10,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2"},{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":"budget: 10"},{"role":"user","content":"go"}]}"#)
        #expect(m[0]["role"]?.string == "system")
        #expect(m[0]["content"]?.string == "x-anthropic-billing-header: cc_version=2\nYou are Claude Code.")
        #expect(roles(m) == ["system", "user", "user", "user"])
        #expect(m[2]["content"]?.string == "<system-reminder>\nbudget: 10\n</system-reminder>")
        #expect(try msgs(#"{"model":"m","system":"plain","messages":[]}"#)[0]["content"]?.string == "plain")
        #expect(try msgs(#"{"model":"m","system":[],"messages":[{"role":"user","content":"x"}]}"#).count == 1)
    }

    @Test func toolUseAndResult() throws {
        let m = try msgs(#"{"model":"m","messages":[{"role":"user","content":"read it"},{"role":"assistant","content":[{"type":"text","text":"Reading."},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"/a","limit":5}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"line1"},{"type":"text","text":"line2"}]}]}]}"#)
        #expect(roles(m) == ["user", "assistant", "tool"])
        #expect(m[1]["content"]?.string == "Reading.")
        let call = m[1]["tool_calls"]?[0]
        #expect(call?["id"]?.string == "toolu_1")
        #expect(call?["type"]?.string == "function")
        #expect(call?["function"]?["name"]?.string == "Read")
        #expect(call?["function"]?["arguments"]?.string == #"{"path":"/a","limit":5}"#)
        #expect(m[2]["tool_call_id"]?.string == "toolu_1")
        #expect(m[2]["content"]?.string == "line1\nline2")
    }

    @Test func toolResultsComeBeforeTheUserTextOfTheSameTurn() throws {
        let m = try msgs(#"{"model":"m","messages":[{"role":"user","content":"go"},{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"A","input":{}},{"type":"tool_use","id":"b","name":"B","input":{}}]},{"role":"user","content":[{"type":"text","text":"<system-reminder>x</system-reminder>"},{"type":"tool_result","tool_use_id":"a","content":"ra"},{"type":"tool_result","tool_use_id":"b","content":"rb"},{"type":"text","text":"now summarise"}]}]}"#)
        #expect(roles(m) == ["user", "assistant", "tool", "tool", "user"])
        #expect(m[1]["content"] == .null)
        #expect(m[2]["tool_call_id"]?.string == "a" && m[3]["tool_call_id"]?.string == "b")
        let parts = m[4]["content"]?.array ?? []
        #expect(parts.compactMap { $0["text"]?.string } == ["<system-reminder>x</system-reminder>", "now summarise"])
    }

    @Test func mediaInToolResultRidesInTheFollowingUserMessage() throws {
        let m = try msgs(#"{"model":"m","messages":[{"role":"user","content":"look"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_9","name":"Screenshot","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_9","content":[{"type":"text","text":"captured"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]}]}"#)
        #expect(roles(m) == ["user", "assistant", "tool", "user"])
        #expect(m[2]["content"]?.string == "captured")
        let parts = m[3]["content"]?.array ?? []
        #expect(parts.map { $0.type ?? "" } == ["text", "image_url"])
        #expect(parts[0]["text"]?.string?.contains("toolu_9") == true)
        #expect(parts[1]["image_url"]?["url"]?.string == "data:image/png;base64,iVBORw0KGgo=")
        // Only media: the tool message says so.
        let m2 = try msgs(#"{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"\#(attachmentTextType)","text":"[document: a.pdf]\nhello"}]}]}]}"#)
        #expect(m2[0]["content"]?.string?.contains("attachments") == true)
        #expect(m2[1]["content"]?.array?[1]["text"]?.string == "[document: a.pdf]\nhello")
    }

    @Test func toolErrorIsPrefixed() throws {
        let m = try msgs(#"{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","is_error":true,"content":"boom"}]}]}"#)
        #expect(m[0]["content"]?.string == "[tool error] boom")
    }

    @Test func thinkingReplayedAsReasoningAndRedactedDropped() throws {
        let m = try msgs(#"{"model":"m","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"first thoughts","signature":"x"},{"type":"redacted_thinking","data":"zzz"},{"type":"text","text":"answer"}]},{"role":"user","content":[{"type":"thinking","thinking":"ignored"},{"type":"text","text":"ok"}]}]}"#)
        #expect(m[0]["reasoning_content"]?.string == "first thoughts")
        #expect(m[0]["reasoning"]?.string == "first thoughts")
        #expect(m[0]["content"]?.string == "answer")
        #expect(m[1]["content"]?.string == "ok")
    }

    @Test func imagesBecomeImageURLParts() throws {
        let m = try msgs(#"{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"AAAA"}},{"type":"image","source":{"type":"url","url":"https://x/y.png"}}]}]}"#)
        let parts = m[0]["content"]?.array ?? []
        #expect(parts.map { $0.type ?? "" } == ["text", "image_url", "image_url"])
        #expect(parts[1]["image_url"]?["url"]?.string == "data:image/jpeg;base64,AAAA")
        #expect(parts[2]["image_url"]?["url"]?.string == "https://x/y.png")
    }

    @Test func deferredToolsLoadAfterAToolReference() throws {
        let tools = #""tools":[{"name":"Read","description":"read","input_schema":{"type":"object"}},{"name":"ToolSearch","description":"find tools","input_schema":{"type":"object"}},{"name":"DeferredToolPlaceholder","description":"never call","input_schema":{"type":"object"},"defer_loading":true},{"name":"mcp__mini__shout","description":"shout","input_schema":{"type":"object"},"defer_loading":true}]"#
        let before = try translate(#"{"model":"m",\#(tools),"messages":[{"role":"user","content":"shout hello"}]}"#)
        #expect(toolNames(before) == ["Read", "ToolSearch"])
        let after = try translate(#"{"model":"m",\#(tools),"messages":[{"role":"user","content":"shout hello"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"ToolSearch","input":{"query":"shout"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"tool_reference","tool_name":"mcp__mini__shout"},{"type":"tool_reference","tool_name":"DeferredToolPlaceholder"}]}]}]}"#)
        #expect(toolNames(after) == ["Read", "ToolSearch", "mcp__mini__shout"])
        let toolMsg = after["messages"]?.array?.last
        #expect(toolMsg?["role"]?.string == "tool")
        #expect(toolMsg?["content"]?.string?.contains("Tool mcp__mini__shout is now loaded and can be called.") == true)
    }

    @Test func toolSchemasPassThroughByteStable() throws {
        let schema = #"{"type":"object","properties":{"zeta":{"type":"string"},"alpha":{"type":"integer","minimum":0.0}},"required":["zeta"]}"#
        let chat = try translate(#"{"model":"m","tools":[{"name":"T","description":"d","input_schema":\#(schema)}],"messages":[]}"#)
        #expect(chat["tools"]?[0]?.text == #"{"type":"function","function":{"name":"T","description":"d","parameters":\#(schema)}}"#)
    }

    @Test func serverToolsDroppedAndWebSearchOffered() throws {
        let chat = try translate(#"{"model":"m","tools":[{"type":"code_execution_20250522","name":"code_execution"},{"name":"Bash","input_schema":{"type":"object"}},{"type":"web_search_20250305","name":"web_search","max_uses":3}],"messages":[]}"#)
        #expect(toolNames(chat) == ["Bash", "web_search"])
        #expect(chat["tools"]?[1]?["function"]?["parameters"]?["required"] == ["query"])
        #expect(WebSearchTool.find(in: try JSON.parse(#"[{"type":"web_search_20250305","name":"web_search","max_uses":3}]"#).array)?.maxUses == 3)
        #expect(WebSearchTool.find(in: try JSON.parse(#"[{"type":"web_search_20250305","name":"web_search"}]"#).array)?.maxUses == 5)
        // Only a server tool that isn't web search: no tools at all.
        let none = try translate(#"{"model":"m","tools":[{"type":"bash_20250124","name":"bash"}],"tool_choice":{"type":"auto"},"messages":[]}"#)
        #expect(none["tools"] == nil && none["tool_choice"] == nil)
    }

    @Test func toolChoiceMapping() throws {
        let tools = #""tools":[{"name":"t","input_schema":{"type":"object"}}]"#
        let cases: [(String, JSON)] = [
            (#"{"type":"auto"}"#, "auto"), (#"{"type":"any"}"#, "required"), (#"{"type":"none"}"#, "none"),
            (#"{"type":"tool","name":"t"}"#, ["type": "function", "function": ["name": "t"]]),
        ]
        for (tc, want) in cases {
            #expect(try translate(#"{"model":"m",\#(tools),"tool_choice":\#(tc),"messages":[]}"#)["tool_choice"] == want)
        }
        let p = try translate(#"{"model":"m",\#(tools),"tool_choice":{"type":"auto","disable_parallel_tool_use":true},"messages":[]}"#)
        #expect(p["parallel_tool_calls"] == false)
        #expect(try translate(#"{"model":"m",\#(tools),"messages":[]}"#)["parallel_tool_calls"] == nil)
        #expect(try translate(#"{"model":"m","tool_choice":{"type":"any"},"messages":[]}"#)["tool_choice"] == nil)
    }

    @Test func thinkingDisabledAppliesThinkingOffAndDropsEffort() throws {
        var m = glm
        m.thinkingOff = .object(["chat_template_kwargs": .object(["enable_thinking": .bool(false)])])
        let off = try translate(#"{"model":"m","max_tokens":100,"thinking":{"type":"disabled"},"output_config":{"effort":"high"},"messages":[]}"#, model: m)
        #expect(off["chat_template_kwargs"]?["enable_thinking"] == false)
        #expect(off["reasoning_effort"] == nil)
        #expect(off["thinking"] == nil)

        let on = try translate(#"{"model":"m","max_tokens":100,"thinking":{"type":"enabled","budget_tokens":2000},"output_config":{"effort":"max"},"messages":[]}"#, model: m)
        #expect(on["chat_template_kwargs"] == nil)
        #expect(on["reasoning_effort"]?.string == "xhigh")
        #expect(on["thinking"] == nil)

        // No thinkingOff configured: nothing is added.
        let plain = try translate(#"{"model":"m","thinking":{"type":"disabled"},"messages":[]}"#)
        #expect(plain["chat_template_kwargs"] == nil && plain["enable_thinking"] == nil)

        // A reasoning_effort thinkingOff (GLM-5.3 with vLLM) merges in.
        m.thinkingOff = .object(["reasoning_effort": .string("low")])
        #expect(try translate(#"{"model":"m","thinking":{"type":"disabled"},"output_config":{"effort":"high"},"messages":[]}"#, model: m)["reasoning_effort"]?.string == "low")
    }

    @Test func thinkingOffMergesIntoExistingObjects() {
        let merged = RequestTranslator.deepMerge(["a": ["x": 1, "y": 2], "b": 1], ["a": ["y": 3, "z": 4], "c": true])
        #expect(merged == ["a": ["x": 1, "y": 3, "z": 4], "b": 1, "c": true])
    }

    @Test func outputFormatBecomesStrictJSONSchema() throws {
        for field in [#""output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"a":{"type":"string"}}}}}"#,
                      #""output_format":{"type":"json_schema","schema":{"type":"object","properties":{"a":{"type":"string"}}}}"#] {
            let rf = try translate(#"{"model":"m","messages":[],\#(field)}"#)["response_format"]
            #expect(rf?["type"]?.string == "json_schema")
            #expect(rf?["json_schema"]?["name"]?.string == "output")
            #expect(rf?["json_schema"]?["strict"] == true)
            #expect(rf?["json_schema"]?["schema"]?["properties"]?["a"] != nil)
        }
        #expect(try translate(#"{"model":"m","messages":[],"output_config":{"format":{"type":"text"}}}"#)["response_format"] == nil)
    }

    @Test func samplingStopAndMetadata() throws {
        let c = try translate(#"{"model":"m","max_tokens":321,"stream":true,"stop_sequences":["</x>","END"],"temperature":0.7,"top_p":0.9,"top_k":40,"metadata":{"user_id":"u1"},"messages":[]}"#)
        #expect(c["max_tokens"] == 321)
        #expect(c["stop"] == ["</x>", "END"])
        #expect(c["temperature"] == .number("0.7") && c["top_p"] == .number("0.9") && c["top_k"] == 40)
        #expect(c["user"]?.string == "u1")
        #expect(c["stream"] == true && c["stream_options"]?["include_usage"] == true)
        #expect(c["model"] == nil) // the backend target sets the upstream name
        let n = try translate(#"{"model":"m","max_tokens":5,"messages":[]}"#)
        #expect(n["stream"] == false && n["stream_options"] == nil && n["stop"] == nil)
    }

    @Test func maxTokensClampedToTheContextWindow() throws {
        let big = String(repeating: "abcd ", count: 4000) // ~5000 tokens
        let body = #"{"model":"m","max_tokens":8000,"messages":[{"role":"user","content":"\#(big)"}]}"#
        let clamped = try translate(body, window: 10_000)["max_tokens"]?.int ?? 0
        #expect(clamped < 5000 && clamped > 3000)
        #expect(try translate(body)["max_tokens"] == 8000)                  // window unknown
        #expect(try translate(body, window: 100_000)["max_tokens"] == 8000)  // fits
        let huge = String(repeating: "abcd ", count: 12000)
        #expect(try translate(#"{"model":"m","max_tokens":8000,"messages":[{"role":"user","content":"\#(huge)"}]}"#, window: 10_000)["max_tokens"] == 8000)
    }

    @Test func earlierWebSearchesReplayAsCallAndResult() throws {
        let enc = Data("Swift 6.4 shipped".utf8).base64EncodedString()
        let m = try msgs(#"{"model":"m","messages":[{"role":"user","content":"search swift"},{"role":"assistant","content":[{"type":"text","text":"Let me look."},{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"swift 6.4"}},{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://swift.org","title":"Swift","encrypted_content":"\#(enc)","page_age":"2026-09-01"}]},{"type":"text","text":"It shipped."}]},{"role":"user","content":"thanks"}]}"#)
        #expect(roles(m) == ["user", "assistant", "tool", "assistant", "user"])
        #expect(m[1]["tool_calls"]?[0]?["function"]?["name"]?.string == "web_search")
        #expect(m[1]["content"]?.string == "Let me look.")
        #expect(m[2]["tool_call_id"]?.string == "srvtoolu_1")
        let result = m[2]["content"]?.string ?? ""
        #expect(result.contains("https://swift.org") && result.contains("Swift 6.4 shipped") && result.contains("2026-09-01"))
        #expect(m[3]["content"]?.string == "It shipped.")

        // A call without its result stays as text.
        let lone = try msgs(#"{"model":"m","messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"s","name":"web_search","input":{"query":"q"}},{"type":"text","text":"done"}]}]}"#)
        #expect(lone.count == 1 && lone[0]["content"]?.string == "[Searched the web: q]done")

        // An error result replays as the failure text.
        let err = try msgs(#"{"model":"m","messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"s","name":"web_search","input":{"query":"q"}},{"type":"web_search_tool_result","tool_use_id":"s","content":{"type":"web_search_tool_result_error","error_code":"max_uses_exceeded"}}]}]}"#)
        #expect(err[1]["content"]?.string?.contains("limit") == true)
    }

    @Test func searchAlongsideClientToolCallsKeepsToolMessagesTogether() throws {
        // Client call first: the search result waits for the client's results.
        let m = try msgs(#"{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}},{"type":"server_tool_use","id":"s1","name":"web_search","input":{"query":"q"}},{"type":"web_search_tool_result","tool_use_id":"s1","content":[]}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file"}]}]}"#)
        #expect(roles(m) == ["assistant", "tool", "tool"])
        #expect(m[0]["tool_calls"]?.array?.count == 2)
        #expect(m[1]["tool_call_id"]?.string == "s1" && m[2]["tool_call_id"]?.string == "t1")
        // Search first: its call and result, then the client's call.
        let n = try msgs(#"{"model":"m","messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"s1","name":"web_search","input":{"query":"q"}},{"type":"web_search_tool_result","tool_use_id":"s1","content":[]},{"type":"tool_use","id":"t1","name":"Read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file"}]}]}"#)
        #expect(roles(n) == ["assistant", "tool", "assistant", "tool"])
    }

    @Test func webSearchResultInUserMessageBecomesText() throws {
        let m = try msgs(#"{"model":"m","messages":[{"role":"user","content":[{"type":"web_search_tool_result","tool_use_id":"x","content":[{"type":"web_search_result","title":"T","url":"https://u"}]},{"type":"text","text":"so?"}]}]}"#)
        let parts = m[0]["content"]?.array ?? []
        #expect(parts[0]["text"]?.string == "[Web search results:\n- T (https://u)]")
    }

    @Test func unknownBlocksAndEmptyMessages() throws {
        let m = try msgs(#"{"model":"m","messages":[{"role":"user","content":[{"type":"mystery"}]},{"role":"assistant","content":[]},{"role":"user","content":"x"}]}"#)
        #expect(roles(m) == ["assistant", "user"])
        #expect(m[0]["content"]?.string == "")
    }
}

@Suite struct ResponseTranslationTests {
    @Test func contentThinkingToolsUsageAndStop() throws {
        let chat = try JSON.parse(Chunks.completion(content: "<think>inner</think>The answer", toolCalls: #"[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"/a\"}"}},{"id":"","type":"function","function":{"name":"Bad","arguments":"not json"}}]"#, finish: "stop", prompt: 100, completion: 4, cached: 60))
        let msg = try #require(ResponseTranslator.message(chat, model: "GLM", stopSequences: []))
        let c = msg["content"]?.array ?? []
        #expect(c.map { $0.type ?? "" } == ["thinking", "text", "tool_use", "tool_use"])
        #expect(c[0]["thinking"]?.string == "inner")
        #expect(c[1]["text"]?.string == "The answer")
        #expect(c[2]["input"] == ["path": "/a"])
        #expect(c[3]["input"] == [:])
        #expect(c[3]["id"]?.string?.hasPrefix("toolu_") == true)
        #expect(msg["stop_reason"]?.string == "tool_use")
        #expect(msg["model"]?.string == "GLM")
        #expect(msg["usage"]?["input_tokens"] == 40 && msg["usage"]?["cache_read_input_tokens"] == 60 && msg["usage"]?["output_tokens"] == 4)
    }

    @Test func reasoningFieldsAndStopSequence() throws {
        let chat = try JSON.parse(Chunks.completion(content: "<block>no", reasoning: "hmm", finish: "stop", stopReason: "</block>"))
        let msg = try #require(ResponseTranslator.message(chat, model: "m", stopSequences: ["</block>"]))
        #expect(msg["content"]?[0]?["thinking"]?.string == "hmm")
        #expect(msg["content"]?[1]?["text"]?.string == "<block>no")
        #expect(msg["stop_reason"]?.string == "stop_sequence" && msg["stop_sequence"]?.string == "</block>")
        // A stop_reason not among the request's sequences is an ordinary end.
        let other = try #require(ResponseTranslator.message(chat, model: "m", stopSequences: []))
        #expect(other["stop_reason"]?.string == "end_turn" && other["stop_sequence"] == .null)
    }

    @Test func stopReasonMapping() {
        #expect(resolveStopReason(finish: "length", backendStop: nil, stopSequences: [], sawToolCalls: false).0 == "max_tokens")
        #expect(resolveStopReason(finish: "tool_calls", backendStop: nil, stopSequences: [], sawToolCalls: false).0 == "tool_use")
        #expect(resolveStopReason(finish: "stop", backendStop: nil, stopSequences: [], sawToolCalls: true).0 == "tool_use")
        #expect(resolveStopReason(finish: "length", backendStop: nil, stopSequences: [], sawToolCalls: true).0 == "max_tokens")
        #expect(resolveStopReason(finish: "content_filter", backendStop: nil, stopSequences: [], sawToolCalls: false).0 == "end_turn")
    }

    @Test func loneLeadingCloseThinkStripped() {
        #expect(ThinkTagFilter.strip("\n</think>\n\n<severity>12").content == "<severity>12")
        #expect(ThinkTagFilter.strip("a<think>b").reasoning == "b")
    }
}

@Suite struct ThinkTagTests {
    func run(_ chunks: [String]) -> [ThinkTagFilter.Segment] {
        var f = ThinkTagFilter()
        var out: [ThinkTagFilter.Segment] = []
        for c in chunks { out += f.process(c) }
        out += f.flush()
        // Merge neighbours of the same kind.
        var merged: [ThinkTagFilter.Segment] = []
        for s in out {
            if let last = merged.last, last.reasoning == s.reasoning { merged[merged.count - 1].text += s.text } else { merged.append(s) }
        }
        return merged
    }

    @Test func tagsSplitAcrossChunks() {
        #expect(run(["<th", "ink>reason", "ing</thi", "nk>answer"]) == [.init(text: "reasoning", reasoning: true), .init(text: "answer", reasoning: false)])
        #expect(run(["a <", "b"]) == [.init(text: "a <b", reasoning: false)])
        #expect(run(["x<think>y"]) == [.init(text: "x", reasoning: false), .init(text: "y", reasoning: true)])
    }

    @Test func loneLeadingCloseDropped() {
        #expect(run(["</think>", "\n\nanswer"]) == [.init(text: "\n\nanswer", reasoning: false)])
        #expect(run(["</th", "ink>yes"]) == [.init(text: "yes", reasoning: false)])
        #expect(run(["ok </think> later"]) == [.init(text: "ok </think> later", reasoning: false)])
    }
}

@Suite struct EstimateAndErrorTests {
    @Test func estimateSkipsBase64Media() {
        let img = String(repeating: "A", count: 4 << 20)
        let est = TokenEstimate.tokens(Data(#"{"messages":[{"image":"data:image/png;base64,\#(img)"}]}"#.utf8))
        #expect(est < 5000 && est > 1000)
        let pdf = "JVBERi" + String(repeating: "B", count: 300_000)
        #expect(TokenEstimate.tokens(Data(pdf.utf8)) == 300_006 * 3 / 4 / 150)
        #expect(TokenEstimate.tokens(Data(String(repeating: "word ", count: 400).utf8)) == 500)
    }

    @Test func contextLengthErrorsRewritten() {
        let cases: [(String, String)] = [
            (#"{"error":{"message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens (35000 in the messages, 5000 in the completion). Please reduce the length of the messages or completion.","type":"BadRequestError","code":400}}"#, "prompt is too long: 35000 tokens > 32768 maximum"),
            (#"{"error":{"message":"You passed 35000 input tokens and requested 5000 output tokens. However, the model's context length is only 32768 tokens, resulting in a maximum input length of 27768 tokens."}}"#, "prompt is too long: 35000 tokens > 32768 maximum"),
            (#"{"error":{"message":"Requested token count exceeds the model's maximum context length of 32768 tokens. You requested a total of 40000 tokens: 35000 tokens from the input messages and 5000 tokens for the completion."}}"#, "prompt is too long: 35000 tokens > 32768 maximum"),
            (#"{"error":{"message":"Input length (40001) exceeds the maximum allowed length (32768)."}}"#, "prompt is too long: 40001 tokens > 32768 maximum"),
            (#"{"error":{"code":400,"message":"the request exceeds the available context size","type":"exceed_context_size_error","n_prompt_tokens":50000,"n_ctx":32768}}"#, "prompt is too long: 50000 tokens > 32768 maximum"),
            (#"{"detail":"This model's maximum context length is 1000 tokens. However, you requested 1500 tokens (1400 in the messages, 100 in the completion)."}"#, "prompt is too long: 1400 tokens > 1000 maximum"),
            (#"{"error":{"message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens (30000 in the messages, 10000 in the completion)."}}"#, "input length and `max_tokens` exceed context limit: 30000 + 10000 > 32768"),
        ]
        for (body, want) in cases {
            let e = buildClientError(status: 400, body: Data(body.utf8))
            #expect(e.status == 400 && e.type == "invalid_request_error" && e.message == want, "\(body)")
        }
    }

    @Test func scrubsAddressesAndCredentials() {
        let s = scrubUpstreamMessage("failed calling http://10.1.2.3:8000/v1/chat with Bearer abcdefghijkl and api_key=sk-secretvalue123 on 192.168.0.5:9000")
        for bad in ["10.1.2.3", "abcdefghijkl", "secretvalue", "192.168.0.5", "http://"] { #expect(!s.contains(bad), "\(bad) in \(s)") }
    }

    @Test func statusMapping() {
        let cases: [(Int, Int, String)] = [(400, 400, "invalid_request_error"), (401, 502, "api_error"), (403, 502, "api_error"),
                                           (404, 404, "not_found_error"), (413, 413, "request_too_large"), (422, 400, "invalid_request_error"),
                                           (429, 429, "rate_limit_error"), (500, 500, "api_error"), (503, 503, "overloaded_error"),
                                           (529, 529, "overloaded_error"), (418, 400, "invalid_request_error")]
        for (up, status, type) in cases {
            let e = buildClientError(status: up, body: Data(#"{"error":{"message":"no"}}"#.utf8))
            #expect(e.status == status && e.type == type, "upstream \(up)")
        }
        #expect(buildClientError(status: 401, body: Data()).message.contains("credentials"))
        #expect(buildClientError(status: 500, body: Data()).message == "backend returned HTTP 500")
        #expect(buildClientError(status: 422, body: Data(#"{"detail":[{"msg":"a"},{"msg":"b"}]}"#.utf8)).message == "a; b")
        let native = buildClientError(status: 429, body: Data(#"{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}"#.utf8), native: true)
        #expect(native.type == "rate_limit_error" && native.message == "slow down")
    }
}

@Suite struct RoutingTests {
    let routing = Routing(config: EngineConfig(
        backends: [Backend(id: "b", baseURL: URL(string: "http://h/v1")!)],
        models: [Model(id: "GLM-5.3", backendID: "b", upstreamModel: "glm"), Model(id: "Flash", backendID: "b", upstreamModel: "flash"),
                 Model(id: "Heavy", backendID: "b", upstreamModel: "heavy")],
        roles: Roles(main: "GLM-5.3", opus: "Heavy", small: "Flash")), secrets: CoreSecrets())

    @Test func claudeNamesMapToRoles() {
        #expect(routing.resolve("GLM-5.3")?.id == "GLM-5.3")
        #expect(routing.resolve("glm-5.3")?.id == "GLM-5.3")
        #expect(routing.resolve("Flash")?.id == "Flash")
        #expect(routing.resolve("claude-opus-5[1m]")?.id == "Heavy")
        #expect(routing.resolve("claude-opus-4-1-20250805")?.id == "Heavy")
        #expect(routing.resolve("claude-fable-5-1")?.id == "Heavy")
        #expect(routing.resolve("claude-mythos-5-1")?.id == "Heavy")
        #expect(routing.resolve("claude-haiku-4-5-20251001")?.id == "Flash")
        #expect(routing.resolve("claude-sonnet-5")?.id == "GLM-5.3")
        #expect(routing.resolve("claude-sonnet-4-5[1m]")?.id == "GLM-5.3")
        #expect(routing.resolve("GLM-5.3[1m]")?.id == "GLM-5.3")
        #expect(routing.resolve("gpt-who")?.id == "GLM-5.3")
        #expect(routing.resolve("opus")?.id == "Heavy")
    }

    @Test func unsetRolesFallBackToMain() {
        let r = Routing(config: EngineConfig(models: [Model(id: "A", backendID: "b", upstreamModel: "a")], roles: Roles(main: "A")), secrets: CoreSecrets())
        #expect(r.resolve("claude-opus-5")?.id == "A")
        #expect(r.resolve("claude-haiku-5")?.id == "A")
        let none = Routing(config: EngineConfig(), secrets: CoreSecrets())
        #expect(none.resolve("claude-sonnet-5") == nil)
    }

    @Test func targetCarriesTheKeyAndURLs() throws {
        let r = Routing(config: EngineConfig(backends: [Backend(id: "b", baseURL: URL(string: "http://h:8000/v1/")!, apiKeyRef: "stub-key")],
                                             models: [Model(id: "A", backendID: "b", upstreamModel: "up-a")], roles: Roles(main: "A")),
                        secrets: CoreSecrets())
        let t = try #require(r.target(r.resolve("A")!))
        let req = t.chatRequest(["messages": [], "model": "ignored"], stream: true)
        #expect(req.url.absoluteString == "http://h:8000/v1/chat/completions")
        #expect(req.headers.contains { $0 == ("Authorization", "Bearer sk-stub-secret") })
        #expect(req.headers.contains { $0 == ("Accept", "text/event-stream") })
        let body = try JSON.parse(req.body!)
        #expect(body.object?.keys.first == "model" && body["model"]?.string == "up-a")
        #expect(r.target(Model(id: "x", backendID: "missing", upstreamModel: "x")) == nil)
    }
}
