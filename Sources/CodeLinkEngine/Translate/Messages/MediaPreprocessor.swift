import CryptoKit
import Foundation

/// What a request's documents and images become before translation
/// (docs/DESIGN.md §4.4).
struct MediaContext: Sendable {
    var reader: (any DocumentReader)?
    /// Set only when the vision role names a model.
    var describer: (any ImageDescriber)?
    var modelHasVision: Bool
    var modelID: String
    var fetcher: UpstreamClient
    var log: any EventLog
    var maxFetchBytes = 32 << 20
}

/// Rewrites `document` blocks (top level and inside tool_result) into text
/// through the DocumentReader, and `image` blocks into descriptions or an
/// "image omitted" note when the model has no vision. Media inside a
/// tool_result becomes attachment text, which the translator puts in the user
/// message after the tool messages. Reads run concurrently, once per distinct
/// document or image in the request.
enum MediaPreprocessor {
    private enum Job: Sendable {
        case document(source: DocSource, title: String?, context: String?)
        case image(source: DocSource)
    }
    private enum DocSource: Sendable {
        case base64(data: String, mediaType: String)
        case url(String)
    }

    static func process(_ req: JSON, _ ctx: MediaContext) async -> JSON {
        guard let messages = req["messages"]?.array else { return req }
        var jobs: [String: Job] = [:]
        _ = rewrite(messages) { block, _ in
            if let (key, job) = jobFor(block, ctx) { jobs[key] = job }
            return nil
        }
        guard !jobs.isEmpty else { return req }

        var results: [String: String] = [:]
        await withTaskGroup(of: (String, String).self) { group in
            var pending = Array(jobs)
            var running = 0
            func next() {
                guard let (key, job) = pending.popLast() else { return }
                running += 1
                group.addTask { (key, await run(job, ctx)) }
            }
            for _ in 0..<4 { next() }
            while running > 0, let (key, text) = await group.next() {
                running -= 1
                results[key] = text
                next()
            }
        }

        let rewritten = rewrite(messages) { block, inToolResult in
            guard let (key, _) = jobFor(block, ctx), let text = results[key] else { return nil }
            return [["type": .string(inToolResult ? attachmentTextType : "text"), "text": .string(text)]]
        }
        var out = req
        out["messages"] = .array(rewritten)
        return out
    }

    /// Applies `replace` to every block of every user message (and to the
    /// blocks inside tool_result content); nil keeps the block. Text and
    /// content document sources are expanded here.
    private static func rewrite(_ messages: [JSON], _ replace: (JSON, Bool) -> [JSON]?) -> [JSON] {
        messages.map { m in
            guard m["role"]?.string == "user", let blocks = m["content"]?.array else { return m }
            var out = m
            out["content"] = .array(rewriteBlocks(blocks, inToolResult: false, replace))
            return out
        }
    }

    private static func rewriteBlocks(_ blocks: [JSON], inToolResult: Bool, _ replace: (JSON, Bool) -> [JSON]?) -> [JSON] {
        var out: [JSON] = []
        for b in blocks {
            if b.type == "tool_result", !inToolResult, let inner = b["content"]?.array {
                var nb = b
                nb["content"] = .array(rewriteBlocks(inner, inToolResult: true, replace))
                out.append(nb)
            } else if b.type == "document", let inline = inlineDocument(b, inToolResult: inToolResult) {
                out += rewriteBlocks(inline, inToolResult: inToolResult, replace)
            } else if let r = replace(b, inToolResult) {
                out += r
            } else {
                out.append(b)
            }
        }
        return out
    }

    private static func header(_ b: JSON) -> String {
        let title = (b["title"]?.string ?? b["name"]?.string ?? "").trimmingCharacters(in: .whitespaces)
        var h = "[document: \(title.isEmpty ? "document" : title)]\n"
        if let c = b["context"]?.string, !c.isEmpty { h += c + "\n" }
        return h
    }

    /// Documents whose text is in the request: `text` and `content` sources
    /// and base64 text/*.
    private static func inlineDocument(_ b: JSON, inToolResult: Bool) -> [JSON]? {
        let src = b["source"]
        let textType = inToolResult ? attachmentTextType : "text"
        switch src?.type {
        case "text":
            return [["type": .string(textType), "text": .string(header(b) + (src?["data"]?.string ?? ""))]]
        case "content":
            if let s = src?["content"]?.string { return [["type": .string(textType), "text": .string(header(b) + s)]] }
            var parts: [JSON] = [["type": .string(textType), "text": .string(String(header(b).dropLast()))]]
            for ib in src?["content"]?.array ?? [] {
                switch ib.type {
                case "text": parts.append(["type": .string(textType), "text": ib["text"] ?? ""])
                case "image": parts.append(ib)
                default: continue
                }
            }
            return parts
        case "base64":
            let mt = src?["media_type"]?.string?.lowercased() ?? ""
            if mt.hasPrefix("text/"), let d = Data(base64Encoded: src?["data"]?.string ?? "") {
                return [["type": .string(textType), "text": .string(header(b) + String(decoding: d, as: UTF8.self))]]
            }
            return nil
        default:
            return nil
        }
    }

