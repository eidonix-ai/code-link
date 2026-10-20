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

    var label: String {
        switch self {
        case .main: "Sonnet"
        case .opus: "Opus"
        case .small: "Small (Haiku)"
        case .decision: "Decision"
        case .vision: "Vision"
        case .codex: "Codex"
        }
    }

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
                if models[existing].vision == nil { models[existing].vision = probed.vision }
            }
            return models[existing].id
        }
        var id = choice.upstream
        if models.contains(where: { $0.id == id }) { id = "\(choice.upstream) (\(choice.backendID))" }
        models.append(Model(id: id, backendID: choice.backendID, upstreamModel: choice.upstream,
                            contextWindow: probed?.contextWindow, vision: probed?.vision, logprobs: probed?.logprobs))
        return id
    }

    /// Points a role at a provider's model (nil clears an optional role).
    mutating func setRole(_ slot: RoleSlot, to choice: ModelChoice?, probed: ProbedModel? = nil) {
        guard let choice, !choice.upstream.isEmpty, !choice.backendID.isEmpty else {
            roles[keyPath: slot.keyPath] = nil
            return
        }
        // Picking a model for a role enables it.
        roles[keyPath: slot.keyPath] = ensureModel(choice, probed: probed)
    }

    func isEnabled(_ choice: ModelChoice) -> Bool {
        models.contains { $0.backendID == choice.backendID && $0.upstreamModel == choice.upstream }
    }

    /// Turns a provider's model on (creating its entry) or off. Turning one off
    /// clears every role that used it and returns those roles, so the caller can say so.
    @discardableResult
    mutating func setEnabled(_ choice: ModelChoice, _ on: Bool, probed: ProbedModel? = nil) -> [RoleSlot] {
        if on { ensureModel(choice, probed: probed); return [] }
        guard let m = models.first(where: { $0.backendID == choice.backendID && $0.upstreamModel == choice.upstream }) else { return [] }
        let cleared = RoleSlot.allCases.filter { modelID(for: $0) == m.id }
        for slot in cleared { roles[keyPath: slot.keyPath] = nil }
        models.removeAll { $0.id == m.id }
        return cleared
    }

    /// Changes the name sessions see for a model (an alias). Refuses an empty or taken name.
    @discardableResult
    mutating func renameModel(from old: String, to new: String) -> Bool {
        let n = new.trimmingCharacters(in: .whitespaces)
        guard !n.isEmpty, n != old, !models.contains(where: { $0.id == n }), let i = models.firstIndex(where: { $0.id == old }) else { return false }
        models[i].id = n
        for slot in RoleSlot.allCases where modelID(for: slot) == old { roles[keyPath: slot.keyPath] = n }
        return true
    }

    /// Drops models whose provider is gone.
    mutating func dropOrphanModels() {
        let ids = Set(backends.map(\.id))
        models.removeAll { !ids.contains($0.backendID) }
    }

    /// Renames a provider and follows it in the models.
    mutating func renameBackend(from old: String, to new: String) {
        guard old != new, let i = backends.firstIndex(where: { $0.id == old }) else { return }
        backends[i].id = new
        for j in models.indices where models[j].backendID == old { models[j].backendID = new }
    }

    /// Removes a provider with its models; returns the roles that lost their model.
    @discardableResult
    mutating func removeBackend(_ id: String) -> [RoleSlot] {
        backends.removeAll { $0.id == id }
        var cleared: [RoleSlot] = []
        for slot in RoleSlot.allCases {
            if let m = model(modelID(for: slot)), m.backendID == id { roles[keyPath: slot.keyPath] = nil; cleared.append(slot) }
        }
        models.removeAll { $0.backendID == id }
        return cleared
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

/// One model in a picker or checklist.
struct ChoiceItem: Hashable, Sendable, Identifiable {
    var choice: ModelChoice
    /// The enabled model's id (its alias); nil when the model isn't enabled.
    var modelID: String?
    var contextWindow: Int?
    var logprobs: Bool?
    var vision: Bool?
    var id: String { choice.tag }
    var enabled: Bool { modelID != nil }
    var title: String { modelID ?? choice.upstream }
}

/// A provider's models, for a grouped picker ("cn3-flare › GLM-5.3").
struct ChoiceGroup: Hashable, Sendable, Identifiable {
    var backendID: String
    var items: [ChoiceItem]
    var id: String { backendID }
}

enum RoutingChoices {
    /// Providers in order, each with its enabled models first and (unless
    /// `enabledOnly`) the models its last probe found that aren't enabled.
    static func groups(config: EngineConfig, probes: [String: BackendProbe] = [:], enabledOnly: Bool = false) -> [ChoiceGroup] {
        config.backends.compactMap { b in
            var items = config.models.filter { $0.backendID == b.id }.map {
                ChoiceItem(choice: ModelChoice(backendID: b.id, upstream: $0.upstreamModel), modelID: $0.id,
                           contextWindow: $0.contextWindow, logprobs: $0.logprobs, vision: $0.vision)
            }
            if !enabledOnly {
                for p in probes[b.id]?.models ?? [] where !items.contains(where: { $0.choice.upstream == p.upstreamModel }) {
                    items.append(ChoiceItem(choice: ModelChoice(backendID: b.id, upstream: p.upstreamModel), modelID: nil,
                                            contextWindow: p.contextWindow, logprobs: p.logprobs, vision: p.vision))
                }
            }
            return items.isEmpty ? nil : ChoiceGroup(backendID: b.id, items: items)
        }
    }

    /// "1M" / "262k" / "8192" for a context window.
    static func shortContext(_ n: Int) -> String {
        if n >= 1_000_000 { return "\(Int((Double(n) / 1_000_000).rounded()))M" }
        if n >= 1000 { return "\(n / 1000)k" }
        return String(n)
    }

    /// "262k · logprobs · vision" facts line.
    static func facts(_ item: ChoiceItem) -> String {
        var parts: [String] = []
        if let c = item.contextWindow { parts.append(shortContext(c)) }
        if item.logprobs == true { parts.append("logprobs") }
        if item.vision == true { parts.append("vision") }
        return parts.joined(separator: " · ")
    }
}
