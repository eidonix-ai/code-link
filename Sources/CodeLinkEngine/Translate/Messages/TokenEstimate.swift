import Foundation

enum TokenEstimate {
    static let imageTokens = 1600
    static let maxPDFTokens = 60_000

    /// The prompt size of a JSON request body: ~4 characters per token for
    /// text, with embedded base64 media costed separately (a flat figure per
    /// image, bytes-based for PDFs) instead of by its base64 length.
    static func tokens(_ body: Data) -> Int {
        body.withUnsafeBytes { raw -> Int in
            let b = raw.bindMemory(to: UInt8.self)
            var tokens = 0
            var textBytes = b.count
            var start = -1
            for i in 0...b.count {
                var inRun = false
                if i < b.count {
                    let c = b[i]
                    inRun = (c >= 65 && c <= 90) || (c >= 97 && c <= 122) || (c >= 48 && c <= 57)
                        || c == 43 || c == 47 || c == 61 || c == 95 || c == 45
                }
                if inRun, start < 0 { start = i }
                else if !inRun, start >= 0 {
                    let n = i - start
                    if n >= 2048 {
                        textBytes -= n
                        // "JVBERi" is "%PDF-" in base64.
                        let isPDF = n >= 6 && b[start] == 74 && b[start + 1] == 86 && b[start + 2] == 66
                            && b[start + 3] == 69 && b[start + 4] == 82 && b[start + 5] == 105
                        tokens += isPDF ? min(n * 3 / 4 / 150, maxPDFTokens) : imageTokens
                    }
                    start = -1
                }
            }
            return tokens + textBytes / 4
        }
    }

    static func tokens(_ json: JSON) -> Int { tokens(json.data) }
}
