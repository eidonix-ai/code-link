import Foundation

/// Turns documents into text: PaddleOCR first when configured and reachable,
/// else the local extraction chosen in `DocumentsConfig` (docs/DESIGN.md §4.4).
public final class DocumentService: DocumentReader, @unchecked Sendable {
    public static let defaultTextCap = 600 * 1024
    static let cacheEntries = 32
    static let cacheBytes = 32 << 20

    private let config: DocumentsConfig
    private let paddle: PaddleOCRClient?
    private let recognizer: any TextRecognizer
    private let textCap: Int
    private let cache = ContentCache<DocumentOutput>(maxEntries: DocumentService.cacheEntries, maxCost: DocumentService.cacheBytes)

    public convenience init(config: DocumentsConfig, session: URLSession = .shared) {
        self.init(config: config, session: session, recognizer: VisionTextRecognizer(), textCap: Self.defaultTextCap)
    }

    init(config: DocumentsConfig, session: URLSession = .shared, recognizer: any TextRecognizer, textCap: Int,
         probeTTL: TimeInterval = 60) {
        self.config = config
        self.paddle = config.paddleOCR.map { PaddleOCRClient(endpoint: $0, session: session, probeTTL: probeTTL) }
        self.recognizer = recognizer
        self.textCap = textCap
    }

    public func read(_ document: DocumentInput) async -> DocumentOutput {
        let key = featureSHA256(document.data)
        if let hit = cache.value(for: key) { return hit }
        let output = await extract(document)
        if output.text != nil { cache.insert(output, cost: output.text?.utf8.count ?? 0, for: key) }
        return output
    }

    private func extract(_ doc: DocumentInput) async -> DocumentOutput {
        var paddleNote: String?
        if let paddle {
            if await paddle.isReachable() {
                do {
                    let pages = try await paddle.read(doc.data, mediaType: doc.mediaType)
                    let text = Self.capped(pages.filter { !$0.isEmpty }.joined(separator: "\n\n"), cap: textCap)
                    return DocumentOutput(text: text, pages: pages.count, ocrPages: pages.count, via: "paddleocr")
                } catch PaddleOCRClient.Failure.unsupported {
                    // not a PDF or image: the local path decides
                } catch {
                    paddleNote = "PaddleOCR failed (\(Self.describe(error)))"
                }
            } else {
                paddleNote = "PaddleOCR unreachable"
            }
        }
        return await local(doc, paddleNote: paddleNote)
    }

    private func local(_ doc: DocumentInput, paddleNote: String?) async -> DocumentOutput {
        let mode: LocalExtraction
        switch config.local {
        case .off:
            return DocumentOutput(text: nil, error: Self.join(paddleNote, "document reading is turned off"), via: "off")
        case .textOnly: mode = .textOnly
        case .appleVision: mode = .appleVision
        }
        let extractor = LocalExtractor(mode: mode, recognizer: recognizer, textCap: textCap)
        let data = doc.data, type = doc.mediaType
        let result = await Task.detached(priority: .userInitiated) { () -> Result<ExtractedDocument, LocalExtractionError> in
            do { return .success(try extractor.extract(data, mediaType: type)) }
            catch let e as LocalExtractionError { return .failure(e) }
            catch { return .failure(.notReadable(error.localizedDescription)) }
        }.value
        let via = mode == .appleVision ? "vision" : "pdfkit"
        switch result {
        case .success(let r):
            return DocumentOutput(text: Self.capped(r.text, cap: textCap), pages: r.pages, ocrPages: r.ocrPages, via: r.via)
        case .failure(.notReadable(let why)), .failure(.noText(let why)):
            return DocumentOutput(text: nil, error: Self.join(paddleNote, why), via: via)
        }
    }

    private static func join(_ note: String?, _ why: String) -> String { note.map { "\($0); \(why)" } ?? why }

    private static func describe(_ error: Error) -> String {
        switch error as? PaddleOCRClient.Failure {
        case .http(let s): return "HTTP \(s)"
        case .server(let m): return m
        case .empty: return "no text returned"
        case .unreachable: return "connection failed"
        default: return error.localizedDescription
        }
    }

    /// Cuts to `cap` UTF-8 bytes on a character boundary and says so.
    static func capped(_ text: String, cap: Int) -> String {
        guard text.utf8.count > cap else { return text }
        var bytes = Array(text.utf8.prefix(cap))
        while !bytes.isEmpty, String(bytes: bytes, encoding: .utf8) == nil { bytes.removeLast() }
        return String(decoding: bytes, as: UTF8.self) + "\n\n[document truncated at \(cap / 1024) KB]"
    }
}
