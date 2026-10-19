import Foundation

/// Translates a Chat Completions SSE stream into Responses API events
/// (docs/DESIGN.md §5), one upstream line at a time, in the order Codex
/// reads them: `response.created`, `response.in_progress`, then each output
/// item as `output_item.added`, its deltas and `output_item.done`, and
/// finally `response.completed` with usage. In search mode one response spans
/// several generations ("rounds", §4.5): the model's web_search calls are held
/// back and answered between rounds, shown as `web_search_call` items.
final class ResponsesStreamTranslator: ChatLineConsumer {
    final class Call {
        let itemID = IDs.make("fc_")
        let callID: String
        var name: String
        var custom: Bool
        let held: Bool
        var outputIndex = -1
        var args = ""
        init(callID: String, name: String, custom: Bool, held: Bool) {
            self.callID = callID; self.name = name; self.custom = custom; self.held = held
        }
    }

    let sink: SSESink
    let model: String
    let registry: ToolRegistry
    /// Structured output: the answer is held back and cleaned of fences.
    let jsonMode: Bool
    let searchMode: Bool
    let responseID = IDs.make("resp_")
    let createdAt = Int(Date().timeIntervalSince1970)

    private var seq = 0
    private(set) var started = false
    /// response.completed / incomplete / failed sent.
    private(set) var done = false
    private var nextIndex = 0
    private var finished: [(index: Int, item: JSON)] = []

    private var reasoning: (id: String, index: Int, text: String)?
    private var message: (id: String, index: Int, text: String)?
    private var tools: [Int: Call] = [:]
    /// This round's client tool calls, in order.
    private(set) var toolOrder: [Call] = []
    /// This round's web_search calls (search mode).
    private(set) var held: [Call] = []
    private(set) var finishReason: String?
    private var usage: ChatUsage?
    private var usageSum = ChatUsage()
    private var think = ThinkTagFilter()
    private var roundClosed = false
    private var jsonBuffer = ""
    private(set) var roundText = ""
    private(set) var roundReasoning = ""
    private var outputText = ""
    private(set) var searches = 0
    private(set) var failure: OpenAIError?
    private(set) var producedOutput = false

    init(sink: SSESink, model: String, registry: ToolRegistry, jsonMode: Bool, searchMode: Bool) {
        self.sink = sink; self.model = model; self.registry = registry; self.jsonMode = jsonMode; self.searchMode = searchMode
    }

    // MARK: events

    private func emit(_ name: String, _ fields: [(String, JSON)]) {
        var data = JSONObject(fields)
        data["sequence_number"] = .int(seq)
        seq += 1
        if name != "response.created" && name != "response.in_progress" { producedOutput = true }
        sink.event(name, .object(data))
    }

    private func responseObject(status: String, output: [JSON] = [], extra: [(String, JSON)] = []) -> JSON {
        var r: JSON = ["id": .string(responseID), "object": "response", "created_at": .int(createdAt),
                       "model": .string(model), "status": .string(status)]
        for (k, v) in extra { r[k] = v }
        r["output"] = .array(output)
        return r
    }

    /// `response.created` and `response.in_progress`.
    func start() {
        guard !started else { return }
        started = true
        emit("response.created", [("response", responseObject(status: "in_progress"))])
        emit("response.in_progress", [("response", responseObject(status: "in_progress"))])
    }

    // MARK: upstream lines

    func line(_ line: String) -> ChatLineResult {
        guard !done else { return .failed }
        guard line.hasPrefix("data:") else { return .more }
        let payload = line.dropFirst(5).trimmingCharacters(in: .whitespaces)
        if payload == "[DONE]" { return .doneMarker }
        guard let chunk = try? JSON.parse(payload) else { return .more }
        if let err = chunk["error"], !err.isNull, chunk["choices"] == nil {
            fail(OpenAIError.inBand(err))
            return .failed
        }
        start()
        if let u = ChatUsage(chunk["usage"]) { usage = u }
        guard let choice = chunk["choices"]?[0] else { return .more }
        let delta = choice["delta"] ?? [:]

        var r = delta["reasoning_content"]?.string ?? ""
        if r.isEmpty { r = delta["reasoning"]?.string ?? "" }
        if !r.isEmpty { reasoningText(r) }
        if let c = delta["content"]?.string, !c.isEmpty {
            for seg in think.process(c) { seg.reasoning ? reasoningText(seg.text) : contentText(seg.text) }
        }
        for tc in delta["tool_calls"]?.array ?? [] { toolDelta(tc) }
        if let f = choice["finish_reason"]?.string, !f.isEmpty { finishReason = f }
        return .more
    }

