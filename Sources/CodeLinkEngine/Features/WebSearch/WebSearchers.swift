import Foundation

/// Results per search.
let webSearchResultCount = 5

/// Stand-in when web search is off or has no key: every search is "unavailable".
public struct UnavailableSearcher: WebSearcher {
    public init() {}
    public func search(query: String, allowedDomains: [String]?, blockedDomains: [String]?) async throws -> [SearchHit] {
        throw WebSearchError.unavailable
    }
}

private func send(_ request: URLRequest, session: URLSession) async throws -> Data {
    let data: Data, response: URLResponse
    do { (data, response) = try await session.data(for: request) }
    catch { throw WebSearchError.unavailable }
    guard let http = response as? HTTPURLResponse else { throw WebSearchError.unavailable }
    switch http.statusCode {
    case 200..<300: return data
    case 429: throw WebSearchError.tooManyRequests
    default: throw WebSearchError.unavailable   // 401/403 (bad key) and everything else
    }
}

private func cleanDomains(_ d: [String]?) -> [String] {
    (d ?? []).map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
}

/// Tavily: `POST https://api.tavily.com/search`.
public struct TavilySearcher: WebSearcher {
    public static let defaultEndpoint = URL(string: "https://api.tavily.com/search")!
    private let key: @Sendable () -> String?
    private let session: URLSession
    private let endpoint: URL

    public init(key: @escaping @Sendable () -> String?, session: URLSession = .shared, endpoint: URL = TavilySearcher.defaultEndpoint) {
        self.key = key; self.session = session; self.endpoint = endpoint
    }

    public func search(query: String, allowedDomains: [String]?, blockedDomains: [String]?) async throws -> [SearchHit] {
        guard let apiKey = key(), !apiKey.isEmpty else { throw WebSearchError.unavailable }
        var body: [String: JSONValue] = [
            "api_key": .string(apiKey), "query": .string(query), "max_results": .number(Double(webSearchResultCount)),
        ]
        let inc = cleanDomains(allowedDomains), exc = cleanDomains(blockedDomains)
        if !inc.isEmpty { body["include_domains"] = .array(inc.map { .string($0) }) }
        if !exc.isEmpty { body["exclude_domains"] = .array(exc.map { .string($0) }) }

        var req = URLRequest(url: endpoint, timeoutInterval: 30)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("Bearer " + apiKey, forHTTPHeaderField: "Authorization")
        req.httpBody = try? JSONEncoder().encode(JSONValue.object(body))

        let data = try await send(req, session: session)
        guard let json = try? JSONDecoder().decode(JSONValue.self, from: data),
              let results = json.feature("results")?.featureArray else { throw WebSearchError.unavailable }
        return results.compactMap { r in
            guard let url = r.feature("url")?.featureString, !url.isEmpty else { return nil }
            let age = r.feature("published_date")?.featureString ?? r.feature("age")?.featureString
            return SearchHit(title: r.feature("title")?.featureString ?? url, url: url,
                             snippet: r.feature("content")?.featureString ?? "", pageAge: age?.isEmpty == false ? age : nil)
        }
    }
}

/// Brave: `GET https://api.search.brave.com/res/v1/web/search?q=…&count=5`.
public struct BraveSearcher: WebSearcher {
    public static let defaultEndpoint = URL(string: "https://api.search.brave.com/res/v1/web/search")!
    private let key: @Sendable () -> String?
    private let session: URLSession
    private let endpoint: URL

    public init(key: @escaping @Sendable () -> String?, session: URLSession = .shared, endpoint: URL = BraveSearcher.defaultEndpoint) {
        self.key = key; self.session = session; self.endpoint = endpoint
    }

    /// Brave has no domain parameters: they become `site:` / `-site:` terms.
    static func composeQuery(_ query: String, allowed: [String]?, blocked: [String]?) -> String {
        var q = query
        let inc = cleanDomains(allowed), exc = cleanDomains(blocked)
        if inc.count == 1 { q += " site:" + inc[0] }
        else if inc.count > 1 { q += " (" + inc.map { "site:" + $0 }.joined(separator: " OR ") + ")" }
        for d in exc { q += " -site:" + d }
        return q
    }

    static func stripMarkup(_ s: String) -> String {
        var t = s.replacingOccurrences(of: "<[^>]+>", with: "", options: .regularExpression)
        for (e, c) in [("&amp;", "&"), ("&lt;", "<"), ("&gt;", ">"), ("&quot;", "\""), ("&#x27;", "'"), ("&#39;", "'")] {
            t = t.replacingOccurrences(of: e, with: c)
        }
        return t
    }

    public func search(query: String, allowedDomains: [String]?, blockedDomains: [String]?) async throws -> [SearchHit] {
        guard let apiKey = key(), !apiKey.isEmpty else { throw WebSearchError.unavailable }
        guard var comps = URLComponents(url: endpoint, resolvingAgainstBaseURL: false) else { throw WebSearchError.unavailable }
        comps.queryItems = [
            URLQueryItem(name: "q", value: Self.composeQuery(query, allowed: allowedDomains, blocked: blockedDomains)),
            URLQueryItem(name: "count", value: String(webSearchResultCount)),
        ]
        guard let url = comps.url else { throw WebSearchError.unavailable }
        var req = URLRequest(url: url, timeoutInterval: 30)
        req.setValue(apiKey, forHTTPHeaderField: "X-Subscription-Token")
        req.setValue("application/json", forHTTPHeaderField: "Accept")

        let data = try await send(req, session: session)
        guard let json = try? JSONDecoder().decode(JSONValue.self, from: data) else { throw WebSearchError.unavailable }
        // A response without web.results is a valid "no hits" answer.
        let results = json.feature(path: "web", "results")?.featureArray ?? []
        return results.compactMap { r in
            guard let url = r.feature("url")?.featureString, !url.isEmpty else { return nil }
            let age = r.feature("age")?.featureString
            return SearchHit(title: Self.stripMarkup(r.feature("title")?.featureString ?? url), url: url,
                             snippet: Self.stripMarkup(r.feature("description")?.featureString ?? ""),
                             pageAge: age?.isEmpty == false ? age : nil)
        }
    }
}
