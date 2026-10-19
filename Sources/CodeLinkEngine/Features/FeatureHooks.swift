import Foundation

/// The four feature hooks built from one config (docs/DESIGN.md §4.3–§4.5).
/// The core builds this once per config and wires the members in:
///
///     let hooks = FeatureHooks(config: config, secrets: secrets, caller: caller, log: log)
///     // hooks.documents, hooks.images (nil = no vision role), hooks.webSearch, hooks.autoMode
public struct FeatureHooks: Sendable {
    public let documents: any DocumentReader
    /// nil when no vision model is configured: images are then omitted with a note.
    public let images: (any ImageDescriber)?
    /// Answers every search with `WebSearchError.unavailable` when off or keyless.
    public let webSearch: any WebSearcher
    /// `decisionModel` is nil when no model can serve as the decision head.
    public let autoMode: any AutoModeResponder

    public init(config: EngineConfig, secrets: any SecretStore, caller: any ModelCaller,
                log: (any EventLog)? = nil, session: URLSession = .shared) {
        documents = DocumentService(config: config.documents, session: session)

        if let id = config.roles.vision, let model = config.models.first(where: { $0.id == id }) {
            images = ModelImageDescriber(model: model, caller: caller)
        } else {
            images = nil
        }

        let search = config.webSearch
        if let provider = search.provider {
            let keyRef = search.keyRef
            let key: @Sendable () -> String? = { keyRef.flatMap { secrets.secret($0) } }
            switch provider {
            case .tavily: webSearch = TavilySearcher(key: key, session: session)
            case .brave: webSearch = BraveSearcher(key: key, session: session)
            }
        } else {
            webSearch = UnavailableSearcher()
        }

        autoMode = DecisionHead(decisionModel: Self.decisionModel(config), caller: caller, log: log)
    }

    /// roles.decision, else the main model unless it is known to lack logprobs.
    static func decisionModel(_ config: EngineConfig) -> Model? {
        let roles = config.roles
        if let id = roles.decision { return config.models.first { $0.id == id } }
        guard let main = config.models.first(where: { $0.id == roles.main }), main.logprobs != false else { return nil }
        return main
    }
}