    // MARK: reasoning

    private func reasoningText(_ s: String) {
        closeMessage()
        if reasoning == nil {
            let id = IDs.make("rs_")
            let index = allocate()
            reasoning = (id, index, "")
            emit("response.output_item.added", [("item", ["id": .string(id), "type": "reasoning", "summary": [], "content": []]),
                                                ("output_index", .int(index))])
            emit("response.reasoning_summary_part.added", [("item_id", .string(id)), ("output_index", .int(index)), ("summary_index", 0)])
        }
        guard let r = reasoning else { return }
        reasoning?.text += s
        roundReasoning += s
        emit("response.reasoning_summary_text.delta", [("delta", .string(s)), ("item_id", .string(r.id)),
                                                       ("output_index", .int(r.index)), ("summary_index", 0)])
        emit("response.reasoning_text.delta", [("delta", .string(s)), ("item_id", .string(r.id)),
                                               ("output_index", .int(r.index)), ("content_index", 0)])
    }

    private func closeReasoning() {
        guard let r = reasoning else { return }
        reasoning = nil
        emit("response.reasoning_summary_text.done", [("text", .string(r.text)), ("item_id", .string(r.id)),
                                                      ("output_index", .int(r.index)), ("summary_index", 0)])
        emit("response.reasoning_text.done", [("text", .string(r.text)), ("item_id", .string(r.id)),
                                              ("output_index", .int(r.index)), ("content_index", 0)])
        let item = Self.reasoningItem(id: r.id, text: r.text)
        emit("response.output_item.done", [("item", item), ("output_index", .int(r.index))])
        finished.append((r.index, item))
    }

    /// A completed reasoning item: the text as a summary (what clients show)
    /// and as reasoning_text content (what they replay in later requests).
    static func reasoningItem(id: String, text: String) -> JSON {
        ["id": .string(id), "type": "reasoning",
         "summary": [["type": "summary_text", "text": .string(text)]],
         "content": [["type": "reasoning_text", "text": .string(text)]]]
    }

    // MARK: text

    private func contentText(_ s: String) {
        if jsonMode { jsonBuffer += s; return }
        emitText(s)
    }

    private func emitText(_ s: String) {
        closeReasoning()
        if message == nil {
            let id = IDs.make("msg_")
            let index = allocate()
            message = (id, index, "")
            emit("response.output_item.added", [("item", ["id": .string(id), "type": "message", "role": "assistant",
                                                          "status": "in_progress", "content": []]),
                                                ("output_index", .int(index))])
            emit("response.content_part.added", [("part", ["type": "output_text", "text": "", "annotations": []]),
                                                 ("content_index", 0), ("output_index", .int(index)), ("item_id", .string(id))])
        }
        guard let m = message else { return }
        message?.text += s
        roundText += s
        outputText += s
        emit("response.output_text.delta", [("delta", .string(s)), ("content_index", 0),
                                            ("output_index", .int(m.index)), ("item_id", .string(m.id))])
    }

    private func closeMessage() {
        guard let m = message else { return }
        message = nil
        let part: JSON = ["type": "output_text", "text": .string(m.text), "annotations": []]
        emit("response.output_text.done", [("text", .string(m.text)), ("content_index", 0),
                                           ("output_index", .int(m.index)), ("item_id", .string(m.id))])
        emit("response.content_part.done", [("part", part), ("content_index", 0),
                                            ("output_index", .int(m.index)), ("item_id", .string(m.id))])
        let item: JSON = ["id": .string(m.id), "type": "message", "role": "assistant", "status": "completed", "content": [part]]
        emit("response.output_item.done", [("item", item), ("output_index", .int(m.index))])
        finished.append((m.index, item))
    }

