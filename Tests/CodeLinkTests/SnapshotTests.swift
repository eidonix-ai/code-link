import Testing
import SwiftUI
import AppKit
import CodeLinkEngine
@testable import CodeLink

/// Renders the settings page in light and dark into $CODE_LINK_SNAPSHOT_DIR
/// (skipped when unset): `CODE_LINK_SNAPSHOT_DIR=/tmp/shots xcodebuild test …`.
@MainActor
@Test(.enabled(if: ProcessInfo.processInfo.environment["CODE_LINK_SNAPSHOT_DIR"] != nil))
func renderSettingsSnapshots() throws {
    let dir = URL(fileURLWithPath: ProcessInfo.processInfo.environment["CODE_LINK_SNAPSHOT_DIR"]!, isDirectory: true)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    for (name, scheme, appearance) in [("light", ColorScheme.light, NSAppearance.Name.aqua), ("dark", .dark, .darkAqua)] {
        let model = AppModel(environment: .isolated())
        model.draft = Fixtures.config
        model.activity.append(.request(route: "/v1/messages", model: "GLM-5.3-Flash", status: 200, latencyMS: 840))
        model.activity.append(.decision(stage: 1, model: "GLM-5.3", pBlock: 0.03, severity: 12, latencyMS: 310, fallback: nil))
        model.activity.append(.webSearch(query: "swiftui menu bar extra", results: 5))
        model.activity.append(.request(route: "/v1/responses", model: "GLM-5.3", status: 529, latencyMS: 15))
        NSAppearance.current = NSAppearance(named: appearance)
        let view = SettingsView(model: model, snapshot: true)
            .environment(\.colorScheme, scheme)
            .background(Color(nsColor: appearance == .darkAqua ? NSColor(white: 0.16, alpha: 1) : NSColor(white: 0.93, alpha: 1)))
        let renderer = ImageRenderer(content: view)
        renderer.scale = 2
        let png = renderer.nsImage.flatMap { NSBitmapImageRep(data: $0.tiffRepresentation ?? Data())?.representation(using: .png, properties: [:]) }
        try #require(png).write(to: dir.appendingPathComponent("settings-\(name).png"))
    }
}

enum Fixtures {
    static let config: EngineConfig = {
        var c = EngineConfig()
        c.backends = [Backend(id: "cn3-vllm", baseURL: URL(string: "http://192.168.13.33:8000/v1")!, apiKeyRef: "k1"),
                      Backend(id: "cn4-flare", baseURL: URL(string: "http://192.168.13.34:8000/v1")!)]
        c.models = [Model(id: "GLM-5.3", backendID: "cn3-vllm", upstreamModel: "GLM-5.3", contextWindow: 1_048_576, logprobs: true),
                    Model(id: "DeepSeek-V4.1-Flash", backendID: "cn4-flare", upstreamModel: "DeepSeek-V4.1-Flash", contextWindow: 262_144, logprobs: false)]
        c.roles = Roles(main: "DeepSeek-V4.1-Flash", opus: "GLM-5.3", decision: "GLM-5.3")
        c.documents.paddleOCR = URL(string: "http://192.168.13.30:8002/layout-parsing")
        c.webSearch = WebSearchConfig(provider: .tavily, keyRef: "websearch-tavily")
        return c
    }()
}
