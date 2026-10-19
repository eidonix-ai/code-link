import Foundation
import Hummingbird
import NIOCore

/// POST /v1/responses for Codex (docs/DESIGN.md §5): the Responses API on a
/// Chat Completions backend.
struct CodexResponses: ResponsesHandler {
    func respond(_ request: IncomingRequest, env: RequestEnvironment) async -> Response {
        await ResponsesRoute.handle(request, env: env)
    }
}

enum ResponsesRoute {
    static let route = "/v1/responses"
    /// The model Codex's auto-review asks for.
    static let reviewModel = "codex-auto-review"

    static func handle(_ request: IncomingRequest, env: RequestEnvironment) async -> Response {
        let started = ContinuousClock.now
        func logged(_ e: OpenAIError, model: String) -> Response {
            env.log.record(.request(route: route, model: model, status: e.status, latencyMS: ms(since: started)))
            return e.response
        }
        guard let req = try? JSON.parse(request.body), req.object != nil else {
            return logged(.invalid("invalid JSON request body"), model: "")
        }
        guard let requested = req["model"]?.string, !requested.isEmpty else {
            return logged(.invalid("missing model field in request"), model: "")
        }
        // Server-side conversation state cannot be kept over Chat Completions.
        if let id = req["previous_response_id"]?.string, !id.isEmpty {
            return logged(.invalid("previous_response_id is not supported; resend the full conversation in input",
                                   code: "previous_response_id_not_supported"), model: requested)
        }
        if req["store"]?.bool == true {
            return logged(.invalid("store=true is not supported; set store to false", code: "store_not_supported"), model: requested)
        }
        guard let model = resolve(requested, env) else {
            return logged(.notFound("unknown model: \(requested) (no main model is configured)"), model: requested)
        }
        guard let target = env.routing.target(model) else {
            return logged(.notFound("model \(model.id) has no configured backend (\(model.backendID))"), model: model.id)
        }
        guard !target.isAnthropic else {
            return logged(.invalid("the Responses API needs an OpenAI-compatible backend; \(model.id) is on an Anthropic backend",
                                   code: "unsupported_backend"), model: model.id)
        }

        // Documents and images.
        let media = MediaContext(reader: env.hooks.documentReader,
                                 describer: env.config.roles.vision != nil ? env.hooks.imageDescriber : nil,
                                 modelHasVision: model.vision ?? false, modelID: model.id, fetcher: env.client, log: env.log)
        var input = req["input"]
        if let items = input?.array { input = .array(await OpenAIMedia.process(responsesInput: items, media)) }
        if Task.isCancelled { return logged(OpenAIError.server(499, "cancelled", "cancelled"), model: model.id) }

        let translated = ToolRegistry.translate((req["tools"]?.array ?? []) + ResponsesRequestTranslator.additionalTools(input))
        let registry = translated.registry
        let webSearch = searchTool(translated.stripped, registry: registry)
        guard let messages = ResponsesRequestTranslator.messages(input: input, instructions: req["instructions"]?.string ?? "",
                                                                  registry: registry) else {
            return logged(.invalid("no valid input items", code: "invalid_prompt"), model: model.id)
        }
        let window = await env.contextWindows.window(for: target)
        let chatReq = ResponsesRequestTranslator.chatRequest(req, messages: messages,
                                                             tools: translated.tools + (webSearch.map { [$0.function] } ?? []),
                                                             registry: registry, model: model, contextWindow: window)
        let call = Call(requested: requested, target: target, chatReq: chatReq, registry: registry,
                        jsonMode: chatReq["response_format"] != nil, stream: req["stream"]?.bool ?? false,
                        env: env, channel: request.channel, started: started)
        if let webSearch { return await ResponsesSearchLoop(call: call, tool: webSearch).run() }
        return await call.run()
    }

    /// The model a request names: a configured id, a tier or Claude name as
    /// on the Messages route, `codex-auto-review` → the decision model
    /// (answering generatively), anything else → roles.codex, else main.
    static func resolve(_ requested: String, _ env: RequestEnvironment) -> Model? {
        let routing = env.routing
        let roles = env.config.roles
        if requested == reviewModel {
            return env.hooks.autoMode?.decisionModel ?? routing.model(id: roles.decision) ?? routing.model(id: roles.codex) ?? routing.mainModel
        }
        if let m = routing.model(id: requested) { return m }
        let lower = requested.lowercased()
        if ["opus", "sonnet", "haiku"].contains(lower) || lower.hasPrefix("claude") { return routing.resolve(requested) }
        if let m = env.config.models.first(where: { $0.id.lowercased() == lower }) { return m }
        return routing.model(id: roles.codex) ?? routing.mainModel
    }

