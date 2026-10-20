import Foundation

/// Describes an image with the vision-role model, one call per image, cached
/// by SHA-256 (docs/DESIGN.md §4.4).
public final class ModelImageDescriber: ImageDescriber, @unchecked Sendable {
    public static let prompt = "Describe this image in detail for someone who cannot see it, including any text it contains."
    static let maxTokens = 1500
    /// Room for a thinking model that keeps thinking on (no `thinkingOff`).
    static let thinkingMaxTokens = 8192

    private let model: Model
    private let caller: any ModelCaller
    private let cache = ContentCache<String>(maxEntries: 128, maxCost: 4 << 20)

    public init(model: Model, caller: any ModelCaller) { self.model = model; self.caller = caller }

    /// Thinking off (the model's `thinkingOff`, else `enable_thinking: false`)
    /// so the budget goes to the description; `thinking` false asks without it.
    static func requestBody(_ image: Data, mediaType: String, thinkingOff: JSONValue? = nil, thinking: Bool = false) -> [String: JSONValue] {
        let url = "data:\(mediaType);base64,\(image.base64EncodedString())"
        var body: [String: JSONValue] = [
            "max_tokens": .number(Double(thinking ? thinkingMaxTokens : maxTokens)),
            "messages": .array([.makeObject([
                "role": .string("user"),
                "content": .array([
                    .makeObject(["type": .string("image_url"), "image_url": .makeObject(["url": .string(url)])]),
                    .makeObject(["type": .string("text"), "text": .string(prompt)]),
                ]),
            ])]),
        ]
        if !thinking {
            let off = thinkingOff ?? .makeObject(["chat_template_kwargs": .makeObject(["enable_thinking": .bool(false)])])
            if case .object(let o) = off { for (k, v) in o { body[k] = v } }
        }
        return body
    }

    public func describe(_ image: Data, mediaType: String) async -> String? {
        let key = featureSHA256(image)
        if let hit = cache.value(for: key) { return hit }
        // Thinking off first; a server that refuses that, or a model that
        // still spends the budget thinking, is asked again with thinking on.
        var text = await ask(Self.requestBody(image, mediaType: mediaType, thinkingOff: model.thinkingOff))
        if text == nil { text = await ask(Self.requestBody(image, mediaType: mediaType, thinking: true)) }
        guard let text else { return nil }
        cache.insert(text, cost: text.utf8.count, for: key)
        return text
    }

    private func ask(_ body: [String: JSONValue]) async -> String? {
        guard let result = try? await caller.chat(model: model, body: body), (200..<300).contains(result.status),
              let raw = result.json.feature(path: "choices", 0, "message", "content")?.featureString else { return nil }
        let text = Self.stripThinking(raw).trimmingCharacters(in: .whitespacesAndNewlines)
        return text.isEmpty ? nil : text
    }

    /// Drops `<think>…</think>` a backend leaves in content, and a lone leading `</think>`.
    static func stripThinking(_ s: String) -> String {
        var t = s.replacingOccurrences(of: "<think>[\\s\\S]*?</think>", with: "", options: .regularExpression)
        if let r = t.range(of: "</think>"), t[..<r.lowerBound].trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || !t.contains("<think>") {
            t = String(t[r.upperBound...])
        }
        return t
    }
}
