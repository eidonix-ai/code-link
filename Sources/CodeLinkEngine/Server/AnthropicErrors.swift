import Foundation

/// Upstream failures as Anthropic errors (docs/DESIGN.md §4.2). Claude Code
/// acts on the status, error.type and a few message formats:
///   - 401/403 from a backend are the engine's own credentials failing, so
///     they become 502 and never start a login flow;
///   - context overflows become `prompt is too long: N tokens > M maximum`
///     (or the max_tokens form), which it parses to compact;
///   - messages are scrubbed of URLs, addresses and credentials.
struct AnthropicError: Error, Sendable, Equatable {
    var status: Int
    var type: String
    var message: String
    /// Retry pacing headers to pass on.
    var headers: [(String, String)] = []

    init(status: Int, type: String, message: String, headers: [(String, String)] = []) {
        self.status = status; self.type = type; self.message = message; self.headers = headers
    }
    static func == (a: AnthropicError, b: AnthropicError) -> Bool {
        a.status == b.status && a.type == b.type && a.message == b.message
    }

    var json: JSON { ["type": "error", "error": ["type": .string(type), "message": .string(message)]] }

    static func invalid(_ m: String) -> AnthropicError { .init(status: 400, type: "invalid_request_error", message: m) }
    static func notFound(_ m: String) -> AnthropicError { .init(status: 404, type: "not_found_error", message: m) }
    static func api(_ status: Int, _ m: String) -> AnthropicError { .init(status: status, type: "api_error", message: m) }
    static let unauthorized = AnthropicError(status: 401, type: "authentication_error", message: "invalid or missing access token")
    static let truncated = AnthropicError(status: 529, type: "overloaded_error", message: "the backend's stream ended before it was complete")

    /// The error for a failure to reach the backend at all.
    static func transport(_ e: any Error) -> AnthropicError {
        switch e as? UpstreamError {
        case .headerTimeout?: return .api(504, "the backend did not answer in time")
        case .cancelled?: return .api(499, "cancelled")
        case let u?: return .api(502, "the backend could not be reached: " + scrubUpstreamMessage(u.description))
        case nil: return .api(502, "the backend could not be reached: " + scrubUpstreamMessage(e.localizedDescription))
        }
    }
}

/// Status and type for an upstream HTTP status.
func anthropicErrorForStatus(_ upstream: Int) -> (Int, String) {
    switch upstream {
    case 400, 422: return (400, "invalid_request_error")
    case 401, 403: return (502, "api_error")
    case 404: return (404, "not_found_error")
    case 413: return (413, "request_too_large")
    case 429: return (429, "rate_limit_error")
    case 503, 529: return (upstream, "overloaded_error")
    case 500...: return (upstream, "api_error")
    case 400...: return (400, "invalid_request_error")
    default: return (502, "api_error")
    }
}

struct UpstreamErrorInfo {
    var message = ""
    var type = ""
    var promptTokens = 0
    var contextSize = 0
}

