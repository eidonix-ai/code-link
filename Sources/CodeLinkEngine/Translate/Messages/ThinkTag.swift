import Foundation

/// Separates `<think>…</think>` reasoning that some backends put in the
/// content field from the answer, across chunk boundaries. A lone leading
/// `</think>` (GLM templates with thinking off) is dropped, so an answer
/// that must start with a verdict does.
struct ThinkTagFilter {
    struct Segment: Equatable { var text: String; var reasoning: Bool }

    private static let open = "<think>"
    private static let close = "</think>"
    private var inside = false
    private var pending = ""
    private var started = false

    mutating func process(_ chunk: String) -> [Segment] {
        var input = pending + chunk
        pending = ""
        var out: [Segment] = []
        while !input.isEmpty {
            if !inside && !started {
                let t = String(input.drop(while: \.isWhitespace))
                if t.hasPrefix(Self.close) {
                    input = String(t.dropFirst(Self.close.count))
                    started = true
                    continue
                }
                if !t.isEmpty, t.count < Self.close.count, Self.close.hasPrefix(t) {
                    pending = input
                    break
                }
                if !t.isEmpty { started = true }
            }
            let tag = inside ? Self.close : Self.open
            if let r = input.range(of: tag) {
                let before = String(input[..<r.lowerBound])
                if !before.isEmpty { out.append(Segment(text: before, reasoning: inside)) }
                input = String(input[r.upperBound...])
                inside.toggle()
                if !inside { started = true }
                continue
            }
            let n = Self.longestTagSuffix(input, tag)
            if n > 0 {
                let keep = String(input.dropLast(n))
                if !keep.isEmpty { out.append(Segment(text: keep, reasoning: inside)) }
                pending = String(input.suffix(n))
            } else {
                out.append(Segment(text: input, reasoning: inside))
            }
            input = ""
        }
        return out
    }

    /// Text held back waiting for a tag to complete.
    mutating func flush() -> [Segment] {
        guard !pending.isEmpty else { return [] }
        defer { pending = "" }
        return [Segment(text: pending, reasoning: inside)]
    }

    static func longestTagSuffix(_ s: String, _ tag: String) -> Int {
        let maxLen = min(tag.count - 1, s.count)
        guard maxLen > 0 else { return 0 }
        for l in stride(from: maxLen, to: 0, by: -1) where s.hasSuffix(tag.prefix(l)) { return l }
        return 0
    }

    /// (reasoning, content) of a complete string.
    static func strip(_ s: String) -> (reasoning: String, content: String) {
        var s = Substring(s)
        let t = s.drop(while: \.isWhitespace)
        if t.hasPrefix(close) { s = t.dropFirst(close.count) }
        var reasoning = "", content = ""
        while !s.isEmpty {
            guard let o = s.range(of: open) else { content += s; break }
            content += s[..<o.lowerBound]
            s = s[o.upperBound...]
            guard let c = s.range(of: close) else { reasoning += s; break }
            reasoning += s[..<c.lowerBound]
            s = s[c.upperBound...]
        }
        return (reasoning, content.trimmingCharacters(in: .whitespacesAndNewlines))
    }
}
