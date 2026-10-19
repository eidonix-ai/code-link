import Foundation
import Hummingbird

/// Failures in the OpenAI error shape, for `/v1/responses` and
/// `/v1/chat/completions` (docs/DESIGN.md §5, §5a). Codex acts on `code`:
/// `context_length_exceeded` makes it compact, `rate_limit_exceeded` and
/// `server_is_overloaded` are retried with backoff, `invalid_prompt` is
/// fatal. Backend messages are scrubbed of addresses and credentials.
struct OpenAIError: Error, Sendable, Equatable {
    var status: Int
    var type: String
    var code: String?
    var message: String
    var retryAfter: String?

    init(status: Int, type: String, code: String?, message: String, retryAfter: String? = nil) {
        self.status = status; self.type = type; self.code = code; self.message = message; self.retryAfter = retryAfter
    }

    /// The `error` object (also `response.error` of a failed response).
    var object: JSON {
        ["code": code.map { .string($0) } ?? nil, "type": .string(type), "message": .string(message)]
    }
    var json: JSON {
        ["error": ["message": .string(message), "type": .string(type), "param": nil, "code": code.map { .string($0) } ?? nil]]
    }
    var response: Response {
        .json(json, status: .init(code: status), headers: retryAfter.map { [("Retry-After", $0)] } ?? [])
    }

    static func invalid(_ message: String, code: String? = nil) -> OpenAIError {
        OpenAIError(status: 400, type: "invalid_request_error", code: code, message: message)
    }
    static func notFound(_ message: String) -> OpenAIError {
        OpenAIError(status: 404, type: "invalid_request_error", code: "model_not_found", message: message)
    }
    static func server(_ status: Int, _ code: String, _ message: String) -> OpenAIError {
        OpenAIError(status: status, type: "server_error", code: code, message: message)
    }

    /// Sent in `response.failed` when the stream ends early.
    static let truncated = server(502, "stream_truncated", "upstream stream ended before the response was complete")
    static let interrupted = server(502, "stream_interrupted", "upstream stream was interrupted before it finished")

    /// A failure to reach the backend at all.
    static func transport(_ e: any Error) -> OpenAIError {
        switch e as? UpstreamError {
        case .headerTimeout?: return server(504, "upstream_timeout", "upstream request timed out")
        case .cancelled?: return server(499, "cancelled", "cancelled")
        default: return server(502, "upstream_unreachable", "upstream request failed")
        }
    }

    /// What a client should see for a backend HTTP error. Raw bodies are
    /// never forwarded: a cleaned message for 400s and context overflows.
    static func upstream(status: Int, body: Data, headers: [String: String] = [:]) -> OpenAIError {
        let info = parseUpstreamError(body)
        let message = scrubUpstreamMessage(info.message)
        var e = server(status, "upstream_error", "backend returned HTTP \(status)")
        let lower = String(decoding: body, as: UTF8.self).lowercased()
        let contextLength = contextLengthMarkers.contains { lower.contains($0) } || rewriteContextLengthError(info) != nil
        switch status {
        case 400..<500 where contextLength && status != 401 && status != 403 && status != 429:
            e.status = 400; e.type = "invalid_request_error"; e.code = "context_length_exceeded"
            if !message.isEmpty { e.message = message }
        case 400, 422:
            e.status = 400; e.type = "invalid_request_error"; e.code = "invalid_prompt"
            if !message.isEmpty { e.message = message }
        case 401, 403:
            e.status = 502; e.code = "upstream_auth_error"
            e.message = "backend rejected the proxy's credentials"
        case 429:
            e.type = "rate_limit_error"; e.code = "rate_limit_exceeded"
            e.message = "backend is rate limiting requests; retry shortly"
            e.retryAfter = headers["retry-after"]
        case 503:
            e.code = "server_is_overloaded"
            e.message = "backend is overloaded or unavailable; retry shortly"
            e.retryAfter = headers["retry-after"]
        case 400..<500:
            e.type = "invalid_request_error"
        default:
            break
        }
        return e
    }

    /// An Anthropic backend's failure, as an OpenAI error.
    static func fromAnthropic(_ a: AnthropicError) -> OpenAIError {
        var type = a.type
        var code: String?
        switch a.type {
        case "invalid_request_error", "rate_limit_error": break
        case "not_found_error": type = "invalid_request_error"; code = "model_not_found"
        case "request_too_large": type = "invalid_request_error"; code = "request_too_large"
        case "overloaded_error": type = "server_error"; code = "server_is_overloaded"
        default: type = "server_error"
        }
        if a.type == "rate_limit_error" { code = "rate_limit_exceeded" }
        return OpenAIError(status: a.status, type: type, code: code, message: a.message,
                           retryAfter: a.headers.first { $0.0 == "retry-after" }?.1)
    }

    /// An in-band `{"error": …}` frame of a stream.
    static func inBand(_ err: JSON) -> OpenAIError {
        let message = err.string ?? err["message"]?.string ?? "the backend reported an error"
        var info = UpstreamErrorInfo()
        info.message = message
        info.type = err["type"]?.string ?? ""
        let text = scrubUpstreamMessage(message)
        if rewriteContextLengthError(info) != nil || contextLengthMarkers.contains(where: { message.lowercased().contains($0) }) {
            return OpenAIError(status: 400, type: "invalid_request_error", code: "context_length_exceeded", message: text)
        }
        return server(502, "upstream_error", text)
    }
}

/// Lowercase fragments backends use when the prompt does not fit the context.
private let contextLengthMarkers = [
    "context_length_exceeded", "maximum context length", "context length", "context window", "max_model_len",
    "maximum model length", "too many tokens", "prompt is too long", "input is too long", "input token count",
    "reduce the length", "exceeds the maximum number of tokens", "exceeds the model",
]