    /// Codex's `web_search` tool as the engine's search loop. A function tool
    /// of the same name the client defined wins.
    static func searchTool(_ stripped: [JSON], registry: ToolRegistry) -> WebSearchTool? {
        guard registry.resolve(WebSearchTool.functionName) == nil,
              let t = stripped.first(where: { $0.type?.hasPrefix("web_search") == true }) else { return nil }
        return WebSearchTool(maxUses: WebSearchTool.defaultMaxUses,
                             allowedDomains: t["filters"]?["allowed_domains"]?.array?.compactMap(\.string),
                             blockedDomains: nil)
    }

    /// One translated generation.
    struct Call: Sendable {
        var requested: String
        var target: BackendTarget
        var chatReq: JSON
        var registry: ToolRegistry
        var jsonMode: Bool
        var stream: Bool
        var env: RequestEnvironment
        var channel: (any Channel)?
        var started: ContinuousClock.Instant

        func log(_ status: Int) {
            env.log.record(.request(route: ResponsesRoute.route, model: target.model.id, status: status, latencyMS: ms(since: started)))
        }

        func fail(_ e: OpenAIError) -> Response {
            log(e.status)
            if e.status >= 500 { env.log.record(.error("\(target.model.id): \(e.message)")) }
            return e.response
        }

        /// Sends `body` and returns the response, or the client error for a
        /// failure before anything was streamed.
        func send(_ body: JSON, stream: Bool) async -> Result<UpstreamResponse, OpenAIError> {
            do {
                let resp = try await env.client.send(target.chatRequest(body, stream: stream), headerTimeout: env.headerTimeout)
                guard resp.status < 400 else {
                    let data = (try? await resp.collect(limit: 1 << 20)) ?? Data()
                    return .failure(.upstream(status: resp.status, body: data, headers: resp.headers))
                }
                return .success(resp)
            } catch {
                return .failure(.transport(error))
            }
        }

        func translator(_ sink: SSESink, searchMode: Bool) -> ResponsesStreamTranslator {
            ResponsesStreamTranslator(sink: sink, model: requested, registry: registry, jsonMode: jsonMode, searchMode: searchMode)
        }

        func run() async -> Response {
            let resp: UpstreamResponse
            switch await send(chatReq, stream: stream) {
            case .failure(let e): return fail(e)
            case .success(let r): resp = r
            }
            if stream, resp.isEventStream {
                let call = self
                return streamingResponse(channel: channel, pingInterval: env.tuning.pingInterval, commentPings: true) { sink in
                    let t = call.translator(sink, searchMode: false)
                    t.start()
                    if await MessagesHandler.pump(resp, into: t) {
                        t.finalize()
                    } else if !t.done {
                        t.fail(.truncated)
                    }
                    call.log(t.failure?.status ?? 200)
                }
            }
            guard let data = try? await resp.collect(), let chat = try? JSON.parse(data) else {
                return fail(.server(502, "invalid_upstream_response", "the backend returned an invalid response"))
            }
            if stream {
                // The backend answered a streaming request with a plain body.
                guard let line = ResponsesResponse.chunkLine(chat) else { return fail(blockingError(chat)) }
                let call = self
                log(200)
                return streamingResponse(channel: channel, pingInterval: nil, commentPings: true) { sink in
                    let t = call.translator(sink, searchMode: false)
                    t.start()
                    _ = t.line(line)
                    t.finalize()
                }
            }
            guard let round = ResponsesResponse.round(chat, registry: registry, jsonMode: jsonMode, searchMode: false) else {
                return fail(blockingError(chat))
            }
            log(200)
            return .json(ResponsesResponse.response(model: requested, items: round.items, text: round.text,
                                                    finishReason: round.finishReason, usage: round.usage))
        }

        func blockingError(_ chat: JSON) -> OpenAIError {
            if let err = chat["error"], !err.isNull { return .inBand(err) }
            return .server(502, "invalid_upstream_response", "the backend returned no choices")
        }
    }
}

/// Codex's `web_search` tool on a chat model (docs/DESIGN.md §4.5): the model
/// gets a `web_search(query)` function; on a call the engine searches, shows
/// the client a `web_search_call` item, hands the results back and generates
/// again, until the model stops searching or max_uses is reached. The same
/// loop as the Messages route's, with Responses events.
struct ResponsesSearchLoop: Sendable {
    let call: ResponsesRoute.Call
    let tool: WebSearchTool

    var maxRounds: Int { tool.maxUses + 2 }

