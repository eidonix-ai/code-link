import Foundation

/// Everything the engine needs, persisted by the app as JSON (no secrets).
public struct EngineConfig: Codable, Sendable, Equatable {
    public var port: Int
    public var backends: [Backend]
    public var models: [Model]
    public var roles: Roles
    public var documents: DocumentsConfig
    public var webSearch: WebSearchConfig

    public init(port: Int = 18765, backends: [Backend] = [], models: [Model] = [], roles: Roles = Roles(main: ""),
                documents: DocumentsConfig = DocumentsConfig(), webSearch: WebSearchConfig = WebSearchConfig()) {
        self.port = port; self.backends = backends; self.models = models; self.roles = roles
        self.documents = documents; self.webSearch = webSearch
    }
}

public enum BackendKind: String, Codable, Sendable, CaseIterable {
    /// vLLM, SGLang, Flare: Chat Completions with SSE.
    case openAICompatible
    /// A native Anthropic Messages endpoint: requests pass through.
    case anthropic
}

public struct Backend: Codable, Sendable, Identifiable, Equatable {
    public var id: String
    public var kind: BackendKind
    public var baseURL: URL
    /// Keychain item name holding the API key; never the key itself.
    public var apiKeyRef: String?
    public init(id: String, kind: BackendKind = .openAICompatible, baseURL: URL, apiKeyRef: String? = nil) {
        self.id = id; self.kind = kind; self.baseURL = baseURL; self.apiKeyRef = apiKeyRef
    }
}

public struct Model: Codable, Sendable, Identifiable, Equatable {
    /// The name Claude Code and Codex see.
    public var id: String
    public var backendID: String
    /// The name the backend expects.
    public var upstreamModel: String
    /// nil = detect from the backend's /v1/models (max_model_len).
    public var contextWindow: Int?
    /// nil = 85% of contextWindow.
    public var autoCompactAt: Int?
    public var vision: Bool?
    /// nil = probe once.
    public var logprobs: Bool?
    /// Merged into the Chat Completions request when a client turns thinking off.
    public var thinkingOff: JSONValue?
    public init(id: String, backendID: String, upstreamModel: String, contextWindow: Int? = nil, autoCompactAt: Int? = nil,
                vision: Bool? = nil, logprobs: Bool? = nil, thinkingOff: JSONValue? = nil) {
        self.id = id; self.backendID = backendID; self.upstreamModel = upstreamModel; self.contextWindow = contextWindow
        self.autoCompactAt = autoCompactAt; self.vision = vision; self.logprobs = logprobs; self.thinkingOff = thinkingOff
    }
    /// Where Claude Code should compact: autoCompactAt, else 85% of the window.
    public var effectiveAutoCompactAt: Int? {
        if let autoCompactAt { return autoCompactAt }
        if let contextWindow { return contextWindow * 85 / 100 }
        return nil
    }
}

/// Model ids for each role.
public struct Roles: Codable, Sendable, Equatable {
    /// Claude Code's default (the Sonnet slot) and ANTHROPIC_MODEL.
    public var main: String
    public var opus: String?
    /// Haiku slot and background tasks.
    public var small: String?
    /// Auto-mode classifier; needs logprobs. nil = main when it has them.
    public var decision: String?
    /// Describes images for a main model without vision.
    public var vision: String?
    public var codex: String?
    public init(main: String, opus: String? = nil, small: String? = nil, decision: String? = nil, vision: String? = nil, codex: String? = nil) {
        self.main = main; self.opus = opus; self.small = small; self.decision = decision; self.vision = vision; self.codex = codex
    }
}

public enum LocalExtraction: String, Codable, Sendable, CaseIterable {
    /// PDFKit text; Apple Vision OCR for pages without a text layer.
    case appleVision
    /// PDFKit text only.
    case textOnly
    case off
}

public struct DocumentsConfig: Codable, Sendable, Equatable {
    /// A PaddleOCR(-VL) server's /layout-parsing endpoint. When set and
    /// reachable, documents go there first.
    public var paddleOCR: URL?
    /// Used when PaddleOCR isn't configured, is unreachable or fails.
    public var local: LocalExtraction
    public init(paddleOCR: URL? = nil, local: LocalExtraction = .appleVision) {
        self.paddleOCR = paddleOCR; self.local = local
    }
}

public enum SearchProvider: String, Codable, Sendable, CaseIterable { case tavily, brave }

public struct WebSearchConfig: Codable, Sendable, Equatable {
    /// nil = web search off (searches answer with an "unavailable" result).
    public var provider: SearchProvider?
    /// Keychain item name of the provider's API key.
    public var keyRef: String?
    public init(provider: SearchProvider? = nil, keyRef: String? = nil) { self.provider = provider; self.keyRef = keyRef }
}

/// Arbitrary JSON (for pass-through objects such as thinkingOff).
public enum JSONValue: Codable, Sendable, Equatable {
    case null, bool(Bool), number(Double), string(String), array([JSONValue]), object([String: JSONValue])
    public init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null }
        else if let b = try? c.decode(Bool.self) { self = .bool(b) }
        else if let n = try? c.decode(Double.self) { self = .number(n) }
        else if let s = try? c.decode(String.self) { self = .string(s) }
        else if let a = try? c.decode([JSONValue].self) { self = .array(a) }
        else { self = .object(try c.decode([String: JSONValue].self)) }
    }
    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .null: try c.encodeNil()
        case .bool(let b): try c.encode(b)
        case .number(let n): try c.encode(n)
        case .string(let s): try c.encode(s)
        case .array(let a): try c.encode(a)
        case .object(let o): try c.encode(o)
        }
    }
}
