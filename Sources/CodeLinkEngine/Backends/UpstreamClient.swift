import Foundation
import Synchronization

/// Why an upstream exchange failed before a response arrived.
enum UpstreamError: Error, Equatable, CustomStringConvertible {
    /// No response headers within the header wait.
    case headerTimeout
    /// Connecting or sending failed.
    case transport(String)
    /// An SSE line above the limit.
    case lineTooLong
    /// A body above the limit.
    case bodyTooLarge
    case cancelled

    var description: String {
        switch self {
        case .headerTimeout: return "the backend did not answer in time"
        case .transport(let m): return m
        case .lineTooLong: return "the backend sent a stream line over 16 MiB"
        case .bodyTooLarge: return "the backend's response is too large"
        case .cancelled: return "cancelled"
        }
    }
}

struct UpstreamRequest: Sendable {
    var url: URL
    var method: String = "POST"
    var headers: [(String, String)] = []
    var body: Data?
    /// Redirects are refused when they lead to a loopback, private or
    /// link-local address (fetching documents by URL).
    var refusePrivateRedirects = false
}

/// A response whose headers are in. The body arrives as it is sent; dropping
/// the response cancels the exchange.
final class UpstreamResponse: Sendable {
    let status: Int
    /// Lower-cased header names.
    let headers: [String: String]
    let body: AsyncThrowingStream<Data, any Error>
    private let task: URLSessionDataTask

    fileprivate init(status: Int, headers: [String: String], body: AsyncThrowingStream<Data, any Error>, task: URLSessionDataTask) {
        self.status = status; self.headers = headers; self.body = body; self.task = task
    }
    deinit { task.cancel() }

    func cancel() { task.cancel() }

    var isEventStream: Bool { headers["content-type"]?.lowercased().contains("text/event-stream") ?? false }

    /// The whole body, at most `limit` bytes.
    func collect(limit: Int = 64 << 20) async throws -> Data {
        var out = Data()
        for try await chunk in body {
            out.append(chunk)
            if out.count > limit { cancel(); throw UpstreamError.bodyTooLarge }
        }
        return out
    }

    /// The body split into lines (without the line break), each at most
    /// `maxLine` bytes.
    func lines(maxLine: Int = 16 << 20) -> LineReader { LineReader(body, keepAlive: self, maxLine: maxLine) }
}

/// Splits a byte stream into lines (`\n`, with a trailing `\r` removed).
struct LineReader {
    /// Keeps the exchange alive while it is read.
    let keepAlive: AnyObject?
    var body: AsyncThrowingStream<Data, any Error>.AsyncIterator
    let maxLine: Int
    private var buffer: [UInt8] = []
    private var start = 0
    private var ended = false

    init(_ stream: AsyncThrowingStream<Data, any Error>, keepAlive: AnyObject? = nil, maxLine: Int = 16 << 20) {
        self.keepAlive = keepAlive; self.body = stream.makeAsyncIterator(); self.maxLine = maxLine
    }

    mutating func next() async throws -> String? {
        while true {
            if let nl = buffer[start...].firstIndex(of: 0x0A) {
                var end = nl
                if end > start, buffer[end - 1] == 0x0D { end -= 1 }
                let line = String(decoding: buffer[start..<end], as: UTF8.self)
                start = nl + 1
                if start > 65536, start * 2 > buffer.count {
                    buffer.removeSubrange(0..<start); start = 0
                }
                return line
            }
            if buffer.count - start > maxLine { throw UpstreamError.lineTooLong }
            if ended {
                guard start < buffer.count else { return nil }
                let line = String(decoding: buffer[start...], as: UTF8.self)
                start = buffer.count
                return line
            }
            guard let chunk = try await body.next() else { ended = true; continue }
            if start > 0, start == buffer.count { buffer.removeAll(keepingCapacity: true); start = 0 }
            buffer.append(contentsOf: chunk)
        }
    }
}

/// HTTP to the backends over URLSession: no overall timeout (a live stream
/// may run as long as it needs), a bounded wait for response headers, and
/// cancellation that closes the connection so the backend stops generating.
final class UpstreamClient: Sendable {
    let session: URLSession

    init() {
        let c = URLSessionConfiguration.ephemeral
        c.timeoutIntervalForRequest = 7 * 86400   // idle between packets: a long prefill is silent
        c.timeoutIntervalForResource = 7 * 86400
        c.httpMaximumConnectionsPerHost = 512      // parallel subagent streams to one backend
        c.requestCachePolicy = .reloadIgnoringLocalCacheData
        c.urlCache = nil
        c.httpCookieStorage = nil
        c.httpShouldSetCookies = false
        c.waitsForConnectivity = false
        session = URLSession(configuration: c)
    }

