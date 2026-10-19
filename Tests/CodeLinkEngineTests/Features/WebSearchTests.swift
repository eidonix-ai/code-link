import Testing
import Foundation
@testable import CodeLinkEngine

private let tavilyBody = #"{"results":[{"title":"Swift","url":"https://swift.org","content":"The Swift language","published_date":"2026-10-01"},{"title":"No date","url":"https://a.example/x","content":"c"},{"title":"bad","content":"no url"}]}"#

@Test func tavilySearchMapsResultsAndSendsRequest() async throws {
    let stub = StubSession { _ in .init(200, tavilyBody) }
    let s = TavilySearcher(key: { "tvly-key" }, session: stub.session)
    let hits = try await s.search(query: "swift 6.4", allowedDomains: ["swift.org"], blockedDomains: ["spam.com"])
    #expect(hits == [SearchHit(title: "Swift", url: "https://swift.org", snippet: "The Swift language", pageAge: "2026-10-01"),
                     SearchHit(title: "No date", url: "https://a.example/x", snippet: "c")])
    let req = try #require(stub.requests.first)
    #expect(req.method == "POST")
    #expect(req.url.absoluteString == "https://api.tavily.com/search")
    #expect(req.headers["Authorization"] == "Bearer tvly-key")
    let body = try JSONDecoder().decode(JSONValue.self, from: req.body)
    #expect(body == json(#"{"api_key":"tvly-key","query":"swift 6.4","max_results":5,"include_domains":["swift.org"],"exclude_domains":["spam.com"]}"#))
}

@Test func tavilyOmitsEmptyDomainLists() async throws {
    let stub = StubSession { _ in .init(200, #"{"results":[]}"#) }
    let hits = try await TavilySearcher(key: { "k" }, session: stub.session).search(query: "q", allowedDomains: [], blockedDomains: nil)
    #expect(hits.isEmpty)
    let body = try JSONDecoder().decode(JSONValue.self, from: stub.requests[0].body)
    #expect(body.feature("include_domains") == nil && body.feature("exclude_domains") == nil)
}

@Test(arguments: [(401, WebSearchError.unavailable), (403, .unavailable), (429, .tooManyRequests), (500, .unavailable), (400, .unavailable)])
func tavilyStatusMapping(status: Int, expected: WebSearchError) async {
    let stub = StubSession { _ in .init(status, "{}") }
    await #expect(throws: expected) { try await TavilySearcher(key: { "k" }, session: stub.session).search(query: "q", allowedDomains: nil, blockedDomains: nil) }
}

@Test func tavilyUnavailableWithoutKeyOrOnTransportFailureOrGarbage() async {
    let noKey = StubSession { _ in .init(200, tavilyBody) }
    await #expect(throws: WebSearchError.unavailable) { try await TavilySearcher(key: { nil }, session: noKey.session).search(query: "q", allowedDomains: nil, blockedDomains: nil) }
    #expect(noKey.requests.isEmpty)
    let down = StubSession { _ in throw URLError(.notConnectedToInternet) }
    await #expect(throws: WebSearchError.unavailable) { try await TavilySearcher(key: { "k" }, session: down.session).search(query: "q", allowedDomains: nil, blockedDomains: nil) }
    let junk = StubSession { _ in .init(200, "<html>") }
    await #expect(throws: WebSearchError.unavailable) { try await TavilySearcher(key: { "k" }, session: junk.session).search(query: "q", allowedDomains: nil, blockedDomains: nil) }
}

private let braveBody = #"{"web":{"results":[{"title":"Swift <strong>6</strong>","url":"https://swift.org","description":"Fast &amp; safe","age":"2 days ago"},{"title":"T","url":"https://b.example","description":"d"}]}}"#

@Test func braveSearchMapsResultsAndSendsRequest() async throws {
    let stub = StubSession { _ in .init(200, braveBody) }
    let s = BraveSearcher(key: { "brave-key" }, session: stub.session)
    let hits = try await s.search(query: "swift", allowedDomains: nil, blockedDomains: nil)
    #expect(hits == [SearchHit(title: "Swift 6", url: "https://swift.org", snippet: "Fast & safe", pageAge: "2 days ago"),
                     SearchHit(title: "T", url: "https://b.example", snippet: "d")])
    let req = try #require(stub.requests.first)
    #expect(req.method == "GET")
    #expect(req.headers["X-Subscription-Token"] == "brave-key")
    let comps = try #require(URLComponents(url: req.url, resolvingAgainstBaseURL: false))
    #expect(comps.host == "api.search.brave.com" && comps.path == "/res/v1/web/search")
    #expect(comps.queryItems?.first { $0.name == "q" }?.value == "swift")
    #expect(comps.queryItems?.first { $0.name == "count" }?.value == "5")
}

@Test func braveDomainFiltersBecomeSiteTerms() async throws {
    #expect(BraveSearcher.composeQuery("q", allowed: ["a.com"], blocked: nil) == "q site:a.com")
    #expect(BraveSearcher.composeQuery("q", allowed: ["a.com", "b.org"], blocked: ["x.com", "y.com"]) == "q (site:a.com OR site:b.org) -site:x.com -site:y.com")
    #expect(BraveSearcher.composeQuery("q", allowed: [], blocked: [" "]) == "q")
    let stub = StubSession { _ in .init(200, braveBody) }
    _ = try await BraveSearcher(key: { "k" }, session: stub.session).search(query: "swift", allowedDomains: ["swift.org"], blockedDomains: ["x.com"])
    let comps = URLComponents(url: stub.requests[0].url, resolvingAgainstBaseURL: false)
    #expect(comps?.queryItems?.first { $0.name == "q" }?.value == "swift site:swift.org -site:x.com")
}

@Test(arguments: [(401, WebSearchError.unavailable), (403, .unavailable), (429, .tooManyRequests), (503, .unavailable)])
func braveStatusMapping(status: Int, expected: WebSearchError) async {
    let stub = StubSession { _ in .init(status, "{}") }
    await #expect(throws: expected) { try await BraveSearcher(key: { "k" }, session: stub.session).search(query: "q", allowedDomains: nil, blockedDomains: nil) }
}

@Test func braveNoResultsIsEmptyNotError() async throws {
    let stub = StubSession { _ in .init(200, "{}") }
    #expect(try await BraveSearcher(key: { "k" }, session: stub.session).search(query: "q", allowedDomains: nil, blockedDomains: nil).isEmpty)
}

@Test func unavailableSearcherThrows() async {
    await #expect(throws: WebSearchError.unavailable) { try await UnavailableSearcher().search(query: "q", allowedDomains: nil, blockedDomains: nil) }
}
