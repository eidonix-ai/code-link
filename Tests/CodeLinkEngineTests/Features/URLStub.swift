import Foundation

/// URLProtocol stub: each StubSession registers its own handler under an id
/// carried in a session-level header, so tests run in parallel without sharing state.
final class StubURLProtocol: URLProtocol, @unchecked Sendable {
    struct Reply { var status: Int; var body: Data; var headers: [String: String] = [:]
        init(_ status: Int = 200, _ body: String = "", headers: [String: String] = [:]) { self.status = status; self.body = Data(body.utf8); self.headers = headers }
    }
    struct Seen { var url: URL; var method: String; var headers: [String: String]; var body: Data }

    nonisolated(unsafe) private static var handlers: [String: @Sendable (Seen) throws -> Reply] = [:]
    nonisolated(unsafe) private static var seen: [String: [Seen]] = [:]
    private static let lock = NSLock()
    static let idHeader = "X-Stub-ID"

    static func register(_ handler: @escaping @Sendable (Seen) throws -> Reply) -> String {
        let id = UUID().uuidString
        lock.lock(); handlers[id] = handler; seen[id] = []; lock.unlock()
        return id
    }
    static func requests(_ id: String) -> [Seen] { lock.lock(); defer { lock.unlock() }; return seen[id] ?? [] }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        var body = request.httpBody ?? Data()
        if let stream = request.httpBodyStream {
            stream.open(); defer { stream.close() }
            var buf = [UInt8](repeating: 0, count: 64 * 1024)
            while stream.hasBytesAvailable {
                let n = stream.read(&buf, maxLength: buf.count)
                if n <= 0 { break }
                body.append(buf, count: n)
            }
        }
        let id = request.value(forHTTPHeaderField: Self.idHeader) ?? ""
        let s = Seen(url: request.url!, method: request.httpMethod ?? "GET", headers: request.allHTTPHeaderFields ?? [:], body: body)
        Self.lock.lock(); let h = Self.handlers[id]; Self.seen[id, default: []].append(s); Self.lock.unlock()
        do {
            guard let h else { throw URLError(.cannotConnectToHost) }
            let r = try h(s)
            let resp = HTTPURLResponse(url: request.url!, statusCode: r.status, httpVersion: "HTTP/1.1", headerFields: r.headers)!
            client?.urlProtocol(self, didReceive: resp, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: r.body)
            client?.urlProtocolDidFinishLoading(self)
        } catch {
            client?.urlProtocol(self, didFailWithError: error)
        }
    }
    override func stopLoading() {}
}

struct StubSession {
    let session: URLSession
    let id: String
    init(_ handler: @escaping @Sendable (StubURLProtocol.Seen) throws -> StubURLProtocol.Reply) {
        id = StubURLProtocol.register(handler)
        let c = URLSessionConfiguration.ephemeral
        c.protocolClasses = [StubURLProtocol.self]
        c.httpAdditionalHeaders = [StubURLProtocol.idHeader: id]
        session = URLSession(configuration: c)
    }
    var requests: [StubURLProtocol.Seen] { StubURLProtocol.requests(id) }
}
