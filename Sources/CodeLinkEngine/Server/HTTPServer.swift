import Foundation
import HTTPTypes
import Hummingbird
import Logging
import NIOCore
import ServiceLifecycle

/// The request context: Hummingbird's, plus the connection, so a streaming
/// response can tell when its client has gone.
struct EngineRequestContext: RequestContext {
    var coreContext: CoreRequestContextStorage
    let channel: any Channel
    init(source: ApplicationRequestContextSource) {
        coreContext = .init(source: source)
        channel = source.channel
    }
}

/// Requires the access token, as `Authorization: Bearer <token>` or
/// `x-api-key: <token>`.
struct AuthMiddleware: RouterMiddleware {
    typealias Context = EngineRequestContext
    let token: String

    func handle(_ request: Request, context: Context, next: (Request, Context) async throws -> Response) async throws -> Response {
        var presented: String?
        if let auth = request.headers[.authorization] {
            let parts = auth.split(separator: " ", maxSplits: 1)
            if parts.count == 2, parts[0].lowercased() == "bearer" { presented = String(parts[1]).trimmingCharacters(in: .whitespaces) }
        }
        if presented == nil, let name = HTTPField.Name("x-api-key"), let k = request.headers[name] { presented = k }
        guard let presented, !token.isEmpty, Self.equal(presented, token) else { return .anthropicError(.unauthorized) }
        return try await next(request, context)
    }

    /// Constant-time comparison.
    static func equal(_ a: String, _ b: String) -> Bool {
        let x = Array(a.utf8), y = Array(b.utf8)
        guard x.count == y.count else { return false }
        var diff: UInt8 = 0
        for i in 0..<x.count { diff |= x[i] ^ y[i] }
        return diff == 0
    }
}

/// The local HTTP server: Hummingbird on 127.0.0.1 only.
final class HTTPServer: Sendable {
    let port: Int
    private let group: ServiceGroup
    private let task: Task<Void, any Error>

    private init(port: Int, group: ServiceGroup, task: Task<Void, any Error>) {
        self.port = port; self.group = group; self.task = task
    }

    struct StartError: Error, CustomStringConvertible { var description: String }

    static func start(port: Int, token: String, environment: @escaping @Sendable () -> RequestEnvironment) async throws -> HTTPServer {
        let logger = Logger(label: "code-link", factory: { _ in SwiftLogNoOpLogHandler() })
        let router = makeRouter(token: token, environment: environment)
        let (ports, portCont) = AsyncStream.makeStream(of: Int.self)
        let app = Application(router: router,
                              configuration: .init(address: .hostname("127.0.0.1", port: port), serverName: nil),
                              onServerRunning: { channel in
                                  portCont.yield(channel.localAddress?.port ?? port)
                                  portCont.finish()
                              },
                              logger: logger)
        let group = ServiceGroup(configuration: .init(services: [app], gracefulShutdownSignals: [], logger: logger))
        let task = Task<Void, any Error> {
            defer { portCont.finish() }
            try await group.run()
        }
        var it = ports.makeAsyncIterator()
        if let bound = await it.next() {
            return HTTPServer(port: bound, group: group, task: task)
        }
        do {
            try await task.value
            throw StartError(description: "the server stopped while starting")
        } catch let e as StartError {
            throw e
        } catch {
            throw StartError(description: "could not listen on 127.0.0.1:\(port): \(error)")
        }
    }

    /// Stops accepting connections and lets requests in flight finish, for
    /// at most `grace`.
    func shutdown(grace: Duration) async {
        await group.triggerGracefulShutdown()
        let task = self.task
        let timer = Task {
            try await Task.sleep(for: grace)
            task.cancel()
        }
        _ = await task.result
        timer.cancel()
    }

    private static func makeRouter(token: String, environment: @escaping @Sendable () -> RequestEnvironment) -> Router<EngineRequestContext> {
        let router = Router(context: EngineRequestContext.self)
        router.get("healthz") { _, _ in Response.json(["status": "ok"]) }
        let v1 = router.group("v1").add(middleware: AuthMiddleware(token: token))

        @Sendable func incoming(_ request: Request, _ context: EngineRequestContext, maxBody: Int) async throws -> IncomingRequest {
            var headers: [String: String] = [:]
            for f in request.headers { headers[f.name.canonicalName] = f.value }
            let buffer = try await request.body.collect(upTo: maxBody)
            return IncomingRequest(body: Data(buffer.readableBytesView), headers: headers, channel: context.channel)
        }
        @Sendable func withBody(_ request: Request, _ context: EngineRequestContext,
                                _ handle: (IncomingRequest, RequestEnvironment) async -> Response) async -> Response {
            let env = environment()
            do {
                return await handle(try await incoming(request, context, maxBody: env.tuning.maxRequestBody), env)
            } catch {
                return .anthropicError(AnthropicError(status: 413, type: "request_too_large", message: "request body too large"))
            }
        }

        v1.get("models") { _, _ in ModelsList.response(environment()) }
        v1.post("messages") { request, context in
            await withBody(request, context) { await MessagesHandler.handle($0, env: $1) }
        }
        v1.post("messages/count_tokens") { request, context in
            await withBody(request, context) { await MessagesHandler.countTokens($0, env: $1) }
        }
        v1.post("responses") { request, context in
            await withBody(request, context) { await $1.responses.respond($0, env: $1) }
        }
        return router
    }
}

/// GET /v1/models: the configured models and the role aliases, in the
/// Anthropic list shape.
enum ModelsList {
    static func response(_ env: RequestEnvironment) -> Response {
        var data: [JSON] = []
        func entry(_ id: String, _ display: String) -> JSON {
            ["type": "model", "id": .string(id), "display_name": .string(display), "created_at": "2026-01-01T00:00:00Z"]
        }
        for m in env.config.models { data.append(entry(m.id, m.id)) }
        for (alias, m) in env.routing.aliases { data.append(entry(alias, "\(m.id) (\(alias))")) }
        return .json(["data": .array(data), "has_more": false,
                      "first_id": data.first?["id"] ?? nil, "last_id": data.last?["id"] ?? nil])
    }
}
