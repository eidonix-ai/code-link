import Foundation
import Testing
@testable import CodeLinkEngine

private let okStream: @Sendable (StubBackend.Seen, Int) -> StubBackend.Reply = { _, _ in
    .sse([Chunks.content("ok"), Chunks.finish()])
}

private let okJSON: @Sendable (StubBackend.Seen, Int) -> StubBackend.Reply = { _, _ in
    .json(200, Chunks.completion(content: "ok"))
}

@Suite(.serialized) struct ResponsesTests {
    // MARK: system and developer messages

    @Test func leadingDeveloperMessagesMergeIntoOneSystemMessage() async throws {
        try await CoreHarness.run(backend: okStream) { h in
            let body = try codexFixture("main_turn")
            let input = try #require(try JSON.parse(body)["input"]?.array)
            var texts: [String] = []
            for i in 0..<2 { for p in input[i]["content"]?.array ?? [] { texts.append(p["text"]?.string ?? "") } }

            let r = try await h.responses(body)
            #expect(r.status == 200)
            let up = try #require(h.backend.lastChat)
            let msgs = try #require(up["messages"]?.array)
            #expect(msgs.count == 3 && msgs.map { $0["role"]?.string ?? "" } == ["system", "user", "user"])
            // Exactly the developer texts, one blank line apart.
            #expect(msgs[0]["content"]?.string == texts.joined(separator: "\n\n"))
            #expect(msgs[2]["content"]?.string == "run it")
            #expect(up["model"]?.string == "glm-upstream")
        }
    }

