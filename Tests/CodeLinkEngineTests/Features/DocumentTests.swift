import Testing
import Foundation
@testable import CodeLinkEngine

private let paddleURL = URL(string: "http://ocr.test:8002/layout-parsing")!

private func input(_ data: Data, _ type: String = "application/pdf", title: String? = "doc") -> DocumentInput {
    DocumentInput(data: data, mediaType: type, title: title)
}

private func service(_ local: LocalExtraction = .appleVision, paddle: URL? = nil, session: URLSession = .shared,
                     recognizer: any TextRecognizer = FakeRecognizer(fail: true), cap: Int = DocumentService.defaultTextCap) -> DocumentService {
    DocumentService(config: DocumentsConfig(paddleOCR: paddle, local: local), session: session, recognizer: recognizer, textCap: cap)
}

private let paddleOK = ##"{"logId":"x","errorCode":0,"errorMsg":"Success","result":{"layoutParsingResults":[{"markdown":{"text":"# Page one\n| a | b |"}},{"markdown":{"text":"Page two text"}}]}}"##

// MARK: PDFKit

@Test func pdfKitReadsTextLayerPerPage() async throws {
    let rec = FakeRecognizer(fail: true)
    let out = await service(recognizer: rec).read(input(Fixtures.textPDF(["Quarterly revenue grew twelve percent", "Second page about margins"])))
    let text = try #require(out.text)
    #expect(text.contains("Quarterly revenue grew twelve percent"))
    #expect(text.contains("Second page about margins"))
    #expect(text.range(of: "Quarterly")!.lowerBound < text.range(of: "Second page")!.lowerBound)
    #expect(out.via == "pdfkit" && out.pages == 2 && out.ocrPages == 0 && out.error == nil)
    #expect(rec.calls == 0)
}

@Test func pagesWithoutTextAreRenderedAndOCRd() async throws {
    let rec = FakeRecognizer("Scanned page words")
    let out = await service(recognizer: rec).read(input(Fixtures.mixedPDF(text: "This page has a real text layer", scanned: "ignored")))
    let text = try #require(out.text)
    #expect(text.contains("This page has a real text layer"))
    #expect(text.contains("Scanned page words"))
    #expect(out.pages == 2 && out.ocrPages == 1 && out.via == "pdfkit+vision")
    #expect(rec.calls == 1)   // only the text-less page was rendered
}

@Test func visionReadsARenderedScannedPage() async throws {
    let out = await DocumentService(config: DocumentsConfig(local: .appleVision)).read(input(Fixtures.scannedPDF("INVOICE 4721")))
    let text = try #require(out.text, "Vision found nothing: \(out.error ?? "")")
    #expect(text.localizedCaseInsensitiveContains("invoice"))
    #expect(text.contains("4721"))
    #expect(out.via == "vision" && out.ocrPages == 1 && out.pages == 1)
}

@Test func visionReadsAnImageSentAsADocument() async throws {
    let png = Fixtures.png(Fixtures.textImage("HELLO WORLD"))
    let out = await DocumentService(config: DocumentsConfig(local: .appleVision)).read(input(png, "image/png"))
    let text = try #require(out.text, "Vision found nothing: \(out.error ?? "")")
    #expect(text.localizedCaseInsensitiveContains("hello"))
    #expect(text.localizedCaseInsensitiveContains("world"))
    #expect(out.via == "vision" && out.pages == 1 && out.ocrPages == 1)
}

@Test func textOnlyDoesNotOCR() async {
    let rec = FakeRecognizer("never")
    let out = await service(.textOnly, recognizer: rec).read(input(Fixtures.scannedPDF("SCAN")))
    #expect(out.text == nil)
    #expect(out.error?.contains("no text layer") == true)
    #expect(rec.calls == 0)
    let img = await service(.textOnly, recognizer: rec).read(input(Fixtures.png(Fixtures.textImage("X")), "image/png"))
    #expect(img.text == nil && img.error != nil && rec.calls == 0)
}

@Test func offReturnsANote() async {
    let out = await service(.off).read(input(Fixtures.textPDF(["Some text that exists"])))
    #expect(out.text == nil)
    #expect(out.error?.contains("turned off") == true)
}

@Test func unreadableInputsReportWhy() async {
    let bad = await service().read(input(Data("not a pdf".utf8)))
    #expect(bad.text == nil && bad.error == "not a readable PDF")
    let other = await service().read(input(Data([1, 2]), "application/zip"))
    #expect(other.text == nil && other.error?.contains("unsupported") == true)
    let plain = await service().read(input(Data("hello text".utf8), "text/plain"))
    #expect(plain.text == "hello text" && plain.via == "text")
}

// MARK: PaddleOCR

@Test func paddleOCRIsUsedFirstAndSendsTheDocument() async throws {
    let stub = StubSession { r in r.method == "POST" ? .init(200, paddleOK) : .init(405, "") }
    let rec = FakeRecognizer(fail: true)
    let pdf = Fixtures.textPDF(["local text that should not be used"])
    let out = await service(paddle: paddleURL, session: stub.session, recognizer: rec).read(input(pdf))
    #expect(out.text == "# Page one\n| a | b |\n\nPage two text")
    #expect(out.via == "paddleocr" && out.pages == 2)
    let post = try #require(stub.requests.first { $0.method == "POST" })
    #expect(post.url == paddleURL)
    let body = try JSONDecoder().decode(JSONValue.self, from: post.body)
    #expect(body.feature("fileType") == .number(0))
    #expect(body.feature("file")?.featureString == pdf.base64EncodedString())
    #expect(body.feature("returnMarkdownImages") == .bool(false))
    #expect(body.feature("visualize") == .bool(false))
    #expect(rec.calls == 0)
}

