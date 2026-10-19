import Foundation

/// A non-streaming Chat Completions response as an Anthropic message
/// (docs/DESIGN.md §4.2).
enum ResponseTranslator {
    static func message(_ chat: JSON, model: String, stopSequences: [String]) -> JSON? {
        guard let choice = chat["choices"]?[0] else { return nil }
        let msg = choice["message"] ?? [:]
        var content: [JSON] = []
        var reasoning = msg["reasoning_content"]?.string ?? ""
        if reasoning.isEmpty { reasoning = msg["reasoning"]?.string ?? "" }
        if !reasoning.isEmpty {
            content.append(["type": "thinking", "thinking": .string(reasoning), "signature": .string(StreamTranslator.signature)])
        }
        if let c = msg["content"]?.string, !c.isEmpty {
            let (think, text) = ThinkTagFilter.strip(c)
            if !think.isEmpty, reasoning.isEmpty {
                content.append(["type": "thinking", "thinking": .string(think), "signature": .string(StreamTranslator.signature)])
            }
            if !text.isEmpty { content.append(["type": "text", "text": .string(text)]) }
        }
        let calls = msg["tool_calls"]?.array ?? []
        for tc in calls {
            var input: JSON = [:]
            if let args = tc["function"]?["arguments"]?.string, let parsed = try? JSON.parse(args), parsed.object != nil { input = parsed }
            let id = tc["id"]?.string.flatMap { $0.isEmpty ? nil : $0 } ?? IDs.make("toolu_")
            content.append(["type": "tool_use", "id": .string(id), "name": tc["function"]?["name"] ?? "", "input": input])
        }
        let (stop, seq) = resolveStopReason(finish: choice["finish_reason"]?.string, backendStop: choice["stop_reason"]?.string,
                                            stopSequences: stopSequences, sawToolCalls: !calls.isEmpty)
        return ["id": .string(IDs.make("msg_")), "type": "message", "role": "assistant", "content": .array(content),
                "model": .string(model), "stop_reason": .string(stop), "stop_sequence": seq.map { .string($0) } ?? nil,
                "usage": (ChatUsage(chat["usage"]) ?? ChatUsage()).anthropic()]
    }
}
