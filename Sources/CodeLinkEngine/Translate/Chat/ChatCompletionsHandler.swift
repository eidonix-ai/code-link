import Foundation
import HTTPTypes
import Hummingbird
import NIOCore

/// POST /v1/chat/completions for every client that speaks Chat Completions
/// (opencode, qwen-code, aider, editor plugins, scripts; docs/DESIGN.md §5a).
///   - An OpenAI-compatible backend gets the request with its upstream model
///     name, and its answer passes through unchanged, chunk by chunk.
///   - An Anthropic backend gets the request translated to Messages, and the
///     answer is translated back.
/// Either way documents and images are handled as on the other routes.
enum ChatCompletionsHandler {
    static let route = "/v1/chat/completions"

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
        guard let messages = req["messages"]?.array, !messages.isEmpty else {
            return logged(.invalid("messages must be a non-empty array"), model: requested)
        }
        guard let model = env.routing.resolve(requested) else {
            return logged(.notFound("unknown model: \(requested) (no main model is configured)"), model: requested)
        }
        guard let target = env.routing.target(model) else {
            return logged(.notFound("model \(model.id) has no configured backend (\(model.backendID))"), model: model.id)
        }

        // Documents and images. A Claude model sees images unless told not to.
        let media = MediaContext(reader: env.hooks.documentReader,
                                 describer: env.config.roles.vision != nil ? env.hooks.imageDescriber : nil,
                                 modelHasVision: model.vision ?? target.isAnthropic, modelID: model.id,
                                 fetcher: env.client, log: env.log)
        var body = req
        body["messages"] = .array(await OpenAIMedia.process(chatMessages: messages, media))
        if Task.isCancelled { return logged(.server(499, "cancelled", "cancelled"), model: model.id) }

        let call = Call(requested: requested, target: target, stream: req["stream"]?.bool ?? false,
                        includeUsage: req["stream_options"]?["include_usage"]?.bool ?? false,
                        env: env, channel: request.channel, started: started)
        return target.isAnthropic ? await call.anthropic(body) : await call.forward(body)
    }

    struct Call: Sendable {
        var requested: String
        var target: BackendTarget
        var stream: Bool
        var includeUsage: Bool
        var env: RequestEnvironment
        var channel: (any Channel)?
        var started: ContinuousClock.Instant

        func log(_ status: Int) {
            env.log.record(.request(route: ChatCompletionsHandler.route, model: target.model.id, status: status, latencyMS: ms(since: started)))
        }

        func fail(_ e: OpenAIError) -> Response {
            log(e.status)
            if e.status >= 500 { env.log.record(.error("\(target.model.id): \(e.message)")) }
            return e.response
        }

        // MARK: OpenAI-compatible backends

        /// The request as sent, with the upstream model name; the answer as received.
        func forward(_ body: JSON) async -> Response {
            let resp: UpstreamResponse
            do {
                resp = try await env.client.send(target.chatRequest(body, stream: stream), headerTimeout: env.headerTimeout)
            } catch {
                return fail(.transport(error))
            }
            guard resp.status < 400 else {
                let data = (try? await resp.collect(limit: 1 << 20)) ?? Data()
                return upstreamError(status: resp.status, body: data, headers: resp.headers)
            }
            log(resp.status)
            if resp.isEventStream {
                return streamingResponse(status: HTTPResponse.Status(code: resp.status), channel: channel, pingInterval: nil) { sink in
                    do { for try await chunk in resp.body { sink.raw(bytes: chunk) } } catch {}
                }
            }
            guard let data = try? await resp.collect() else { return fail(.server(502, "upstream_error", "failed to read the backend's response")) }
            var headers: HTTPFields = [.contentType: resp.headers["content-type"] ?? "application/json"]
            if let id = resp.headers["x-request-id"], let n = HTTPField.Name("x-request-id") { headers[n] = id }
            return Response(status: HTTPResponse.Status(code: resp.status), headers: headers, body: ResponseBody(byteBuffer: ByteBuffer(bytes: data)))
        }

        /// A backend's error: its body and status as they are. The engine's own
        /// credentials failing (401/403) is a gateway problem, never the
        /// client's, so it is a 502.
        func upstreamError(status: Int, body: Data, headers: [String: String]) -> Response {
            log(status)
            if status >= 500 { env.log.record(.error("\(target.model.id): backend returned HTTP \(status)")) }
            if status == 401 || status == 403 {
                return OpenAIError.upstream(status: status, body: body, headers: headers).response
            }
            var out = HTTPFields()
            out[.contentType] = "application/json"
            if let r = headers["retry-after"] { out[.init("retry-after")!] = r }
            if let j = try? JSON.parse(body), j["error"] != nil {
                return Response(status: HTTPResponse.Status(code: status), headers: out, body: ResponseBody(byteBuffer: ByteBuffer(bytes: body)))
            }
            let info = parseUpstreamError(body)
            let e = OpenAIError(status: status, type: status < 500 ? "invalid_request_error" : "server_error", code: nil,
                                message: scrubUpstreamMessage(info.message).isEmpty ? "backend returned HTTP \(status)" : scrubUpstreamMessage(info.message),
                                retryAfter: headers["retry-after"])
            return e.response
        }

        // MARK: Anthropic backends

        func anthropic(_ body: JSON) async -> Response {
            guard let upstream = ChatToAnthropic.request(body, upstreamModel: target.model.upstreamModel) else {
                return fail(.invalid("at least one user or assistant message is required"))
            }
            let resp: UpstreamResponse
            do {
                let clientHeaders = ["accept": stream ? "text/event-stream" : "application/json"]
                resp = try await env.client.send(target.anthropicRequest(path: "/messages", body: upstream.data, clientHeaders: clientHeaders),
                                                 headerTimeout: env.headerTimeout)
            } catch {
                return fail(.transport(error))
            }
            guard resp.status < 400 else {
                let data = (try? await resp.collect(limit: 1 << 20)) ?? Data()
                return fail(.fromAnthropic(buildClientError(status: resp.status, body: data, headers: resp.headers, native: true)))
            }
            if stream, resp.isEventStream {
                let call = self
                return streamingResponse(channel: channel, pingInterval: env.tuning.pingInterval, commentPings: true) { sink in
                    let t = AnthropicChatStream(sink: sink, model: call.requested, includeUsage: call.includeUsage)
                    var reader = resp.lines()
                    do {
                        while let line = try await reader.next(), t.line(line) == .more {}
                    } catch {}
                    t.finish()
                    call.log(t.failed ? 502 : 200)
                }
            }
            guard let data = try? await resp.collect(), let message = try? JSON.parse(data),
                  let chat = ChatToAnthropic.response(message, model: requested) else {
                return fail(.server(502, "upstream_error", "the backend returned an invalid response"))
            }
            log(200)
            if stream {
                // Streaming was asked for but a whole message came back.
                return streamingResponse(channel: channel, pingInterval: nil) { sink in
                    AnthropicChatStream.replay(chat, sink: sink, includeUsage: includeUsage)
                }
            }
            return .json(chat)
        }
    }
}