@Test func paddleOCRGetsImagesAsFileTypeOne() async throws {
    let stub = StubSession { r in r.method == "POST" ? .init(200, paddleOK) : .init(405, "") }
    _ = await service(paddle: paddleURL, session: stub.session).read(input(Data([1, 2, 3]), "image/png"))
    let post = try #require(stub.requests.first { $0.method == "POST" })
    #expect(try JSONDecoder().decode(JSONValue.self, from: post.body).feature("fileType") == .number(1))
}

@Test func unreachablePaddleFallsBackToLocal() async throws {
    let stub = StubSession { _ in throw URLError(.cannotConnectToHost) }
    let out = await service(paddle: paddleURL, session: stub.session).read(input(Fixtures.textPDF(["Local fallback text works fine"])))
    #expect(out.text?.contains("Local fallback text works fine") == true)
    #expect(out.via == "pdfkit")
    #expect(stub.requests.allSatisfy { $0.method == "GET" })  // probe only, no document sent
}

@Test func paddleErrorFallsBackToLocal() async throws {
    for reply in [StubURLProtocol.Reply(500, "boom"), .init(200, #"{"errorCode":10,"errorMsg":"bad"}"#), .init(200, "<html>"), .init(200, #"{"errorCode":0,"result":{"layoutParsingResults":[]}}"#)] {
        let stub = StubSession { r in r.method == "POST" ? reply : .init(405, "") }
        let out = await service(paddle: paddleURL, session: stub.session).read(input(Fixtures.textPDF(["Local fallback text works fine"])))
        #expect(out.text?.contains("Local fallback text works fine") == true, "reply \(reply.status) \(String(data: reply.body, encoding: .utf8) ?? "")")
        #expect(out.via == "pdfkit")
        #expect(stub.requests.contains { $0.method == "POST" })
    }
}

@Test func failureNamesPaddleAndTheLocalReason() async {
    let stub = StubSession { r in r.method == "POST" ? .init(503, "") : .init(405, "") }
    let out = await service(.off, paddle: paddleURL, session: stub.session).read(input(Fixtures.textPDF(["x"])))
    #expect(out.text == nil)
    #expect(out.error == "PaddleOCR failed (HTTP 503); document reading is turned off")
}

@Test func reachabilityProbeIsCachedForAMinute() async {
    let stub = StubSession { _ in throw URLError(.cannotConnectToHost) }
    let s = service(paddle: paddleURL, session: stub.session)
    _ = await s.read(input(Fixtures.textPDF(["First document with enough text"])))
    _ = await s.read(input(Fixtures.textPDF(["Second document with enough text"])))
    _ = await s.read(input(Fixtures.textPDF(["Third document with enough text"])))
    #expect(stub.requests.count == 1)
}

@Test func probeTTLExpires() async {
    let stub = StubSession { _ in .init(405, "") }
    let client = PaddleOCRClient(endpoint: paddleURL, session: stub.session, probeTTL: 60)
    let t0 = Date()
    #expect(await client.isReachable(now: t0))
    #expect(await client.isReachable(now: t0.addingTimeInterval(30)))
    #expect(stub.requests.count == 1)
    #expect(await client.isReachable(now: t0.addingTimeInterval(61)))
    #expect(stub.requests.count == 2)
}

@Test func gatewayErrorOnProbeMeansUnreachable() async {
    let stub = StubSession { _ in .init(502, "") }
    #expect(await PaddleOCRClient(endpoint: paddleURL, session: stub.session).isReachable() == false)
}

// MARK: Cache and cap

@Test func resentDocumentsHitTheCache() async {
    let stub = StubSession { r in r.method == "POST" ? .init(200, paddleOK) : .init(405, "") }
    let s = service(paddle: paddleURL, session: stub.session)
    let pdf = Fixtures.textPDF(["cached"])
    let a = await s.read(input(pdf, title: "one"))
    let b = await s.read(input(pdf, title: "two"))
    #expect(a == b)
    #expect(stub.requests.filter { $0.method == "POST" }.count == 1)
}

@Test func failuresAreNotCached() async {
    let calls = Counter()
    let stub = StubSession { r in
        guard r.method == "POST" else { return .init(405, "") }
        return calls.next() == 1 ? .init(500, "") : .init(200, paddleOK)
    }
    let s = service(.off, paddle: paddleURL, session: stub.session)
    let pdf = Fixtures.textPDF(["x"])
    #expect(await s.read(input(pdf)).text == nil)
    #expect(await s.read(input(pdf)).via == "paddleocr")
}

@Test func textIsCappedOnACharacterBoundary() async throws {
    let long = String(repeating: "é", count: 5000)   // 2 bytes each
    let reply = #"{"errorCode":0,"result":{"layoutParsingResults":[{"markdown":{"text":"\#(long)"}}]}}"#
    let stub = StubSession { r in r.method == "POST" ? .init(200, reply) : .init(405, "") }
    let out = await service(paddle: paddleURL, session: stub.session, cap: 1001).read(input(Data([1])))
    let text = try #require(out.text)
    let body = text.components(separatedBy: "\n\n[document truncated").first!
    #expect(body == String(repeating: "é", count: 500))
    #expect(text.hasSuffix("[document truncated at 0 KB]"))
    #expect(DocumentService.capped("short", cap: 100) == "short")
}

@Test func defaultCapIs600KB() { #expect(DocumentService.defaultTextCap == 600 * 1024) }

final class Counter: @unchecked Sendable {
    private let lock = NSLock(); private var n = 0
    func next() -> Int { lock.withLock { n += 1; return n } }
}
