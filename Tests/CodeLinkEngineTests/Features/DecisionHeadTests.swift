import Testing
import Foundation
@testable import CodeLinkEngine

private let marker = DecisionHead.marker
private let sysSeverity = "\(marker). Output <severity>N</severity> where N is 0-100."
private let sysBlock = "\(marker). Output <block>yes</block> or <block>no</block>."
private let model = Model(id: "GLM", backendID: "b", upstreamModel: "glm-5.3")

private func request(system: JSONValue, stops: [String]?, user: String = "<transcript>rm -rf /</transcript>", model: String = "claude-sonnet-5") -> JSONValue {
    var o: [String: JSONValue] = [
        "model": .string(model), "max_tokens": .number(64),
        "system": system,
        "messages": .array([.makeObject(["role": .string("user"), "content": .string(user)])]),
    ]
    if let stops { o["stop_sequences"] = .array(stops.map { .string($0) }) }
    return .object(o)
}

private func top(_ pairs: [(String, Double)]) -> [DecisionHead.TokenLogprob] {
    pairs.map { .init(token: $0.0, logprob: log($0.1)) }
}

private func logprobResult(_ pairs: [(String, Double)], usage: Bool = true) -> ChatResult {
    let entries = pairs.map { "{\"token\":\"\($0.0)\",\"logprob\":\(log($0.1))}" }.joined(separator: ",")
    let u = usage ? ",\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":1,\"prompt_tokens_details\":{\"cached_tokens\":400}}" : ""
    return ChatResult(status: 200, json: json("{\"choices\":[{\"message\":{\"content\":null,\"reasoning\":\"x\"},\"logprobs\":{\"content\":[{\"top_logprobs\":[\(entries)]}]}}]\(u)}"))
}

private func text(_ r: JSONValue?) -> String? { r?.feature(path: "content", 0, "text")?.featureString }

// MARK: Detection

@Test func detectsSeverityStage1() {
    let h = DecisionHead(decisionModel: model, caller: FakeModelCaller(result: logprobResult([])))
    let c = h.classify(request(system: .string(sysSeverity), stops: ["</severity>"]))
    #expect(c?.format == .severity)
    #expect(c?.stage == 1)
}

@Test func detectsSeverityStage2WithBlockSystem() {
    let h = DecisionHead(decisionModel: model, caller: FakeModelCaller(result: logprobResult([])))
    let sys: JSONValue = .array([.makeObject(["type": .string("text"), "text": .string("x-anthropic-billing-header: cc")]),
                                 .makeObject(["type": .string("text"), "text": .string(sysSeverity)])])
    let c = h.classify(request(system: sys, stops: nil))
    #expect(c?.format == .severity)
    #expect(c?.stage == 2)
    #expect(c?.system.hasPrefix("x-anthropic-billing-header: cc\n") == true)
}

@Test func detectsBlockFormat() {
    let h = DecisionHead(decisionModel: model, caller: FakeModelCaller(result: logprobResult([])))
    let s1 = h.classify(request(system: .string(sysBlock), stops: ["</block>"]))
    #expect(s1?.format == .block)
    #expect(s1?.stage == 1)
    #expect(h.classify(request(system: .string(sysBlock), stops: nil))?.stage == 2)
}

@Test func ordinaryRequestsAreNotClassifiers() {
    let h = DecisionHead(decisionModel: model, caller: FakeModelCaller(result: logprobResult([])))
    #expect(!h.isClassifierRequest(request(system: .string("You are Claude Code."), stops: nil)))
    // mentions a security monitor but not the exact marker / format
    #expect(!h.isClassifierRequest(request(system: .string("You are a security monitor."), stops: ["</severity>"])))
    // marker without any output format or stop sequence
    #expect(!h.isClassifierRequest(request(system: .string("\(marker). Be careful."), stops: nil)))
    // classifier shape but no user text
    var r = request(system: .string(sysSeverity), stops: ["</severity>"]).featureObject!
    r["messages"] = .array([])
    #expect(!h.isClassifierRequest(.object(r)))
    #expect(!h.isClassifierRequest(.string("nope")))
}

