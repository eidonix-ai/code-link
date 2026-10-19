import Foundation

/// A PaddleOCR(-VL) `/layout-parsing` server (docs/DESIGN.md §4.4).
/// Request: `{file: base64, fileType: 0 (PDF) | 1 (image), returnMarkdownImages: false, visualize: false}`.
/// Response: `{errorCode, errorMsg, result: {layoutParsingResults: [{markdown: {text}}]}}`, one entry per page.
final class PaddleOCRClient: @unchecked Sendable {
    enum Failure: Error, Equatable {
        case unreachable
        case http(Int)
        case server(String)
        case empty
        case unsupported
    }

    private let endpoint: URL
    private let session: URLSession
    private let probeTTL: TimeInterval
    private let readTimeout: TimeInterval
    private let lock = NSLock()
    private var probed: (reachable: Bool, at: Date)?

    init(endpoint: URL, session: URLSession = .shared, probeTTL: TimeInterval = 60, readTimeout: TimeInterval = 300) {
        self.endpoint = endpoint; self.session = session; self.probeTTL = probeTTL; self.readTimeout = readTimeout
    }

    /// A cheap probe, cached: any HTTP answer below 500 means the server is up
    /// (the endpoint itself answers a bare GET with 405). Connection errors,
    /// timeouts and gateway errors mean it is not.
    func isReachable(now: Date = Date()) async -> Bool {
        if let cached = cachedProbe(now: now) { return cached }
        var req = URLRequest(url: endpoint, timeoutInterval: 3)
        req.httpMethod = "GET"
        let ok: Bool
        if let (_, resp) = try? await session.data(for: req), let http = resp as? HTTPURLResponse { ok = http.statusCode < 500 } else { ok = false }
        remember(ok, at: now)
        return ok
    }

    private func cachedProbe(now: Date) -> Bool? {
        lock.withLock {
            if let p = probed, now.timeIntervalSince(p.at) < probeTTL { return p.reachable }
            return nil
        }
    }

    private func remember(_ reachable: Bool, at: Date = Date()) {
        lock.withLock { probed = (reachable, at) }
    }

    /// Page texts in order, empty pages dropped. Throws a `Failure`.
    func read(_ data: Data, mediaType: String) async throws -> [String] {
        let fileType: Int
        if mediaType == "application/pdf" { fileType = 0 }
        else if mediaType.hasPrefix("image/") { fileType = 1 }
        else { throw Failure.unsupported }

        let body: [String: JSONValue] = [
            "file": .string(data.base64EncodedString()), "fileType": .number(Double(fileType)),
            "returnMarkdownImages": .bool(false), "visualize": .bool(false),
        ]
        var req = URLRequest(url: endpoint, timeoutInterval: readTimeout)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONEncoder().encode(JSONValue.object(body))

        let responseData: Data, response: URLResponse
        do { (responseData, response) = try await session.data(for: req) }
        catch {
            remember(false)   // a dead server: skip it for the next minute
            throw Failure.unreachable
        }
        guard let http = response as? HTTPURLResponse else { throw Failure.unreachable }
        guard http.statusCode == 200 else { throw Failure.http(http.statusCode) }
        guard let json = try? JSONDecoder().decode(JSONValue.self, from: responseData) else { throw Failure.server("invalid response") }
        if let code = json.feature("errorCode")?.featureNumber, code != 0 {
            throw Failure.server(json.feature("errorMsg")?.featureString ?? "error \(Int(code))")
        }
        let pages = json.feature(path: "result", "layoutParsingResults")?.featureArray ?? []
        let texts = pages.map { ($0.feature(path: "markdown", "text")?.featureString ?? "").trimmingCharacters(in: .whitespacesAndNewlines) }
        guard texts.contains(where: { !$0.isEmpty }) else { throw Failure.empty }
        return texts
    }
}
