import Foundation

/// The feature modules the core calls for one configuration (Hooks.swift).
/// A nil hook means the feature is off: documents become a "could not be
/// read" note, images for a model without vision an "image omitted" note,
/// web searches an `unavailable` result, and classifier requests ordinary
/// requests.
public struct EngineHooks: Sendable {
    public var documentReader: (any DocumentReader)?
    public var imageDescriber: (any ImageDescriber)?
    public var webSearcher: (any WebSearcher)?
    public var autoMode: (any AutoModeResponder)?
    public init(documentReader: (any DocumentReader)? = nil, imageDescriber: (any ImageDescriber)? = nil,
                webSearcher: (any WebSearcher)? = nil, autoMode: (any AutoModeResponder)? = nil) {
        self.documentReader = documentReader; self.imageDescriber = imageDescriber
        self.webSearcher = webSearcher; self.autoMode = autoMode
    }
}

/// Builds the hooks for a configuration. The engine calls it at init and on
/// every `apply`; requests already running keep the hooks they started with.
/// The app (or the feature team) supplies one; the default has none.
public protocol EngineHooksFactory: Sendable {
    func makeHooks(config: EngineConfig, caller: any ModelCaller, secrets: any SecretStore, log: any EventLog) -> EngineHooks
}

/// No feature modules: every hook off.
public struct NoHooksFactory: EngineHooksFactory {
    public init() {}
    public func makeHooks(config: EngineConfig, caller: any ModelCaller, secrets: any SecretStore, log: any EventLog) -> EngineHooks {
        EngineHooks()
    }
}
