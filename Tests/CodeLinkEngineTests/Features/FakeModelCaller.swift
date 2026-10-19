import Foundation
@testable import CodeLinkEngine

/// Records every call and answers from a handler.
final class FakeModelCaller: ModelCaller, @unchecked Sendable {
    struct Call { var model: Model; var body: [String: JSONValue] }
    private let lock = NSLock()
    private var _calls: [Call] = []
    private let handler: @Sendable (Int, Model, [String: JSONValue]) throws -> ChatResult

    init(_ handler: @escaping @Sendable (Int, Model, [String: JSONValue]) throws -> ChatResult) { self.handler = handler }
    convenience init(result: ChatResult) { self.init { _, _, _ in result } }

    var calls: [Call] { lock.lock(); defer { lock.unlock() }; return _calls }

    func chat(model: Model, body: [String: JSONValue]) async throws -> ChatResult {
        let index: Int = { lock.lock(); defer { lock.unlock() }; _calls.append(Call(model: model, body: body)); return _calls.count - 1 }()
        return try handler(index, model, body)
    }
}

struct FakeError: Error {}

/// Parses a JSON literal into a JSONValue.
func json(_ s: String) -> JSONValue {
    try! JSONDecoder().decode(JSONValue.self, from: Data(s.utf8))
}