    @Test func instructionsAndTheLeadingRunMergeExactBytes() async throws {
        try await CoreHarness.run(backend: okStream) { h in
            let input = #"""
            [{"type":"additional_tools","role":"developer","tools":[]},
             {"type":"message","role":"developer","content":[{"type":"input_text","text":"B"},{"type":"input_text","text":"C"}]},
             {"type":"message","role":"system","content":"D"},
             {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
             {"type":"message","role":"assistant","content":[{"type":"output_text","text":"yo"}]},
             {"type":"message","role":"developer","content":[{"type":"input_text","text":"later"}]},
             {"type":"message","role":"user","content":"next"}]
            """#
            _ = try await h.responses(responsesBody(input: input, extra: #""instructions":"A""#))
            let msgs = try #require(h.backend.lastChat?["messages"])
            #expect(JSON.array(msgs.array ?? []).text == #"""
            [{"role":"system","content":"A\n\nB\n\nC\n\nD"},{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"<developer>\nlater\n</developer>"},{"role":"user","content":"next"}]
            """#)
        }
    }

    // MARK: tools

    @Test func additionalToolsBecomeChatToolsAndTheOutputFormat() async throws {
        try await CoreHarness.run(backend: { _, _ in .sse([Chunks.content(#"{"outcome":"allow"}"#), Chunks.finish()]) }) { h in
            let r = try await h.responses(try codexFixture("auto_review"))
            #expect(r.status == 200)
            let up = try #require(h.backend.lastChat)
            let tools = up["tools"]?.array ?? []
            let fns = Dictionary(uniqueKeysWithValues: tools.map { ($0["function"]?["name"]?.string ?? "", $0["function"]!) })
            // The implicit `functions` namespace is dropped: exec, wait.
            #expect(Set(fns.keys) == ["exec", "wait"])
            // exec is a freeform tool: one string parameter, the grammar in its description.
            let params = try #require(fns["exec"]?["parameters"])
            #expect(params["properties"]?["input"]?["type"]?.string == "string" && params["required"] == ["input"])
            #expect(fns["exec"]?["description"]?.string?.contains("lark grammar") == true)

            let msgs = up["messages"]?.array ?? []
            #expect(msgs.first?["role"]?.string == "system")
            #expect(!msgs.contains { $0["role"]?.string == "developer" })
            #expect((msgs[0]["content"]?.string ?? "").components(separatedBy: "<permissions instructions>").count == 2)

            let rf = try #require(up["response_format"])
            #expect(rf["type"]?.string == "json_schema" && rf["json_schema"]?["name"]?.string == "codex_output_schema")
            #expect(rf["json_schema"]?["strict"] == false)
            #expect(rf["json_schema"]?["schema"]?["properties"]?["outcome"] != nil)
            #expect(up["reasoning_effort"]?.string == "low")
            #expect(up["parallel_tool_calls"] == nil || up["tools"] != nil)
            for k in ["verbosity", "text", "store", "include", "prompt_cache_key", "reasoning", "input"] {
                #expect(up[k] == nil, "\(k) leaked into the chat request")
            }
            let out = completedOutput(r.events)
            let text = out.last?["content"]?[0]?["text"]?.string ?? ""
            let verdict = try JSON.parse(text)
            #expect(verdict["outcome"]?.string == "allow")
        }
    }

    @Test func customToolCallRoundTripsWithItsEvents() async throws {
        let chunks = [
            Chunks.toolCall(index: 0, id: "call_x", name: "exec", args: ""),
            Chunks.toolCall(index: 0, id: nil, name: nil, args: #"{"input":"// @exec: {}\nawait tools."#),
            Chunks.toolCall(index: 0, id: nil, name: nil, args: #"exec_command({cmd:'ls'})"}"#),
            Chunks.finish("tool_calls"),
        ]
        try await CoreHarness.run(backend: { _, _ in .sse(chunks) }) { h in
            let r = try await h.responses(try codexFixture("auto_review"))
            let ev = r.events
            #expect(!ev.names.contains("response.failed"))
            let added = ev.data("response.output_item.added")
            #expect(added.count == 1)
            let item = try #require(added.first?["item"])
            #expect(item.type == "custom_tool_call" && item["name"]?.string == "exec")
            #expect(item["namespace"]?.string == "functions" && item["call_id"]?.string == "call_x")
            #expect(ev.data("response.function_call_arguments.delta").isEmpty)

            let want = "// @exec: {}\nawait tools.exec_command({cmd:'ls'})"
            let deltas = ev.data("response.custom_tool_call_input.delta")
            let done = ev.data("response.custom_tool_call_input.done")
            #expect(deltas.count == 1 && deltas[0]["delta"]?.string == want)
            #expect(done.count == 1 && done[0]["input"]?.string == want)
            #expect(deltas[0]["item_id"] == item["id"])
            let fin = try #require(ev.data("response.output_item.done").last?["item"])
            #expect(fin.type == "custom_tool_call" && fin["input"]?.string == want && fin["status"]?.string == "completed")
            #expect(completedOutput(ev).first?.type == "custom_tool_call")
        }
    }

    @Test func namespacedToolCallMapsBackWithItsNamespace() async throws {
        for stream in [true, false] {
            let call = Chunks.toolCall(index: 0, id: "call_n", name: "multi_agent_v1__close_agent", args: #"{"target":"a1"}"#)
            let message = #"[{"id":"call_n","type":"function","function":{"name":"multi_agent_v1__close_agent","arguments":"{\"target\":\"a1\"}"}}]"#
            try await CoreHarness.run(backend: { _, _ in
                stream ? .sse([call, Chunks.finish("tool_calls")])
                       : .json(200, Chunks.completion(content: nil, toolCalls: message, finish: "tool_calls"))
            }) { h in
                let r = try await h.responses(try codexFixture("main_turn") { $0["stream"] = .bool(stream) })
                #expect(r.status == 200)
                let names = (h.backend.lastChat?["tools"]?.array ?? []).compactMap { $0["function"]?["name"]?.string }
                #expect(names.contains("multi_agent_v1__close_agent") && names.contains("exec_command") && names.contains("web_search"))
                let item: JSON?
                if stream { item = r.events.data("response.output_item.done").last?["item"] } else { item = r.json?["output"]?[0] }
                #expect(item?.type == "function_call" && item?["name"]?.string == "close_agent")
                #expect(item?["namespace"]?.string == "multi_agent_v1" && item?["arguments"]?.string == #"{"target":"a1"}"#)
                #expect(item?["call_id"]?.string == "call_n")
            }
        }
    }

    @Test func toolFollowupHistoryBecomesAToolCallAndItsResult() async throws {
        try await CoreHarness.run(backend: okStream) { h in
            _ = try await h.responses(try codexFixture("tool_followup"))
            let msgs = h.backend.lastChat?["messages"]?.array ?? []
            #expect(msgs.map { $0["role"]?.string ?? "" } == ["system", "user", "user", "assistant", "tool"])
            let call = msgs[3]["tool_calls"]?[0]
            #expect(call?["id"]?.string == "call_1" && call?["function"]?["name"]?.string == "exec_command")
            #expect(call?["function"]?["arguments"]?.string?.contains("escalated-test") == true)
            #expect(msgs[3]["content"]?.isNull == true)
            #expect(msgs[4]["tool_call_id"]?.string == "call_1" && msgs[4]["content"]?.string?.contains("Process exited with code 0") == true)
        }
    }

    @Test func historyOfNamespacedAndCustomCallsUsesTheFlatNames() async throws {
        let input = #"""
        [{"role":"user","content":"go"},
         {"type":"custom_tool_call","call_id":"c1","name":"exec","namespace":"functions","input":"ls -la"},
         {"type":"custom_tool_call_output","call_id":"c1","output":"files"},
         {"type":"function_call","call_id":"c2","name":"wait","namespace":"multi_agent_v1","arguments":"{}"}]
        """#
        let tools = #"""
        "tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]},
                 {"type":"namespace","name":"multi_agent_v1","tools":[{"type":"function","name":"wait"}]}]
        """#
        try await CoreHarness.run(backend: okStream) { h in
            _ = try await h.responses(responsesBody(input: input, extra: tools))
            let msgs = h.backend.lastChat?["messages"]?.array ?? []
            #expect(msgs[1]["tool_calls"]?[0]?["function"]?["name"]?.string == "exec")
            #expect(msgs[1]["tool_calls"]?[0]?["function"]?["arguments"]?.string == #"{"input":"ls -la"}"#)
            #expect(msgs[2]["role"]?.string == "tool")
            #expect(msgs[3]["tool_calls"]?[0]?["function"]?["name"]?.string == "multi_agent_v1__wait")
        }
    }

    @Test func flatteningIsCollisionSafeAndBounded() {
        let tools: [JSON] = [
            ["type": "function", "name": "exec"],
            ["type": "namespace", "name": "functions", "tools": [["type": "custom", "name": "exec"], ["type": "function", "name": "wait"]]],
            ["type": "namespace", "name": "mcp.server", "tools": [["type": "function", "name": "do thing"], ["type": "function", "name": "wait"]]],
            ["type": "namespace", "name": "mcp_server", "tools": [["type": "function", "name": "do_thing"]]],
            ["type": "web_search"],
        ]
        let t = ToolRegistry.translate(tools)
        #expect(t.tools.compactMap { $0["function"]?["name"]?.string }
                == ["exec", "functions__exec", "wait", "mcp_server__do_thing", "mcp_server__wait", "mcp_server__do_thing_2"])
        #expect(t.stripped.count == 1 && t.stripped[0].type == "web_search")
        let b = t.registry.resolve("functions__exec")
        #expect(b == ToolBinding(namespace: "functions", name: "exec", custom: true))
        #expect(t.registry.resolve("exec") == ToolBinding(namespace: "", name: "exec", custom: false))
        let long = ToolRegistry.sanitize("ns__" + String(repeating: "a", count: 100))
        #expect(long.count == 64 && long.hasPrefix("ns__aaaa"))
        #expect(ToolRegistry.sanitize("ns__" + String(repeating: "a", count: 99)) != long)
        let choice = ToolRegistry.translate([["type": "namespace", "name": "ns", "tools": [["type": "function", "name": "f"]]]])
        #expect(choice.registry.toolChoice(["type": "function", "namespace": "ns", "name": "f"]).text
                == #"{"type":"function","function":{"name":"ns__f"}}"#)
        #expect(choice.registry.toolChoice("required") == "required")
        #expect(choice.registry.toolChoice(["type": "allowed_tools", "mode": "auto"]) == "auto")
    }

    @Test func toolCallDeltasAreKeyedByIndex() async throws {
        let chunks = [
            Chunks.toolCall(index: 0, id: "call_a", name: "f", args: #"{"x""#),
            Chunks.toolCall(index: 0, id: nil, name: nil, args: ":1}"),
            Chunks.toolCall(index: 1, id: nil, name: "g", args: "{}"),
            Chunks.toolCall(index: 0, id: "call_a", name: nil, args: ""),
            Chunks.finish("tool_calls"),
        ]
        try await CoreHarness.run(backend: { _, _ in .sse(chunks) }) { h in
            let r = try await h.responses(responsesBody(extra: #""tools":[{"type":"function","name":"f"},{"type":"function","name":"g"}]"#))
            let out = completedOutput(r.events)
            #expect(out.count == 2)
            #expect(out[0]["name"]?.string == "f" && out[0]["arguments"]?.string == #"{"x":1}"# && out[0]["call_id"]?.string == "call_a")
            #expect(out[1]["name"]?.string == "g" && out[1]["arguments"]?.string == "{}" && out[1]["call_id"]?.string?.isEmpty == false)
        }
    }

    // MARK: request fields

    @Test func requestFieldsMapToChatFields() async throws {
        try await CoreHarness.run(backend: okStream) { h in
            let extra = #"""
            "reasoning":{"effort":"high","summary":"auto"},"max_output_tokens":99,"temperature":0.3,"top_p":0.9,"user":"u1",
            "parallel_tool_calls":false,"tool_choice":"required","store":false,"include":["reasoning.encrypted_content"],
            "text":{"format":{"type":"json_object"},"verbosity":"low"},
            "tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object"},"strict":true}]
            """#
            _ = try await h.responses(responsesBody(extra: extra))
            let up = try #require(h.backend.lastChat)
            #expect(up["reasoning_effort"]?.string == "high" && up["max_tokens"] == 99 && up["temperature"] == 0.3 && up["top_p"] == 0.9)
            #expect(up["user"]?.string == "u1" && up["parallel_tool_calls"] == false && up["tool_choice"]?.string == "required")
            #expect(up["response_format"]?["type"]?.string == "json_object")
            #expect(up["stream"] == true && up["stream_options"]?["include_usage"] == true)
            #expect(up["tools"]?[0]?["function"]?["strict"] == true && up["tools"]?[0]?["function"]?["description"]?.string == "d")
            #expect(up["store"] == nil && up["include"] == nil && up["text"] == nil)
        }
    }

    @Test func effortNoneUsesTheModelsThinkingOff() async throws {
        try await CoreHarness.run(configure: { $0.models[0].thinkingOff = .object(["chat_template_kwargs": .object(["enable_thinking": .bool(false)])]) },
                                  backend: okStream) { h in
            _ = try await h.responses(responsesBody(extra: #""reasoning":{"effort":"none"}"#))
            #expect(h.backend.lastChat?["reasoning_effort"] == nil)
            #expect(h.backend.lastChat?["chat_template_kwargs"]?["enable_thinking"] == false)
        }
    }

    @Test func reasoningItemsReplayAsReasoningContent() async throws {
        let input = #"""
        [{"role":"user","content":"q"},
         {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"S"}],"content":[{"type":"reasoning_text","text":"raw thoughts"}],"encrypted_content":"xyz"},
         {"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
         {"type":"function_call_output","call_id":"c1","output":"ok"},
         {"type":"reasoning","summary":[{"type":"summary_text","text":"only summary"}]},
         {"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},
         {"type":"reasoning","summary":[{"type":"summary_text","text":"orphan"}]},
         {"role":"user","content":"next"}]
        """#
        try await CoreHarness.run(backend: okStream) { h in
            _ = try await h.responses(responsesBody(input: input))
            let msgs = h.backend.lastChat?["messages"]?.array ?? []
            #expect(msgs.map { $0["role"]?.string ?? "" } == ["user", "assistant", "tool", "assistant", "user"])
            #expect(msgs[1]["reasoning_content"]?.string == "raw thoughts" && msgs[1]["reasoning"]?.string == "raw thoughts")
            #expect(msgs[1]["tool_calls"] != nil)
            #expect(msgs[3]["reasoning_content"]?.string == "only summary" && msgs[3]["content"]?.string == "done")
            #expect(msgs[4]["reasoning_content"] == nil)
        }
    }

    @Test func statefulRequestsAreRefused() async throws {
        try await CoreHarness.run(backend: okStream) { h in
            for extra in [#""previous_response_id":"resp_1""#, #""store":true"#] {
                let r = try await h.responses(responsesBody(extra: extra))
                #expect(r.status == 400)
                #expect(r.json?["error"]?["type"]?.string == "invalid_request_error")
                #expect(r.json?["error"]?["code"]?.string?.hasSuffix("_not_supported") == true, "\(extra)")
            }
            #expect(h.backend.chatRequests.isEmpty)
            // Codex's own shape.
            let ok = try await h.responses(responsesBody(extra: #""store":false"#))
            #expect(ok.status == 200 && ok.events.names.last == "response.completed")
            let bad = try await h.responses("{nope")
            #expect(bad.status == 400 && bad.json?["error"]?["message"]?.string?.contains("JSON") == true)
            let none = try await h.responses(#"{"model":"GLM","input":[]}"#)
            #expect(none.status == 400 && none.json?["error"]?["code"]?.string == "invalid_prompt")
            let noModel = try await h.responses(#"{"input":"hi"}"#)
            #expect(noModel.status == 400)
        }
    }

    // MARK: model routing

    @Test func modelNamesRoute() async throws {
        try await CoreHarness.run(configure: { $0.roles.codex = "Small" }, backend: okJSON) { h in
            for (name, upstream) in [("Big", "big-upstream"), ("gpt-5-codex", "small-upstream"), ("opus", "big-upstream"),
                                     ("sonnet", "glm-upstream"), ("claude-haiku-4-5", "small-upstream"), ("Small", "small-upstream")] {
                let r = try await h.responses(responsesBody(model: name, stream: false))
                #expect(r.status == 200)
                #expect(h.backend.lastChat?["model"]?.string == upstream, "\(name)")
                #expect(r.json?["model"]?.string == name)
            }
        }
        try await CoreHarness.run(backend: okJSON) { h in
            _ = try await h.responses(responsesBody(model: "gpt-5-codex"))
            #expect(h.backend.lastChat?["model"]?.string == "glm-upstream")
        }
        try await CoreHarness.run(configure: { $0.roles = Roles(main: "") }, backend: okJSON) { h in
            let r = try await h.responses(responsesBody(model: "gpt-5-codex"))
            #expect(r.status == 404 && r.json?["error"]?["code"]?.string == "model_not_found")
        }
    }

    @Test func autoReviewRunsOnTheDecisionModelGeneratively() async throws {
        // roles.decision
        try await CoreHarness.run(configure: { $0.roles.decision = "Big"; $0.roles.codex = "Small" }, backend: okStream) { h in
            let r = try await h.responses(try codexFixture("auto_review"))
            #expect(r.status == 200)
            #expect(h.backend.lastChat?["model"]?.string == "big-upstream")
            #expect(h.backend.lastChat?["logprobs"] == nil && h.backend.lastChat?["max_tokens"] == nil)
            #expect(h.backend.lastChat?["response_format"]?["type"]?.string == "json_schema")
        }
        // The decision head's own model wins when the hook has one.
        let big = Model(id: "Big", backendID: "stub", upstreamModel: "big-upstream", contextWindow: 400_000)
        let hooks = EngineHooks(autoMode: CoreAutoMode(answer: nil, decisionModel: big))
        try await CoreHarness.run(hooks: hooks, configure: { $0.roles.codex = "Small" }, backend: okStream) { h in
            _ = try await h.responses(try codexFixture("auto_review"))
            #expect(h.backend.lastChat?["model"]?.string == "big-upstream")
            #expect(h.backend.chatRequests.count == 1)
        }
        // Else the Codex model, else main.
        try await CoreHarness.run(configure: { $0.roles.codex = "Small" }, backend: okStream) { h in
            _ = try await h.responses(try codexFixture("auto_review"))
            #expect(h.backend.lastChat?["model"]?.string == "small-upstream")
        }
        try await CoreHarness.run(backend: okStream) { h in
            _ = try await h.responses(try codexFixture("auto_review"))
            #expect(h.backend.lastChat?["model"]?.string == "glm-upstream")
        }
    }

    @Test func anthropicBackendsAreRefusedClearly() async throws {
        try await CoreHarness.run(configure: { $0.backends[0].kind = .anthropic }, backend: okStream) { h in
            let r = try await h.responses(responsesBody())
            #expect(r.status == 400 && r.json?["error"]?["code"]?.string == "unsupported_backend")
            #expect(h.backend.requests.isEmpty)
        }
    }

    // MARK: streaming

    @Test func streamingEventOrder() async throws {
        let chunks = [
            Chunks.reasoning("think "), Chunks.reasoning("hard"),
            Chunks.content("an"), Chunks.content("swer"),
            Chunks.toolCall(index: 0, id: "call_a", name: "exec_command", args: ""),
            Chunks.toolCall(index: 0, id: nil, name: nil, args: #"{"cmd":"#),
            Chunks.toolCall(index: 0, id: nil, name: nil, args: #""ls"}"#),
            Chunks.finish("tool_calls"),
            Chunks.usage(prompt: 100, completion: 20, cached: 64),
        ]
        try await CoreHarness.run(backend: { _, _ in .sse(chunks) }) { h in
            let r = try await h.responses(responsesBody())
            #expect(r.status == 200 && r.headers["content-type"] == "text/event-stream")
            let ev = r.events
            #expect(ev.names == [
                "response.created", "response.in_progress",
                "response.output_item.added", "response.reasoning_summary_part.added",
                "response.reasoning_summary_text.delta", "response.reasoning_text.delta",
                "response.reasoning_summary_text.delta", "response.reasoning_text.delta",
                "response.reasoning_summary_text.done", "response.reasoning_text.done", "response.output_item.done",
                "response.output_item.added", "response.content_part.added",
                "response.output_text.delta", "response.output_text.delta",
                "response.output_text.done", "response.content_part.done", "response.output_item.done",
                "response.output_item.added",
                "response.function_call_arguments.delta", "response.function_call_arguments.delta",
                "response.function_call_arguments.done", "response.output_item.done",
                "response.completed",
            ])
            // Every payload carries its event name as `type` and a rising sequence number.
            #expect(ev.allSatisfy { $0.data?.type == $0.event })
            #expect(ev.compactMap { $0.data?["sequence_number"]?.int } == Array(0..<ev.count))
            let created = ev[0].data?["response"]
            #expect(created?["status"]?.string == "in_progress" && created?["model"]?.string == "GLM" && created?["id"]?.string?.hasPrefix("resp_") == true)
            #expect(ev.data("response.output_item.added").compactMap { $0["output_index"]?.int } == [0, 1, 2])

            let reasoning = try #require(ev.data("response.output_item.done").first?["item"])
            #expect(reasoning.type == "reasoning" && reasoning["content"]?[0]?["text"]?.string == "think hard")
            #expect(reasoning["summary"]?[0]?["type"]?.string == "summary_text" && reasoning["summary"]?[0]?["text"]?.string == "think hard")
            #expect(ev.data("response.reasoning_text.done").first?["text"]?.string == "think hard")

            let done = try #require(ev.data("response.completed").first?["response"])
            #expect(done["status"]?.string == "completed" && done["output_text"]?.string == "answer")
            #expect(done["output"]?.array?.compactMap { $0.type } == ["reasoning", "message", "function_call"])
            #expect(done["output"]?[1]?["content"]?[0]?["text"]?.string == "answer")
            #expect(done["output"]?[2]?["arguments"]?.string == #"{"cmd":"ls"}"# && done["output"]?[2]?["status"]?.string == "completed")
            let usage = try #require(done["usage"])
            #expect(usage["input_tokens"] == 100 && usage["output_tokens"] == 20 && usage["total_tokens"] == 120)
            #expect(usage["input_tokens_details"]?["cached_tokens"] == 64)
            #expect(h.log.requests.last?.route == "/v1/responses" && h.log.requests.last?.status == 200)
        }
    }

    @Test func usageCarriesReasoningTokens() async throws {
        let usage = #"{"id":"c","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":64},"completion_tokens_details":{"reasoning_tokens":12}}}"#
        try await CoreHarness.run(backend: { _, _ in .sse([Chunks.content("x"), Chunks.finish(), usage]) }) { h in
            let u = try #require(try await h.responses(responsesBody()).events.data("response.completed").first?["response"]?["usage"])
            #expect(u["output_tokens_details"]?["reasoning_tokens"] == 12 && u["input_tokens_details"]?["cached_tokens"] == 64)
            #expect(u["total_tokens"] == 120)
        }
    }

    @Test func thinkTagsInContentBecomeReasoning() async throws {
        let chunks = [Chunks.content("<thi"), Chunks.content("nk>let me see</th"), Chunks.content("ink>The answer"), Chunks.finish()]
        try await CoreHarness.run(backend: { _, _ in .sse(chunks) }) { h in
            let ev = try await h.responses(responsesBody()).events
            let out = completedOutput(ev)
            #expect(out.compactMap { $0.type } == ["reasoning", "message"])
            #expect(out[0]["content"]?[0]?["text"]?.string == "let me see" && out[1]["content"]?[0]?["text"]?.string == "The answer")
        }
    }

    @Test func lengthEndsIncomplete() async throws {
        try await CoreHarness.run(backend: { _, _ in .sse([Chunks.content("cut"), Chunks.finish("length")]) }) { h in
            let ev = try await h.responses(responsesBody()).events
            #expect(ev.names.last == "response.incomplete")
            let r = ev.data("response.incomplete").first?["response"]
            #expect(r?["status"]?.string == "incomplete" && r?["incomplete_details"]?["reason"]?.string == "max_output_tokens")
        }
    }

    @Test func missingDoneAfterAFinishReasonStillCompletes() async throws {
        try await CoreHarness.run(backend: { _, _ in .sse([Chunks.content("hi"), Chunks.finish()], done: false) }) { h in
            let ev = try await h.responses(responsesBody()).events
            #expect(ev.names.last == "response.completed")
        }
    }

    @Test func largeLinesPassIntact() async throws {
        let big = String(repeating: "x", count: 2 << 20)
        try await CoreHarness.run(backend: { _, _ in .sse([Chunks.content(big), Chunks.finish()]) }) { h in
            let ev = try await h.responses(responsesBody()).events
            #expect(completedOutput(ev).first?["content"]?[0]?["text"]?.string?.count == big.count)
        }
    }

    @Test func truncatedStreamsFail() async throws {
        let cases: [(String, StubBackend.Reply)] = [
            ("no finish", .sse([Chunks.content("partial")], done: false)),
            ("done without finish", .sse([Chunks.content("partial")])),
            ("empty", .sse([], done: false)),
            ("mid tool call", .sse([Chunks.toolCall(index: 0, id: "call_1", name: "f", args: #"{"a":"#)], done: false)),
            ("cut line", .rawStream("data: " + Chunks.content("par") + "\n\ndata: {\"choi")),
            ("error frame", .sse([Chunks.content("par"), #"{"error":{"message":"engine died at 10.0.0.5","code":500}}"#])),
        ]
        for (name, reply) in cases {
            try await CoreHarness.run(backend: { _, _ in reply }) { h in
                let r = try await h.responses(responsesBody(extra: #""tools":[{"type":"function","name":"f"}]"#))
                let ev = r.events
                #expect(r.status == 200)
                #expect(ev.names.last == "response.failed", "\(name): \(ev.names)")
                #expect(!ev.names.contains("response.completed"), "\(name)")
                let failed = ev.data("response.failed").first?["response"]
                #expect(failed?["status"]?.string == "failed" && failed?["error"]?["message"]?.string?.isEmpty == false, "\(name)")
                #expect(failed?["error"]?["code"]?.string?.isEmpty == false)
                #expect(!r.text.contains("10.0.0.5"))
                // Only items that finished before the failure are reported.
                #expect(failed?["output"]?.array?.allSatisfy { $0["status"]?.string == "completed" } == true, "\(name)")
                #expect(h.log.requests.last?.status == 502, "\(name)")
            }
        }
    }

    @Test func aStreamThatGoesQuietGetsCommentKeepalives() async throws {
        var tuning = EngineTuning()
        tuning.pingInterval = .milliseconds(25)
        try await CoreHarness.run(tuning: tuning, backend: { _, _ in
            .sse([Chunks.content("slow"), Chunks.finish()], every: .milliseconds(150))
        }) { h in
            let r = try await h.responses(responsesBody())
            #expect(r.text.contains(": keepalive\n\n"))
            #expect(!r.text.contains("event: ping"))
            #expect(r.events.names.last == "response.completed")
        }
    }

    @Test func clientDisconnectCancelsTheUpstream() async throws {
        try await CoreHarness.run(backend: { _, _ in .endless(every: .milliseconds(20)) }) { h in
            var r = URLRequest(url: h.url("/v1/responses"))
            r.httpMethod = "POST"
            r.httpBody = Data(responsesBody().utf8)
            r.setValue("Bearer \(CoreHarness.token)", forHTTPHeaderField: "Authorization")
            let reader = Task {
                let (bytes, _) = try await h.session.bytes(for: r)
                var n = 0
                for try await line in bytes.lines where line.contains("output_text.delta") && line.hasPrefix("event:") {
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

    // MARK: non-streaming

    @Test func nonStreamingResponse() async throws {
        let calls = #"[{"id":"call_n","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]"#
        try await CoreHarness.run(backend: { _, _ in
            .json(200, Chunks.completion(content: "<think>hmm</think>done", reasoning: nil, toolCalls: calls, finish: "tool_calls",
                                         prompt: 30, completion: 8, cached: 10))
        }) { h in
            let r = try await h.responses(responsesBody(stream: false, extra: #""tools":[{"type":"function","name":"f"}]"#))
            #expect(r.status == 200 && r.headers["content-type"]?.contains("json") == true)
            let j = try #require(r.json)
            #expect(j["object"]?.string == "response" && j["status"]?.string == "completed" && j["model"]?.string == "GLM")
            #expect(j["output"]?.array?.compactMap { $0.type } == ["reasoning", "message", "function_call"])
            #expect(j["output"]?[0]?["content"]?[0]?["text"]?.string == "hmm" && j["output_text"]?.string == "done")
            #expect(j["output"]?[2]?["name"]?.string == "f" && j["output"]?[2]?["call_id"]?.string == "call_n")
            #expect(j["usage"]?["input_tokens"] == 30 && j["usage"]?["input_tokens_details"]?["cached_tokens"] == 10)
            #expect(h.backend.lastChat?["stream"] == false && h.backend.lastChat?["stream_options"] == nil)
        }
    }

    @Test func nonStreamingLengthIsIncomplete() async throws {
        try await CoreHarness.run(backend: { _, _ in .json(200, Chunks.completion(content: "cut", finish: "length")) }) { h in
            let j = try await h.responses(responsesBody(stream: false)).json
            #expect(j?["status"]?.string == "incomplete" && j?["incomplete_details"]?["reason"]?.string == "max_output_tokens")
        }
    }

    @Test func aBackendThatIgnoresStreamStillAnswersAStream() async throws {
        try await CoreHarness.run(backend: { _, _ in .json(200, Chunks.completion(content: "plain", reasoning: "r")) }) { h in
            let ev = try await h.responses(responsesBody()).events
            #expect(ev.names.last == "response.completed")
            #expect(completedOutput(ev).compactMap { $0.type } == ["reasoning", "message"])
            #expect(ev.data("response.completed").first?["response"]?["output_text"]?.string == "plain")
        }
    }

    // MARK: structured output

    @Test func fencedJSONIsCleaned() async throws {
        let fenced = "```json\n{\"outcome\":\"deny\"}\n```"
        for stream in [true, false] {
            try await CoreHarness.run(backend: { _, _ in
                stream ? .sse([Chunks.content(String(fenced.prefix(12))), Chunks.content(String(fenced.dropFirst(12))), Chunks.finish()])
                       : .json(200, Chunks.completion(content: fenced))
            }) { h in
                let r = try await h.responses(try codexFixture("auto_review") { $0["stream"] = .bool(stream) })
                let text = stream ? r.events.data("response.completed").first?["response"]?["output_text"]?.string : r.json?["output_text"]?.string
                #expect(text == #"{"outcome":"deny"}"#, "stream=\(stream)")
                if stream {
                    #expect(r.events.data("response.output_text.delta").map { $0["delta"]?.string ?? "" }.joined() == #"{"outcome":"deny"}"#)
                }
            }
        }
        #expect(ResponsesStreamTranslator.cleanStructuredOutput(#"Sure: {"a":1} done"#) == #"{"a":1}"#)
        #expect(ResponsesStreamTranslator.cleanStructuredOutput("not json") == "not json")
    }

    // MARK: errors before output

    @Test func errorsBeforeOutputAreRealHTTPErrors() async throws {
        let cases: [(Int, String, [String: String], Int, String)] = [
            (400, #"{"error":{"message":"This model's maximum context length is 131072 tokens. However, you requested 140000 tokens. See http://203.0.113.3:8000/help","type":"BadRequestError","code":400}}"#,
             [:], 400, "context_length_exceeded"),
            (400, #"{"error":{"message":"bad schema"}}"#, [:], 400, "invalid_prompt"),
            (422, #"{"detail":"nope"}"#, [:], 400, "invalid_prompt"),
            (401, "{}", [:], 502, "upstream_auth_error"),
            (403, "{}", [:], 502, "upstream_auth_error"),
            (429, "{}", ["retry-after": "7"], 429, "rate_limit_exceeded"),
            (503, "{}", ["retry-after": "3"], 503, "server_is_overloaded"),
            (500, "secret internals at 10.1.2.3", [:], 500, "upstream_error"),
        ]
        for stream in [true, false] {
            for (status, body, headers, want, code) in cases {
                try await CoreHarness.run(backend: { _, _ in .json(status, body, headers: headers) }) { h in
                    let r = try await h.responses(responsesBody(stream: stream))
                    #expect(r.status == want, "\(status) stream=\(stream)")
                    #expect(r.headers["content-type"]?.contains("event-stream") != true)
                    #expect(r.json?["error"]?["code"]?.string == code, "\(status): \(r.text)")
                    #expect(r.json?["error"]?["message"]?.string?.isEmpty == false)
                    #expect(!r.text.contains("203.0.113.3") && !r.text.contains("secret") && !r.text.contains("10.1.2.3"))
                    if let retry = headers["retry-after"] { #expect(r.headers["retry-after"] == retry) }
                    #expect(h.log.requests.last?.status == want)
                }
            }
        }
        try await CoreHarness.run(backend: { _, _ in .json(400, cases[0].1) }) { h in
            let r = try await h.responses(responsesBody())
            #expect(r.json?["error"]?["message"]?.string?.contains("131072") == true)
            #expect(r.json?["error"]?["type"]?.string == "invalid_request_error")
        }
    }

    @Test func unreachableBackendIs502AndSlowOneIs504() async throws {
        try await CoreHarness.run(configure: { $0.backends[0].baseURL = URL(string: "http://127.0.0.1:1/v1")! }, backend: okStream) { h in
            let r = try await h.responses(responsesBody())
            #expect(r.status == 502 && r.json?["error"]?["code"]?.string == "upstream_unreachable")
        }
        try await CoreHarness.run(configure: { $0.headerTimeoutSeconds = 0.3 },
                                  backend: { _, _ in .delayed(.seconds(3), .json(200, Chunks.completion(content: "late"))) }) { h in
            let r = try await h.responses(responsesBody(stream: false))
            #expect(r.status == 504 && r.json?["error"]?["code"]?.string == "upstream_timeout")
        }
    }

    // MARK: documents and images

    @Test func inputFilePDFsGoThroughTheDocumentReader() async throws {
        let reader = CoreDocumentReader()
        let pdf = "JVBERi0xLjQK"
        let input = #"""
        [{"role":"user","content":[
          {"type":"input_text","text":"read these"},
          {"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,\#(pdf)"},
          {"type":"input_file","filename":"b.pdf","file_data":"\#(pdf)"},
          {"type":"input_file","filename":"c.txt","file_data":"data:text/plain;base64,aGkgdGhlcmU="},
          {"type":"input_file","file_id":"file-123"},
          {"type":"input_image","image_url":"data:application/pdf;base64,\#(pdf)"}]}]
        """#
        try await CoreHarness.run(hooks: EngineHooks(documentReader: reader), backend: okStream) { h in
            let r = try await h.responses(responsesBody(input: input))
            #expect(r.status == 200)
            let content = try #require(h.backend.lastChat?["messages"]?[0]?["content"]?.array)
            let texts = content.map { $0["text"]?.string ?? "" }
            #expect(content.allSatisfy { $0.type == "text" })
            #expect(texts[0] == "read these")
            #expect(texts[1] == "[document: a.pdf]\nEXTRACTED 9 bytes of application/pdf")
            #expect(texts[2] == "[document: b.pdf]\nEXTRACTED 9 bytes of application/pdf")
            #expect(texts[3] == "[document: c.txt]\nhi there")
            #expect(texts[4].contains("could not be read") && texts[4].contains("unnamed file"))
            #expect(texts[5].hasPrefix("[document: document]\nEXTRACTED"))
            // The same PDF is read once per distinct name.
            #expect(reader.inputs.count == 3)
            #expect(reader.inputs.allSatisfy { $0.mediaType == "application/pdf" })
            #expect(h.log.all.contains { if case .document(let n, _, _) = $0 { return n == "a.pdf" } else { return false } })
        }
    }

    @Test func documentsWithoutAReaderAreNotedNotDropped() async throws {
        let input = #"[{"role":"user","content":[{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0="}]}]"#
        try await CoreHarness.run(backend: okStream) { h in
            _ = try await h.responses(responsesBody(input: input))
            let text = h.backend.lastChat?["messages"]?[0]?["content"]?.string
            #expect(text == "[document: a.pdf could not be read (document reading is not available)]")
        }
    }

    @Test func imagesForAModelWithoutVisionAreDescribed() async throws {
        let png = Data(repeating: 7, count: 64).base64EncodedString()
        let input = #"""
        [{"role":"user","content":[{"type":"input_text","text":"what is this"},
           {"type":"input_image","image_url":"data:image/png;base64,\#(png)"}]},
         {"type":"function_call","call_id":"c1","name":"view_image","arguments":"{}"},
         {"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"data:image/png;base64,\#(png)"}]}]
        """#
        let describer = CoreImageDescriber()
        try await CoreHarness.run(hooks: EngineHooks(imageDescriber: describer), configure: { $0.roles.vision = "Big"; $0.models[0].vision = false }, backend: okStream) { h in
            _ = try await h.responses(responsesBody(input: input))
            let msgs = h.backend.lastChat?["messages"]?.array ?? []
            #expect(msgs[0]["content"]?[0]?["text"]?.string == "what is this")
            #expect(msgs[0]["content"]?[1]?["text"]?.string == "[image: a cat on a mat]")
            #expect(msgs[0]["content"]?[1]?.type == "text")
            // A tool's image rides in a user message after the tool message.
            #expect(msgs.map { $0["role"]?.string ?? "" } == ["user", "assistant", "tool", "user"])
            #expect(msgs[2]["content"]?.string == "[the tool returned attachments; they follow in the next message]")
            #expect(msgs[3]["content"]?[0]?["text"]?.string == "[attachments from tool result c1]")
            #expect(msgs[3]["content"]?[1]?["text"]?.string == "[image: a cat on a mat]")
            #expect(describer.calls == 1) // the same image is described once
        }
    }

    @Test func imagesWithoutAnyVisionAreOmittedWithANote() async throws {
        let input = #"[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]"#
        try await CoreHarness.run(configure: { $0.models[0].vision = false }, backend: okStream) { h in
            _ = try await h.responses(responsesBody(input: input))
            #expect(h.backend.lastChat?["messages"]?[0]?["content"]?.string
                    == "[image omitted: GLM has no vision and no vision model is configured]")
        }
    }

    @Test func imagesStayForAModelWithVision() async throws {
        let input = #"[{"role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,AAAA","detail":"high"}]}]"#
        let describer = CoreImageDescriber()
        try await CoreHarness.run(hooks: EngineHooks(imageDescriber: describer), configure: { $0.models[0].vision = true; $0.roles.vision = "Big" },
                                  backend: okStream) { h in
            _ = try await h.responses(responsesBody(input: input))
            let parts = h.backend.lastChat?["messages"]?[0]?["content"]?.array ?? []
            #expect(parts.map { $0.type ?? "" } == ["text", "image_url"])
            #expect(parts[1]["image_url"]?["url"]?.string == "data:image/png;base64,AAAA" && parts[1]["image_url"]?["detail"]?.string == "high")
            #expect(describer.calls == 0)
        }
    }

    // MARK: web search

    private static let searchCall = Chunks.toolCall(index: 0, id: "call_s", name: "web_search", args: #"{"query":"swift testing"}"#)

    @Test func webSearchLoopStreamsSearchCallItems() async throws {
        let searcher = CoreSearcher()
        try await CoreHarness.run(hooks: EngineHooks(webSearcher: searcher), backend: { _, n in
            n == 0 ? .sse([Chunks.content("Let me look. "), Self.searchCall, Chunks.finish("tool_calls")])
                   : .sse([Chunks.content("Found it."), Chunks.finish(), Chunks.usage(prompt: 20, completion: 3)])
        }) { h in
            let body = try codexFixture("main_turn")
            let r = try await h.responses(body)
            #expect(r.status == 200)
            let ev = r.events
            #expect(ev.names.last == "response.completed")
            #expect(searcher.queries == ["swift testing"])
            #expect(h.backend.chatRequests.count == 2)
            // The model was offered the function, and got the results back.
            let first = try #require(h.backend.chatRequests.first?.json)
            #expect((first["tools"]?.array ?? []).contains { $0["function"]?["name"]?.string == "web_search" })
            let second = try #require(h.backend.lastChat)
            let msgs = second["messages"]?.array ?? []
            #expect(msgs.suffix(2).map { $0["role"]?.string ?? "" } == ["assistant", "tool"])
            #expect(msgs[msgs.count - 2]["tool_calls"]?[0]?["function"]?["name"]?.string == "web_search")
            #expect(msgs[msgs.count - 2]["content"]?.string == "Let me look. ")
            #expect(msgs.last?["content"]?.string?.contains("Result for swift testing") == true)

            let out = completedOutput(ev)
            #expect(out.compactMap { $0.type } == ["message", "web_search_call", "message"])
            let ws = out[1]
            #expect(ws["status"]?.string == "completed" && ws["action"]?["query"]?.string == "swift testing")
            #expect(ws["sources"]?[0]?["type"]?.string == "url_citation" && ws["sources"]?[0]?["url"]?.string?.hasPrefix("https://example.com") == true)
            #expect(ev.names.contains("response.web_search_call.searching"))
            #expect(ev.data("response.output_item.added").compactMap { $0["output_index"]?.int } == [0, 1, 2])
            #expect(ev.data("response.output_item.done").compactMap { $0["output_index"]?.int } == [0, 1, 2])
            // No function_call item for the search itself.
            #expect(!out.contains { $0.type == "function_call" })
            #expect(ev.data("response.completed").first?["response"]?["usage"]?["input_tokens"] == 20)
            #expect(h.log.all.contains { if case .webSearch(let q, _) = $0 { return q == "swift testing" } else { return false } })
        }
    }

    @Test func webSearchLoopNonStreaming() async throws {
        let searcher = CoreSearcher()
        let calls = #"[{"id":"call_s","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"q\"}"}}]"#
        try await CoreHarness.run(hooks: EngineHooks(webSearcher: searcher), backend: { _, n in
            n == 0 ? .json(200, Chunks.completion(content: nil, toolCalls: calls, finish: "tool_calls", prompt: 10, completion: 2))
                   : .json(200, Chunks.completion(content: "Answer.", prompt: 30, completion: 4))
        }) { h in
            let r = try await h.responses(responsesBody(stream: false, extra: #""tools":[{"type":"web_search"}]"#))
            let j = try #require(r.json)
            #expect(j["output"]?.array?.compactMap { $0.type } == ["web_search_call", "message"])
            #expect(j["output_text"]?.string == "Answer.")
            #expect(j["usage"]?["input_tokens"] == 40 && j["usage"]?["output_tokens"] == 6)
            #expect(searcher.queries == ["q"])
        }
    }

    @Test func webSearchWithoutAProviderTellsTheModel() async throws {
        try await CoreHarness.run(backend: { _, n in
            n == 0 ? .sse([Self.searchCall, Chunks.finish("tool_calls")]) : .sse([Chunks.content("I could not search."), Chunks.finish()])
        }) { h in
            let r = try await h.responses(responsesBody(extra: #""tools":[{"type":"web_search"}]"#))
            let ev = r.events
            #expect(ev.names.last == "response.completed")
            #expect(completedOutput(ev).first?["status"]?.string == "failed")
            #expect(h.backend.lastChat?["messages"]?.array?.last?["content"]?.string?.contains("unavailable") == true)
        }
    }

    @Test func aClientFunctionNamedWebSearchWins() async throws {
        let searcher = CoreSearcher()
        try await CoreHarness.run(hooks: EngineHooks(webSearcher: searcher), backend: { _, _ in
            .sse([Self.searchCall, Chunks.finish("tool_calls")])
        }) { h in
            let r = try await h.responses(responsesBody(extra: #""tools":[{"type":"function","name":"web_search"},{"type":"web_search"}]"#))
            #expect(searcher.queries.isEmpty)
            #expect(completedOutput(r.events).first?.type == "function_call")
            #expect(h.backend.chatRequests.count == 1)
        }
    }
}
