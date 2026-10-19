import Foundation

/// Documents and images in OpenAI-shaped requests (docs/DESIGN.md §4.4 for
/// `/v1/responses` and `/v1/chat/completions`). It is MediaPreprocessor's
/// counterpart for the Responses `input` items and the Chat `messages`: the
/// same jobs, the same readers and the same notes.
///   - `input_file` / `file` parts and PDFs sent as images go through the
///     DocumentReader and become text;
///   - images go to the ImageDescriber (or become an "omitted" note) when the
///     model has no vision, and are left alone when it has;
///   - a file that is only a reference (a file id) becomes a note.
enum OpenAIMedia {
    enum Dialect: Sendable { case responses, chat }

    private enum Action {
        case keep
        case job(String, MediaPreprocessor.Job)
        case text(String)
    }

    /// Responses `input` items with their media rewritten. Message parts
    /// become `input_text`; parts inside tool outputs become attachment text
    /// (the translator moves it after the tool message, as Messages does).
    static func process(responsesInput items: [JSON], _ ctx: MediaContext) async -> [JSON] {
        await rewrite(items, dialect: .responses, ctx)
    }

    /// Chat Completions `messages` with their media rewritten to `text` parts.
    static func process(chatMessages messages: [JSON], _ ctx: MediaContext) async -> [JSON] {
        await rewrite(messages, dialect: .chat, ctx)
    }

    private static func rewrite(_ items: [JSON], dialect: Dialect, _ ctx: MediaContext) async -> [JSON] {
        var jobs: [String: MediaPreprocessor.Job] = [:]
        _ = map(items, dialect) { part, _ in
            if case .job(let key, let job) = classify(part, ctx) { jobs[key] = job }
            return nil
        }
        let results = jobs.isEmpty ? [:] : await MediaPreprocessor.runAll(jobs, ctx)
        return map(items, dialect) { part, inOutput in
            let text: String
            switch classify(part, ctx) {
            case .keep: return nil
            case .text(let t): text = t
            case .job(let key, _): text = results[key] ?? ""
            }
            let type = dialect == .chat ? "text" : (inOutput ? attachmentTextType : "input_text")
            return [["type": .string(type), "text": .string(text)]]
        }
    }

    /// Applies `replace` to the parts of every message / item (nil keeps the
    /// part). `inOutput`: the part is in a tool's output.
    private static func map(_ items: [JSON], _ dialect: Dialect, _ replace: (JSON, Bool) -> [JSON]?) -> [JSON] {
        func parts(_ list: [JSON], _ inOutput: Bool) -> [JSON] {
            list.flatMap { replace($0, inOutput) ?? [$0] }
        }
        return items.map { item in
            var out = item
            switch dialect {
            case .chat:
                if let content = item["content"]?.array { out["content"] = .array(parts(content, item["role"]?.string == "tool")) }
            case .responses:
                if let content = item["content"]?.array { out["content"] = .array(parts(content, false)) }
                if let output = item["output"]?.array {
                    out["output"] = .array(parts(output, true))
                } else if let inner = item["output"]?["content"]?.array {
                    var o = item["output"]!
                    o["content"] = .array(parts(inner, true))
                    out["output"] = o
                }
            }
            return out
        }
    }

    // MARK: parts

    private static func classify(_ part: JSON, _ ctx: MediaContext) -> Action {
        switch part.type {
        case "input_image", "image_url": return image(part, ctx)
        case "input_file", "file": return file(part)
        default: return .keep
        }
    }

    private static func image(_ part: JSON, _ ctx: MediaContext) -> Action {
        let raw = part["image_url"] ?? part["url"]
        let url = raw?.string ?? raw?["url"]?.string ?? ""
        guard !url.isEmpty else {
            return .text("[image omitted: it is attached by file id, which code-link cannot read]")
        }
        if let (mediaType, data) = dataURL(url) {
            if mediaType == "application/pdf" {
                let title = part["filename"]?.string
                return .job("doc|\(title ?? "")||application/pdf|" + MediaPreprocessor.sha(data),
                            .document(source: .base64(data: data, mediaType: mediaType), title: title, context: nil))
            }
            guard !ctx.modelHasVision else { return .keep }
            return .job("img|\(mediaType)|" + MediaPreprocessor.sha(data), .image(source: .base64(data: data, mediaType: mediaType)))
        }
        guard !ctx.modelHasVision else { return .keep }
        return .job("imgurl|" + url, .image(source: .url(url)))
    }

    private static func file(_ part: JSON) -> Action {
        let f = part["file"] ?? part
        let name = (f["filename"]?.string ?? part["filename"]?.string)?.trimmingCharacters(in: .whitespaces)
        let title = (name?.isEmpty ?? true) ? nil : name
        if let data = f["file_data"]?.string ?? part["file_data"]?.string, !data.isEmpty {
            let (mediaType, b64) = dataURL(data) ?? (guessMediaType(name: title, base64: data), data)
            return .job("doc|\(title ?? "")||\(mediaType)|" + MediaPreprocessor.sha(b64),
                        .document(source: .base64(data: b64, mediaType: mediaType), title: title, context: nil))
        }
        if let url = f["file_url"]?.string ?? part["file_url"]?.string, !url.isEmpty {
            return .job("docurl|\(title ?? "")||" + url, .document(source: .url(url), title: title, context: nil))
        }
        return .text("[attached file \"\(title ?? "unnamed file")\" could not be read: only inline file_data is supported]")
    }

    /// (media type, base64) of a `data:<type>;base64,<data>` URL.
    static func dataURL(_ s: String) -> (String, String)? {
        guard s.hasPrefix("data:"), let comma = s.firstIndex(of: ",") else { return nil }
        let header = s[s.index(s.startIndex, offsetBy: 5)..<comma].split(separator: ";").map { $0.trimmingCharacters(in: .whitespaces) }
        guard header.contains(where: { $0.lowercased() == "base64" }) else { return nil }
        let type = header.first.map { $0.lowercased() } ?? ""
        return (type.isEmpty || type == "base64" ? "application/octet-stream" : type, String(s[s.index(after: comma)...]))
    }

    private static func guessMediaType(name: String?, base64: String) -> String {
        // "JVBERi" is "%PDF-" in base64.
        if base64.hasPrefix("JVBERi") { return "application/pdf" }
        switch (name?.split(separator: ".").last).map({ $0.lowercased() }) {
        case "pdf": return "application/pdf"
        case "txt", "md", "log": return "text/plain"
        case "csv": return "text/csv"
        case "json": return "text/json"
        case "html", "htm": return "text/html"
        case "png": return "image/png"
        case "jpg", "jpeg": return "image/jpeg"
        case "gif": return "image/gif"
        case "webp": return "image/webp"
        case "tif", "tiff": return "image/tiff"
        default: return "application/octet-stream"
        }
    }
}
