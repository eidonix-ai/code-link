import Foundation
import HTTPTypes
import Hummingbird
import NIOCore
import Synchronization

/// Where a streaming response's events go. Producers (the translator, the
/// ping timer) enqueue whole events; the response body writes them in order
/// and flushes each one.
final class SSESink: Sendable {
    private let continuation: AsyncStream<ByteBuffer>.Continuation
    let stream: AsyncStream<ByteBuffer>
    private struct State { var lastWrite = ContinuousClock.now; var finished = false }
    private let state = Mutex(State())

    init() {
        (stream, continuation) = AsyncStream.makeStream(of: ByteBuffer.self)
    }

    var isFinished: Bool { state.withLock { $0.finished } }
    var idle: Duration { ContinuousClock.now - state.withLock { $0.lastWrite } }

    func raw(_ s: String) {
        let done = state.withLock { s -> Bool in
            if s.finished { return true }
            s.lastWrite = .now
            return false
        }
        if !done { continuation.yield(ByteBuffer(string: s)) }
    }

    /// `event: <name>` with `data` (its `type` set to the event name, first).
    func event(_ name: String, _ data: JSON = [:]) {
        var o = JSONObject([("type", .string(name))])
        for (k, v) in data.object ?? JSONObject() where k != "type" { o[k] = v }
        raw("event: \(name)\ndata: \(JSON.object(o).text)\n\n")
    }

    func ping() { event("ping") }

    func finish() {
        state.withLock { $0.finished = true }
        continuation.finish()
    }
}

/// A streaming HTTP response: `produce` fills the sink while the body
/// writes it out. Pings go out whenever the stream has been quiet for
/// `pingInterval`. A client that goes away (a failed write, or the
/// connection closing) cancels `produce`, and with it the upstream request.
func streamingResponse(status: HTTPResponse.Status = .ok, headers extra: HTTPFields = [:], channel: (any Channel)?,
                       pingInterval: Duration?, produce: @escaping @Sendable (SSESink) async -> Void) -> Response {
    var headers = extra
    headers[.contentType] = "text/event-stream"
    headers[.cacheControl] = "no-cache"
    let body = ResponseBody { writer in
        let sink = SSESink()
        channel?.closeFuture.whenComplete { _ in sink.finish() }
        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask {
                await produce(sink)
                sink.finish()
            }
            if let pingInterval {
                group.addTask {
                    while !sink.isFinished {
                        let idle = sink.idle
                        if idle >= pingInterval { sink.ping(); continue }
                        do { try await Task.sleep(for: pingInterval - idle) } catch { return }
                    }
                }
            }
            do {
                for await chunk in sink.stream { try await writer.write(chunk) }
            } catch {
                sink.finish()
                group.cancelAll()
                throw error
            }
            group.cancelAll()
        }
        try await writer.finish(nil)
    }
    return Response(status: status, headers: headers, body: body)
}

extension Response {
    static func json(_ j: JSON, status: HTTPResponse.Status = .ok, headers extra: [(String, String)] = []) -> Response {
        var headers: HTTPFields = [.contentType: "application/json"]
        for (k, v) in extra { if let name = HTTPField.Name(k) { headers[name] = v } }
        return Response(status: status, headers: headers, body: ResponseBody(byteBuffer: ByteBuffer(bytes: j.bytes)))
    }

    static func anthropicError(_ e: AnthropicError) -> Response {
        json(e.json, status: HTTPResponse.Status(code: e.status), headers: e.headers)
    }
}
