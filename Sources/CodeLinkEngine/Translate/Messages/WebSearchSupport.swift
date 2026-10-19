import Foundation

/// Anthropic's web_search_tool_result_error codes.
enum SearchErrorCode {
    static let unavailable = "unavailable"
    static let tooManyRequests = "too_many_requests"
    static let maxUsesExceeded = "max_uses_exceeded"
    static let invalidInput = "invalid_tool_input"
}

/// The model-facing text of searches.
enum WebSearchText {
    static let snippetMax = 1500
    static let hitsPerSearch = 5

    static func failed(_ code: String) -> String {
        switch code {
        case SearchErrorCode.maxUsesExceeded:
            return "Web search was not run: the search limit for this request was reached. Answer from what you already have."
        case SearchErrorCode.tooManyRequests:
            return "Web search failed: too many searches right now. Answer without it and say that you could not search."
        case SearchErrorCode.invalidInput:
            return "Web search was not run: the call needs a non-empty \"query\" string."
        default:
            return "Web search is unavailable. Answer without it and say that you could not search."
        }
    }

    static func hits(query: String, _ hits: [SearchHit]) -> String {
        var s = query.isEmpty ? "" : "Web search results for \"\(query)\":\n"
        guard !hits.isEmpty else { return s + "No results found." }
        for (i, h) in hits.enumerated() {
            s += "\n\(i + 1). \(h.title)\n   \(h.url)\n"
            if let age = h.pageAge, !age.isEmpty { s += "   Published: \(age)\n" }
            if !h.snippet.isEmpty { s += "   \(clip(h.snippet))\n" }
        }
        return s.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    static func clip(_ s: String) -> String {
        let (c, cut) = s.clipped(toBytes: snippetMax)
        return cut ? c + "…" : c
    }

    /// The tool result for a web_search_tool_result block from an earlier
    /// turn: the hits as they were shown (the snippet rides in
    /// encrypted_content, base64) or the error.
    static func historic(_ content: JSON?) -> String {
        if content?.type == "web_search_tool_result_error" {
            return failed(content?["error_code"]?.string ?? SearchErrorCode.unavailable)
        }
        let hits = (content?.array ?? []).map { r -> SearchHit in
            let title = r["title"]?.string ?? ""
            var snippet = ""
            if let enc = r["encrypted_content"]?.string, let d = Data(base64Encoded: enc) {
                let s = String(decoding: d, as: UTF8.self)
                if s != title { snippet = s }
            }
            return SearchHit(title: title, url: r["url"]?.string ?? "", snippet: snippet, pageAge: r["page_age"]?.string)
        }
        return Self.hits(query: "", hits)
    }
}

/// One search the model asked for, answered.
struct SearchOutcome: Sendable {
    /// srvtoolu_…: the id the client sees.
    var id: String
    /// The model's own id for the call.
    var callID: String
    var query: String
    var hits: [SearchHit] = []
    var errorCode: String?

    /// The web_search_tool_result content.
    var resultContent: JSON {
        if let errorCode { return ["type": "web_search_tool_result_error", "error_code": .string(errorCode)] }
        return .array(hits.map { h in
            var r: JSON = ["type": "web_search_result", "url": .string(h.url), "title": .string(h.title),
                           "encrypted_content": .string(Data(WebSearchText.clip(h.snippet.isEmpty ? h.title : h.snippet).utf8).base64EncodedString())]
            if let age = h.pageAge, !age.isEmpty { r["page_age"] = .string(age) }
            return r
        })
    }

    /// The two content blocks of a non-streaming message.
    var blocks: [JSON] {
        [["type": "server_tool_use", "id": .string(id), "name": .string(WebSearchTool.functionName), "input": ["query": .string(query)]],
         ["type": "web_search_tool_result", "tool_use_id": .string(id), "content": resultContent]]
    }

    var modelText: String { errorCode.map(WebSearchText.failed) ?? WebSearchText.hits(query: query, hits) }
}

/// Runs the searches of one request, counting them against max_uses.
final class SearchRunner {
    let tool: WebSearchTool
    let searcher: (any WebSearcher)?
    let log: any EventLog
    private(set) var searched = 0
    /// Searches that returned results (usage.server_tool_use).
    private(set) var succeeded = 0

    init(tool: WebSearchTool, searcher: (any WebSearcher)?, log: any EventLog) {
        self.tool = tool; self.searcher = searcher; self.log = log
    }

    /// Answers the model's web_search calls (id, JSON arguments), in order.
    func answer(_ calls: [(id: String, args: String)]) async -> [SearchOutcome] {
        var out: [SearchOutcome] = []
        for c in calls {
            let query = ((try? JSON.parse(c.args))?["query"]?.string ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
            var o = SearchOutcome(id: IDs.make("srvtoolu_"), callID: c.id, query: query)
            if query.isEmpty {
                o.errorCode = SearchErrorCode.invalidInput
            } else if searched >= tool.maxUses {
                o.errorCode = SearchErrorCode.maxUsesExceeded
            } else if let searcher {
                searched += 1
                do {
                    o.hits = Array(try await searcher.search(query: query, allowedDomains: tool.allowedDomains,
                                                             blockedDomains: tool.blockedDomains).prefix(WebSearchText.hitsPerSearch))
                    succeeded += 1
                    log.record(.webSearch(query: query, results: o.hits.count))
                } catch WebSearchError.tooManyRequests {
                    o.errorCode = SearchErrorCode.tooManyRequests
                } catch {
                    o.errorCode = SearchErrorCode.unavailable
                }
            } else {
                searched += 1
                o.errorCode = SearchErrorCode.unavailable
            }
            out.append(o)
        }
        return out
    }
}
