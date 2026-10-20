import Foundation
import HTTPTypes
import Hummingbird
import NIOCore

/// POST /v1/messages and /v1/messages/count_tokens (docs/DESIGN.md §4).
enum MessagesHandler {
    static let route = "/v1/messages"

    static func handle(_ request: IncomingRequest, env: RequestEnvironment) async -> Response {
        let started = ContinuousClock.now
        func logged(_ r: Response, model: String) -> Response {
            env.log.record(.request(route: route, model: model, status: Int(r.status.code), latencyMS: ms(since: started)))
            return r
        }
        guard let req = try? JSON.parse(request.body), req.object != nil else {
            return logged(.anthropicError(.invalid("invalid JSON request body")), model: "")
        }
        guard let requested = req["model"]?.string, !requested.isEmpty else {
            return logged(.anthropicError(.invalid("missing model field")), model: "")
        }

        // Claude Code's auto-mode classifier: answered by the decision head
        // whatever model it names; a miss runs the same request on the
        // decision model.
        var forced: Model?
        if let autoMode = env.hooks.autoMode {
            let value = req.jsonValue
            if autoMode.isClassifierRequest(value) {
                if let answer = await autoMode.respond(to: value) {
                    let msg = JSON(answer)
                    if req["stream"]?.bool == true {
                        return logged(streamingResponse(channel: request.channel, pingInterval: nil) { sink in
                            emitMessageAsSSE(msg, sink: sink)
                        }, model: requested)
                    }
                    return logged(.json(msg), model: requested)
                }
                forced = autoMode.decisionModel
            }
        }

        guard let model = forced ?? env.routing.resolve(requested) else {
            return logged(.anthropicError(.notFound("unknown model: \(requested) (no main model is configured)")), model: requested)
        }
        guard let target = env.routing.target(model) else {
            return logged(.anthropicError(.notFound("model \(model.id) has no configured backend (\(model.backendID))")), model: model.id)
        }
        if target.isAnthropic {
            return await passthrough(req, path: "/messages", target: target, request: request, env: env, started: started)
        }

        let media = MediaContext(reader: env.hooks.documentReader,
                                 describer: env.config.roles.vision != nil ? env.hooks.imageDescriber : nil,
                                 modelHasVision: model.vision ?? false, modelID: model.id, fetcher: env.client, log: env.log)
        let processed = await MediaPreprocessor.process(req, media)
        if Task.isCancelled { return logged(.anthropicError(.api(499, "cancelled")), model: model.id) }

        let webSearch = WebSearchTool.find(in: req["tools"]?.array)
        let window = await env.contextWindows.window(for: target)
        let chatReq = RequestTranslator.chatRequest(processed, model: model, contextWindow: window, webSearch: webSearch)
        let call = Call(requested: requested, target: target, chatReq: chatReq,
                        stopSequences: req["stop_sequences"]?.array?.compactMap(\.string) ?? [],
                        stream: req["stream"]?.bool ?? false, env: env, channel: request.channel, started: started)
        if let webSearch {
            return await WebSearchLoop(call: call, tool: webSearch).run()
        }
        return await call.run()
    }

    /// One translated generation.
    struct Call: Sendable {
        var requested: String
        var target: BackendTarget
        var chatReq: JSON
        var stopSequences: [String]
        var stream: Bool
        var env: RequestEnvironment
        var channel: (any Channel)?
        var started: ContinuousClock.Instant

        func log(_ status: Int) {
            env.log.record(.request(route: MessagesHandler.route, model: target.model.id, status: status, latencyMS: ms(since: started)))
        }

        func fail(_ e: AnthropicError) -> Response {
            log(e.status)
            if e.status >= 500 { env.log.record(.error("\(target.model.id): \(e.message)")) }
            return .anthropicError(e)
        }

        /// Sends `body` and returns the response, or the client error for a
        /// failure before anything was streamed.
        func send(_ body: JSON, stream: Bool) async -> Result<UpstreamResponse, AnthropicError> {
            do {
                let resp = try await env.client.send(target.chatRequest(body, stream: stream), headerTimeout: env.headerTimeout)
                guard resp.status < 400 else {
                    let data = (try? await resp.collect(limit: 1 << 20)) ?? Data()
                    return .failure(buildClientError(status: resp.status, body: data, headers: resp.headers))
                }
                return .success(resp)
            } catch {
                return .failure(.transport(error))
            }
        }

