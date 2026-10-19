import Foundation
import CodeLinkEngine

/// What the menu bar icon and the status line show.
struct EngineStatus: Equatable {
    enum Kind: Equatable { case notConfigured, stopped, starting, running, backendUnreachable, failed }
    var kind: Kind
    var line: String

    static func make(config: EngineConfig, state: EngineState, backendReachable: Bool?, startError: String? = nil) -> EngineStatus {
        guard let main = config.mainModel else {
            return EngineStatus(kind: .notConfigured, line: "Not configured — open Settings")
        }
        switch state {
        case .stopped:
            return EngineStatus(kind: .stopped, line: startError.map { "Stopped — \($0)" } ?? "Stopped · \(main.id)")
        case .starting:
            return EngineStatus(kind: .starting, line: "Starting…")
        case .failed(let message):
            return EngineStatus(kind: .failed, line: "Failed — \(message)")
        case .running(let port):
            if backendReachable == false {
                return EngineStatus(kind: .backendUnreachable, line: "\(main.backendID) is unreachable · \(main.id)")
            }
            return EngineStatus(kind: .running, line: "Running on 127.0.0.1:\(port) · \(main.id)")
        }
    }

    /// SF Symbol for the menu bar and the colour of the status dot.
    var symbolName: String { kind == .running || kind == .starting ? "link.circle.fill" : kind == .stopped || kind == .notConfigured ? "link.circle" : "exclamationmark.triangle.fill" }
}
