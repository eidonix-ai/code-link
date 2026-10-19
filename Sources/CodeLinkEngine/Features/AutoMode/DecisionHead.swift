import Foundation

/// Claude Code's auto-mode classifier answered from one logprob read on the
/// decision model (docs/DESIGN.md §4.3). Every failure returns nil from
/// `respond`, so the core falls back to ordinary translation.
public final class DecisionHead: AutoModeResponder, @unchecked Sendable {
    public static let marker = "You are a security monitor for autonomous AI coding agents"
    static let severityStop = "</severity>"
    static let blockStop = "</block>"
    static let minMass = 0.5
    static let topLogprobs = 20

    public let decisionModel: Model?
    private let caller: any ModelCaller
    private let threshold: Double
    private let log: (any EventLog)?
    private let lock = NSLock()
    private var noLogprobsUntil: Date?
    private let noLogprobsTTL: TimeInterval = 600

    public init(decisionModel: Model?, caller: any ModelCaller, threshold: Double = 0.5, log: (any EventLog)? = nil) {
        self.decisionModel = decisionModel
        self.caller = caller
        self.threshold = threshold
        self.log = log
    }

    // MARK: Detection

    public enum Format: Sendable, Equatable { case severity, block }

    public struct Classified: Sendable, Equatable {
        public var format: Format
        public var stage: Int          // 1 = has stop sequences, else 2
        public var system: String
        public var user: String
    }

    public func isClassifierRequest(_ request: JSONValue) -> Bool { classify(request) != nil }

    public func classify(_ request: JSONValue) -> Classified? {
        let system = Self.systemText(request.feature("system"))
        guard system.contains(Self.marker) else { return nil }
        var hasSeverity = false, hasBlock = false
        let stops = request.feature("stop_sequences")?.featureArray ?? []
        for s in stops {
            switch s.featureString {
            case Self.severityStop: hasSeverity = true
            case Self.blockStop: hasBlock = true
            default: break
            }
        }
        if system.contains("<severity>N</severity>") { hasSeverity = true }
        if system.contains("<block>") { hasBlock = true }
        guard hasSeverity || hasBlock else { return nil }
        let user = Self.userText(request.feature("messages"))
        guard !user.isEmpty else { return nil }
        return Classified(format: hasBlock && !hasSeverity ? .block : .severity,
                          stage: stops.isEmpty ? 2 : 1, system: system, user: user)
    }

    static func systemText(_ system: JSONValue?) -> String {
        switch system {
        case .string(let s): return s
        case .array(let blocks):
            return blocks.compactMap { $0.feature("text")?.featureString }.filter { !$0.isEmpty }.joined(separator: "\n")
        default: return ""
        }
    }

    /// The text of the user messages, verbatim, joined by blank lines.
    static func userText(_ messages: JSONValue?) -> String {
        var parts: [String] = []
        for m in messages?.featureArray ?? [] where m.feature("role")?.featureString == "user" {
            switch m.feature("content") {
            case .string(let s): parts.append(s)
            case .array(let blocks):
                let texts = blocks.compactMap { b -> String? in
                    let t = b.feature("type")?.featureString
                    return (t == "text" || t == nil) ? (b.feature("text")?.featureString ?? "") : nil
                }
                parts.append(texts.joined(separator: "\n"))
            default: break
            }
        }
        return parts.joined(separator: "\n\n")
    }

    // MARK: Score math

    public struct Score: Sendable, Equatable {
        public var pBlock: Double
        public var expected: Double
        public var mass: Double
    }
    public struct TokenLogprob: Sendable, Equatable {
        public var token: String
        public var logprob: Double
        public init(token: String, logprob: Double) { self.token = token; self.logprob = logprob }
    }

    /// Integer tokens 0-100 (spaces trimmed), normalised. nil when the numeric
    /// mass is under 0.5.
    static func scoreSeverity(_ top: [TokenLogprob]) -> Score? {
        var probs: [Int: Double] = [:]
        var mass = 0.0
        for t in top {
            guard let v = parseSeverityToken(t.token) else { continue }
            let p = exp(t.logprob)
            guard p.isFinite else { continue }
            probs[v, default: 0] += p
            mass += p
        }
        guard mass >= minMass else { return nil }
        var s = Score(pBlock: 0, expected: 0, mass: mass)
        for (v, p0) in probs {
            let p = p0 / mass
            s.expected += Double(v) * p
            if v > 50 { s.pBlock += p }
        }
        return s
    }

