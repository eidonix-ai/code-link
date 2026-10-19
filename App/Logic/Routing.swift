import Foundation
import CodeLinkEngine

/// A model on a provider, as the pickers see it.
struct ModelChoice: Hashable, Sendable {
    var backendID: String
    var upstream: String
    var tag: String { backendID + "\u{1}" + upstream }
    init(backendID: String, upstream: String) { self.backendID = backendID; self.upstream = upstream }
    init?(tag: String) {
        let parts = tag.split(separator: "\u{1}", maxSplits: 1, omittingEmptySubsequences: false)
        guard parts.count == 2 else { return nil }
        self.init(backendID: String(parts[0]), upstream: String(parts[1]))
    }
}

/// The roles a tier or an advanced override can set.
enum RoleSlot: CaseIterable, Sendable {
    case main, opus, small, decision, vision, codex

    var keyPath: WritableKeyPath<Roles, String?> {
        switch self {
        case .opus: \.opus
        case .small: \.small
        case .decision: \.decision
        case .vision: \.vision
        case .codex: \.codex
        case .main: \.mainOptional
        }
    }
}

extension Roles {
    /// `main` as an optional, so every slot can share one key path type.
    var mainOptional: String? {
        get { main.isEmpty ? nil : main }
        set { main = newValue ?? "" }
    }
}

extension EngineConfig {
    func model(_ id: String?) -> Model? {
        guard let id, !id.isEmpty else { return nil }
        return models.first { $0.id == id }
    }

    func modelID(for slot: RoleSlot) -> String? { roles[keyPath: slot.keyPath] }

    func choice(for slot: RoleSlot) -> ModelChoice? {
        guard let m = model(modelID(for: slot)) else { return nil }
        return ModelChoice(backendID: m.backendID, upstream: m.upstreamModel)
    }

    /// The model Claude Code's default slot, and everything unset, resolves to.
    var mainModel: Model? { model(roles.main) }

    /// Finds or creates the model entry for a provider's model and returns its
    /// id (the name sessions see). A same-named model on another provider gets
    /// the provider appended.
    @discardableResult
    mutating func ensureModel(_ choice: ModelChoice, probed: ProbedModel? = nil) -> String {
        if let existing = models.firstIndex(where: { $0.backendID == choice.backendID && $0.upstreamModel == choice.upstream }) {
            if let probed {
                if models[existing].contextWindow == nil { models[existing].contextWindow = probed.contextWindow }
                if models[existing].logprobs == nil { models[existing].logprobs = probed.logprobs }
            }
            return models[existing].id
        }
        var id = choice.upstream
        if models.contains(where: { $0.id == id }) { id = "\(choice.upstream) (\(choice.backendID))" }
        models.append(Model(id: id, backendID: choice.backendID, upstreamModel: choice.upstream,
                            contextWindow: probed?.contextWindow, logprobs: probed?.logprobs))
        return id
    }

    /// Points a role at a provider's model (nil clears an optional role).
    mutating func setRole(_ slot: RoleSlot, to choice: ModelChoice?, probed: ProbedModel? = nil) {
        guard let choice, !choice.upstream.isEmpty, !choice.backendID.isEmpty else {
            roles[keyPath: slot.keyPath] = nil
            pruneModels()
            return
        }
        let id = ensureModel(choice, probed: probed)
        roles[keyPath: slot.keyPath] = id
        pruneModels()
    }

    /// Drops models no role refers to, so config.json stays what the page shows.
    mutating func pruneModels() {
        let used = Set([roles.main, roles.opus, roles.small, roles.decision, roles.vision, roles.codex].compactMap { $0 })
        models.removeAll { !used.contains($0.id) }
    }

    /// Renames a provider and follows it in the models.
    mutating func renameBackend(from old: String, to new: String) {
        guard old != new, let i = backends.firstIndex(where: { $0.id == old }) else { return }
        backends[i].id = new
        for j in models.indices where models[j].backendID == old { models[j].backendID = new }
    }

    mutating func removeBackend(_ id: String) {
        backends.removeAll { $0.id == id }
        for slot in RoleSlot.allCases {
            if let m = model(modelID(for: slot)), m.backendID == id { roles[keyPath: slot.keyPath] = nil }
        }
        models.removeAll { $0.backendID == id }
    }

    /// True when the engine has what it needs to start.
    var isConfigured: Bool {
        guard let m = mainModel else { return false }
        return backends.contains { $0.id == m.backendID }
    }

    /// Problems that stop a save; empty when fine.
    func problems() -> [String] {
        var out: [String] = []
        let ids = backends.map(\.id)
        if ids.contains(where: { $0.trimmingCharacters(in: .whitespaces).isEmpty }) { out.append("Every provider needs a name.") }
        if Set(ids).count != ids.count { out.append("Provider names must be unique.") }
        if !(1024...65535).contains(port) { out.append("The port must be between 1024 and 65535.") }
        for b in backends where b.baseURL.host == nil || !["http", "https"].contains(b.baseURL.scheme ?? "") {
            out.append("Provider \(b.id): the base URL must be http:// or https://.")
        }
        for m in models where !backends.contains(where: { $0.id == m.backendID }) {
            out.append("Model \(m.id) refers to a provider that no longer exists.")
        }
        if !roles.main.isEmpty, model(roles.main) == nil { out.append("The Sonnet model is not available on any provider.") }
        if let o = roles.opus, model(o) == nil { out.append("The Opus model is not available on any provider.") }
        return out
    }
}
