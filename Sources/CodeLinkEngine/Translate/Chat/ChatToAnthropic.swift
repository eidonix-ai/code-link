import Foundation

/// Chat Completions ↔ Anthropic Messages, for a Chat client on an Anthropic
/// backend (docs/DESIGN.md §5a).
enum ChatToAnthropic {
    static let defaultMaxTokens = 4096

    // MARK: request

    /// The Messages request for a Chat Completions request, or nil when it has
    /// no user or assistant message to send.
    static func request(_ chat: JSON, upstreamModel: String) -> JSON? {
        var system: [String] = []
        var messages: [JSON] = []
        var toolResults: [JSON] = []
        func flushResults() {
            guard !toolResults.isEmpty else { return }
            messages.append(["role": "user", "content": .array(toolResults)])
            toolResults = []
        }
        for m in chat["messages"]?.array ?? [] {
            switch m["role"]?.string {
            case "system", "developer":
                flushResults()
                let t = text(m["content"])
                if !t.isEmpty { system.append(t) }
            case "user":
                flushResults()
                let blocks = userBlocks(m["content"])
                if !blocks.isEmpty { messages.append(["role": "user", "content": .array(blocks)]) }
            case "assistant":
                flushResults()
                let blocks = assistantBlocks(m)
                if !blocks.isEmpty { messages.append(["role": "assistant", "content": .array(blocks)]) }
            case "tool":
                guard let id = m["tool_call_id"]?.string, !id.isEmpty else { continue }
                toolResults.append(["type": "tool_result", "tool_use_id": .string(id), "content": .string(toolText(m["content"]))])
            default:
                continue
            }
        }
        flushResults()
        guard !messages.isEmpty else { return nil }

        var out = JSONObject()
        out["model"] = .string(upstreamModel)
        out["messages"] = .array(messages)
        let maxTokens = [chat["max_completion_tokens"]?.int, chat["max_tokens"]?.int].compactMap { $0 }.first { $0 > 0 }
        out["max_tokens"] = .int(maxTokens ?? defaultMaxTokens)
        out["stream"] = .bool(chat["stream"]?.bool ?? false)
        if !system.isEmpty { out["system"] = .string(system.joined(separator: "\n")) }
        for k in ["temperature", "top_p", "top_k"] { if let v = chat[k], !v.isNull { out[k] = v } }
        if let stop = chat["stop"] {
            let stops = stop.string.map { [$0] } ?? (stop.array ?? []).compactMap(\.string)
            if !stops.isEmpty { out["stop_sequences"] = .array(stops.map { .string($0) }) }
        }
        if let user = chat["user"]?.string, !user.isEmpty { out["metadata"] = ["user_id": .string(user)] }
        let tools = self.tools(chat["tools"]?.array ?? [])
        if !tools.isEmpty {
            out["tools"] = .array(tools)
            if let choice = toolChoice(chat["tool_choice"], parallel: chat["parallel_tool_calls"]?.bool) { out["tool_choice"] = choice }
        }
        return .object(out)
    }

    /// Message content as plain text: a string, or its text parts joined.
    private static func text(_ content: JSON?) -> String {
        guard let content, !content.isNull else { return "" }
        if let s = content.string { return s }
        return (content.array ?? []).compactMap { p -> String? in
            p.type == "text" || p.type == "input_text" ? p["text"]?.string : nil
        }.joined()
    }

    private static func toolText(_ content: JSON?) -> String {
        guard let content, !content.isNull else { return "" }
        if let s = content.string { return s }
        let t = text(content)
        return t.isEmpty ? content.text : t
    }

    private static func userBlocks(_ content: JSON?) -> [JSON] {
        guard let content, !content.isNull else { return [] }
        if let s = content.string { return s.isEmpty ? [] : [["type": "text", "text": .string(s)]] }
        var out: [JSON] = []
        for p in content.array ?? [] {
            var block: JSON?
            switch p.type {
            case "text", "input_text":
                if let t = p["text"]?.string, !t.isEmpty { block = ["type": "text", "text": .string(t)] }
            case "image_url", "input_image":
                block = image(p)
            default:
                break
            }
            guard var b = block else { continue }
            if let cc = p["cache_control"], !cc.isNull { b["cache_control"] = cc }
            out.append(b)
        }
        return out
    }

    private static func image(_ p: JSON) -> JSON? {
        let raw = p["image_url"] ?? p["url"]
        let url = raw?.string ?? raw?["url"]?.string ?? ""
        guard !url.isEmpty else { return nil }
        if url.hasPrefix("data:") {
            guard let (mediaType, data) = OpenAIMedia.dataURL(url) else { return nil }
            return ["type": "image", "source": ["type": "base64", "media_type": .string(mediaType), "data": .string(data)]]
        }
        return ["type": "image", "source": ["type": "url", "url": .string(url)]]
    }

