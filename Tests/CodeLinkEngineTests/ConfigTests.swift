import Testing
import Foundation
@testable import CodeLinkEngine

@Test func autoCompactDefaultsTo85Percent() {
    #expect(Model(id: "m", backendID: "b", upstreamModel: "u", contextWindow: 1_048_576).effectiveAutoCompactAt == 891_289)
    #expect(Model(id: "m", backendID: "b", upstreamModel: "u", contextWindow: 200_000, autoCompactAt: 150_000).effectiveAutoCompactAt == 150_000)
    #expect(Model(id: "m", backendID: "b", upstreamModel: "u").effectiveAutoCompactAt == nil)
}

@Test func configRoundTrips() throws {
    let c = EngineConfig(backends: [Backend(id: "cn3", baseURL: URL(string: "http://127.0.0.1:8000/v1")!)],
                         models: [Model(id: "GLM", backendID: "cn3", upstreamModel: "glm", thinkingOff: .object(["chat_template_kwargs": .object(["reasoning_effort": .string("low")])]))],
                         roles: Roles(main: "GLM"), documents: DocumentsConfig(paddleOCR: URL(string: "http://ocr.local/layout-parsing")!, local: .appleVision),
                         webSearch: WebSearchConfig(provider: .brave, keyRef: "brave"))
    let back = try JSONDecoder().decode(EngineConfig.self, from: JSONEncoder().encode(c))
    #expect(back == c)
}