    static func parseSeverityToken(_ tok: String) -> Int? {
        let t = tok.trimmingCharacters(in: .whitespaces)
        guard !t.isEmpty, t.count <= 3, t.allSatisfy({ $0 >= "0" && $0 <= "9" }), let v = Int(t), v <= 100 else { return nil }
        return v
    }

    static func scoreBlock(_ top: [TokenLogprob]) -> Score? {
        var yes = 0.0, no = 0.0
        for t in top {
            let p = exp(t.logprob)
            guard p.isFinite else { continue }
            switch t.token.trimmingCharacters(in: .whitespaces).lowercased() {
            case "yes": yes += p
            case "no": no += p
            default: break
            }
        }
        let mass = yes + no
        guard mass >= minMass else { return nil }
        return Score(pBlock: yes / mass, expected: 0, mass: mass)
    }

    /// N = round(E[v]) forced to the side p_block chooses; never 50.
    static func decide(_ s: Score, threshold: Double) -> (n: Int, block: Bool) {
        var n = Int(s.expected.rounded())
        let block = s.pBlock >= threshold
        if block { n = max(n, 51) } else { n = min(n, 49) }
        return (min(max(n, 0), 100), block)
    }

    // MARK: Requests

    static func decisionBody(_ c: Classified, prefill: String) -> [String: JSONValue] {
        [
            "max_tokens": .number(1), "temperature": .number(0),
            "logprobs": .bool(true), "top_logprobs": .number(Double(topLogprobs)),
            "continue_final_message": .bool(true), "add_generation_prompt": .bool(false),
            "messages": messages(c, prefill: prefill),
        ]
    }

    static func categoryBody(_ c: Classified, n: Int) -> [String: JSONValue] {
        [
            "max_tokens": .number(16), "temperature": .number(0),
            "stop": .array([.string("</category>")]),
            "continue_final_message": .bool(true), "add_generation_prompt": .bool(false),
            "messages": messages(c, prefill: "<severity>\(n)</severity><category>"),
        ]
    }

    private static func messages(_ c: Classified, prefill: String) -> JSONValue {
        .array([
            .makeObject(["role": .string("system"), "content": .string(c.system)]),
            .makeObject(["role": .string("user"), "content": .string(c.user)]),
            .makeObject(["role": .string("assistant"), "content": .string(prefill)]),
        ])
    }

    static func topLogprobs(of json: JSONValue) -> [TokenLogprob] {
        let arr = json.feature(path: "choices", 0, "logprobs", "content", 0, "top_logprobs")?.featureArray ?? []
        return arr.compactMap { e in
            guard let t = e.feature("token")?.featureString, let lp = e.feature("logprob")?.featureNumber else { return nil }
            return TokenLogprob(token: t, logprob: lp)
        }
    }

    static func generatedText(_ json: JSONValue) -> String {
        guard let m = json.feature(path: "choices", 0, "message") else { return "" }
        for key in ["content", "reasoning", "reasoning_content"] {
            if let s = m.feature(key)?.featureString, !s.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { return s }
        }
        return ""
    }

    static func sanitizeCategory(_ s: String) -> String {
        String(s.unicodeScalars.filter { CharacterSet.letters.contains($0) || CharacterSet.decimalDigits.contains($0) || $0 == " " })
            .trimmingCharacters(in: .whitespaces)
    }

    // MARK: Answer

    /// Text and stop fields of the classifier reply.
    static func answer(_ c: Classified, n: Int, block: Bool, category: String?) -> (text: String, stopReason: String, stopSequence: String?) {
        if c.format == .block {
            let a = block ? "yes" : "no"
            return c.stage == 1 ? ("<block>" + a, "stop_sequence", blockStop) : ("<block>\(a)</block>", "end_turn", nil)
        }
        if c.stage == 1 { return ("<severity>\(n)", "stop_sequence", severityStop) }
        var text = "<severity>\(n)</severity>"
        if n > 50, let category, !category.isEmpty { text += "<category>\(category)</category>" }
        return (text, "end_turn", nil)
    }

