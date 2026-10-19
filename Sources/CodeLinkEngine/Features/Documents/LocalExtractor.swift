import Foundation
import PDFKit
import Vision
import CoreGraphics

/// Reads the text in an image. A protocol so tests can stand in for Vision.
protocol TextRecognizer: Sendable {
    func recognize(_ image: CGImage) throws -> String
}

/// Apple Vision OCR: `.accurate`, language correction on.
struct VisionTextRecognizer: TextRecognizer {
    func recognize(_ image: CGImage) throws -> String {
        let request = VNRecognizeTextRequest()
        request.recognitionLevel = .accurate
        request.usesLanguageCorrection = true
        request.automaticallyDetectsLanguage = true
        try VNImageRequestHandler(cgImage: image, options: [:]).perform([request])
        return (request.results ?? []).compactMap { $0.topCandidates(1).first?.string }.joined(separator: "\n")
    }
}

struct ExtractedDocument: Sendable {
    var text: String            // pages joined by blank lines (empty pages dropped)
    var pages: Int
    var ocrPages: Int
    var via: String
}

enum LocalExtractionError: Error, Equatable {
    case notReadable(String)
    case noText(String)
}

/// PDFKit text per page; pages with fewer than `minTextChars` characters are
/// rendered at ~200 dpi and read with OCR. Images go straight to OCR.
struct LocalExtractor: Sendable {
    static let minTextChars = 20
    static let renderDPI: CGFloat = 200
    static let maxRenderSide: CGFloat = 5000

    var mode: LocalExtraction
    var recognizer: any TextRecognizer = VisionTextRecognizer()
    var textCap: Int

    func extract(_ data: Data, mediaType: String) throws -> ExtractedDocument {
        if mediaType == "application/pdf" { return try extractPDF(data) }
        if mediaType.hasPrefix("image/") { return try extractImage(data) }
        if mediaType.hasPrefix("text/") || mediaType == "application/json" {
            guard let s = String(data: data, encoding: .utf8) else { throw LocalExtractionError.notReadable("not valid UTF-8 text") }
            return ExtractedDocument(text: s, pages: 1, ocrPages: 0, via: "text")
        }
        throw LocalExtractionError.notReadable("unsupported document type \(mediaType)")
    }

    private func extractImage(_ data: Data) throws -> ExtractedDocument {
        guard mode == .appleVision else { throw LocalExtractionError.notReadable("images need OCR (Apple Vision or PaddleOCR)") }
        guard let src = CGImageSourceCreateWithData(data as CFData, nil), let image = CGImageSourceCreateImageAtIndex(src, 0, nil) else {
            throw LocalExtractionError.notReadable("not a readable image")
        }
        let text = try recognizer.recognize(image).trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty else { throw LocalExtractionError.noText("no text found in the image") }
        return ExtractedDocument(text: text, pages: 1, ocrPages: 1, via: "vision")
    }

    private func extractPDF(_ data: Data) throws -> ExtractedDocument {
        guard let doc = PDFDocument(data: data) else { throw LocalExtractionError.notReadable("not a readable PDF") }
        if doc.isLocked { throw LocalExtractionError.notReadable("the PDF is password protected") }
        var texts: [String] = []
        var ocrPages = 0
        var size = 0
        let ocrAllowed = mode == .appleVision
        for i in 0..<doc.pageCount {
            if Task.isCancelled { break }
            guard let page = doc.page(at: i) else { continue }
            var text = (page.string ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
            if text.count < Self.minTextChars, ocrAllowed, let image = Self.render(page) {
                if let ocr = try? recognizer.recognize(image).trimmingCharacters(in: .whitespacesAndNewlines), ocr.count > text.count {
                    text = ocr; ocrPages += 1
                }
            }
            if !text.isEmpty { texts.append(text); size += text.utf8.count }
            if size > textCap { break }   // the cap will cut here; don't OCR the rest
        }
        guard !texts.isEmpty else {
            throw LocalExtractionError.noText(ocrAllowed ? "no text found" : "the PDF has no text layer (scanned); enable Apple Vision or PaddleOCR")
        }
        let via = ocrPages == 0 ? "pdfkit" : (ocrPages == texts.count ? "vision" : "pdfkit+vision")
        return ExtractedDocument(text: texts.joined(separator: "\n\n"), pages: doc.pageCount, ocrPages: ocrPages, via: via)
    }

    /// The page drawn on white at ~200 dpi (capped so a poster-size page stays bounded).
    static func render(_ page: PDFPage) -> CGImage? {
        let box = page.bounds(for: .mediaBox)
        guard box.width > 0, box.height > 0 else { return nil }
        var scale = renderDPI / 72
        scale = min(scale, maxRenderSide / max(box.width, box.height))
        let w = max(Int((box.width * scale).rounded()), 1), h = max(Int((box.height * scale).rounded()), 1)
        guard let ctx = CGContext(data: nil, width: w, height: h, bitsPerComponent: 8, bytesPerRow: 0,
                                  space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else { return nil }
        ctx.setFillColor(CGColor(red: 1, green: 1, blue: 1, alpha: 1))
        ctx.fill(CGRect(x: 0, y: 0, width: w, height: h))
        ctx.scaleBy(x: scale, y: scale)
        page.draw(with: .mediaBox, to: ctx)
        return ctx.makeImage()
    }
}
