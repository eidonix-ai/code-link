import Testing
import Foundation
@testable import CodeLinkEngine

private let vmodel = Model(id: "V", backendID: "b", upstreamModel: "vl", vision: true)
private func reply(_ content: String) -> ChatResult {
    ChatResult(status: 200, json: .makeObject(["choices": .array([.makeObject(["message": .makeObject(["content": .string(content)])])])]))
}

@Test func describeSendsOneImageRequestAndReturnsText() async throws {
    let caller = FakeModelCaller(result: reply("  A red square with the text HELLO.  "))
    let d = ModelImageDescriber(model: vmodel, caller: caller)
    let png = Data([1, 2, 3, 4])
    let text = await d.describe(png, mediaType: "image/png")
    #expect(text == "A red square with the text HELLO.")
    let call = try #require(caller.calls.first)
    #expect(call.model == vmodel)
    let expected = json("""
    {"max_tokens":600,"messages":[{"role":"user","content":[
      {"type":"image_url","image_url":{"url":"data:image/png;base64,AQIDBA=="}},
      {"type":"text","text":"Describe this image in detail for someone who cannot see it, including any text it contains."}]}]}
    """)
    #expect(JSONValue.object(call.body) == expected)
}

@Test func describeCachesBySHA256() async {
    let caller = FakeModelCaller(result: reply("cat"))
    let d = ModelImageDescriber(model: vmodel, caller: caller)
    let a = Data([9, 9, 9])
    #expect(await d.describe(a, mediaType: "image/png") == "cat")
    #expect(await d.describe(a, mediaType: "image/png") == "cat")
    #expect(caller.calls.count == 1)
    _ = await d.describe(Data([8]), mediaType: "image/png")
    #expect(caller.calls.count == 2)
}

@Test func describeFailuresAreNilAndNotCached() async {
    let flaky = FakeModelCaller { i, _, _ in
        if i == 0 { throw FakeError() }
        if i == 1 { return ChatResult(status: 500, json: .null) }
        if i == 2 { return reply("  ") }
        return reply("<think>hmm</think>A dog.")
    }
    let d = ModelImageDescriber(model: vmodel, caller: flaky)
    let img = Data([7])
    #expect(await d.describe(img, mediaType: "image/jpeg") == nil)
    #expect(await d.describe(img, mediaType: "image/jpeg") == nil)
    #expect(await d.describe(img, mediaType: "image/jpeg") == nil)
    #expect(await d.describe(img, mediaType: "image/jpeg") == "A dog.")
}

@Test func lruCacheEvictsByCountAndCost() {
    let c = ContentCache<Int>(maxEntries: 2, maxCost: 100)
    c.insert(1, cost: 10, for: "a"); c.insert(2, cost: 10, for: "b")
    _ = c.value(for: "a")           // a is now most recent
    c.insert(3, cost: 10, for: "c") // evicts b
    #expect(c.value(for: "b") == nil && c.value(for: "a") == 1 && c.value(for: "c") == 3)
    c.insert(4, cost: 95, for: "d") // over cost: evicts until it fits
    #expect(c.count == 1 && c.value(for: "d") == 4)
    c.insert(5, cost: 101, for: "huge") // larger than the whole budget: not stored
    #expect(c.value(for: "huge") == nil)
}