        func run() async -> Response {
            let resp: UpstreamResponse
            switch await send(chatReq, stream: stream) {
            case .failure(let e): return fail(e)
            case .success(let r): resp = r
            }
            if !stream || !resp.isEventStream {
                guard let data = try? await resp.collect(), let chat = try? JSON.parse(data) else {
                    return fail(.api(502, "the backend returned an invalid response"))
                }
                guard let msg = ResponseTranslator.message(chat, model: requested, stopSequences: stopSequences) else {
                    if let err = chat["error"] { return fail(StreamTranslator.inBandError(err)) }
                    return fail(.api(502, "the backend returned no choices"))
                }
                log(200)
                if stream {
                    return streamingResponse(channel: channel, pingInterval: nil) { sink in emitMessageAsSSE(msg, sink: sink) }
                }
                return .json(msg)
            }
            let call = self
            return streamingResponse(channel: channel, pingInterval: env.tuning.pingInterval) { sink in
                let t = StreamTranslator(sink: sink, model: call.requested, stopSequences: call.stopSequences, searchMode: false)
                t.start()
                if await pump(resp, into: t) {
                    t.finalize()
                } else if !t.done {
                    t.fail(.truncated)
                }
                call.log(t.failure?.status ?? 200)
            }
        }
    }

    /// Reads one generation into the translator. True when it completed: a
    /// finish reason arrived and the stream was not cut.
    static func pump(_ resp: UpstreamResponse, into t: some ChatLineConsumer) async -> Bool {
        var reader = resp.lines()
        do {
            while let line = try await reader.next() {
                switch t.line(line) {
                case .more: continue
                case .doneMarker: return t.finishReason != nil
                case .failed: return false
                }
            }
        } catch {
            return false
        }
        return t.finishReason != nil
    }

    // MARK: native

    /// An Anthropic backend: the request as sent, model name rewritten.
    static func passthrough(_ req: JSON, path: String, target: BackendTarget, request: IncomingRequest,
                            env: RequestEnvironment, started: ContinuousClock.Instant) async -> Response {
        func log(_ status: Int) {
            env.log.record(.request(route: "/v1" + path, model: target.model.id, status: status, latencyMS: ms(since: started)))
        }
        var body = req
        body["model"] = .string(target.model.upstreamModel)
        let resp: UpstreamResponse
        do {
            resp = try await env.client.send(target.anthropicRequest(path: path, body: body.data, clientHeaders: request.headers),
                                             headerTimeout: env.headerTimeout)
        } catch {
            let e = AnthropicError.transport(error)
            log(e.status)
            return .anthropicError(e)
        }
        if path == "/messages/count_tokens", [404, 405, 501].contains(resp.status) {
            log(200)
            return countEstimate(request.body)
        }
        guard resp.status < 400 else {
            let data = (try? await resp.collect(limit: 1 << 20)) ?? Data()
            let e = buildClientError(status: resp.status, body: data, headers: resp.headers, native: true)
            log(e.status)
            return .anthropicError(e)
        }
        var headers: HTTPFields = [:]
        for (k, v) in retryHeaders(resp.headers) + [("request-id", resp.headers["request-id"] ?? "")] where !v.isEmpty {
            if let n = HTTPField.Name(k) { headers[n] = v }
        }
        log(resp.status)
        if resp.isEventStream {
            return streamingResponse(status: HTTPResponse.Status(code: resp.status), headers: headers, channel: request.channel,
                                     pingInterval: nil) { sink in
                do { for try await chunk in resp.body { sink.raw(bytes: chunk) } } catch {}
            }
        }
        guard let data = try? await resp.collect() else { return .anthropicError(.api(502, "failed to read the backend's response")) }
        headers[.contentType] = resp.headers["content-type"] ?? "application/json"
        return Response(status: HTTPResponse.Status(code: resp.status), headers: headers, body: ResponseBody(byteBuffer: ByteBuffer(bytes: data)))
    }

    // MARK: count_tokens

    static func countTokens(_ request: IncomingRequest, env: RequestEnvironment) async -> Response {
        guard let req = try? JSON.parse(request.body), let requested = req["model"]?.string, !requested.isEmpty else {
            return .anthropicError(.invalid("missing model field"))
        }
        guard let model = env.routing.resolve(requested) else {
            return .anthropicError(.notFound("unknown model: \(requested) (no main model is configured)"))
        }
        if let target = env.routing.target(model), target.isAnthropic {
            return await passthrough(req, path: "/messages/count_tokens", target: target, request: request, env: env, started: .now)
        }
        return countEstimate(request.body)
    }

    static func countEstimate(_ body: Data) -> Response {
        .json(["input_tokens": .int(TokenEstimate.tokens(body))])
    }
}

func ms(since start: ContinuousClock.Instant) -> Int {
    Int((ContinuousClock.now - start) / .milliseconds(1))
}