    /// Structured output held back is sent now: cleaned of fences and prose
    /// when it is the answer, as written when a tool call follows it.
    private func flushJSON(clean: Bool) {
        guard !jsonBuffer.isEmpty else { return }
        let text = clean ? Self.cleanStructuredOutput(jsonBuffer) : jsonBuffer
        jsonBuffer = ""
        emitText(text)
    }

    /// The final text of a structured-output request. Models without enforced
    /// decoding wrap the JSON in Markdown fences or prose; if a valid JSON
    /// value can be pulled out, exactly that is returned, else the text as is.
    static func cleanStructuredOutput(_ text: String) -> String {
        func valid(_ s: String) -> Bool { (try? JSON.parse(s)) != nil }
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        if valid(trimmed) { return trimmed }
        if trimmed.hasPrefix("```") {
            var body = String(trimmed.dropFirst(3))
            if let nl = body.firstIndex(of: "\n") { body = String(body[body.index(after: nl)...]) }
            body = body.trimmingCharacters(in: .whitespacesAndNewlines)
            if body.hasSuffix("```") { body = String(body.dropLast(3)) }
            body = body.trimmingCharacters(in: .whitespacesAndNewlines)
            if valid(body) { return body }
        }
        if let a = trimmed.firstIndex(of: "{"), let b = trimmed.lastIndex(of: "}"), a < b {
            let cand = String(trimmed[a...b])
            if valid(cand) { return cand }
        }
        return text
    }

    // MARK: tool calls

    private func allocate() -> Int {
        defer { nextIndex += 1 }
        return nextIndex
    }

    private func toolDelta(_ tc: JSON) {
        let index = tc["index"]?.int ?? 0
        let id = tc["id"]?.string ?? ""
        let name = tc["function"]?["name"]?.string ?? ""
        let args = tc["function"]?["arguments"]?.string ?? ""
        var call = tools[index]
        if call == nil || (!id.isEmpty && id != call!.callID) {
            if call == nil, id.isEmpty, name.isEmpty, args.isEmpty { return }
            if !jsonBuffer.isEmpty { flushJSON(clean: false) }
            closeReasoning()
            closeMessage()
            let callID = id.isEmpty ? IDs.make("call_") : id
            let isHeld = searchMode && name == WebSearchTool.functionName
            let c = Call(callID: callID, name: name, custom: registry.resolve(name)?.custom ?? false, held: isHeld)
            tools[index] = c
            call = c
            if isHeld {
                held.append(c)
            } else {
                c.outputIndex = allocate()
                toolOrder.append(c)
                emit("response.output_item.added", [("item", item(c, inProgress: true)), ("output_index", .int(c.outputIndex))])
            }
        } else if let c = call, !name.isEmpty, c.name.isEmpty {
            c.name = name
            c.custom = registry.resolve(name)?.custom ?? false
        }
        guard let c = call, !args.isEmpty else { return }
        c.args += args
        // A freeform tool's input is sent whole when the call completes.
        if !c.held, !c.custom {
            emit("response.function_call_arguments.delta", [("delta", .string(args)), ("item_id", .string(c.itemID)),
                                                            ("output_index", .int(c.outputIndex))])
        }
    }

    private func item(_ c: Call, inProgress: Bool) -> JSON {
        let args = inProgress ? "" : (c.args.isEmpty ? "{}" : c.args)
        return registry.outputItem(itemID: c.itemID, callID: c.callID, flatName: c.name, args: args, inProgress: inProgress)
    }

