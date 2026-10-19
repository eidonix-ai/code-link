import Testing
import Foundation
import CodeLinkEngine
@testable import CodeLink

private func base() -> EngineConfig {
    EngineConfig(backends: [Backend(id: "a", baseURL: URL(string: "http://a/v1")!), Backend(id: "b", baseURL: URL(string: "http://b/v1")!)])
}

@Test func settingATierCreatesTheModelWithProbedFacts() {
    var c = base()
    c.setRole(.main, to: ModelChoice(backendID: "a", upstream: "GLM"), probed: ProbedModel(upstreamModel: "GLM", contextWindow: 131_072, logprobs: true))
    #expect(c.roles.main == "GLM" && c.isConfigured)
    #expect(c.mainModel?.contextWindow == 131_072 && c.mainModel?.logprobs == true)
}

@Test func tiersMayShareOrSplitProviders() {
    var c = base()
    c.setRole(.main, to: ModelChoice(backendID: "a", upstream: "GLM"))
    c.setRole(.opus, to: ModelChoice(backendID: "a", upstream: "GLM"))
    #expect(c.models.count == 1 && c.roles.opus == "GLM")
    c.setRole(.opus, to: ModelChoice(backendID: "b", upstream: "GLM"))
    #expect(c.roles.opus == "GLM (b)" && c.models.count == 2)
    #expect(c.choice(for: .opus) == ModelChoice(backendID: "b", upstream: "GLM"))
}

@Test func clearingARolePrunesUnusedModels() {
    var c = base()
    c.setRole(.main, to: ModelChoice(backendID: "a", upstream: "M"))
    c.setRole(.small, to: ModelChoice(backendID: "a", upstream: "S"))
    #expect(c.models.count == 2)
    c.setRole(.small, to: nil)
    #expect(c.roles.small == nil && c.models.map(\.id) == ["M"])
    c.setRole(.main, to: nil)
    #expect(c.roles.main == "" && !c.isConfigured && c.models.isEmpty)
}

@Test func renamingAProviderFollowsIntoModels() {
    var c = base()
    c.setRole(.main, to: ModelChoice(backendID: "a", upstream: "M"))
    c.renameBackend(from: "a", to: "cn3")
    #expect(c.backends.first?.id == "cn3" && c.mainModel?.backendID == "cn3" && c.isConfigured)
}

@Test func removingAProviderClearsItsRoles() {
    var c = base()
    c.setRole(.main, to: ModelChoice(backendID: "a", upstream: "M"))
    c.setRole(.opus, to: ModelChoice(backendID: "b", upstream: "O"))
    c.removeBackend("a")
    #expect(c.roles.main == "" && c.roles.opus == "O" && c.backends.map(\.id) == ["b"])
}

@Test func problemsAreReportedBeforeSaving() {
    var c = base()
    #expect(c.problems().isEmpty)
    c.backends.append(Backend(id: "a", baseURL: URL(string: "ftp://x")!))
    c.port = 80
    let p = c.problems().joined(separator: "\n")
    #expect(p.contains("unique") && p.contains("port") && p.contains("http"))
}

@Test func statusLineAndIcon() {
    var c = base()
    #expect(EngineStatus.make(config: c, state: .stopped, backendReachable: nil).kind == .notConfigured)
    c.setRole(.main, to: ModelChoice(backendID: "a", upstream: "GLM"))
    #expect(EngineStatus.make(config: c, state: .stopped, backendReachable: nil).kind == .stopped)
    let running = EngineStatus.make(config: c, state: .running(port: 18765), backendReachable: true)
    #expect(running.kind == .running && running.line == "Running on 127.0.0.1:18765 · GLM")
    let down = EngineStatus.make(config: c, state: .running(port: 18765), backendReachable: false)
    #expect(down.kind == .backendUnreachable && down.line.contains("a is unreachable"))
    #expect(EngineStatus.make(config: c, state: .failed("port in use"), backendReachable: nil).kind == .failed)
}

@Test func activityDescribesEventsAndCaps() async {
    let d = ActivityEntry.describe(.request(route: "/v1/messages", model: "GLM", status: 529, latencyMS: 12))
    #expect(d.text == "/v1/messages · GLM · 529 · 12 ms" && d.level == .error)
    let dec = ActivityEntry.describe(.decision(stage: 1, model: "GLM", pBlock: 0.5, severity: 51, latencyMS: 300, fallback: "no logprobs"))
    #expect(dec.text.contains("p(block) 0.50") && dec.level == .warning)
    await MainActor.run {
        let store = ActivityStore()
        for i in 0..<(ActivityStore.limit + 20) { store.append(.error("e\(i)")) }
        #expect(store.entries.count == ActivityStore.limit)
        #expect(store.entries.first?.text == "e\(ActivityStore.limit + 19)")
    }
}

@Test func searchTestRequests() {
    let t = EndpointCheck.searchRequest(.tavily, key: "k")
    #expect(t.httpMethod == "POST" && t.url?.host == "api.tavily.com" && t.value(forHTTPHeaderField: "Authorization") == "Bearer k")
    let b = EndpointCheck.searchRequest(.brave, key: "k")
    #expect(b.httpMethod == "GET" && b.value(forHTTPHeaderField: "X-Subscription-Token") == "k")
}