    func request(round: Int, messages: [JSON]) -> JSON {
        WebSearchLoop.request(call.chatReq, round: round, maxRounds: maxRounds, messages: messages)
    }

    func run() async -> Response {
        let first: UpstreamResponse
        switch await call.send(request(round: 0, messages: call.chatReq["messages"]?.array ?? []), stream: call.stream) {
        case .failure(let e): return call.fail(e)
        case .success(let r): first = r
        }
        let loop = self
        if call.stream {
            guard first.isEventStream else {
                return call.fail(.server(502, "invalid_upstream_response", "the backend did not stream its answer"))
            }
            return streamingResponse(channel: call.channel, pingInterval: call.env.tuning.pingInterval, commentPings: true) { sink in
                await loop.stream(first: first, sink: sink)
            }
        }
        switch await blocking(first: first) {
        case .failure(let e): return call.fail(e)
        case .success(let json):
            call.log(200)
            return .json(json)
        }
    }

    /// The assistant message and tool results that carry a round's searches
    /// back to the model.
    private func continuation(text: String, reasoning: String, searches: [(id: String, args: String)],
                              outcomes: [SearchOutcome]) -> [JSON] {
        var assistant: JSON = ["role": "assistant", "content": text.isEmpty ? nil : .string(text)]
        if !reasoning.isEmpty {
            assistant["reasoning_content"] = .string(reasoning)
            assistant["reasoning"] = .string(reasoning)
        }
        assistant["tool_calls"] = .array(searches.map {
            RequestTranslator.functionCall(id: $0.id, name: WebSearchTool.functionName,
                                           input: (try? JSON.parse($0.args.isEmpty ? "{}" : $0.args)) ?? [:])
        })
        return [assistant] + outcomes.map { ["role": "tool", "tool_call_id": .string($0.callID), "content": .string($0.modelText)] }
    }

    private func stream(first: UpstreamResponse, sink: SSESink) async {
        let runner = SearchRunner(tool: tool, searcher: call.env.hooks.webSearcher, log: call.env.log)
        let t = call.translator(sink, searchMode: true)
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
            let searches = t.held.map { (id: $0.callID, args: $0.args) }
            let outcomes = await runner.answer(searches)
            if Task.isCancelled { return }
            for o in outcomes { t.webSearch(o) }
            if !t.toolOrder.isEmpty || round >= maxRounds - 1 {
                // The client answers its own tool calls; its next request
                // carries the searches in history.
                t.finalize()
                return
            }
            messages += continuation(text: t.roundText, reasoning: t.roundReasoning, searches: searches, outcomes: outcomes)
        }
        t.finalize()
    }

    private func blocking(first: UpstreamResponse) async -> Result<JSON, OpenAIError> {
        let runner = SearchRunner(tool: tool, searcher: call.env.hooks.webSearcher, log: call.env.log)
        var messages = call.chatReq["messages"]?.array ?? []
        var items: [JSON] = []
        var text = ""
        var usage = ChatUsage()
        var finish: String?
        var resp = first
        for round in 0..<maxRounds {
            if round > 0 {
                switch await call.send(request(round: round, messages: messages), stream: false) {
                case .failure(let e): return .failure(e)
                case .success(let r): resp = r
                }
            }
            guard let data = try? await resp.collect(), let chat = try? JSON.parse(data) else {
                return .failure(.server(502, "invalid_upstream_response", "the backend returned an invalid response"))
            }
            guard let r = ResponsesResponse.round(chat, registry: call.registry, jsonMode: call.jsonMode, searchMode: true) else {
                return .failure(call.blockingError(chat))
            }
            usage.add(r.usage)
            finish = r.finishReason
            items += r.items
            text += r.text
            if r.searchCalls.isEmpty { break }
            let outcomes = await runner.answer(r.searchCalls)
            items += outcomes.map(ResponsesResponse.webSearchItem)
            let clientCalls = r.items.contains { $0.type == "function_call" || $0.type == "custom_tool_call" }
            if clientCalls || round >= maxRounds - 1 || Task.isCancelled { break }
            var assistant = chat["choices"]?[0]?["message"] ?? ["role": "assistant"]
            assistant["role"] = "assistant"
            if assistant["content"] == nil { assistant["content"] = .null }
            messages.append(assistant)
            for o in outcomes { messages.append(["role": "tool", "tool_call_id": .string(o.callID), "content": .string(o.modelText)]) }
        }
        return .success(ResponsesResponse.response(model: call.requested, items: items, text: text, finishReason: finish, usage: usage))
    }
}
