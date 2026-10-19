import Foundation

/// Text that came from an attachment (a document read, an image described)
/// inside a tool_result: it rides in the user message after the tool
/// messages, like the media it replaces (MediaPreprocessor writes it).
let attachmentTextType = "_code_link_attachment_text"

/// The `web_search_*` server tool of a request (docs/DESIGN.md §4.5).
struct WebSearchTool: Sendable, Equatable {
    static let functionName = "web_search"
    static let defaultMaxUses = 5

    var maxUses: Int
    var allowedDomains: [String]?
    var blockedDomains: [String]?

    static func find(in tools: [JSON]?) -> WebSearchTool? {
        for t in tools ?? [] {
            guard let type = t.type, type.hasPrefix("web_search_") else { continue }
            let maxUses = t["max_uses"]?.int ?? 0
            return WebSearchTool(maxUses: maxUses > 0 ? maxUses : defaultMaxUses,
                                 allowedDomains: t["allowed_domains"]?.array?.compactMap(\.string),
                                 blockedDomains: t["blocked_domains"]?.array?.compactMap(\.string))
        }
        return nil
    }

    var function: JSON {
        ["type": "function", "function": [
            "name": .string(Self.functionName),
            "description": "Search the web for current information. Returns the top results with title, URL and a snippet. Use it for anything that may have changed or that you do not know; you can search again with a better query.",
            "parameters": ["type": "object",
                           "properties": ["query": ["type": "string", "description": "The search query"]],
                           "required": ["query"]],
        ]]
    }
}

/// Anthropic Messages → Chat Completions (docs/DESIGN.md §4.1). The request
/// has been through MediaPreprocessor: documents are text, and images are
/// left only for a model with vision.
enum RequestTranslator {
    /// The Chat Completions body (without `model`) for an Anthropic request.
    static func chatRequest(_ req: JSON, model: Model, contextWindow: Int?, webSearch: WebSearchTool?) -> JSON {
        var out = JSONObject()
        var messages: [JSON] = []
        let sys = system(req["system"])
        if !sys.isEmpty { messages.append(["role": "system", "content": .string(sys)]) }
        messages += translateMessages(req["messages"]?.array ?? [])
        out["messages"] = .array(messages)
        let stream = req["stream"]?.bool ?? false
        out["stream"] = .bool(stream)
        if stream { out["stream_options"] = ["include_usage": true] }

        var tools = translateTools(req["tools"]?.array ?? [], loaded: referencedTools(req["messages"]?.array ?? []))
        if let webSearch { tools.append(webSearch.function) }
        if !tools.isEmpty {
            out["tools"] = .array(tools)
            if let tc = toolChoice(req["tool_choice"]) { out["tool_choice"] = tc }
            if req["tool_choice"]?["disable_parallel_tool_use"]?.bool == true { out["parallel_tool_calls"] = false }
        }
        if let n = req["max_tokens"]?.int, n > 0 { out["max_tokens"] = .int(n) }
        if let stops = req["stop_sequences"]?.array, !stops.isEmpty { out["stop"] = .array(stops) }
        for k in ["temperature", "top_p", "top_k"] { if let v = req[k], !v.isNull { out[k] = v } }

        let thinkingOff = req["thinking"]?["type"]?.string == "disabled"
        let effort = outputEffort(req["output_config"])
        if let effort, !thinkingOff { out["reasoning_effort"] = .string(effort) }
        if let rf = responseFormat(req["output_config"]?["format"] ?? req["output_format"]) { out["response_format"] = rf }
        if let user = req["metadata"]?["user_id"]?.string, !user.isEmpty { out["user"] = .string(user) }

        var result = JSON.object(out)
        if thinkingOff, let off = model.thinkingOff { result = deepMerge(result, JSON(off)) }
        if let contextWindow { clampMaxTokens(&result, contextWindow: contextWindow) }
        return result
    }

    // MARK: system