    private static func assistantBlocks(_ m: JSON) -> [JSON] {
        var out: [JSON] = []
        let t = text(m["content"])
        if !t.isEmpty { out.append(["type": "text", "text": .string(t)]) }
        for call in m["tool_calls"]?.array ?? [] {
            guard let name = call["function"]?["name"]?.string, !name.isEmpty else { continue }
            var input: JSON = [:]
            if let args = call["function"]?["arguments"]?.string, !args.isEmpty, let parsed = try? JSON.parse(args) { input = parsed }
            out.append(["type": "tool_use", "id": call["id"] ?? "", "name": .string(name), "input": input])
        }
        return out
    }

    private static func tools(_ list: [JSON]) -> [JSON] {
        list.compactMap { t in
            guard t.type == nil || t.type == "function", let fn = t["function"], let name = fn["name"]?.string, !name.isEmpty else { return nil }
            var out: JSON = ["name": .string(name), "input_schema": ["type": "object", "properties": [:]]]
            if let d = fn["description"]?.string, !d.isEmpty { out["description"] = .string(d) }
            if let p = fn["parameters"], !p.isNull { out["input_schema"] = p }
            return out
        }
    }

    private static func toolChoice(_ tc: JSON?, parallel: Bool?) -> JSON? {
        var choice = JSONObject()
        if let s = tc?.string {
            switch s {
            case "auto", "none": choice["type"] = .string(s)
            case "required": choice["type"] = "any"
            default: break
            }
        } else if tc?.type == "function", let name = tc?["function"]?["name"]?.string, !name.isEmpty {
            choice["type"] = "tool"
            choice["name"] = .string(name)
        }
        if parallel == false {
            if choice.isEmpty { choice["type"] = "auto" }
            choice["disable_parallel_tool_use"] = true
        }
        return choice.isEmpty ? nil : .object(choice)
    }

    // MARK: response

    /// An Anthropic stop_reason as a Chat finish_reason.
    static func finishReason(_ stop: String?) -> String {
        switch stop {
        case "tool_use": return "tool_calls"
        case "max_tokens", "model_context_window_exceeded": return "length"
        case "refusal": return "content_filter"
        default: return "stop"
        }
    }

    /// Chat usage for Anthropic usage: the prompt is the input plus every
    /// cached token, and the cache reads are the cached ones.
    static func usage(_ u: JSON?) -> JSON {
        let input = (u?["input_tokens"]?.int ?? 0) + (u?["cache_creation_input_tokens"]?.int ?? 0) + (u?["cache_read_input_tokens"]?.int ?? 0)
        let output = u?["output_tokens"]?.int ?? 0
        var out: JSON = ["prompt_tokens": .int(input), "completion_tokens": .int(output), "total_tokens": .int(input + output)]
        if let cached = u?["cache_read_input_tokens"]?.int, cached > 0 { out["prompt_tokens_details"] = ["cached_tokens": .int(cached)] }
        return out
    }

    /// A non-streaming Anthropic message as a Chat Completions response.
    static func response(_ message: JSON, model: String) -> JSON? {
        guard message["content"]?.array != nil || message["type"]?.string == "message" else { return nil }
        var texts: [String] = [], thinking: [String] = [], calls: [JSON] = []
        for b in message["content"]?.array ?? [] {
            switch b.type {
            case "text": if let t = b["text"]?.string { texts.append(t) }
            case "thinking": if let t = b["thinking"]?.string, !t.isEmpty { thinking.append(t) }
            case "tool_use":
                let input = b["input"].flatMap { $0.isNull ? nil : $0 } ?? [:]
                calls.append(["id": b["id"] ?? "", "type": "function",
                              "function": ["name": b["name"] ?? "", "arguments": .string(input.text)]])
            default: continue
            }
        }
        var msg = JSONObject([("role", "assistant")])
        msg["content"] = texts.isEmpty ? .null : .string(texts.joined())
        if !thinking.isEmpty { msg["reasoning_content"] = .string(thinking.joined()) }
        if !calls.isEmpty { msg["tool_calls"] = .array(calls) }
        let id = message["id"]?.string.flatMap { $0.hasPrefix("chatcmpl-") ? $0 : nil } ?? IDs.make("chatcmpl-")
        return ["id": .string(id), "object": "chat.completion", "created": .int(Int(Date().timeIntervalSince1970)),
                "model": .string(model),
                "choices": [["index": 0, "message": .object(msg), "finish_reason": .string(finishReason(message["stop_reason"]?.string))]],
                "usage": usage(message["usage"])]
    }
}