    private static func sha(_ s: String) -> String {
        SHA256.hash(data: Data(s.utf8)).map { String(format: "%02x", $0) }.joined()
    }

    private static func jobFor(_ b: JSON, _ ctx: MediaContext) -> (String, Job)? {
        let src = b["source"]
        switch b.type {
        case "document":
            let title = (b["title"]?.string ?? b["name"]?.string)?.trimmingCharacters(in: .whitespaces)
            let context = b["context"]?.string
            switch src?.type {
            case "base64":
                let data = src?["data"]?.string ?? ""
                let mt = src?["media_type"]?.string ?? "application/pdf"
                return ("doc|\(title ?? "")|\(context ?? "")|\(mt)|" + sha(data),
                        .document(source: .base64(data: data, mediaType: mt), title: title, context: context))
            case "url":
                let url = src?["url"]?.string ?? ""
                return ("docurl|\(title ?? "")|\(context ?? "")|" + url, .document(source: .url(url), title: title, context: context))
            default:
                return nil // translator notes it
            }
        case "image":
            guard !ctx.modelHasVision else { return nil }
            switch src?.type {
            case "base64":
                let data = src?["data"]?.string ?? ""
                let mt = src?["media_type"]?.string ?? "image/png"
                return ("img|\(mt)|" + sha(data), .image(source: .base64(data: data, mediaType: mt)))
            case "url":
                let url = src?["url"]?.string ?? ""
                return ("imgurl|" + url, .image(source: .url(url)))
            default:
                return nil
            }
        default:
            return nil
        }
    }

    private static func run(_ job: Job, _ ctx: MediaContext) async -> String {
        switch job {
        case .document(let source, let title, let context):
            let name = (title?.isEmpty ?? true) ? "document" : title!
            func note(_ why: String) -> String { "[document: \(name) could not be read (\(why))]" }
            let loaded: (Data, String)
            switch await load(source, ctx) {
            case .success(let l): loaded = l
            case .failure(let why): return note(why)
            }
            if loaded.1.lowercased().hasPrefix("text/") {
                return "[document: \(name)]\n" + (context.map { $0 + "\n" } ?? "") + String(decoding: loaded.0, as: UTF8.self)
            }
            guard let reader = ctx.reader else { return note("document reading is not available") }
            let out = await reader.read(DocumentInput(data: loaded.0, mediaType: loaded.1, title: title))
            ctx.log.record(.document(name: name, pages: out.pages, ocrPages: out.ocrPages))
            guard let text = out.text?.trimmingCharacters(in: .whitespacesAndNewlines), !text.isEmpty else {
                return note(out.error ?? "no text found in it")
            }
            return "[document: \(name)]\n" + (context.map { $0 + "\n" } ?? "") + text
        case .image(let source):
            guard let describer = ctx.describer else {
                return "[image omitted: \(ctx.modelID) has no vision and no vision model is configured]"
            }
            guard case .success(let (data, mt)) = await load(source, ctx) else {
                return "[image omitted: it could not be fetched]"
            }
            guard let d = await describer.describe(data, mediaType: mt)?.trimmingCharacters(in: .whitespacesAndNewlines), !d.isEmpty else {
                return "[image omitted: the vision model could not describe it]"
            }
            return "[image: \(d)]"
        }
    }

    private enum Loaded { case success((Data, String)), failure(String) }

    private static func load(_ source: DocSource, _ ctx: MediaContext) async -> Loaded {
        switch source {
        case .base64(let s, let mt):
            guard s.count / 4 * 3 <= ctx.maxFetchBytes else { return .failure("too large, over \(ctx.maxFetchBytes >> 20) MiB") }
            guard let d = Data(base64Encoded: s, options: .ignoreUnknownCharacters) else { return .failure("invalid base64") }
            return .success((d, mt))
        case .url(let s):
            guard let url = URL(string: s), let scheme = url.scheme?.lowercased(), scheme == "http" || scheme == "https" else {
                return .failure("only http and https URLs are fetched")
            }
            do {
                let resp = try await ctx.fetcher.send(UpstreamRequest(url: url, method: "GET", refusePrivateRedirects: true),
                                                      headerTimeout: .seconds(60))
                guard (200..<300).contains(resp.status) else { return .failure("HTTP \(resp.status)") }
                let data = try await resp.collect(limit: ctx.maxFetchBytes)
                let mt = resp.headers["content-type"]?.split(separator: ";").first.map {
                    $0.trimmingCharacters(in: .whitespaces).lowercased()
                } ?? "application/octet-stream"
                return .success((data, mt))
            } catch UpstreamError.bodyTooLarge {
                return .failure("too large, over \(ctx.maxFetchBytes >> 20) MiB")
            } catch {
                return .failure("could not fetch it: " + scrubUpstreamMessage("\(error)"))
            }
        }
    }
}
