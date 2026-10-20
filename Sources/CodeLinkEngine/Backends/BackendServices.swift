import Foundation

/// The core's ModelCaller: one non-streaming Chat Completions call on a
/// configured model, with that backend's key and the header wait.
struct EngineModelCaller: ModelCaller {
    let routing: Routing
    let client: UpstreamClient
    let headerTimeout: Duration

    enum CallError: Error, CustomStringConvertible {
        case unknownBackend(String)
        case notChatCompletions(String)
        var description: String {
            switch self {
            case .unknownBackend(let m): return "model \(m) has no configured backend"
            case .notChatCompletions(let m): return "model \(m) is on an Anthropic backend, not Chat Completions"
            }
        }
    }

    func chat(model: Model, body: [String: JSONValue]) async throws -> ChatResult {
        guard let target = routing.target(model) else { throw CallError.unknownBackend(model.id) }
        guard !target.isAnthropic else { throw CallError.notChatCompletions(model.id) }
        var b = JSON(JSONValue.object(body))
        b["model"] = nil
        b["stream"] = false
        let resp = try await client.send(target.chatRequest(b, stream: false), headerTimeout: headerTimeout)
        let data = try await resp.collect()
        let json = (try? JSON.parse(data))?.jsonValue ?? .object(["error": .object(["message": .string(String(decoding: data, as: UTF8.self))])])
        return ChatResult(status: resp.status, json: json)
    }
}

enum BackendProber {
    /// The ids and context windows a backend's `/models` lists.
    static func listModels(_ backend: Backend, apiKey: String?, client: UpstreamClient, timeout: Duration) async throws -> [ProbedModel] {
        let target = BackendTarget(model: Model(id: "", backendID: backend.id, upstreamModel: ""), backend: backend, apiKey: apiKey)
        var headers: [(String, String)] = [("Accept", "application/json")]
        if let apiKey, !apiKey.isEmpty {
            if backend.kind == .anthropic {
                headers += [("x-api-key", apiKey), ("anthropic-version", "2023-06-01")]
            } else {
                headers.append(("Authorization", "Bearer " + apiKey))
            }
        }
        let resp = try await client.send(UpstreamRequest(url: target.url("/models"), method: "GET", headers: headers), headerTimeout: timeout)
        let data = try await resp.collect(limit: 16 << 20)
        guard (200..<300).contains(resp.status) else {
            throw UpstreamError.transport("HTTP \(resp.status): " + scrubUpstreamMessage(parseUpstreamError(data).message))
        }
        guard let j = try? JSON.parse(data) else { throw UpstreamError.transport("the model list is not JSON") }
        let items = j["data"]?.array ?? j["models"]?.array ?? j.array ?? []
        return items.compactMap { m in
            guard let id = m["id"]?.string ?? m["name"]?.string else { return nil }
            let window = m["max_model_len"]?.int ?? m["context_length"]?.int ?? m["context_window"]?.int
                ?? m["max_input_tokens"]?.int ?? m["meta"]?["n_ctx_train"]?.int
            return ProbedModel(upstreamModel: id, contextWindow: window)
        }
    }

    /// Whether a model returns logprobs: a 1-token request asking for them.
    /// No temperature: some servers (Flare) refuse temperature 0, and the
    /// question is only whether logprobs come back.
    static func logprobs(_ target: BackendTarget, client: UpstreamClient, timeout: Duration) async -> Bool? {
        let body: JSON = ["messages": [["role": "user", "content": "Hi"]], "max_tokens": 1,
                          "logprobs": true, "top_logprobs": 2, "stream": false]
        guard let resp = try? await client.send(target.chatRequest(body, stream: false), headerTimeout: timeout),
              let data = try? await resp.collect(limit: 4 << 20) else { return nil }
        guard (200..<300).contains(resp.status), let j = try? JSON.parse(data) else { return false }
        guard let content = j["choices"]?[0]?["logprobs"]?["content"]?.array, let first = content.first else { return false }
        return !(first["top_logprobs"]?.array ?? []).isEmpty || first["logprob"] != nil
    }

    static func probe(_ backend: Backend, secrets: any SecretStore, client: UpstreamClient) async -> BackendProbe {
        let apiKey = backend.apiKeyRef.flatMap { secrets.secret($0) }
        let started = ContinuousClock.now
        let models: [ProbedModel]
        do {
            models = try await listModels(backend, apiKey: apiKey, client: client, timeout: .seconds(15))
        } catch {
            return BackendProbe(reachable: false, error: scrubUpstreamMessage("\(error)"))
        }
        let latency = Int((ContinuousClock.now - started) / .milliseconds(1))
        guard backend.kind == .openAICompatible else { return BackendProbe(reachable: true, models: models, latencyMS: latency) }
        // Logprobs for the first 16 models, four at a time.
        let probed = await withTaskGroup(of: (Int, Bool?).self) { group -> [Int: Bool?] in
            var out: [Int: Bool?] = [:]
            var next = 0
            let limit = min(models.count, 16)
            func add() {
                guard next < limit else { return }
                let i = next, m = models[i]
                next += 1
                let target = BackendTarget(model: Model(id: m.upstreamModel, backendID: backend.id, upstreamModel: m.upstreamModel),
                                           backend: backend, apiKey: apiKey)
                group.addTask { (i, await logprobs(target, client: client, timeout: .seconds(30))) }
            }
            for _ in 0..<4 { add() }
            while let (i, v) = await group.next() { out[i] = v; add() }
            return out
        }
        var result = models
        for (i, v) in probed { result[i].logprobs = v }
        return BackendProbe(reachable: true, models: result, latencyMS: latency)
    }
}

/// Context windows detected from the backends' `/models` (max_model_len),
/// for models configured without one. Looked up once per model; a failed
/// lookup is retried after five minutes.
actor ContextWindows {
    private var known: [String: (window: Int?, at: ContinuousClock.Instant)] = [:]
    private let client: UpstreamClient

    init(client: UpstreamClient) { self.client = client }

    func window(for target: BackendTarget) async -> Int? {
        if let w = target.model.contextWindow { return w }
        guard target.backend.kind == .openAICompatible else { return nil }
        let key = target.backend.id + "|" + target.backend.baseURL.absoluteString + "|" + target.model.upstreamModel
        if let k = known[key], k.window != nil || ContinuousClock.now - k.at < .seconds(300) { return k.window }
        known[key] = (nil, .now) // one lookup at a time
        let models = try? await BackendProber.listModels(target.backend, apiKey: target.apiKey, client: client, timeout: .seconds(5))
        let w = models?.first { $0.upstreamModel == target.model.upstreamModel }?.contextWindow
        known[key] = (w, .now)
        return w
    }
}
