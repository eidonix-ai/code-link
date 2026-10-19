import Foundation

/// A model with the backend it runs on and that backend's key.
struct BackendTarget: Sendable {
    var model: Model
    var backend: Backend
    var apiKey: String?

    var isAnthropic: Bool { backend.kind == .anthropic }

    func url(_ path: String) -> URL {
        var base = backend.baseURL.absoluteString
        while base.hasSuffix("/") { base.removeLast() }
        return URL(string: base + path) ?? backend.baseURL.appendingPathComponent(path)
    }

    /// A Chat Completions request on this model: `body` gets the upstream
    /// model name as its first key.
    func chatRequest(_ body: JSON, stream: Bool) -> UpstreamRequest {
        var b = JSONObject()
        b["model"] = .string(model.upstreamModel)
        for (k, v) in body.object ?? JSONObject() where k != "model" { b[k] = v }
        var headers = [("Content-Type", "application/json")]
        headers.append(("Accept", stream ? "text/event-stream" : "application/json"))
        if let apiKey, !apiKey.isEmpty { headers.append(("Authorization", "Bearer " + apiKey)) }
        return UpstreamRequest(url: url("/chat/completions"), headers: headers, body: JSON.object(b).data)
    }

    /// A native Anthropic request (`/messages`, `/messages/count_tokens`).
    func anthropicRequest(path: String, body: Data, clientHeaders: [String: String]) -> UpstreamRequest {
        var headers = [("Content-Type", "application/json")]
        if let a = clientHeaders["accept"] { headers.append(("Accept", a)) }
        headers.append(("anthropic-version", clientHeaders["anthropic-version"] ?? "2023-06-01"))
        if let beta = clientHeaders["anthropic-beta"] { headers.append(("anthropic-beta", beta)) }
        if let apiKey, !apiKey.isEmpty { headers.append(("x-api-key", apiKey)) }
        return UpstreamRequest(url: url(path), headers: headers, body: body)
    }
}

/// Model lookup and Claude Code's model names (docs/DESIGN.md §3).
struct Routing: Sendable {
    let config: EngineConfig
    let secrets: any SecretStore

    func model(id: String?) -> Model? {
        guard let id, !id.isEmpty else { return nil }
        return config.models.first { $0.id == id }
    }

    var mainModel: Model? { model(id: config.roles.main) }

    /// The model a request names: a configured id, else a Claude model name
    /// mapped to its role (opus / haiku / sonnet → opus / small / main) with
    /// any `[1m]`-style suffix removed, else main.
    func resolve(_ requested: String) -> Model? {
        if let m = model(id: requested) { return m }
        var name = requested.trimmingCharacters(in: .whitespaces)
        if let open = name.lastIndex(of: "["), name.hasSuffix("]") { name = String(name[..<open]) }
        if let m = model(id: name) { return m }
        let lower = name.lowercased()
        if let m = config.models.first(where: { $0.id.lowercased() == lower }) { return m }
        if lower.contains("opus") { return model(id: config.roles.opus) ?? mainModel }
        if lower.contains("haiku") { return model(id: config.roles.small) ?? mainModel }
        return mainModel
    }

    func target(_ model: Model) -> BackendTarget? {
        guard let b = config.backends.first(where: { $0.id == model.backendID }) else { return nil }
        let key = b.apiKeyRef.flatMap { secrets.secret($0) }
        return BackendTarget(model: model, backend: b, apiKey: key)
    }

    /// Role aliases listed by /v1/models.
    var aliases: [(alias: String, model: Model)] {
        var out: [(String, Model)] = []
        if let m = mainModel { out.append(("sonnet", m)) }
        if let m = model(id: config.roles.opus) ?? mainModel { out.append(("opus", m)) }
        if let m = model(id: config.roles.small) ?? mainModel { out.append(("haiku", m)) }
        return out
    }
}
