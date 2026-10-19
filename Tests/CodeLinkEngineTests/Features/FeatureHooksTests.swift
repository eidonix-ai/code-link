import Testing
import Foundation
@testable import CodeLinkEngine

private struct Secrets: SecretStore {
    var values: [String: String]
    func secret(_ ref: String) -> String? { values[ref] }
}

private let glm = Model(id: "GLM", backendID: "b", upstreamModel: "glm")
private let vl = Model(id: "VL", backendID: "b", upstreamModel: "vl", vision: true)
private let noLP = Model(id: "Flare", backendID: "b", upstreamModel: "f", logprobs: false)
private let caller = FakeModelCaller(result: ChatResult(status: 200, json: .null))

private func hooks(_ c: EngineConfig, secrets: [String: String] = [:], session: URLSession = .shared) -> FeatureHooks {
    FeatureHooks(config: c, secrets: Secrets(values: secrets), caller: caller, session: session)
}

@Test func minimalConfigGivesOffVariants() async {
    let h = hooks(EngineConfig(models: [glm], roles: Roles(main: "GLM")))
    #expect(h.images == nil)
    await #expect(throws: WebSearchError.unavailable) { try await h.webSearch.search(query: "q", allowedDomains: nil, blockedDomains: nil) }
    #expect(h.autoMode.decisionModel == glm)   // main, logprobs unknown
    #expect(h.documents is DocumentService)
}

@Test func rolesPickDecisionAndVisionModels() {
    let c = EngineConfig(models: [glm, vl, noLP], roles: Roles(main: "Flare", decision: "GLM", vision: "VL"))
    let h = hooks(c)
    #expect(h.autoMode.decisionModel == glm)
    #expect(h.images is ModelImageDescriber)
}

@Test func mainWithoutLogprobsIsNotTheDecisionModel() {
    #expect(hooks(EngineConfig(models: [noLP], roles: Roles(main: "Flare"))).autoMode.decisionModel == nil)
    #expect(hooks(EngineConfig(models: [glm], roles: Roles(main: "GLM", decision: "missing"))).autoMode.decisionModel == nil)
}

@Test func searchersReadTheirKeyFromTheSecretStore() async throws {
    let stub = StubSession { _ in .init(200, #"{"results":[{"title":"t","url":"https://x.test","content":"c"}]}"#) }
    var c = EngineConfig(models: [glm], roles: Roles(main: "GLM"))
    c.webSearch = WebSearchConfig(provider: .tavily, keyRef: "tavily")
    let h = hooks(c, secrets: ["tavily": "secret-key"], session: stub.session)
    #expect(try await h.webSearch.search(query: "q", allowedDomains: nil, blockedDomains: nil).count == 1)
    #expect(stub.requests.first?.headers["Authorization"] == "Bearer secret-key")

    // provider set but no stored key -> unavailable, nothing sent
    let none = StubSession { _ in .init(200, "{}") }
    let h2 = hooks(c, secrets: [:], session: none.session)
    await #expect(throws: WebSearchError.unavailable) { try await h2.webSearch.search(query: "q", allowedDomains: nil, blockedDomains: nil) }
    #expect(none.requests.isEmpty)

    c.webSearch = WebSearchConfig(provider: .brave, keyRef: "brave")
    let brave = StubSession { _ in .init(200, #"{"web":{"results":[]}}"#) }
    _ = try await hooks(c, secrets: ["brave": "bk"], session: brave.session).webSearch.search(query: "q", allowedDomains: nil, blockedDomains: nil)
    #expect(brave.requests.first?.headers["X-Subscription-Token"] == "bk")
}
