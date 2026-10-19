import Foundation
import Observation
import CodeLinkEngine

/// One line in the Recent activity list. Never prompt text.
struct ActivityEntry: Identifiable, Equatable, Sendable {
    enum Level: Sendable { case normal, warning, error }
    let id: Int
    let date: Date
    let kind: String
    let text: String
    let level: Level

    static func describe(_ e: EngineEvent) -> (kind: String, text: String, level: Level) {
        switch e {
        case let .request(route, model, status, latency):
            ("request", "\(route) · \(model) · \(status) · \(latency) ms", status >= 500 ? .error : status >= 400 ? .warning : .normal)
        case let .decision(stage, model, pBlock, severity, latency, fallback):
            ("decision", "auto mode stage \(stage) · \(model) · p(block) \(String(format: "%.2f", pBlock)) · severity \(severity) · \(latency) ms"
                + (fallback.map { " · fallback: \($0)" } ?? ""), fallback == nil ? .normal : .warning)
        case let .webSearch(query, results):
            ("search", "web search \"\(query)\" · \(results) result\(results == 1 ? "" : "s")", .normal)
        case let .document(name, pages, ocrPages):
            ("document", "\(name) · \(pages) page\(pages == 1 ? "" : "s")" + (ocrPages > 0 ? " · \(ocrPages) read by OCR" : ""), .normal)
        case let .error(message):
            ("error", message, .error)
        }
    }
}

/// The Activity list: newest first, capped.
@MainActor @Observable
final class ActivityStore {
    static let limit = 200
    private(set) var entries: [ActivityEntry] = []
    private var counter = 0

    func append(_ event: EngineEvent, date: Date = Date()) {
        counter += 1
        let d = ActivityEntry.describe(event)
        entries.insert(ActivityEntry(id: counter, date: date, kind: d.kind, text: d.text, level: d.level), at: 0)
        if entries.count > Self.limit { entries.removeLast(entries.count - Self.limit) }
    }

    func clear() { entries = [] }
}

/// The engine's `EventLog`: callable from any thread, delivers on the main actor.
struct ActivityLog: EventLog {
    let store: ActivityStore
    func record(_ event: EngineEvent) {
        let store = store
        Task { @MainActor in store.append(event) }
    }
}
