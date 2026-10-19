import Foundation
import CoreGraphics
import CoreText
import ImageIO
import UniformTypeIdentifiers
@testable import CodeLinkEngine

enum Fixtures {
    static let pageBox = CGRect(x: 0, y: 0, width: 612, height: 792)

    static func drawText(_ text: String, in ctx: CGContext, at point: CGPoint, size: CGFloat) {
        let font = CTFontCreateWithName("Helvetica" as CFString, size, nil)
        let attrs: [NSAttributedString.Key: Any] = [.font: font, kCTForegroundColorFromContextAttributeName as NSAttributedString.Key: true]
        let line = CTLineCreateWithAttributedString(NSAttributedString(string: text, attributes: attrs))
        ctx.textPosition = point
        CTLineDraw(line, ctx)
    }

    /// A PDF whose pages carry the given strings as real text (nil = an empty page).
    static func textPDF(_ pages: [String?]) -> Data {
        pdf(pages.count) { i, ctx in
            if let t = pages[i] { ctx.setFillColor(CGColor(gray: 0, alpha: 1)); drawText(t, in: ctx, at: CGPoint(x: 72, y: 700), size: 18) }
        }
    }

    /// A PDF page that is only a picture of text (no text layer).
    static func scannedPDF(_ text: String) -> Data {
        let image = textImage(text)
        return pdf(1) { _, ctx in ctx.draw(image, in: pageBox) }
    }

    /// Page 1 real text, page 2 a scan.
    static func mixedPDF(text: String, scanned: String) -> Data {
        let image = textImage(scanned)
        return pdf(2) { i, ctx in
            if i == 0 { ctx.setFillColor(CGColor(gray: 0, alpha: 1)); drawText(text, in: ctx, at: CGPoint(x: 72, y: 700), size: 18) }
            else { ctx.draw(image, in: pageBox) }
        }
    }

    private static func pdf(_ count: Int, draw: (Int, CGContext) -> Void) -> Data {
        let data = NSMutableData()
        var box = pageBox
        let ctx = CGContext(consumer: CGDataConsumer(data: data)!, mediaBox: &box, nil)!
        for i in 0..<count {
            ctx.beginPDFPage(nil)
            draw(i, ctx)
            ctx.endPDFPage()
        }
        ctx.closePDF()
        return data as Data
    }

    /// Black text on white, big enough for OCR.
    static func textImage(_ text: String, size: CGSize = CGSize(width: 1224, height: 1584), fontSize: CGFloat = 96) -> CGImage {
        let ctx = CGContext(data: nil, width: Int(size.width), height: Int(size.height), bitsPerComponent: 8, bytesPerRow: 0,
                            space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
        ctx.setFillColor(CGColor(gray: 1, alpha: 1)); ctx.fill(CGRect(origin: .zero, size: size))
        ctx.setFillColor(CGColor(gray: 0, alpha: 1))
        drawText(text, in: ctx, at: CGPoint(x: 60, y: size.height / 2), size: fontSize)
        return ctx.makeImage()!
    }

    static func png(_ image: CGImage) -> Data {
        let data = NSMutableData()
        let dest = CGImageDestinationCreateWithData(data, UTType.png.identifier as CFString, 1, nil)!
        CGImageDestinationAddImage(dest, image, nil)
        CGImageDestinationFinalize(dest)
        return data as Data
    }
}

/// A recognizer that returns canned text and counts calls; throws if `fail`.
final class FakeRecognizer: TextRecognizer, @unchecked Sendable {
    private let lock = NSLock()
    private var _calls = 0
    let text: String
    let fail: Bool
    init(_ text: String = "", fail: Bool = false) { self.text = text; self.fail = fail }
    var calls: Int { lock.withLock { _calls } }
    func recognize(_ image: CGImage) throws -> String {
        lock.withLock { _calls += 1 }
        if fail { throw FakeError() }
        return text
    }
}