    struct Usage {
        var prompt = 0, completion = 0, cached = 0
        mutating func add(_ json: JSONValue) {
            guard let u = json.feature("usage") else { return }
            prompt += Int(u.feature("prompt_tokens")?.featureNumber ?? 0)
            completion += Int(u.feature("completion_tokens")?.featureNumber ?? 0)
            cached += Int(u.feature(path: "prompt_tokens_details", "cached_tokens")?.featureNumber ?? 0)
        }
        var json: JSONValue {
            .makeObject([
                "input_tokens": .number(Double(max(prompt - cached, 0))),
                "output_tokens": .number(Double(completion)),
                "cache_creation_input_tokens": .number(0),
                "cache_read_input_tokens": .number(Double(cached)),
            ])
        }
    }

    // MARK: Respond

    public func respond(to request: JSONValue) async -> JSONValue? {
        guard let c = classify(request), let model = decisionModel else { return nil }
        let started = Date()
        if model.logprobs == false || logprobsKnownMissing() {
            record(c, model, 0, 0, started: started, "no logprobs"); return nil
        }
        var usage = Usage()

        let prefill = c.format == .block ? "<block>" : "<severity>"
        let first: ChatResult
        do { first = try await caller.chat(model: model, body: Self.decisionBody(c, prefill: prefill)) }
        catch { record(c, model, 0, 0, started: started, "upstream error"); return nil }
        guard (200..<300).contains(first.status) else {
            record(c, model, 0, 0, started: started, "upstream HTTP \(first.status)"); return nil
        }
        usage.add(first.json)

        let top = Self.topLogprobs(of: first.json)
        if top.isEmpty { markNoLogprobs(); record(c, model, 0, 0, started: started, "no logprobs"); return nil }
        guard let score = c.format == .block ? Self.scoreBlock(top) : Self.scoreSeverity(top) else {
            record(c, model, 0, 0, started: started, "low token mass"); return nil
        }
        let (n, block) = Self.decide(score, threshold: threshold)

        var category: String?
        if c.format == .severity, c.stage == 2, block {
            if let r = try? await caller.chat(model: model, body: Self.categoryBody(c, n: n)), (200..<300).contains(r.status) {
                usage.add(r.json)
                category = Self.sanitizeCategory(Self.generatedText(r.json))
            }
        }

        let a = Self.answer(c, n: n, block: block, category: category)
        record(c, model, score.pBlock, n, started: started, nil)
        let requested = request.feature("model")?.featureString ?? model.id
        return .makeObject([
            "id": .string("msg_" + Self.randomID()),
            "type": .string("message"),
            "role": .string("assistant"),
            "content": .array([.makeObject(["type": .string("text"), "text": .string(a.text)])]),
            "model": .string(requested),
            "stop_reason": .string(a.stopReason),
            "stop_sequence": a.stopSequence.map { .string($0) } ?? .null,
            "usage": usage.json,
        ])
    }

    private func record(_ c: Classified, _ model: Model, _ pBlock: Double, _ n: Int, started: Date, _ fallback: String?) {
        log?.record(.decision(stage: c.stage, model: model.id, pBlock: pBlock, severity: n,
                              latencyMS: Int(Date().timeIntervalSince(started) * 1000), fallback: fallback))
    }

    private func logprobsKnownMissing() -> Bool {
        lock.lock(); defer { lock.unlock() }
        if let t = noLogprobsUntil, t > Date() { return true }
        return false
    }
    private func markNoLogprobs() {
        lock.lock(); defer { lock.unlock() }
        noLogprobsUntil = Date().addingTimeInterval(noLogprobsTTL)
    }

    static func randomID() -> String {
        let chars = Array("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
        return String((0..<24).map { _ in chars.randomElement()! })
    }
}
