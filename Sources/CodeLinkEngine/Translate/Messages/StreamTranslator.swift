import Foundation

/// Chat Completions usage.
struct ChatUsage: Sendable, Equatable {
    var promptTokens = 0
    var completionTokens = 0
    var cachedTokens = 0

    init(promptTokens: Int = 0, completionTokens: Int = 0, cachedTokens: Int = 0) {
        self.promptTokens = promptTokens; self.completionTokens = completionTokens; self.cachedTokens = cachedTokens
    }
    init?(_ j: JSON?) {
        guard let j, j.object != nil else { return nil }
        promptTokens = j["prompt_tokens"]?.int ?? 0
        completionTokens = j["completion_tokens"]?.int ?? 0
        cachedTokens = j["prompt_tokens_details"]?["cached_tokens"]?.int ?? 0
    }
    mutating func add(_ o: ChatUsage) {
        promptTokens += o.promptTokens; completionTokens += o.completionTokens; cachedTokens += o.cachedTokens
    }
    /// Anthropic usage: cached prompt tokens are cache reads, not input.
    func anthropic(webSearches: Int? = nil) -> JSON {
        let cached = min(max(cachedTokens, 0), promptTokens)
        var u: JSON = ["input_tokens": .int(promptTokens - cached), "output_tokens": .int(completionTokens),
                       "cache_creation_input_tokens": 0, "cache_read_input_tokens": .int(cached)]
        if let webSearches { u["server_tool_use"] = ["web_search_requests": .int(webSearches)] }
        return u
    }
}

/// Anthropic stop_reason and stop_sequence for a Chat Completions finish. Any
/// turn with tool calls ends with tool_use; a "stop" whose backend
/// `stop_reason` names one of the request's stop sequences is stop_sequence.
func resolveStopReason(finish: String?, backendStop: String?, stopSequences: [String], sawToolCalls: Bool) -> (String, String?) {
    let f = finish ?? ""
    if sawToolCalls, f == "stop" || f == "tool_calls" || f == "" { return ("tool_use", nil) }
    if f == "stop", let s = backendStop, !s.isEmpty, stopSequences.contains(s) { return ("stop_sequence", s) }
    switch f {
    case "tool_calls", "function_call": return ("tool_use", nil)
    case "length": return ("max_tokens", nil)
    default: return ("end_turn", nil)
    }
}

/// Translates a Chat Completions SSE stream into Anthropic Messages events
/// (docs/DESIGN.md §4.2), one upstream line at a time. In search mode one
/// message spans several generations ("rounds", §4.5): the model's
/// web_search calls are held back from the client and answered between
/// rounds.
final class StreamTranslator {
    static let signature = "code-link"

    final class ToolState {
        let callID: String
        let name: String
        let blockIndex: Int
        let held: Bool
        var args = ""
        init(callID: String, name: String, blockIndex: Int, held: Bool) {
            self.callID = callID; self.name = name; self.blockIndex = blockIndex; self.held = held
        }
    }

    let sink: SSESink
    let model: String
    let stopSequences: [String]
    let searchMode: Bool
    let msgID = IDs.make("msg_")

    private(set) var started = false
    /// message_stop or error sent.
    private(set) var done = false
    private var blockIndex = 0
    private var textOpen = false
    private var thinkingOpen = false
    private var tools: [Int: ToolState] = [:]
    /// This round's client tool calls, in order.
    private(set) var toolOrder: [ToolState] = []
    /// This round's web_search calls (search mode).
    private(set) var held: [ToolState] = []
    private(set) var finishReason: String?
    private var backendStop: String?
    private var usage: ChatUsage?
    private var usageSum = ChatUsage()
    private var think = ThinkTagFilter()
    private var roundClosed = false
    private(set) var roundText = ""
    private(set) var roundReasoning = ""
    private(set) var searches = 0
    /// Whether anything but message_start and pings went out.
    private(set) var producedOutput = false
    /// The error the stream ended with, if it did.
    private(set) var failure: AnthropicError?

    init(sink: SSESink, model: String, stopSequences: [String], searchMode: Bool) {
        self.sink = sink; self.model = model; self.stopSequences = stopSequences; self.searchMode = searchMode
    }

    private func emit(_ event: String, _ data: JSON) {
        if event != "ping" && event != "message_start" { producedOutput = true }
        sink.event(event, data)
    }

