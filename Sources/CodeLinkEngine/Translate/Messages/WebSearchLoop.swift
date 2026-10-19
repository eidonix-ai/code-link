import Foundation
import Hummingbird
import NIOCore

/// Claude Code's WebSearch on a chat model (docs/DESIGN.md §4.5). Claude Code
/// runs WebSearch as a separate request whose only tool is the server tool
/// `web_search_20250305`; on the real API the server searches and the answer
/// carries server_tool_use + web_search_tool_result blocks. Here the model
/// gets a `web_search(query)` function, and when it calls it the engine
/// searches, shows the client the pair, hands the results back to the model
/// and generates again, until the model stops searching or max_uses is
/// reached. A search that can't run is a web_search_tool_result_error the
/// model sees too; the request never fails because of it.
struct WebSearchLoop: Sendable {
    let call: MessagesHandler.Call
    let tool: WebSearchTool

    /// Every searched round uses a search; the extra rounds let the model be
    /// told the limit was reached, and the last round has no tools so it
    /// must answer.
    var maxRounds: Int { tool.maxUses + 2 }

    /// The Chat Completions request of a round.
    func request(round: Int, messages: [JSON]) -> JSON {
        Self.request(call.chatReq, round: round, maxRounds: maxRounds, messages: messages)
    }

    /// The Chat Completions request of a round for any route's base request.
    static func request(_ base: JSON, round: Int, maxRounds: Int, messages: [JSON]) -> JSON {
        var r = base
        r["messages"] = .array(messages)
        if round >= maxRounds - 1 {
            r["tools"] = nil
            r["tool_choice"] = nil
            r["parallel_tool_calls"] = nil
        } else if round > 0, let tc = r["tool_choice"], tc != "none" {
            r["tool_choice"] = "auto" // a forced call would be forced every round
        }
        return r
    }

    func run() async -> Response {
        let first: UpstreamResponse
        switch await call.send(request(round: 0, messages: call.chatReq["messages"]?.array ?? []), stream: call.stream) {
        case .failure(let e): return call.fail(e)
        case .success(let r): first = r
        }
        let runner = SearchRunner(tool: tool, searcher: call.env.hooks.webSearcher, log: call.env.log)
        if !call.stream || !first.isEventStream {
            return await blocking(first: first, runner: runner)
        }
        let loop = self
        return streamingResponse(channel: call.channel, pingInterval: call.env.tuning.pingInterval) { sink in
            await loop.stream(first: first, sink: sink)
        }
    }

    private func stream(first: UpstreamResponse, sink: SSESink) async {
        let runner = SearchRunner(tool: tool, searcher: call.env.hooks.webSearcher, log: call.env.log)
        let t = StreamTranslator(sink: sink, model: call.requested, stopSequences: call.stopSequences, searchMode: true)
        t.start()
        var messages = call.chatReq["messages"]?.array ?? []
        var resp = first
        defer { call.log(t.failure?.status ?? 200) }
        for round in 0..<maxRounds {
            if round > 0 {
                t.nextRound()
                switch await call.send(request(round: round, messages: messages), stream: true) {
                case .failure(let e): t.fail(e); return
                case .success(let r): resp = r
                }
            }
            let complete = await MessagesHandler.pump(resp, into: t)
            if t.done { return }
            if !complete { t.fail(.truncated); return }
            t.finishRound()
            if t.held.isEmpty { t.finalize(); return }

            sink.ping() // the searches take a moment
            let outcomes = await runner.answer(t.held.map { ($0.callID, $0.args) })
            if Task.isCancelled { return }
            for o in outcomes { t.serverSearch(o) }
            if !t.toolOrder.isEmpty || round >= maxRounds - 1 {
                // The client answers its own tool calls; its next request
                // replays the searches from history.
                t.finalize()
                return
            }
            var assistant: JSON = ["role": "assistant", "content": t.roundText.isEmpty ? nil : .string(t.roundText)]
            if !t.roundReasoning.isEmpty {
                assistant["reasoning_content"] = .string(t.roundReasoning)
                assistant["reasoning"] = .string(t.roundReasoning)
            }
            assistant["tool_calls"] = .array(t.held.map {
                RequestTranslator.functionCall(id: $0.callID, name: WebSearchTool.functionName,
                                               input: (try? JSON.parse($0.args.isEmpty ? "{}" : $0.args)) ?? [:])
            })
            messages.append(assistant)
            for o in outcomes { messages.append(["role": "tool", "tool_call_id": .string(o.callID), "content": .string(o.modelText)]) }
        }
        t.finalize()
    }

    private func blocking(first: UpstreamResponse, runner: SearchRunner) async -> Response {
        var messages = call.chatReq["messages"]?.array ?? []
        var blocks: [JSON] = []
        var usage = ChatUsage()
        var resp = first
        var last: JSON = [:]
        for round in 0..<maxRounds {
            if round > 0 {
                switch await call.send(request(round: round, messages: messages), stream: false) {
                case .failure(let e): return call.fail(e)
                case .success(let r): resp = r
                }
            }
            guard let data = try? await resp.collect(), let chat = try? JSON.parse(data),
                  let msg = ResponseTranslator.message(chat, model: call.requested, stopSequences: call.stopSequences) else {
                return call.fail(.api(502, "the backend returned an invalid response"))
            }
            usage.add(ChatUsage(chat["usage"]) ?? ChatUsage())
            last = msg
            var calls: [(id: String, args: String)] = []
            var regular = 0
            for b in msg["content"]?.array ?? [] {
                if b.type == "tool_use", b["name"]?.string == WebSearchTool.functionName {
                    calls.append((b["id"]?.string ?? "", (b["input"] ?? [:]).text))
                    continue
                }
                if b.type == "tool_use" { regular += 1 }
                blocks.append(b)
            }
            var outcomes: [SearchOutcome] = []
            if !calls.isEmpty {
                outcomes = await runner.answer(calls)
                for o in outcomes { blocks += o.blocks }
            }
            if calls.isEmpty || regular > 0 || round >= maxRounds - 1 || Task.isCancelled { break }
            var assistant = chat["choices"]?[0]?["message"] ?? ["role": "assistant"]
            assistant["role"] = "assistant"
            if assistant["content"] == nil { assistant["content"] = .null }
            messages.append(assistant)
            for o in outcomes { messages.append(["role": "tool", "tool_call_id": .string(o.callID), "content": .string(o.modelText)]) }
        }
        let hasClientCalls = blocks.contains { $0.type == "tool_use" }
        var stop = last["stop_reason"]?.string ?? "end_turn"
        if hasClientCalls { stop = "tool_use" } else if stop == "tool_use" { stop = "end_turn" }
        var msg = last
        msg["content"] = .array(blocks)
        msg["stop_reason"] = .string(stop)
        if stop != "stop_sequence" { msg["stop_sequence"] = .null }
        msg["usage"] = usage.anthropic(webSearches: runner.succeeded)
        call.log(200)
        let final = msg
        if call.stream {
            return streamingResponse(channel: call.channel, pingInterval: nil) { sink in emitMessageAsSSE(final, sink: sink) }
        }
        return .json(final)
    }
}