    func send(_ req: UpstreamRequest, headerTimeout: Duration) async throws -> UpstreamResponse {
        var r = URLRequest(url: req.url)
        r.httpMethod = req.method
        r.httpBody = req.body
        for (k, v) in req.headers { r.setValue(v, forHTTPHeaderField: k) }
        let task = session.dataTask(with: r)
        let delegate = TaskDelegate(task: task, refusePrivateRedirects: req.refusePrivateRedirects)
        task.delegate = delegate
        return try await withTaskCancellationHandler {
            let timer = Task {
                try await Task.sleep(for: headerTimeout)
                delegate.timedOut()
            }
            defer { timer.cancel() }
            return try await delegate.response()
        } onCancel: {
            delegate.cancelled()
        }
    }
}

private final class TaskDelegate: NSObject, URLSessionDataDelegate, Sendable {
    private struct State {
        var waiter: CheckedContinuation<UpstreamResponse, any Error>?
        var resolved = false
        var failure: UpstreamError?
    }
    private let task: URLSessionDataTask
    private let refusePrivateRedirects: Bool
    private let state = Mutex(State())
    private let stream: AsyncThrowingStream<Data, any Error>
    private let continuation: AsyncThrowingStream<Data, any Error>.Continuation

    init(task: URLSessionDataTask, refusePrivateRedirects: Bool) {
        self.task = task
        self.refusePrivateRedirects = refusePrivateRedirects
        (stream, continuation) = AsyncThrowingStream.makeStream(of: Data.self, throwing: (any Error).self)
        super.init()
        continuation.onTermination = { [task] _ in task.cancel() }
    }

    func response() async throws -> UpstreamResponse {
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<UpstreamResponse, any Error>) in
            let failed: UpstreamError? = state.withLock { s in
                if let f = s.failure { s.resolved = true; return f }
                s.waiter = c
                return nil
            }
            if let failed { c.resume(throwing: failed); return }
            task.resume()
        }
    }

    private func fail(_ e: UpstreamError) {
        let w: CheckedContinuation<UpstreamResponse, any Error>? = state.withLock { s in
            if s.resolved { return nil }
            if s.failure == nil { s.failure = e }
            guard let w = s.waiter else { return nil }
            s.waiter = nil; s.resolved = true
            return w
        }
        w?.resume(throwing: e)
    }

    func timedOut() {
        fail(.headerTimeout)
        task.cancel()
    }

    func cancelled() {
        fail(.cancelled)
        task.cancel()
        continuation.finish(throwing: CancellationError())
    }

    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive response: URLResponse,
                    completionHandler: @escaping @Sendable (URLSession.ResponseDisposition) -> Void) {
        completionHandler(.allow)
        let http = response as? HTTPURLResponse
        var headers: [String: String] = [:]
        for (k, v) in http?.allHeaderFields ?? [:] {
            if let k = k as? String { headers[k.lowercased()] = "\(v)" }
        }
        let resp = UpstreamResponse(status: http?.statusCode ?? 0, headers: headers, body: stream, task: task)
        let w: CheckedContinuation<UpstreamResponse, any Error>? = state.withLock { s in
            if s.resolved { return nil }
            s.resolved = true
            defer { s.waiter = nil }
            return s.waiter
        }
        w?.resume(returning: resp)
    }

    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
        continuation.yield(data)
    }

    func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: (any Error)?) {
        if let error {
            let failure = state.withLock { $0.failure }
            let e: UpstreamError
            if let failure { e = failure }
            else if (error as? URLError)?.code == .cancelled { e = .cancelled }
            else { e = .transport(error.localizedDescription) }
            fail(e)
            continuation.finish(throwing: e)
        } else {
            fail(.transport("the backend closed the connection without a response"))
            continuation.finish()
        }
    }

    func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
                    newRequest request: URLRequest, completionHandler: @escaping @Sendable (URLRequest?) -> Void) {
        guard refusePrivateRedirects else { completionHandler(request); return }
        if let host = request.url?.host, let scheme = request.url?.scheme?.lowercased(),
           scheme == "http" || scheme == "https", !NetworkAddress.isPrivateHost(host) {
            completionHandler(request)
        } else {
            completionHandler(nil)
        }
    }
}
