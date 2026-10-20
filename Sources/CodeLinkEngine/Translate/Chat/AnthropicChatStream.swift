import Foundation

/// Translates an Anthropic Messages SSE stream into Chat Completions chunks
/// (docs/DESIGN.md §5a), one upstream line at a time. Unknown event types are
/// ignored, so additions to the Anthropic protocol do not break it. A stream
/// that ends before `message_stop` is an error, never a quiet "stop".
final class AnthropicChatStream {
    private let sink: SSESink
    private let model: String
    private let includeUsage: Bool
    private let id = IDs.make("chatcmpl-")
    private let created = Int(Date().timeIntervalSince1970)

    private var roleSent = false
    private var finishReason = "stop"
    private var inputUsage: JSON = [:]
    private var outputTokens = 0
    private var nextTool = 0
    private var toolIndex: [Int: Int] = [:]
    private var sawStop = false
    private(set) var failed = false
    private var finished = false

    init(sink: SSESink, model: String, includeUsage: Bool) {
        self.sink = sink; self.model = model; self.includeUsage = includeUsage
    }

    private func chunk(_ delta: JSON, finish: String? = nil, usage: JSON? = nil) {
        var c: JSON = ["id": .string(id), "object": "chat.completion.chunk", "created": .int(created), "model": .string(model),
                       "choices": usage == nil ? [["index": 0, "delta": delta, "finish_reason": finish.map { .string($0) } ?? nil]] : []]
        if let usage { c["usage"] = usage }
        sink.raw("data: \(c.text)\n\n")
    }

    private func role() {
        guard !roleSent else { return }
        roleSent = true
        chunk(["role": "assistant"])
    }

    private var usage: JSON {
        var u = inputUsage
        u["completion_tokens"] = .int(outputTokens)
        u["total_tokens"] = .int((u["prompt_tokens"]?.int ?? 0) + outputTokens)
        return u
    }

    func line(_ line: String) -> ChatLineResult {
        guard !finished, line.hasPrefix("data:") else { return finished ? .failed : .more }
        let payload = line.dropFirst(5).trimmingCharacters(in: .whitespaces)
        if payload.isEmpty { return .more }
        if payload == "[DONE]" { return .doneMarker }
        guard let e = try? JSON.parse(payload) else { return .more }
        switch e.type {
        case "message_start":
            let u = ChatToAnthropic.usage(e["message"]?["usage"])
            inputUsage = u
            outputTokens = e["message"]?["usage"]?["output_tokens"]?.int ?? 0
            role()
        case "content_block_start":
            role()
            let b = e["content_block"]
            if b?.type == "tool_use", let index = e["index"]?.int {
                let t = nextTool
                nextTool += 1
                toolIndex[index] = t
                chunk(["tool_calls": [["index": .int(t), "id": b?["id"] ?? "", "type": "function",
                                       "function": ["name": b?["name"] ?? "", "arguments": ""]]]])
            }
        case "content_block_delta":
            role()
            let d = e["delta"]
            switch d?.type {
            case "text_delta":
                if let t = d?["text"]?.string, !t.isEmpty { chunk(["content": .string(t)]) }
            case "thinking_delta":
                if let t = d?["thinking"]?.string, !t.isEmpty { chunk(["reasoning_content": .string(t)]) }
            case "input_json_delta":
                if let index = e["index"]?.int, let t = toolIndex[index] {
                    chunk(["tool_calls": [["index": .int(t), "function": ["arguments": d?["partial_json"] ?? ""]]]])
                }
            default: break
            }
        case "message_delta":
            if let stop = e["delta"]?["stop_reason"]?.string, !stop.isEmpty { finishReason = ChatToAnthropic.finishReason(stop) }
            if let u = e["usage"] {
                if let out = u["output_tokens"]?.int, out != 0 { outputTokens = out }
                if (u["input_tokens"]?.int ?? 0) != 0 || (u["cache_read_input_tokens"]?.int ?? 0) != 0 {
                    inputUsage = ChatToAnthropic.usage(u)
                }
            }
        case "message_stop":
            sawStop = true
            role()
            chunk([:], finish: finishReason)
            if includeUsage { chunk([:], usage: usage) }
            finished = true
        case "error":
            fail(e["error"]?["message"]?.string.map(scrubUpstreamMessage) ?? "upstream stream error")
            return .failed
        default:
            break // ping, content_block_stop, …
        }
        return .more
    }

    private func fail(_ message: String) {
        guard !finished else { return }
        failed = true
        finished = true
        sink.raw("data: \(OpenAIError.server(502, "upstream_error", message).json.text)\n\n")
    }

    /// The end of the upstream stream: a stream cut short is an error frame;
    /// every stream ends with `[DONE]`.
    func finish() {
        if !sawStop { fail("the backend's stream ended before it was complete") }
        sink.raw("data: [DONE]\n\n")
    }

    /// A whole completion as the chunks a streaming request expects.
    static func replay(_ chat: JSON, sink: SSESink, includeUsage: Bool) {
        let t = AnthropicChatStream(sink: sink, model: chat["model"]?.string ?? "", includeUsage: includeUsage)
        let msg = chat["choices"]?[0]?["message"] ?? [:]
        t.role()
        if let c = msg["content"]?.string, !c.isEmpty { t.chunk(["content": .string(c)]) }
        if let r = msg["reasoning_content"]?.string, !r.isEmpty { t.chunk(["reasoning_content": .string(r)]) }
        for (i, call) in (msg["tool_calls"]?.array ?? []).enumerated() {
            var c = call
            c["index"] = .int(i)
            t.chunk(["tool_calls": [c]])
        }
        t.chunk([:], finish: chat["choices"]?[0]?["finish_reason"]?.string ?? "stop")
        if includeUsage, let u = chat["usage"] { t.chunk([:], usage: u) }
        sink.raw("data: [DONE]\n\n")
    }
}
