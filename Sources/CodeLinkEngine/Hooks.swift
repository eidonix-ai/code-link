import Foundation

// The seams between the engine core (server + translation) and the feature
// modules (docs/DESIGN.md §4.3–§4.5). The core calls these; each feature
// implements one and is unit-tested on its own.

/// A non-streaming Chat Completions call on one configured model, provided
/// by the core (it knows backends, keys and timeouts).
public protocol ModelCaller: Sendable {
    /// `body` is a Chat Completions request without `model` (the caller sets
    /// the upstream name). Returns the HTTP status and the decoded JSON body.
    func chat(model: Model, body: [String: JSONValue]) async throws -> ChatResult
}
public struct ChatResult: Sendable {
    public var status: Int
    public var json: JSONValue
    public init(status: Int, json: JSONValue) { self.status = status; self.json = json }
}

/// Turns a document into text for the model (§4.4).
public protocol DocumentReader: Sendable {
    func read(_ document: DocumentInput) async -> DocumentOutput
}
public struct DocumentInput: Sendable {
    public var data: Data
    public var mediaType: String      // application/pdf, image/png, …
    public var title: String?
    public init(data: Data, mediaType: String, title: String?) { self.data = data; self.mediaType = mediaType; self.title = title }
}
public struct DocumentOutput: Sendable, Equatable {
    public var text: String?          // nil = could not be read
    public var error: String?         // why, when text is nil
    public var pages: Int
    public var ocrPages: Int
    public var via: String            // "paddleocr", "pdfkit", "vision", …
    public init(text: String?, error: String? = nil, pages: Int = 0, ocrPages: Int = 0, via: String) {
        self.text = text; self.error = error; self.pages = pages; self.ocrPages = ocrPages; self.via = via
    }
}

/// Describes an image for a model without vision (§4.4).
public protocol ImageDescriber: Sendable {
    func describe(_ image: Data, mediaType: String) async -> String?
}

/// A web search provider (§4.5).
public protocol WebSearcher: Sendable {
    func search(query: String, allowedDomains: [String]?, blockedDomains: [String]?) async throws -> [SearchHit]
}
public struct SearchHit: Sendable, Equatable {
    public var title: String
    public var url: String
    public var snippet: String
    public var pageAge: String?
    public init(title: String, url: String, snippet: String, pageAge: String? = nil) {
        self.title = title; self.url = url; self.snippet = snippet; self.pageAge = pageAge
    }
}
public enum WebSearchError: Error, Sendable, Equatable { case unavailable, tooManyRequests }

/// Claude Code's auto-mode classifier (§4.3).
public protocol AutoModeResponder: Sendable {
    /// Whether an Anthropic Messages request body is a classifier request.
    func isClassifierRequest(_ request: JSONValue) -> Bool
    /// The complete Anthropic Messages response body (non-streaming shape)
    /// for a classifier request, or nil to fall back to ordinary
    /// translation on the decision model.
    func respond(to request: JSONValue) async -> JSONValue?
    /// The model classifier requests run on (decision role), for the fallback.
    var decisionModel: Model? { get }
}