/// The message (and type / token counts) of a backend error body: OpenAI and
/// vLLM `{"error":{"message"}}`, `{"error":"…"}`, FastAPI `{"detail"}`,
/// `{"message"}` and Anthropic `{"type":"error","error":{…}}`.
func parseUpstreamError(_ body: Data) -> UpstreamErrorInfo {
    var info = UpstreamErrorInfo()
    let trimmed = String(decoding: body, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
    guard !trimmed.isEmpty else { return info }
    guard let j = try? JSON.parse(body), j.object != nil else { info.message = trimmed; return info }
    if let e = j["error"] {
        if let s = e.string { info.message = s }
        else if e.object != nil {
            info.message = e["message"]?.string ?? e["detail"]?.string ?? ""
            info.type = e["type"]?.string ?? ""
            info.promptTokens = e["n_prompt_tokens"]?.int ?? 0
            info.contextSize = e["n_ctx"]?.int ?? 0
        }
    }
    if info.message.isEmpty {
        for k in ["message", "detail", "msg"] {
            guard let v = j[k] else { continue }
            if let s = v.string, !s.isEmpty { info.message = s; break }
            if let items = v.array, !items.isEmpty {
                info.message = items.compactMap { $0["msg"]?.string }.filter { !$0.isEmpty }.joined(separator: "; ")
                break
            }
        }
    }
    if info.message.isEmpty { info.message = trimmed }
    return info
}

private func regex(_ p: String) -> NSRegularExpression {
    // Patterns are constants; a failure is a programming error.
    try! NSRegularExpression(pattern: p, options: [.caseInsensitive, .dotMatchesLineSeparators])
}

private enum Patterns {
    static let url = regex(#"\b(?:https?|wss?|ftp)://[^\s"'<>)]+"#)
    static let host = regex(#"\b\d{1,3}(?:\.\d{1,3}){3}(?::\d{2,5})?\b"#)
    static let bearer = regex(#"\bbearer\s+[A-Za-z0-9._~+/=-]{8,}"#)
    static let key = try! NSRegularExpression(pattern: #"\b(?:sk|pk|rk|key|tok|tvly|ghp|xox[a-z])[-_][A-Za-z0-9_-]{8,}"#)
    static let kv = regex(#"(api[_-]?key|authorization|secret|token|password)(["']?\s*[:=]\s*["']?)[^\s"',}]+"#)

    static let vllmRequested = regex(#"maximum context length is (\d+) tokens.*?requested (\d+) tokens\s*\((\d+) in the messages,\s*(\d+) in the completion"#)
    static let vllmPassed = regex(#"you passed (\d+) input tokens and requested (\d+) output tokens.*?context length is only (\d+) tokens"#)
    static let vllmTooLarge = regex(#"too large: (\d+)\.\s*this model's maximum context length is (\d+) tokens and your request has (\d+) input tokens"#)
    static let sglang = regex(#"maximum context length of (\d+) tokens.*?requested a total of (\d+) tokens:\s*(\d+) tokens from the input.*?(\d+) tokens for the completion"#)
    static let inputLen = regex(#"input length \((\d+)\) exceeds the maximum allowed length \((\d+)\)"#)
    static let genericHint = regex(#"(context length|context window|context size|too many tokens|prompt is too long|too long)"#)
}

private func replace(_ re: NSRegularExpression, in s: String, with t: String) -> String {
    re.stringByReplacingMatches(in: s, range: NSRange(s.startIndex..., in: s), withTemplate: t)
}

private func groups(_ re: NSRegularExpression, _ s: String) -> [Int]? {
    guard let m = re.firstMatch(in: s, range: NSRange(s.startIndex..., in: s)) else { return nil }
    return (1..<m.numberOfRanges).map { i in
        Range(m.range(at: i), in: s).flatMap { Int(s[$0]) } ?? 0
    }
}

/// A backend message with URLs, IPv4 addresses and credential-looking
/// strings removed, at most 1000 bytes.
func scrubUpstreamMessage(_ s: String) -> String {
    var s = replace(Patterns.url, in: s, with: "[url]")
    s = replace(Patterns.bearer, in: s, with: "Bearer [redacted]")
    s = replace(Patterns.kv, in: s, with: "$1$2[redacted]")
    s = replace(Patterns.key, in: s, with: "[redacted]")
    s = replace(Patterns.host, in: s, with: "[host]")
    s = s.trimmingCharacters(in: .whitespacesAndNewlines)
    let (c, cut) = s.clipped(toBytes: 1000)
    return cut ? c + "..." : c
}

/// Context-overflow errors in the form Claude Code understands.
func rewriteContextLengthError(_ info: UpstreamErrorInfo) -> String? {
    let msg = info.message
    func tooLong(_ n: Int, _ m: Int) -> String { "prompt is too long: \(n) tokens > \(m) maximum" }
    func overflow(_ n: Int, _ maxTok: Int, _ m: Int) -> String {
        n > m ? tooLong(n, m) : "input length and `max_tokens` exceed context limit: \(n) + \(maxTok) > \(m)"
    }
    if info.promptTokens > 0, info.contextSize > 0 { return tooLong(info.promptTokens, info.contextSize) }
    if let g = groups(Patterns.vllmRequested, msg) { return overflow(g[2], g[3], g[0]) }
    if let g = groups(Patterns.vllmPassed, msg) { return overflow(g[0], g[1], g[2]) }
    if let g = groups(Patterns.vllmTooLarge, msg) { return overflow(g[2], g[0], g[1]) }
    if let g = groups(Patterns.sglang, msg) { return overflow(g[2], g[3], g[0]) }
    if let g = groups(Patterns.inputLen, msg) { return tooLong(g[0], g[1]) }
    if info.type == "exceed_context_size_error"
        || (groups(Patterns.genericHint, msg) != nil && msg.lowercased().contains("context")) {
        return "prompt is too long: " + scrubUpstreamMessage(msg)
    }
    return nil
}

private let retryHeaderNames = ["retry-after", "retry-after-ms", "x-should-retry"]

/// The retry pacing and rate-limit headers of an upstream response.
func retryHeaders(_ upstream: [String: String]) -> [(String, String)] {
    var out: [(String, String)] = []
    for k in retryHeaderNames { if let v = upstream[k] { out.append((k, v)) } }
    for (k, v) in upstream.sorted(by: { $0.key < $1.key }) where k.hasPrefix("anthropic-ratelimit-") { out.append((k, v)) }
    return out
}

/// The client error for a backend error response. `native`: an Anthropic
/// backend, whose own meaningful error.type is kept.
func buildClientError(status: Int, body: Data, headers: [String: String] = [:], native: Bool = false) -> AnthropicError {
    let info = parseUpstreamError(body)
    var (outStatus, outType) = anthropicErrorForStatus(status)
    if native, !info.type.isEmpty, status != 401, status != 403,
       ["invalid_request_error", "rate_limit_error", "overloaded_error", "api_error", "not_found_error", "request_too_large"].contains(info.type) {
        outType = info.type
    }
    let retry = retryHeaders(headers)
    if [400, 413, 422].contains(status), let rewritten = rewriteContextLengthError(info) {
        return AnthropicError(status: 400, type: "invalid_request_error", message: rewritten, headers: retry)
    }
    var msg: String
    if status == 401 || status == 403 {
        msg = "the model backend rejected code-link's credentials"
        if !info.message.isEmpty { msg += ": " + scrubUpstreamMessage(info.message) }
    } else {
        msg = scrubUpstreamMessage(info.message)
    }
    if msg.isEmpty { msg = "backend returned HTTP \(status)" }
    return AnthropicError(status: outStatus, type: outType, message: msg, headers: retry)
}
