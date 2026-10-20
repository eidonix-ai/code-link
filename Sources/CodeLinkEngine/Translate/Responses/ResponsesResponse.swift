import Foundation

/// A complete (non-streaming) Chat Completions response as Responses output
/// (docs/DESIGN.md §5).
enum ResponsesResponse {
    /// One generation's output items.
    struct Round {
        var items: [JSON] = []
        var text = ""
        /// The model's web_search calls (search mode), held back from the client.
        var searchCalls: [(id: String, args: String)] = []
        var finishReason: String?
        var usage = ChatUsage()
    }

    /// The items of a chat completion: reasoning, the answer, then the tool
    /// calls — in the order the stream emits them. nil when it has no choice.
    static func round(_ chat: JSON, registry: ToolRegistry, jsonMode: Bool, searchMode: Bool) -> Round? {
        guard let choice = chat["choices"]?[0] else { return nil }
        let msg = choice["message"] ?? [:]
        var round = Round()
        round.finishReason = choice["finish_reason"]?.string
        round.usage = ChatUsage(chat["usage"]) ?? ChatUsage()

        var reasoning = msg["reasoning_content"]?.string ?? ""
        if reasoning.isEmpty { reasoning = msg["reasoning"]?.string ?? "" }
        var text = ""
        if let c = msg["content"]?.string, !c.isEmpty {
            let (think, rest) = ThinkTagFilter.strip(c)
            text = rest
            if reasoning.isEmpty { reasoning = think }
        }
        let calls = msg["tool_calls"]?.array ?? []
        if jsonMode, calls.isEmpty { text = ResponsesStreamTranslator.cleanStructuredOutput(text) }

        if !reasoning.isEmpty { round.items.append(ResponsesStreamTranslator.reasoningItem(id: IDs.make("rs_"), text: reasoning)) }
        if !text.isEmpty {
            round.text = text
            round.items.append(["id": .string(IDs.make("msg_")), "type": "message", "role": "assistant", "status": "completed",
                                "content": [["type": "output_text", "text": .string(text), "annotations": []]]])
        }
        for tc in calls {
            let name = tc["function"]?["name"]?.string ?? ""
            let args = tc["function"]?["arguments"]?.string ?? ""
            let id = tc["id"]?.string.flatMap { $0.isEmpty ? nil : $0 } ?? IDs.make("call_")
            if searchMode, name == WebSearchTool.functionName {
                round.searchCalls.append((id, args))
                continue
            }
            round.items.append(registry.outputItem(itemID: IDs.make("fc_"), callID: id, flatName: name,
                                                   args: args.isEmpty ? "{}" : args, inProgress: false))
        }
        return round
    }

    /// The `web_search_call` item of a search the engine ran.
    static func webSearchItem(_ o: SearchOutcome) -> JSON {
        var item: JSON = ["id": .string(IDs.make("ws_")), "type": "web_search_call",
                          "status": o.errorCode == nil ? "completed" : "failed",
                          "action": ["type": "search", "query": .string(o.query)]]
        if !o.hits.isEmpty {
            item["sources"] = .array(o.hits.map { ["type": "url_citation", "url": .string($0.url), "title": .string($0.title)] })
        }
        return item
    }

    /// The response object of a finished generation (or a search loop's).
    static func response(model: String, items: [JSON], text: String, finishReason: String?, usage: ChatUsage) -> JSON {
        var status = "completed"
        var incomplete: JSON = nil
        var error: JSON = nil
        switch finishReason {
        case "length":
            status = "incomplete"
            incomplete = ["reason": "max_output_tokens"]
        case "content_filter":
            status = "failed"
            error = ["code": "content_filter", "message": "the response was blocked by the backend's content filter"]
        default:
            break
        }
        return ["id": .string(IDs.make("resp_")), "object": "response", "created_at": .int(Int(Date().timeIntervalSince1970)),
                "model": .string(model), "status": .string(status), "error": error, "incomplete_details": incomplete,
                "output": .array(items), "output_text": .string(text), "usage": usage.responses]
    }

    /// A completion as one stream chunk, for a backend that answered a
    /// streaming request with a plain JSON body.
    static func chunkLine(_ chat: JSON) -> String? {
        guard let choice = chat["choices"]?[0] else { return nil }
        let msg = choice["message"] ?? [:]
        var delta = JSONObject([("role", "assistant")])
        for k in ["content", "reasoning_content", "reasoning"] { if let v = msg[k], !v.isNull { delta[k] = v } }
        if let calls = msg["tool_calls"]?.array, !calls.isEmpty {
            delta["tool_calls"] = .array(calls.enumerated().map { i, c in
                var call = c
                call["index"] = .int(i)
                return call
            })
        }
        var out: JSON = ["choices": [["index": 0, "delta": .object(delta), "finish_reason": choice["finish_reason"] ?? "stop"]]]
        if let u = chat["usage"] { out["usage"] = u }
        return "data: " + out.text
    }
}