    /// The top-level system field (a string or text blocks), joined in order.
    static func system(_ s: JSON?) -> String {
        guard let s else { return "" }
        if let str = s.string { return str }
        return (s.array ?? []).compactMap { $0["text"]?.string }.filter { !$0.isEmpty }.joined(separator: "\n")
    }

    // MARK: messages

    static func translateMessages(_ msgs: [JSON]) -> [JSON] {
        var out: [JSON] = []
        for m in msgs {
            switch m["role"]?.string {
            case "user": out += userMessage(m["content"])
            case "assistant": out += assistantMessage(m["content"])
            case "system":
                // Many templates refuse a second system message.
                let text = system(m["content"])
                if !text.isEmpty { out.append(["role": "user", "content": .string("<system-reminder>\n" + text + "\n</system-reminder>")]) }
            default: continue
            }
        }
        return out
    }

    static func textPart(_ s: String) -> JSON { ["type": "text", "text": .string(s)] }

    /// One Anthropic user message: its tool_result blocks as `tool` messages
    /// FIRST (they must follow the assistant message that called them), then
    /// one user message with tool attachments followed by the user's parts.
    static func userMessage(_ content: JSON?) -> [JSON] {
        guard let content else { return [] }
        if let s = content.string { return [["role": "user", "content": .string(s)]] }
        guard let blocks = content.array else { return [["role": "user", "content": .string(content.text)]] }
        var toolMessages: [JSON] = []
        var toolMedia: [JSON] = []
        var parts: [JSON] = []
        for b in blocks {
            switch b.type {
            case "text", attachmentTextType:
                parts.append(textPart(b["text"]?.string ?? ""))
            case "image":
                parts.append(imagePart(b))
            case "document":
                // Not read by MediaPreprocessor (unknown source): a note.
                parts.append(textPart(documentNote(b, why: "unsupported document source")))
            case "tool_result":
                let id = b["tool_use_id"]?.string ?? ""
                var (text, media) = splitToolResult(b["content"])
                if !media.isEmpty {
                    if text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                        text = "[the tool returned attachments; they follow in the next message]"
                    }
                    toolMedia.append(textPart("[attachments from tool result \(id)]"))
                    toolMedia += media
                }
                if b["is_error"]?.bool == true { text = "[tool error] " + text }
                toolMessages.append(["role": "tool", "tool_call_id": .string(id), "content": .string(text)])
            case "web_search_tool_result":
                let results = b["content"]?.array ?? []
                let lines = results.compactMap { r -> String? in
                    let t = r["title"]?.string ?? "", u = r["url"]?.string ?? ""
                    return t.isEmpty && u.isEmpty ? nil : "\n- \(t) (\(u))"
                }
                if !lines.isEmpty { parts.append(textPart("[Web search results:" + lines.joined() + "]")) }
            case "search_result":
                if let t = searchResultText(b) { parts.append(textPart(t)) }
            default:
                continue // thinking, redacted_thinking, unknown
            }
        }
        var out = toolMessages
        let all = toolMedia + parts
        if all.count == 1, all[0].type == "text" {
            out.append(["role": "user", "content": all[0]["text"] ?? ""])
        } else if !all.isEmpty {
            out.append(["role": "user", "content": .array(all)])
        }
        return out
    }

    static func imagePart(_ b: JSON) -> JSON {
        let src = b["source"]
        switch src?.type {
        case "base64":
            let url = "data:" + (src?["media_type"]?.string ?? "image/png") + ";base64," + (src?["data"]?.string ?? "")
            return ["type": "image_url", "image_url": ["url": .string(url)]]
        case "url":
            return ["type": "image_url", "image_url": ["url": src?["url"] ?? ""]]
        default:
            return textPart("[unsupported image format]")
        }
    }

    static func documentNote(_ b: JSON, why: String) -> String {
        let title = (b["title"]?.string ?? b["name"]?.string ?? "").trimmingCharacters(in: .whitespaces)
        return "[document: \(title.isEmpty ? "document" : title) could not be read (\(why))]"
    }

    static func searchResultText(_ b: JSON) -> String? {
        var lines: [String] = []
        if let t = b["title"]?.string, !t.isEmpty { lines.append(t) }
        for c in b["content"]?.array ?? [] { lines.append(c["text"]?.string ?? "") }
        let s = lines.joined(separator: "\n").trimmingCharacters(in: .newlines)
        return s.isEmpty ? nil : s
    }

    /// A tool_result's content as the tool message text and the media parts
    /// a tool message cannot carry.
    static func splitToolResult(_ content: JSON?) -> (String, [JSON]) {
        guard let content, !content.isNull else { return ("", []) }
        if let s = content.string { return (s, []) }
        guard let blocks = content.array else { return (content.text, []) }
        var texts: [String] = []
        var media: [JSON] = []
        for b in blocks {
            switch b.type {
            case "text":
                if let t = b["text"]?.string, !t.isEmpty { texts.append(t) }
            case attachmentTextType:
                media.append(textPart(b["text"]?.string ?? ""))
            case "image":
                media.append(imagePart(b))
            case "document":
                media.append(textPart(documentNote(b, why: "unsupported document source")))
            case "tool_reference":
                if let name = b["tool_name"]?.string, !name.isEmpty { texts.append("Tool \(name) is now loaded and can be called.") }
            case "search_result":
                if let t = searchResultText(b) { texts.append(t) }
            default:
                continue
            }
        }
        return (texts.joined(separator: "\n"), media)
    }

    /// One Anthropic assistant message. Text becomes content, tool_use
    /// tool_calls, thinking `reasoning_content` and `reasoning`;
    /// redacted_thinking is dropped. A web search the engine ran earlier
    /// (server_tool_use + its web_search_tool_result in this message) is
    /// replayed as the function call and tool result the model made and saw,
    /// so the answer can be several chat messages.
    static func assistantMessage(_ content: JSON?) -> [JSON] {
        guard let content else { return [] }
        if let s = content.string { return [["role": "assistant", "content": .string(s)]] }
        guard let blocks = content.array else { return [["role": "assistant", "content": .string(content.text)]] }

        var results: [String: JSON] = [:]
        for b in blocks where b.type == "web_search_tool_result" {
            if let id = b["tool_use_id"]?.string { results[id] = b }
        }
        var out: [JSON] = [], deferred: [JSON] = []
        var texts: [String] = [], thinking: [String] = [], calls: [JSON] = []
        var clientCalls = 0
        func flush() -> JSON {
            var m = JSONObject([("role", "assistant")])
            let text = texts.joined()
            m["content"] = !text.isEmpty ? .string(text) : (calls.isEmpty ? "" : .null)
            if !thinking.isEmpty {
                let r = thinking.joined(separator: "\n")
                m["reasoning_content"] = .string(r)
                m["reasoning"] = .string(r)
            }
            if !calls.isEmpty { m["tool_calls"] = .array(calls) }
            texts = []; thinking = []; calls = []; clientCalls = 0
            return .object(m)
        }
        for b in blocks {
            switch b.type {
            case "text":
                texts.append(b["text"]?.string ?? "")
            case "thinking":
                if let t = b["thinking"]?.string, !t.isEmpty { thinking.append(t) }
            case "tool_use":
                calls.append(functionCall(id: b["id"]?.string ?? "", name: b["name"]?.string ?? "", input: b["input"]))
                clientCalls += 1
            case "server_tool_use":
                let id = b["id"]?.string ?? ""
                guard let query = b["input"]?["query"]?.string, !query.isEmpty else { continue }
                if results[id] != nil {
                    calls.append(functionCall(id: id, name: WebSearchTool.functionName, input: b["input"]))
                } else {
                    texts.append("[Searched the web: \(query)]")
                }
            case "web_search_tool_result":
                let id = b["tool_use_id"]?.string ?? ""
                guard calls.contains(where: { $0["id"]?.string == id }) else { continue }
                let tool: JSON = ["role": "tool", "tool_call_id": .string(id), "content": .string(WebSearchText.historic(b["content"]))]
                if clientCalls > 0 {
                    deferred.append(tool) // the client's results come in the next user message
                } else {
                    out.append(flush()); out.append(tool)
                }
            default:
                continue
            }
        }
        if !texts.isEmpty || !thinking.isEmpty || !calls.isEmpty || out.isEmpty { out.append(flush()) }
        return out + deferred
    }

    static func functionCall(id: String, name: String, input: JSON?) -> JSON {
        var args = "{}"
        if let input, !input.isNull { args = input.text }
        return ["id": .string(id), "type": "function", "function": ["name": .string(name), "arguments": .string(args)]]
    }

    // MARK: tools

    /// The tools Claude Code's ToolSearch loaded: every tool_reference in a
    /// tool_result of the conversation.
    static func referencedTools(_ msgs: [JSON]) -> Set<String> {
        var names = Set<String>()
        for m in msgs {
            for b in m["content"]?.array ?? [] where b.type == "tool_result" {
                for c in b["content"]?.array ?? [] where c.type == "tool_reference" {
                    if let n = c["tool_name"]?.string, !n.isEmpty { names.insert(n) }
                }
            }
        }
        return names
    }

    /// Function tools. A deferred tool (`defer_loading`) is sent only once a
    /// tool_reference has loaded it; `DeferredToolPlaceholder` never; server
    /// tools are dropped (web search is offered separately).
    static func translateTools(_ tools: [JSON], loaded: Set<String>) -> [JSON] {
        var out: [JSON] = []
        for t in tools {
            guard let name = t["name"]?.string, !name.isEmpty else { continue }
            if name == "DeferredToolPlaceholder" { continue }
            if t["defer_loading"]?.bool == true, !loaded.contains(name) { continue }
            if let type = t.type, type != "custom" { continue }
            var fn = JSONObject()
            fn["name"] = .string(name)
            if let d = t["description"]?.string, !d.isEmpty { fn["description"] = .string(d) }
            if let s = t["input_schema"], !s.isNull { fn["parameters"] = s }
            out.append(["type": "function", "function": .object(fn)])
        }
        return out
    }

    static func toolChoice(_ tc: JSON?) -> JSON? {
        switch tc?.type {
        case "auto": return "auto"
        case "any": return "required"
        case "none": return "none"
        case "tool": return ["type": "function", "function": ["name": tc?["name"] ?? ""]]
        default: return nil
        }
    }

    // MARK: options

    /// output_config.effort as reasoning_effort (max → xhigh).
    static func outputEffort(_ oc: JSON?) -> String? {
        guard let e = oc?["effort"]?.string?.trimmingCharacters(in: .whitespaces).lowercased(), !e.isEmpty else { return nil }
        return e == "max" ? "xhigh" : e
    }

    /// A json_schema output format as a strict Chat Completions response_format.
    static func responseFormat(_ f: JSON?) -> JSON? {
        guard let f, f.type == "json_schema", let schema = f["schema"], !schema.isNull else { return nil }
        return ["type": "json_schema", "json_schema": ["name": "output", "schema": schema, "strict": true]]
    }

    /// src merged into dst, src winning; nested objects merged key by key.
    static func deepMerge(_ dst: JSON, _ src: JSON) -> JSON {
        guard case .object(var d) = dst, case .object(let s) = src else { return src }
        for (k, v) in s {
            if let existing = d[k], existing.object != nil, v.object != nil { d[k] = deepMerge(existing, v) }
            else { d[k] = v }
        }
        return .object(d)
    }

    /// Lowers max_tokens so prompt + completion fit the context window. The
    /// prompt size is an estimate (with 10% + 128 tokens of headroom); when
    /// it leaves no sensible room the request goes as-is and the backend's
    /// context error (rewritten for the client) takes over.
    static func clampMaxTokens(_ req: inout JSON, contextWindow: Int) {
        guard contextWindow > 0, let maxTok = req["max_tokens"]?.int, maxTok > 0 else { return }
        let prompt = TokenEstimate.tokens(req)
        let room = contextWindow - prompt - prompt / 10 - 128
        guard room >= 256, maxTok > room else { return }
        req["max_tokens"] = .int(room)
    }
}
