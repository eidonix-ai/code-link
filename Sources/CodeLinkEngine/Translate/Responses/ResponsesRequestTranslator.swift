import Foundation

/// Responses API → Chat Completions (docs/DESIGN.md §5). The request's
/// `input` has been through OpenAIMedia: documents are text, and images are
/// left only for a model with vision.
enum ResponsesRequestTranslator {
    /// Joins the parts of the merged leading system message: `instructions`
    /// and each leading system/developer message are one blank line apart.
    static let systemSeparator = "\n\n"

    // MARK: input → messages

    /// The chat messages for a request's `input` and `instructions`, or nil
    /// when there is nothing to send.
    ///
    /// `instructions` and the LEADING run of system/developer messages become
    /// ONE system message (strict chat templates reject a system message
    /// anywhere but first, and one stable first message keeps prefix caches
    /// warm). Items that make no message (additional_tools, reasoning, …) do
    /// not end the run. A system/developer message after the conversation has
    /// started becomes user text `<developer>…</developer>`.
    static func messages(input: JSON?, instructions: String, registry: ToolRegistry) -> [JSON]? {
        var messages: [JSON] = []
        var system: [String] = []
        if !instructions.isEmpty { system.append(instructions) }

        func finish() -> [JSON]? {
            if messages.isEmpty && system.isEmpty { return nil }
            if !system.isEmpty {
                messages.insert(["role": "system", "content": .string(system.joined(separator: systemSeparator))], at: 0)
            }
            return messages
        }

        if let s = input?.string {
            messages.append(["role": "user", "content": .string(s)])
            return finish()
        }
        guard let items = input?.array else { return nil }

        // Reasoning waiting for the next assistant message; anything else in
        // between drops it.
        var pendingReasoning = ""
        func attachReasoning(_ i: Int) {
            guard !pendingReasoning.isEmpty else { return }
            let prev = messages[i]["reasoning_content"]?.string ?? ""
            let r = prev.isEmpty ? pendingReasoning : prev + "\n\n" + pendingReasoning
            messages[i]["reasoning_content"] = .string(r)
            messages[i]["reasoning"] = .string(r)
            pendingReasoning = ""
        }
        // Media from tool outputs: one user message after the tool messages.
        var toolMedia: [JSON] = []
        func flushToolMedia() {
            guard !toolMedia.isEmpty else { return }
            messages.append(["role": "user", "content": .array(toolMedia)])
            toolMedia = []
        }

        for item in items {
            let type = item.type ?? ""
            let role = item["role"]?.string ?? ""
            switch true {
            case type == "function_call" || type == "local_shell_call" || type == "custom_tool_call":
                var args: String
                if type == "custom_tool_call" {
                    args = (["input": .string(item["input"]?.string ?? "")] as JSON).text
                } else {
                    args = item["arguments"]?.string ?? ""
                    if args.isEmpty, let i = item["input"]?.string, !i.isEmpty { args = i }
                    if args.isEmpty, let a = item["action"], !a.isNull { args = a.text }
                    if args.isEmpty { args = "{}" }
                }
                var name = item["name"]?.string ?? ""
                if name.isEmpty, type == "local_shell_call" { name = "shell" }
                let call: JSON = ["id": .string(item["call_id"]?.string ?? ""), "type": "function",
                                  "function": ["name": .string(registry.flatName(item["namespace"]?.string ?? "", name)),
                                               "arguments": .string(args)]]
                flushToolMedia()
                if let n = messages.indices.last, messages[n]["role"]?.string == "assistant" {
                    var calls = messages[n]["tool_calls"]?.array ?? []
                    calls.append(call)
                    messages[n]["tool_calls"] = .array(calls)
                    if messages[n]["content"]?.string == "" { messages[n]["content"] = .null }
                    attachReasoning(n)
                } else {
                    messages.append(["role": "assistant", "content": .null, "tool_calls": [call]])
                    attachReasoning(messages.count - 1)
                }

            case type == "function_call_output" || type == "local_shell_call_output"
                || type == "custom_tool_call_output" || type == "mcp_tool_call_output":
                pendingReasoning = ""
                let id = item["call_id"]?.string ?? ""
                var (text, media) = toolOutput(item["output"])
                if !media.isEmpty {
                    if text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                        text = "[the tool returned attachments; they follow in the next message]"
                    }
                    toolMedia.append(RequestTranslator.textPart("[attachments from tool result \(id)]"))
                    toolMedia += media
                }
                messages.append(["role": "tool", "tool_call_id": .string(id), "content": .string(text)])

            case type == "reasoning":
                let t = reasoningText(item)
                if !t.isEmpty { pendingReasoning += pendingReasoning.isEmpty ? t : "\n\n" + t }

            case ["additional_tools", "compaction", "tool_search_call", "tool_search_output", "web_search_call",
                  "image_generation_call", "mcp_list_tools"].contains(type):
                continue // tool definitions are collected separately; the rest has no chat equivalent

            case role == "system" || role == "developer":
                let text = messageText(item["content"])
                if text.isEmpty { continue }
                if messages.isEmpty {
                    system.append(text)
                } else {
                    flushToolMedia()
                    pendingReasoning = ""
                    messages.append(["role": "user", "content": .string("<developer>\n" + text + "\n</developer>")])
                }

            case role == "user" || role == "assistant":
                flushToolMedia()
                messages.append(["role": .string(role), "content": content(item["content"], role: role)])
                if role == "assistant" { attachReasoning(messages.count - 1) } else { pendingReasoning = "" }

            default:
                continue
            }
        }
        flushToolMedia()
        return finish()
    }

    // MARK: pieces

    /// Message content as plain text: a string, or the text parts joined by a
    /// blank line. Other parts are ignored.
    static func messageText(_ content: JSON?) -> String {
        guard let content, !content.isNull else { return "" }
        if let s = content.string { return s }
        return (content.array ?? []).compactMap { p -> String? in
            guard let t = p["text"]?.string, !t.isEmpty, ["input_text", "output_text", "text"].contains(p.type ?? "") else { return nil }
            return t
        }.joined(separator: systemSeparator)
    }

    /// The text of a replayed reasoning item: its reasoning_text parts, else
    /// its summary (this engine writes both with the same text).
    static func reasoningText(_ item: JSON) -> String {
        func collect(_ parts: JSON?) -> String {
            (parts?.array ?? []).compactMap { $0["text"]?.string }.filter { !$0.isEmpty }.joined(separator: "\n\n")
        }
        let c = collect(item["content"])
        return c.isEmpty ? collect(item["summary"]) : c
    }

    /// The chat content of a user or assistant message.
    static func content(_ content: JSON?, role: String) -> JSON {
        guard let content, !content.isNull else { return "" }
        if content.string != nil { return content }
        guard let parts = content.array else { return .string(content.text) }
        if role == "assistant" {
            return .string(parts.filter { $0.type == "output_text" || $0.type == "text" }.compactMap { $0["text"]?.string }.joined())
        }
        var out: [JSON] = []
        for p in parts {
            switch p.type {
            case "input_text", "text": out.append(RequestTranslator.textPart(p["text"]?.string ?? ""))
            case "input_image": out.append(imagePart(p))
            case "input_file": out.append(RequestTranslator.textPart(unreadableFile(p)))
            default: out.append(p)
            }
        }
        if out.count == 1, out[0].type == "text" { return out[0]["text"] ?? "" }
        return out.isEmpty ? "" : .array(out)
    }

    static func imagePart(_ p: JSON) -> JSON {
        var image: JSON = ["url": .string(p["image_url"]?.string ?? p["image_url"]?["url"]?.string ?? "")]
        if let d = p["detail"]?.string, !d.isEmpty { image["detail"] = .string(d) }
        return ["type": "image_url", "image_url": image]
    }

    private static func unreadableFile(_ p: JSON) -> String {
        let name = p["filename"]?.string ?? "unnamed file"
        return "[attached file \"\(name)\" could not be read: only inline file_data is supported]"
    }

    /// A tool output as the tool message text and the parts a tool message
    /// cannot carry (images, attachment text). The output is a string, parts,
    /// or an object `{content, success}`.
    static func toolOutput(_ output: JSON?) -> (String, [JSON]) {
        guard let output, !output.isNull else { return ("", []) }
        if let s = output.string { return (s, []) }
        if let parts = output.array {
            var texts: [String] = []
            var media: [JSON] = []
            for p in parts {
                switch p.type {
                case "input_text", "text", "output_text":
                    if let t = p["text"]?.string, !t.isEmpty { texts.append(t) }
                case attachmentTextType: media.append(RequestTranslator.textPart(p["text"]?.string ?? ""))
                case "input_image": media.append(imagePart(p))
                case "input_file": media.append(RequestTranslator.textPart(unreadableFile(p)))
                default: if let t = p["text"]?.string, !t.isEmpty { texts.append(t) }
                }
            }
            return (texts.joined(separator: "\n"), media)
        }
        if output.object != nil, let inner = output["content"], !inner.isNull { return toolOutput(inner) }
        return (output.text, [])
    }

    /// The tool definitions `additional_tools` input items carry (Codex's
    /// auto-review sends its tools this way).
    static func additionalTools(_ input: JSON?) -> [JSON] {
        (input?.array ?? []).filter { $0.type == "additional_tools" }.flatMap { $0["tools"]?.array ?? [] }
    }

    // MARK: request

    /// The `text.format` of a request as a chat `response_format`.
    static func responseFormat(_ text: JSON?) -> JSON? {
        guard let f = text?["format"] else { return nil }
        switch f.type {
        case "json_schema":
            var schema = JSONObject([("name", .string(f["name"]?.string.flatMap { $0.isEmpty ? nil : $0 } ?? "response"))])
            if let s = f["schema"], !s.isNull { schema["schema"] = s }
            if let strict = f["strict"]?.bool { schema["strict"] = .bool(strict) }
            return ["type": "json_schema", "json_schema": .object(schema)]
        case "json_object":
            return ["type": "json_object"]
        default:
            return nil
        }
    }

    /// The Chat Completions body (without `model`) for a Responses request.
    static func chatRequest(_ req: JSON, messages: [JSON], tools: [JSON], registry: ToolRegistry,
                            model: Model, contextWindow: Int?) -> JSON {
        var out = JSONObject()
        out["messages"] = .array(messages)
        let stream = req["stream"]?.bool ?? false
        out["stream"] = .bool(stream)
        if stream { out["stream_options"] = ["include_usage": true] }
        if !tools.isEmpty {
            out["tools"] = .array(tools)
            if let tc = req["tool_choice"], !tc.isNull { out["tool_choice"] = registry.toolChoice(tc) }
            if let p = req["parallel_tool_calls"]?.bool { out["parallel_tool_calls"] = .bool(p) }
        }
        if let n = req["max_output_tokens"]?.int, n > 0 { out["max_tokens"] = .int(n) }
        for k in ["temperature", "top_p"] { if let v = req[k], !v.isNull { out[k] = v } }
        var thinkingOff = false
        if let effort = req["reasoning"]?["effort"]?.string?.trimmingCharacters(in: .whitespaces), !effort.isEmpty {
            // "none" turns thinking off where the model says how.
            if effort == "none", model.thinkingOff != nil { thinkingOff = true } else { out["reasoning_effort"] = .string(effort) }
        }
        if let rf = responseFormat(req["text"]) { out["response_format"] = rf }
        if let user = req["user"]?.string, !user.isEmpty { out["user"] = .string(user) }

        var result = JSON.object(out)
        if thinkingOff, let off = model.thinkingOff { result = RequestTranslator.deepMerge(result, JSON(off)) }
        if let contextWindow { RequestTranslator.clampMaxTokens(&result, contextWindow: contextWindow) }
        return result
    }
}
