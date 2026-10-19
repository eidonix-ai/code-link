import Foundation
import Testing
@testable import CodeLinkEngine

extension CoreHarness {
    func responses(_ body: String, headers: [String: String] = [:]) async throws -> HTTPReply {
        try await request("POST", "/v1/responses", body: body, headers: headers)
    }
    func chat(_ body: String, headers: [String: String] = [:]) async throws -> HTTPReply {
        try await request("POST", "/v1/chat/completions", body: body, headers: headers)
    }
}

/// A real Codex 0.163 request (Tests/Fixtures/codex, long texts shortened,
/// identifiers replaced), with `edit` applied.
func codexFixture(_ name: String, _ edit: (inout JSON) -> Void = { _ in }) throws -> String {
    let url = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        .appendingPathComponent("Fixtures/codex/\(name).json")
    var json = try JSON.parse(try Data(contentsOf: url))
    edit(&json)
    return json.text
}

/// A Responses request.
func responsesBody(input: String = #""hi""#, model: String = "GLM", stream: Bool = true, extra: String = "") -> String {
    #"{"model":"\#(model)","stream":\#(stream),"input":\#(input)\#(extra.isEmpty ? "" : "," + extra)}"#
}

extension Array where Element == SSEEvent {
    /// The data of every event with this name.
    func data(_ name: String) -> [JSON] { filter { $0.event == name }.compactMap(\.data) }
}

/// The items of a response's final output, from the completed event.
func completedOutput(_ events: [SSEEvent]) -> [JSON] {
    events.data("response.completed").first?["response"]?["output"]?.array ?? []
}
