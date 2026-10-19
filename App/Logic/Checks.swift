import Foundation
import CodeLinkEngine

struct CheckResult: Equatable, Sendable {
    var ok: Bool
    var detail: String
}

/// Reachability tests for the Documents and Web search sections.
enum EndpointCheck {
    /// Any HTTP answer means the server is there (a layout-parsing endpoint
    /// answers 405/422 to a bare HEAD); only a connection failure is "unreachable".
    static func ocr(_ url: URL, session: URLSession = .shared) async -> CheckResult {
        var req = URLRequest(url: url, timeoutInterval: 5)
        req.httpMethod = "HEAD"
        let start = Date()
        do {
            let (_, resp) = try await session.data(for: req)
            let ms = Int(Date().timeIntervalSince(start) * 1000)
            let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
            return CheckResult(ok: true, detail: "Reachable (HTTP \(code), \(ms) ms)")
        } catch {
            return CheckResult(ok: false, detail: error.localizedDescription)
        }
    }

    /// The request that tests a search key with a one-result query.
    static func searchRequest(_ provider: SearchProvider, key: String) -> URLRequest {
        switch provider {
        case .tavily:
            var r = URLRequest(url: URL(string: "https://api.tavily.com/search")!, timeoutInterval: 10)
            r.httpMethod = "POST"
            r.setValue("application/json", forHTTPHeaderField: "Content-Type")
            r.setValue("Bearer \(key)", forHTTPHeaderField: "Authorization")
            r.httpBody = Data(#"{"query":"test","max_results":1}"#.utf8)
            return r
        case .brave:
            var c = URLComponents(string: "https://api.search.brave.com/res/v1/web/search")!
            c.queryItems = [URLQueryItem(name: "q", value: "test"), URLQueryItem(name: "count", value: "1")]
            var r = URLRequest(url: c.url!, timeoutInterval: 10)
            r.setValue(key, forHTTPHeaderField: "X-Subscription-Token")
            r.setValue("application/json", forHTTPHeaderField: "Accept")
            return r
        }
    }

    static func search(_ provider: SearchProvider, key: String, session: URLSession = .shared) async -> CheckResult {
        do {
            let (_, resp) = try await session.data(for: searchRequest(provider, key: key))
            switch (resp as? HTTPURLResponse)?.statusCode ?? 0 {
            case 200..<300: return CheckResult(ok: true, detail: "Key accepted")
            case 401, 403: return CheckResult(ok: false, detail: "Key rejected")
            case 429: return CheckResult(ok: false, detail: "Rate limited (the key is valid)")
            case let c: return CheckResult(ok: false, detail: "HTTP \(c)")
            }
        } catch {
            return CheckResult(ok: false, detail: error.localizedDescription)
        }
    }
}

/// Runs a process with a timeout without blocking the caller's thread.
enum ProcessRunner {
    /// Exit status, or nil when it couldn't start or timed out (the process is killed).
    static func run(_ path: String, _ args: [String], timeout: TimeInterval) async -> Int32? {
        await withCheckedContinuation { cont in
            let p = Process()
            p.executableURL = URL(fileURLWithPath: path)
            p.arguments = args
            p.standardInput = FileHandle.nullDevice
            p.standardOutput = FileHandle.nullDevice
            p.standardError = FileHandle.nullDevice
            let once = Once(cont)
            p.terminationHandler = { once.resume(with: $0.terminationReason == .exit ? $0.terminationStatus : nil) }
            do { try p.run() } catch { once.resume(with: nil); return }
            DispatchQueue.global().asyncAfter(deadline: .now() + timeout) {
                if p.isRunning { p.terminate() }
                once.resume(with: nil)
            }
        }
    }

    private final class Once: @unchecked Sendable {
        private let lock = NSLock()
        private var cont: CheckedContinuation<Int32?, Never>?
        init(_ c: CheckedContinuation<Int32?, Never>) { cont = c }
        func resume(with value: Int32?) {
            lock.lock(); let c = cont; cont = nil; lock.unlock()
            c?.resume(returning: value)
        }
    }
}

/// Is the tool on the user's login-shell PATH? (`$SHELL -lic '<tool> --version'`, 5 s.)
enum CLICheck {
    static func isAvailable(_ tool: CodingTool, shell: String = ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/zsh") async -> Bool {
        await ProcessRunner.run(shell, ["-lic", "\(tool.executable) --version >/dev/null 2>&1"], timeout: 5) == 0
    }
}