@Test func userTextIsVerbatimAcrossBlocks() {
    let msgs = json(#"[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},{"role":"assistant","content":"z"},{"role":"user","content":"c"}]"#)
    #expect(DecisionHead.userText(msgs) == "a\nb\n\nc")
}

// MARK: Score math

@Test func scoreParsesIntegersAndTrimsSpaces() throws {
    let s = try #require(DecisionHead.scoreSeverity(top([("0", 0.45), (" 5", 0.05), ("85", 0.4), ("hello", 0.05)])))
    #expect(abs(s.mass - 0.9) < 1e-9)
    #expect(abs(s.pBlock - 0.4 / 0.9) < 1e-9)
    #expect(abs(s.expected - (0.0 * 0.45 + 5 * 0.05 + 85 * 0.4) / 0.9) < 1e-9)
}

@Test func nonNumericTokensAreIgnored() {
    #expect(DecisionHead.parseSeverityToken("101") == nil)
    #expect(DecisionHead.parseSeverityToken("-5") == nil)
    #expect(DecisionHead.parseSeverityToken("1.5") == nil)
    #expect(DecisionHead.parseSeverityToken("") == nil)
    #expect(DecisionHead.parseSeverityToken(" 100") == 100)
    #expect(DecisionHead.parseSeverityToken("0") == 0)
}

@Test func lowNumericMassIsNil() {
    #expect(DecisionHead.scoreSeverity(top([("7", 0.2), ("The", 0.8)])) == nil)
    #expect(DecisionHead.scoreSeverity([]) == nil)
    #expect(DecisionHead.scoreBlock(top([("yes", 0.1), ("The", 0.9)])) == nil)
}

@Test func blockScoreReadsYesNo() throws {
    let s = try #require(DecisionHead.scoreBlock(top([("yes", 0.6), (" No", 0.3), ("maybe", 0.1)])))
    #expect(abs(s.pBlock - 0.6 / 0.9) < 1e-9)
}

@Test func nIsForcedToTheBlockSideAndNeverFifty() {
    // mean 50 but 60% of the mass above the line -> block, N forced to 51
    let a = DecisionHead.decide(.init(pBlock: 0.6, expected: 50, mass: 1), threshold: 0.5)
    #expect(a.block && a.n == 51)
    let b = DecisionHead.decide(.init(pBlock: 0.4, expected: 50, mass: 1), threshold: 0.5)
    #expect(!b.block && b.n == 49)
    let c = DecisionHead.decide(.init(pBlock: 0.97, expected: 92.4, mass: 1), threshold: 0.5)
    #expect(c.block && c.n == 92)
    let d = DecisionHead.decide(.init(pBlock: 0.0, expected: 3.2, mass: 1), threshold: 0.5)
    #expect(!d.block && d.n == 3)
    // exactly at the threshold blocks
    #expect(DecisionHead.decide(.init(pBlock: 0.5, expected: 10, mass: 1), threshold: 0.5).block)
}

// MARK: Request shape

@Test func decisionRequestShapeIsExact() async throws {
    let caller = FakeModelCaller(result: logprobResult([("5", 0.9)]))
    let h = DecisionHead(decisionModel: model, caller: caller)
    let req = request(system: .string(sysSeverity), stops: ["</severity>"], user: "<transcript>ls</transcript>")
    _ = await h.respond(to: req)
    let call = try #require(caller.calls.first)
    #expect(call.model == model)
    let expected = json("""
    {"max_tokens":1,"logprobs":true,"top_logprobs":20,
     "continue_final_message":true,"add_generation_prompt":false,
     "messages":[{"role":"system","content":\(String(data: try JSONEncoder().encode(sysSeverity), encoding: .utf8)!)},
                 {"role":"user","content":"<transcript>ls</transcript>"},
                 {"role":"assistant","content":"<severity>"}]}
    """)
    #expect(JSONValue.object(call.body) == expected)
}

@Test func blockRequestPrefillsBlock() async throws {
    let caller = FakeModelCaller(result: logprobResult([("no", 0.9)]))
    let h = DecisionHead(decisionModel: model, caller: caller)
    _ = await h.respond(to: request(system: .string(sysBlock), stops: ["</block>"]))
    let last = try #require(caller.calls.first?.body["messages"]?.featureArray?.last)
    #expect(last == json(#"{"role":"assistant","content":"<block>"}"#))
}

// MARK: Response shapes

@Test func stage1ResponseShape() async throws {
    let caller = FakeModelCaller(result: logprobResult([("90", 0.6), ("95", 0.35)]))
    let h = DecisionHead(decisionModel: model, caller: caller)
    let r = try #require(await h.respond(to: request(system: .string(sysSeverity), stops: ["</severity>"])))
    #expect(caller.calls.count == 1)
    #expect(text(r) == "<severity>92")  // 0.6/0.95*90 + 0.35/0.95*95 = 92.1
    #expect(r.feature("stop_reason")?.featureString == "stop_sequence")
    #expect(r.feature("stop_sequence")?.featureString == "</severity>")
    #expect(r.feature("type")?.featureString == "message")
    #expect(r.feature("role")?.featureString == "assistant")
    #expect(r.feature("model")?.featureString == "claude-sonnet-5")
    #expect(r.feature("id")?.featureString?.hasPrefix("msg_") == true)
    #expect(r.feature(path: "usage", "input_tokens") == .number(600))
    #expect(r.feature(path: "usage", "cache_read_input_tokens") == .number(400))
    #expect(r.feature(path: "usage", "output_tokens") == .number(1))
}

@Test func stage1AllowForcedBelowFifty() async throws {
    let caller = FakeModelCaller(result: logprobResult([("2", 0.7), ("60", 0.1), ("0", 0.2)]))
    let h = DecisionHead(decisionModel: model, caller: caller)
    let r = await h.respond(to: request(system: .string(sysSeverity), stops: ["</severity>"]))
    let t = try #require(text(r))
    let n = try #require(Int(t.dropFirst("<severity>".count)))
    #expect(n <= 49)
}

@Test func stage2BlockAddsCategory() async throws {
    let caller = FakeModelCaller { i, _, _ in
        if i == 0 { return logprobResult([("90", 0.97), ("5", 0.03)]) }
        return ChatResult(status: 200, json: json(#"{"choices":[{"message":{"content":"Force Push!"}}],"usage":{"prompt_tokens":1000,"completion_tokens":3}}"#))
    }
    let h = DecisionHead(decisionModel: model, caller: caller)
    let r = try #require(await h.respond(to: request(system: .string(sysSeverity), stops: nil)))
    #expect(caller.calls.count == 2)
    let n = try #require(Int(text(r)?.dropFirst("<severity>".count).prefix(while: \.isNumber) ?? ""))
    #expect(n > 50)
    #expect(text(r) == "<severity>\(n)</severity><category>Force Push</category>")
    #expect(r.feature("stop_reason")?.featureString == "end_turn")
    #expect(r.feature("stop_sequence") == .null)
    // category request shape
    let cat = caller.calls[1].body
    #expect(cat["max_tokens"] == .number(16))
    #expect(cat["stop"] == .array([.string("</category>")]))
    #expect(cat["logprobs"] == nil)
    #expect(cat["messages"]?.featureArray?.last == json("{\"role\":\"assistant\",\"content\":\"<severity>\(n)</severity><category>\"}"))
    // usage summed over both calls
    #expect(r.feature(path: "usage", "output_tokens") == .number(4))
}

@Test func stage2AllowHasNoCategoryAndOneCall() async throws {
    let caller = FakeModelCaller(result: logprobResult([("3", 0.95)]))
    let h = DecisionHead(decisionModel: model, caller: caller)
    let r = try #require(await h.respond(to: request(system: .string(sysSeverity), stops: nil)))
    #expect(caller.calls.count == 1)
    #expect(text(r) == "<severity>3</severity>")
}

@Test func categoryFailureOmitsCategory() async throws {
    let caller = FakeModelCaller { i, _, _ in
        if i == 0 { return logprobResult([("90", 1.0)]) }
        throw FakeError()
    }
    let h = DecisionHead(decisionModel: model, caller: caller)
    let r = try #require(await h.respond(to: request(system: .string(sysSeverity), stops: nil)))
    #expect(text(r) == "<severity>90</severity>")
}

@Test func blockFormatResponses() async throws {
    let yes = DecisionHead(decisionModel: model, caller: FakeModelCaller(result: logprobResult([("yes", 0.9), ("no", 0.1)])))
    let r1 = try #require(await yes.respond(to: request(system: .string(sysBlock), stops: ["</block>"])))
    #expect(text(r1) == "<block>yes")
    #expect(r1.feature("stop_reason")?.featureString == "stop_sequence")
    #expect(r1.feature("stop_sequence")?.featureString == "</block>")
    let r2 = try #require(await yes.respond(to: request(system: .string(sysBlock), stops: nil)))
    #expect(text(r2) == "<block>yes</block>")
    #expect(r2.feature("stop_reason")?.featureString == "end_turn")
    let no = DecisionHead(decisionModel: model, caller: FakeModelCaller(result: logprobResult([("no", 0.8), ("yes", 0.2)])))
    let r3 = try #require(await no.respond(to: request(system: .string(sysBlock), stops: nil)))
    #expect(text(r3) == "<block>no</block>")
}

// MARK: Failures -> nil

@Test func failuresReturnNil() async {
    let sys = request(system: .string(sysSeverity), stops: ["</severity>"])
    let throwing = DecisionHead(decisionModel: model, caller: FakeModelCaller { _, _, _ in throw FakeError() })
    #expect(await throwing.respond(to: sys) == nil)
    let http500 = DecisionHead(decisionModel: model, caller: FakeModelCaller(result: ChatResult(status: 500, json: .null)))
    #expect(await http500.respond(to: sys) == nil)
    let lowMass = DecisionHead(decisionModel: model, caller: FakeModelCaller(result: logprobResult([("The", 0.9), ("5", 0.1)])))
    #expect(await lowMass.respond(to: sys) == nil)
    let noModel = DecisionHead(decisionModel: nil, caller: FakeModelCaller(result: logprobResult([("5", 1.0)])))
    #expect(await noModel.respond(to: sys) == nil)
    let notClassifier = DecisionHead(decisionModel: model, caller: FakeModelCaller(result: logprobResult([("5", 1.0)])))
    #expect(await notClassifier.respond(to: request(system: .string("hi"), stops: nil)) == nil)
}

@Test func missingLogprobsIsRememberedAndSkipsTheCall() async {
    let caller = FakeModelCaller(result: ChatResult(status: 200, json: json(#"{"choices":[{"message":{"content":"<severity>5"}}]}"#)))
    let h = DecisionHead(decisionModel: model, caller: caller)
    let sys = request(system: .string(sysSeverity), stops: ["</severity>"])
    #expect(await h.respond(to: sys) == nil)
    #expect(await h.respond(to: sys) == nil)
    #expect(caller.calls.count == 1)
    // a model declared without logprobs is never called
    var m = model; m.logprobs = false
    let none = FakeModelCaller(result: logprobResult([("5", 1.0)]))
    #expect(await DecisionHead(decisionModel: m, caller: none).respond(to: sys) == nil)
    #expect(none.calls.isEmpty)
}

@Test func categorySanitizing() {
    #expect(DecisionHead.sanitizeCategory("  Force-Push: main! ") == "ForcePush main")
}

// MARK: Servers that refuse a prefill (Flare's DeepSeek)

/// A located reply: the model wrote "<severity>" itself, then the score token.
private func locatedResult(_ pairs: [(String, Double)], extra: [String] = []) -> ChatResult {
    func entry(_ t: String, _ top: [(String, Double)]) -> JSONValue {
        .makeObject(["token": .string(t), "logprob": .number(-0.1),
                     "top_logprobs": .array(top.map { .makeObject(["token": .string($0.0), "logprob": .number(log($0.1))]) })])
    }
    var content: [JSONValue] = ["<", "sever", "ity", ">"].map { entry($0, [($0, 1)]) }
    content.append(entry(pairs[0].0, pairs))
    content += extra.map { entry($0, [($0, 1)]) }
    return ChatResult(status: 200, json: .makeObject([
        "choices": .array([.makeObject(["message": .makeObject(["content": .string("")]), "logprobs": .makeObject(["content": .array(content)])])]),
        "usage": .makeObject(["prompt_tokens": .number(10), "completion_tokens": .number(5)]),
    ]))
}

@Test func refusedPrefillFallsBackToLocatingTheScoreAndRemembersIt() async throws {
    let refused = ChatResult(status: 400, json: json(#"{"error":{"message":"DeepSeek prompt encoding does not support continue_final_message"}}"#))
    let caller = FakeModelCaller { i, _, _ in i == 0 ? refused : locatedResult([("85", 0.8), ("10", 0.2)]) }
    let h = DecisionHead(decisionModel: model, caller: caller)
    let req = request(system: .string(sysSeverity), stops: ["</severity>"])
    let out = try #require(await h.respond(to: req))
    #expect(out.feature(path: "content", 0, "text")?.featureString == "<severity>70")
    let located = caller.calls[1].body
    #expect(located["continue_final_message"] == nil)
    #expect(located["temperature"] == nil)
    #expect(located["chat_template_kwargs"] == .makeObject(["enable_thinking": .bool(false)]))
    // The next decision goes straight to the located form.
    _ = await h.respond(to: req)
    #expect(caller.calls.count == 3)
    #expect(caller.calls[2].body["continue_final_message"] == nil)
}

@Test func locatedStageTwoReadsTheCategoryFromTheSameReply() async throws {
    let refused = ChatResult(status: 400, json: json(#"{"error":{"message":"continue_final_message is not supported"}}"#))
    let caller = FakeModelCaller { i, _, _ in
        i == 0 ? refused : locatedResult([("90", 0.95), ("5", 0.05)], extra: ["</", "severity", ">", "<category>", "Force", " Push", "</", "category", ">"])
    }
    let h = DecisionHead(decisionModel: model, caller: caller)
    let out = try #require(await h.respond(to: request(system: .string(sysSeverity), stops: nil)))
    let text = try #require(out.feature(path: "content", 0, "text")?.featureString)
    #expect(text.hasPrefix("<severity>") && text.hasSuffix("<category>Force Push</category>"))
    #expect(caller.calls.count == 2)
}

@Test func locatedReplyWithoutTheTagFallsBack() async throws {
    let refused = ChatResult(status: 400, json: json(#"{"error":{"message":"no continue_final_message"}}"#))
    let thinking = ChatResult(status: 200, json: json(#"{"choices":[{"message":{"content":null},"logprobs":{"content":[{"token":"We","logprob":-0.1,"top_logprobs":[{"token":"We","logprob":-0.1}]}]}}]}"#))
    let caller = FakeModelCaller { i, _, _ in i == 0 ? refused : thinking }
    let h = DecisionHead(decisionModel: model, caller: caller)
    #expect(await h.respond(to: request(system: .string(sysSeverity), stops: ["</severity>"])) == nil)
}