    /// message_start and a first ping.
    func start(inputTokens: Int = 0) {
        guard !started else { return }
        started = true
        emit("message_start", ["message": [
            "id": .string(msgID), "type": "message", "role": "assistant", "content": [], "model": .string(model),
            "stop_reason": nil, "stop_sequence": nil,
            "usage": ["input_tokens": .int(inputTokens), "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0],
        ]])
        emit("ping", [:])
    }

    enum LineResult { case more, doneMarker, failed }

    /// One upstream line.
    func line(_ line: String) -> LineResult {
        guard !done else { return .failed }
        guard line.hasPrefix("data:") else { return .more } // event:, id:, comments, blank lines
        let payload = line.dropFirst(5).trimmingCharacters(in: .whitespaces)
        if payload == "[DONE]" { return .doneMarker }
        guard let chunk = try? JSON.parse(payload) else { return .more }
        start()
        if let err = chunk["error"], !err.isNull, chunk["choices"] == nil {
            fail(Self.inBandError(err))
            return .failed
        }
        if let u = ChatUsage(chunk["usage"]) { usage = u }
        guard let choice = chunk["choices"]?[0] else { return .more }
        let delta = choice["delta"] ?? [:]

        var reasoning = delta["reasoning_content"]?.string ?? ""
        if reasoning.isEmpty { reasoning = delta["reasoning"]?.string ?? "" }
        if !reasoning.isEmpty { thinking(reasoning) }
        if let c = delta["content"]?.string, !c.isEmpty {
            for seg in think.process(c) { seg.reasoning ? thinking(seg.text) : text(seg.text) }
        }
        for tc in delta["tool_calls"]?.array ?? [] { toolDelta(tc) }
        if let f = choice["finish_reason"]?.string, !f.isEmpty { finishReason = f }
        if let s = choice["stop_reason"]?.string, !s.isEmpty { backendStop = s }
        return .more
    }

    /// An in-band `{"error": …}` frame.
    static func inBandError(_ err: JSON) -> AnthropicError {
        let message = err.string ?? err["message"]?.string ?? "the backend reported an error"
        let code = err["code"]?.int ?? err["status"]?.int ?? 0
        var info = UpstreamErrorInfo()
        info.message = message
        info.type = err["type"]?.string ?? ""
        if let rewritten = rewriteContextLengthError(info) { return .invalid(rewritten) }
        if (400..<500).contains(code) {
            let (s, t) = anthropicErrorForStatus(code)
            return AnthropicError(status: s, type: t, message: scrubUpstreamMessage(message))
        }
        return AnthropicError(status: 529, type: "overloaded_error", message: scrubUpstreamMessage(message))
    }

    private func toolDelta(_ tc: JSON) {
        let index = tc["index"]?.int ?? 0
        let id = tc["id"]?.string ?? ""
        var state = tools[index]
        if state == nil || (!id.isEmpty && id != state!.callID) {
            let name = tc["function"]?["name"]?.string ?? ""
            if state == nil, id.isEmpty, name.isEmpty, (tc["function"]?["arguments"]?.string ?? "").isEmpty { return }
            closeThinking()
            closeText()
            let callID = id.isEmpty ? IDs.make("toolu_") : id
            let isHeld = searchMode && name == WebSearchTool.functionName
            let s = ToolState(callID: callID, name: name, blockIndex: isHeld ? -1 : blockIndex, held: isHeld)
            tools[index] = s
            state = s
            if isHeld {
                held.append(s)
            } else {
                toolOrder.append(s)
                emit("content_block_start", ["index": .int(blockIndex), "content_block": [
                    "type": "tool_use", "id": .string(callID), "name": .string(name), "input": [:]]])
                blockIndex += 1
            }
        }
        guard let state, let args = tc["function"]?["arguments"]?.string, !args.isEmpty else { return }
        state.args += args
        if !state.held {
            emit("content_block_delta", ["index": .int(state.blockIndex),
                                         "delta": ["type": "input_json_delta", "partial_json": .string(args)]])
        }
    }

    private func thinking(_ s: String) {
        closeText()
        if !thinkingOpen {
            emit("content_block_start", ["index": .int(blockIndex), "content_block": ["type": "thinking", "thinking": "", "signature": ""]])
            thinkingOpen = true
        }
        if searchMode { roundReasoning += s }
        emit("content_block_delta", ["index": .int(blockIndex), "delta": ["type": "thinking_delta", "thinking": .string(s)]])
    }

    private func text(_ s: String) {
        closeThinking()
        if !textOpen {
            emit("content_block_start", ["index": .int(blockIndex), "content_block": ["type": "text", "text": ""]])
            textOpen = true
        }
        if searchMode { roundText += s }
        emit("content_block_delta", ["index": .int(blockIndex), "delta": ["type": "text_delta", "text": .string(s)]])
    }

    private func closeThinking() {
        guard thinkingOpen else { return }
        emit("content_block_delta", ["index": .int(blockIndex), "delta": ["type": "signature_delta", "signature": .string(Self.signature)]])
        emit("content_block_stop", ["index": .int(blockIndex)])
        blockIndex += 1
        thinkingOpen = false
    }

    private func closeText() {
        guard textOpen else { return }
        emit("content_block_stop", ["index": .int(blockIndex)])
        blockIndex += 1
        textOpen = false
    }

    /// Closes everything the generation left open and adds its usage to the
    /// message's total.
    func finishRound() {
        guard !roundClosed, !done else { return }
        roundClosed = true
        start()
        for seg in think.flush() { seg.reasoning ? thinking(seg.text) : text(seg.text) }
        closeThinking()
        closeText()
        for t in toolOrder { emit("content_block_stop", ["index": .int(t.blockIndex)]) }
        if let usage { usageSum.add(usage); self.usage = nil }
    }

    /// message_delta (stop reason, every round's usage) and message_stop.
    func finalize() {
        guard !done else { return }
        finishRound()
        var (stop, seq) = resolveStopReason(finish: finishReason, backendStop: backendStop,
                                            stopSequences: stopSequences, sawToolCalls: !toolOrder.isEmpty)
        if searchMode, toolOrder.isEmpty {
            // Held web_search calls are not the client's tool calls.
            stop = finishReason == "length" ? "max_tokens" : (stop == "stop_sequence" ? stop : "end_turn")
            if stop != "stop_sequence" { seq = nil }
        }
        emit("message_delta", ["delta": ["stop_reason": .string(stop), "stop_sequence": seq.map { .string($0) } ?? nil],
                               "usage": usageSum.anthropic(webSearches: searchMode ? searches : nil)])
        emit("message_stop", [:])
        done = true
    }

    /// Ends the stream with an error event (open blocks closed first).
    func fail(_ e: AnthropicError) {
        guard !done else { return }
        start()
        closeThinking()
        closeText()
        done = true
        failure = e
        sink.raw("event: error\ndata: \(e.json.text)\n\n")
    }

    /// Readies the translator for the model's next generation (search mode).
    func nextRound() {
        tools = [:]; toolOrder = []; held = []
        finishReason = nil; backendStop = nil; usage = nil
        roundClosed = false
        think = ThinkTagFilter()
        roundText = ""; roundReasoning = ""
    }

    /// Shows the client one search the engine ran, as the Anthropic API
    /// streams it: server_tool_use and its web_search_tool_result.
    func serverSearch(_ o: SearchOutcome) {
        start()
        emit("content_block_start", ["index": .int(blockIndex), "content_block": [
            "type": "server_tool_use", "id": .string(o.id), "name": .string(WebSearchTool.functionName), "input": [:]]])
        emit("content_block_delta", ["index": .int(blockIndex), "delta": [
            "type": "input_json_delta", "partial_json": .string((["query": .string(o.query)] as JSON).text)]])
        emit("content_block_stop", ["index": .int(blockIndex)])
        blockIndex += 1
        emit("content_block_start", ["index": .int(blockIndex), "content_block": [
            "type": "web_search_tool_result", "tool_use_id": .string(o.id), "content": o.resultContent]])
        emit("content_block_stop", ["index": .int(blockIndex)])
        blockIndex += 1
        if o.errorCode == nil { searches += 1 }
    }
}

/// A complete (non-streaming) Anthropic message as the SSE event sequence a
/// streaming request expects (classifier answers when `stream: true`).
func emitMessageAsSSE(_ msg: JSON, sink: SSESink) {
    var start = msg
    start["content"] = []
    start["stop_reason"] = .null
    start["stop_sequence"] = .null
    var u = msg["usage"] ?? [:]
    u["output_tokens"] = 0
    start["usage"] = u
    sink.event("message_start", ["message": start])
    sink.ping()
    for (i, b) in (msg["content"]?.array ?? []).enumerated() {
        let idx = JSON.int(i)
        switch b.type {
        case "text":
            sink.event("content_block_start", ["index": idx, "content_block": ["type": "text", "text": ""]])
            sink.event("content_block_delta", ["index": idx, "delta": ["type": "text_delta", "text": b["text"] ?? ""]])
        case "thinking":
            sink.event("content_block_start", ["index": idx, "content_block": ["type": "thinking", "thinking": "", "signature": ""]])
            sink.event("content_block_delta", ["index": idx, "delta": ["type": "thinking_delta", "thinking": b["thinking"] ?? ""]])
            sink.event("content_block_delta", ["index": idx, "delta": ["type": "signature_delta", "signature": b["signature"] ?? .string(StreamTranslator.signature)]])
        case "tool_use", "server_tool_use":
            var cb = b
            cb["input"] = [:]
            sink.event("content_block_start", ["index": idx, "content_block": cb])
            sink.event("content_block_delta", ["index": idx, "delta": ["type": "input_json_delta", "partial_json": .string((b["input"] ?? [:]).text)]])
        default:
            sink.event("content_block_start", ["index": idx, "content_block": b])
        }
        sink.event("content_block_stop", ["index": idx])
    }
    sink.event("message_delta", ["delta": ["stop_reason": msg["stop_reason"] ?? "end_turn", "stop_sequence": msg["stop_sequence"] ?? nil],
                                 "usage": msg["usage"] ?? [:]])
    sink.event("message_stop")
}