    private func closeCall(_ c: Call) {
        if c.custom {
            let input = ToolRegistry.customInput(c.args)
            emit("response.custom_tool_call_input.delta", [("delta", .string(input)), ("item_id", .string(c.itemID)),
                                                           ("output_index", .int(c.outputIndex))])
            emit("response.custom_tool_call_input.done", [("input", .string(input)), ("item_id", .string(c.itemID)),
                                                          ("output_index", .int(c.outputIndex))])
        } else {
            emit("response.function_call_arguments.done", [("arguments", .string(c.args.isEmpty ? "{}" : c.args)),
                                                           ("item_id", .string(c.itemID)), ("output_index", .int(c.outputIndex))])
        }
        let complete = item(c, inProgress: false)
        emit("response.output_item.done", [("item", complete), ("output_index", .int(c.outputIndex))])
        finished.append((c.outputIndex, complete))
    }

    // MARK: rounds and the end

    /// Closes everything the generation left open and adds its usage to the
    /// response's total.
    func finishRound() {
        guard !roundClosed, !done else { return }
        roundClosed = true
        start()
        for seg in think.flush() { seg.reasoning ? reasoningText(seg.text) : contentText(seg.text) }
        flushJSON(clean: toolOrder.isEmpty)
        closeReasoning()
        closeMessage()
        for c in toolOrder { closeCall(c) }
        if let usage { usageSum.add(usage); self.usage = nil }
    }

    /// The final event: `response.completed`, or `response.incomplete` when
    /// the model hit its output limit, or `response.failed` for a blocked answer.
    func finalize() {
        guard !done else { return }
        finishRound()
        if finishReason == "content_filter" {
            fail(.server(502, "content_filter", "the response was blocked by the backend's content filter"))
            return
        }
        let incomplete = finishReason == "length"
        let output = outputItems
        let response = responseObject(status: incomplete ? "incomplete" : "completed", output: output, extra: [
            ("error", nil), ("incomplete_details", incomplete ? ["reason": "max_output_tokens"] : nil),
            ("output_text", .string(outputText)), ("usage", usageSum.responses)])
        emit(incomplete ? "response.incomplete" : "response.completed", [("response", response)])
        done = true
    }

    /// Ends the response with `response.failed`: only the items that
    /// completed before the failure are in it.
    func fail(_ e: OpenAIError) {
        guard !done else { return }
        start()
        done = true
        failure = e
        var response = responseObject(status: "failed", output: outputItems, extra: [("error", e.object)])
        response["output"] = .array(outputItems)
        emit("response.failed", [("response", response)])
    }

    private var outputItems: [JSON] { finished.sorted { $0.index < $1.index }.map(\.item) }

    /// Readies the translator for the model's next generation (search mode).
    func nextRound() {
        tools = [:]; toolOrder = []; held = []
        finishReason = nil; usage = nil
        roundClosed = false
        think = ThinkTagFilter()
        roundText = ""; roundReasoning = ""
    }

    /// Shows the client one search the engine ran, as a `web_search_call` item.
    func webSearch(_ o: SearchOutcome) {
        start()
        let index = allocate()
        let id = IDs.make("ws_")
        let action: JSON = ["type": "search", "query": .string(o.query)]
        emit("response.output_item.added", [("item", ["id": .string(id), "type": "web_search_call", "status": "in_progress", "action": action]),
                                            ("output_index", .int(index))])
        emit("response.web_search_call.in_progress", [("item_id", .string(id)), ("output_index", .int(index))])
        emit("response.web_search_call.searching", [("item_id", .string(id)), ("output_index", .int(index))])
        emit("response.web_search_call.completed", [("item_id", .string(id)), ("output_index", .int(index))])
        var complete: JSON = ["id": .string(id), "type": "web_search_call", "status": o.errorCode == nil ? "completed" : "failed", "action": action]
        if !o.hits.isEmpty {
            complete["sources"] = .array(o.hits.map { ["type": "url_citation", "url": .string($0.url), "title": .string($0.title)] })
        }
        emit("response.output_item.done", [("item", complete), ("output_index", .int(index))])
        finished.append((index, complete))
        if o.errorCode == nil { searches += 1 }
    }
}
