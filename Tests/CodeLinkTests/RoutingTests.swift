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

@Test func clearingARoleKeepsTheModelEnabled() {
    var c = base()
    c.setRole(.main, to: ModelChoice(backendID: "a", upstream: "M"))
    c.setRole(.small, to: ModelChoice(backendID: "a", upstream: "S"))
    c.setRole(.small, to: nil)
    #expect(c.roles.small == nil && c.models.map(\.id) == ["M", "S"])
    c.setRole(.main, to: nil)
    #expect(c.roles.main == "" && !c.isConfigured && c.models.count == 2)
}

@Test func turningAModelOffClearsEveryRoleThatUsedIt() {
    var c = base()
    let m = ModelChoice(backendID: "a", upstream: "M")
    c.setRole(.main, to: m)
    c.setRole(.opus, to: m)
    c.setRole(.codex, to: ModelChoice(backendID: "b", upstream: "X"))
    let cleared = c.setEnabled(m, false)
    #expect(cleared == [.main, .opus])
    #expect(c.roles.main == "" && c.roles.opus == nil && c.roles.codex == "X" && !c.isEnabled(m))
    let again = c.setEnabled(m, false)
    #expect(again.isEmpty)
    c.setEnabled(m, true)
    #expect(c.isEnabled(m) && c.roles.main == "")
}

@Test func modelAliasesFollowIntoRoles() {
    var c = base()
    c.setRole(.main, to: ModelChoice(backendID: "a", upstream: "GLM-5.3"))
    c.setRole(.opus, to: ModelChoice(backendID: "b", upstream: "Q"))
    let renamed = c.renameModel(from: "GLM-5.3", to: "sonnet-glm")
    #expect(renamed)
    #expect(c.roles.main == "sonnet-glm" && c.choice(for: .main)?.upstream == "GLM-5.3")
    let taken = c.renameModel(from: "sonnet-glm", to: "Q")
    let blank = c.renameModel(from: "sonnet-glm", to: "  ")
    #expect(!taken && !blank)
}

@Test func groupedChoicesListEnabledFirstThenProbedAndHonourEnabledOnly() {
    var c = base()
    c.setRole(.main, to: ModelChoice(backendID: "b", upstream: "Q2"), probed: ProbedModel(upstreamModel: "Q2", contextWindow: 262_144, logprobs: true))
    let probes = ["a": BackendProbe(reachable: true, models: [ProbedModel(upstreamModel: "A1", contextWindow: 1_048_576)]),
                  "b": BackendProbe(reachable: true, models: [ProbedModel(upstreamModel: "Q1"), ProbedModel(upstreamModel: "Q2")])]
    let all = RoutingChoices.groups(config: c, probes: probes)
    #expect(all.map(\.backendID) == ["a", "b"])
    #expect(all[1].items.map(\.title) == ["Q2", "Q1"] && all[1].items.map(\.enabled) == [true, false])
    #expect(RoutingChoices.facts(all[1].items[0]) == "262k · logprobs")
    #expect(RoutingChoices.facts(all[0].items[0]) == "1M")
    let enabled = RoutingChoices.groups(config: c, probes: probes, enabledOnly: true)
    #expect(enabled.map(\.backendID) == ["b"] && enabled[0].items.count == 1)
}

@Test func menuModelsListEnabledModelsWithCurrentRouting() {
    var c = base()
    c.setRole(.main, to: ModelChoice(backendID: "a", upstream: "S"))
    c.setRole(.opus, to: ModelChoice(backendID: "b", upstream: "O"))
    let m = MenuModels.make(config: c)
    #expect(m.groups.map(\.backendID) == ["a", "b"] && m.sonnetID == "S" && m.opusID == "O")
    c.roles.opus = nil
    #expect(MenuModels.make(config: c).opusTitle == "same as Sonnet")
    #expect(MenuModels.make(config: base()).groups.isEmpty)
}

@Test func enabledModelsPersistWithoutRoles() throws {
    var c = base()
    c.setEnabled(ModelChoice(backendID: "a", upstream: "Spare"), true, probed: ProbedModel(upstreamModel: "Spare", contextWindow: 8192))
    c.models[0].id = "spare-alias"
    let env = AppEnvironment.isolated()
    defer { try? FileManager.default.removeItem(at: env.paths.supportDirectory.deletingLastPathComponent()) }
    let store = ConfigStore(paths: env.paths)
    try store.save(c)
    let back = store.load().config
    #expect(back == c && back.models.first?.id == "spare-alias" && back.models.first?.contextWindow == 8192)
}

@Test func shortContextLabels() {
    #expect(RoutingChoices.shortContext(1_048_576) == "1M")
    #expect(RoutingChoices.shortContext(262_144) == "262k")
    #expect(RoutingChoices.shortContext(8192) == "8k")
    #expect(RoutingChoices.shortContext(512) == "512")
}

@MainActor @Test func disablingAUsedModelWarnsAndClearsTheRole() async {
    let model = AppModel(environment: .isolated())
    model.draft = base()
    let m = ModelChoice(backendID: "a", upstream: "M")
    model.setRole(.main, to: m)
    model.setRole(.opus, to: m)
    model.setModelEnabled(m, false)
    #expect(model.draft.roles.main == "" && model.draft.roles.opus == nil)
    #expect(model.warnings.count == 1 && model.warnings[0].contains("Sonnet, Opus"))
    model.revert()
    #expect(model.warnings.isEmpty)
}

@MainActor @Test func removingAProviderWarnsAboutLostRoles() {
    let model = AppModel(environment: .isolated())
    model.draft = base()
    model.setRole(.opus, to: ModelChoice(backendID: "b", upstream: "O"))
    model.removeProvider("b")
    #expect(model.draft.roles.opus == nil && model.warnings.first?.contains("Opus") == true)
}

@MainActor @Test func quickSwitchReroutesAndPersists() async throws {
    let env = AppEnvironment.isolated()
    defer { try? FileManager.default.removeItem(at: env.paths.supportDirectory.deletingLastPathComponent()) }
    let model = AppModel(environment: env)
    model.draft = base()
    model.setRole(.main, to: ModelChoice(backendID: "a", upstream: "S1"))
    model.setRole(.opus, to: ModelChoice(backendID: "b", upstream: "O1"))
    model.setModelEnabled(ModelChoice(backendID: "b", upstream: "S2"), true)
    await model.save()
    #expect(model.saveMessage == .saved)
    await model.switchRole(.main, to: "S2")
    #expect(model.config.roles.main == "S2" && ConfigStore(paths: env.paths).load().config.roles.main == "S2")
    await model.switchRole(.opus, to: nil)
    #expect(model.config.roles.opus == nil)
    await model.switchRole(.main, to: "not-a-model")
    #expect(model.config.roles.main == "S2")
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
