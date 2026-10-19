// Headless engine for testing: runs CodeLinkEngine from a config file,
// without the app or its settings. Secrets come from the environment.
//
//   CODE_LINK_TOKEN=<access token> CODE_LINK_SECRET_<REF>=<key> \
//     swift run code-link-serve path/to/config.json
//
// <REF> is a backend's apiKeyRef (or the web-search keyRef) upper-cased,
// with characters other than A-Z and 0-9 replaced by "_".
import Foundation
import CodeLinkEngine

struct EnvSecrets: SecretStore {
    func secret(_ ref: String) -> String? {
        let name = "CODE_LINK_SECRET_" + ref.uppercased().map { $0.isLetter || $0.isNumber ? String($0) : "_" }.joined()
        return ProcessInfo.processInfo.environment[name]
    }
}

struct PrintLog: EventLog {
    func record(_ event: EngineEvent) { print("[event]", event); fflush(stdout) }
}

let args = CommandLine.arguments
guard args.count == 2, let data = FileManager.default.contents(atPath: args[1]) else {
    FileHandle.standardError.write(Data("usage: code-link-serve config.json\n".utf8)); exit(2)
}
let config = try JSONDecoder().decode(EngineConfig.self, from: data)
guard let token = ProcessInfo.processInfo.environment["CODE_LINK_TOKEN"], !token.isEmpty else {
    FileHandle.standardError.write(Data("set CODE_LINK_TOKEN\n".utf8)); exit(2)
}
let engine = Engine(config: config, secrets: EnvSecrets(), log: PrintLog(), accessToken: token)
try await engine.start()
print("code-link-serve:", await engine.state); fflush(stdout)
while true { try await Task.sleep(for: .seconds(3600)) }
