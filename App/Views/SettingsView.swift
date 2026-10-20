import SwiftUI
import CodeLinkEngine

/// The one configuration page, as a native grouped form: providers and the
/// models used from each, routing, documents, web search, launch.
struct SettingsView: View {
    @Bindable var model: AppModel
    /// Snapshots draw the page at its natural height.
    var snapshot = false

    var body: some View {
        VStack(spacing: 0) {
            Form {
                ProvidersSection(model: model)
                RoutingSection(model: model)
                DocumentsSection(model: model)
                WebSearchSection(model: model)
                LaunchSection(model: model)
                ActivitySection(store: model.activity)
            }
            .formStyle(.grouped)
            Footer(model: model)
        }
        .frame(minWidth: 540, idealWidth: 560, minHeight: snapshot ? nil : 520, idealHeight: 680)
        .tint(Theme.accent)
    }
}

// MARK: Footer

private struct Footer: View {
    @Bindable var model: AppModel
    var body: some View {
        HStack(spacing: 8) {
            status
            Spacer()
            Button("Revert") { model.revert() }.disabled(!model.hasUnsavedChanges)
            Button("Save") { Task { await model.save() } }
                .keyboardShortcut("s", modifiers: .command)
                .buttonStyle(.borderedProminent)
                .disabled(!model.hasUnsavedChanges)
        }
        .padding(.horizontal, 20).padding(.vertical, 12)
        .background(.bar)
    }

    @ViewBuilder private var status: some View {
        switch model.saveMessage {
        case .failed(let problems): StatusLabel(level: .error, text: problems.joined(separator: " "))
        case .saved where !model.hasUnsavedChanges: StatusLabel(level: .ok, text: "Saved")
        default:
            if model.hasUnsavedChanges { Text("Unsaved changes").foregroundStyle(.secondary) }
            else { EngineStatusLine(model: model) }
        }
    }
}

private struct EngineStatusLine: View {
    var model: AppModel
    var body: some View {
        let s = model.status
        HStack(spacing: 6) {
            Circle().fill(color(s.kind)).frame(width: 7, height: 7)
            Text(s.line).foregroundStyle(.secondary).lineLimit(1)
        }
    }
    private func color(_ k: EngineStatus.Kind) -> Color {
        switch k {
        case .running: Theme.ok
        case .starting: Theme.warn
        case .backendUnreachable, .failed: Theme.error
        case .stopped, .notConfigured: .secondary.opacity(0.5)
        }
    }
}

// MARK: Providers

private struct ProvidersSection: View {
    @Bindable var model: AppModel
    var body: some View {
        Section {
            ForEach($model.draft.backends) { $backend in
                ProviderRow(model: model, backend: $backend)
            }
            Button { model.addProvider() } label: { Label("Add Provider", systemImage: "plus") }
                .buttonStyle(.borderless)
                .task(id: model.draft.backends.map(\.id)) { for b in model.draft.backends { await model.probeIfNeeded(b.id) } }
        } header: {
            Text("Providers")
        } footer: {
            if model.draft.backends.isEmpty {
                Text("A vLLM, SGLang or Flare server, or an Anthropic endpoint.").foregroundStyle(.secondary)
            }
        }
    }
}

private struct ProviderRow: View {
    var model: AppModel
    @Binding var backend: Backend
    @State private var expanded = false
    @State private var name = ""
    @State private var url = ""
    @State private var key = ""
    @FocusState private var nameFocused: Bool

    var body: some View {
        DisclosureGroup(isExpanded: $expanded) {
            TextField("Name", text: $name)
                .focused($nameFocused)
                .onSubmit(commitName)
                .onChange(of: nameFocused) { if !nameFocused { commitName() } }
            Picker("Kind", selection: $backend.kind) {
                Text("OpenAI-compatible (vLLM, SGLang, Flare)").tag(BackendKind.openAICompatible)
                Text("Anthropic").tag(BackendKind.anthropic)
            }
            TextField("URL", text: $url, prompt: Text("http://host:8000/v1"))
                .onChange(of: url) { if let u = Self.parse(url) { backend.baseURL = u } }
            SecureField("API key", text: $key, prompt: Text(model.hasAPIKey(forProvider: backend.id) ? "Saved in Keychain" : "Optional"))
                .onChange(of: key) { if !key.isEmpty { model.setAPIKey(key, forProvider: backend.id) } }
            LabeledContent("Connection") {
                HStack(spacing: 8) {
                    ProbeSummary(probe: model.probes[backend.id])
                    Button(model.probing.contains(backend.id) ? "Testing…" : "Test") { Task { await model.probe(backend.id) } }
                        .disabled(model.probing.contains(backend.id) || Self.parse(url) == nil)
                }
            }
            ProviderModels(model: model, backendID: backend.id)
            HStack {
                Spacer()
                Button("Remove Provider", role: .destructive) { model.removeProvider(backend.id) }
                    .buttonStyle(.borderless).foregroundStyle(Theme.error)
            }
        } label: {
            HStack(spacing: 8) {
                Circle().fill(dot).frame(width: 7, height: 7)
                Text(backend.id)
                Text(backend.baseURL.host() ?? backend.baseURL.absoluteString).foregroundStyle(.secondary).lineLimit(1)
                Spacer()
                let inUse = model.draft.models.filter { $0.backendID == backend.id }.count
                if inUse > 0 { Text("\(inUse) in use").foregroundStyle(.secondary) }
            }
        }
        .onAppear {
            name = backend.id; url = backend.baseURL.absoluteString
            if backend.baseURL.host() == nil || backend.baseURL.host() == "localhost" && model.probes[backend.id] == nil { expanded = true }
        }
        .onChange(of: backend.id) { name = backend.id }
        .onChange(of: backend.baseURL) { if Self.parse(url) != backend.baseURL { url = backend.baseURL.absoluteString } }
    }

    private var dot: Color {
        guard let p = model.probes[backend.id] else { return .secondary.opacity(0.4) }
        return p.reachable ? Theme.ok : Theme.error
    }

    private func commitName() {
        let n = name.trimmingCharacters(in: .whitespaces)
        if n.isEmpty || (n != backend.id && model.draft.backends.contains { $0.id == n }) { name = backend.id; return }
        model.draft.renameBackend(from: backend.id, to: n)
    }

    static func parse(_ s: String) -> URL? {
        let t = s.trimmingCharacters(in: .whitespaces)
        guard let u = URL(string: t), let scheme = u.scheme, ["http", "https"].contains(scheme), u.host != nil else { return nil }
        return u
    }
}

/// The provider's models: tick the ones to use.
private struct ProviderModels: View {
    var model: AppModel
    var backendID: String

    var body: some View {
        let items = model.groups().first { $0.backendID == backendID }?.items ?? []
        if items.isEmpty {
            if model.probes[backendID] != nil {
                Text("No models reported. Test again, or type a model name under Models.").foregroundStyle(.secondary)
            }
        } else {
            ForEach(items) { ModelToggleRow(model: model, item: $0) }
        }
    }
}

private struct ModelToggleRow: View {
    var model: AppModel
    var item: ChoiceItem
    @State private var alias = ""
    @FocusState private var aliasFocused: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Toggle(isOn: Binding(get: { item.enabled }, set: { model.setModelEnabled(item.choice, $0) })) {
                HStack {
                    Text(item.choice.upstream).lineLimit(1).truncationMode(.middle)
                    Spacer()
                    Text(RoutingChoices.facts(item)).foregroundStyle(.secondary)
                }
            }
            .toggleStyle(.checkbox)
            if item.enabled {
                HStack(spacing: 8) {
                    TextField("Name", text: $alias, prompt: Text("Name shown to Claude Code"))
                        .focused($aliasFocused)
                        .onSubmit(commitAlias)
                        .onChange(of: aliasFocused) { if !aliasFocused { commitAlias() } }
                    TextField("Context", value: contextBinding, format: .number.grouping(.never), prompt: Text("Context"))
                        .frame(width: 110)
                        .help("Context window in tokens. Empty = detected from the provider.")
                }
                .labelsHidden()
                .padding(.leading, 20)
            }
        }
        .onAppear { alias = item.modelID ?? "" }
        .onChange(of: item.modelID) { alias = item.modelID ?? "" }
    }

    private func commitAlias() {
        guard let old = item.modelID else { return }
        if !model.draft.renameModel(from: old, to: alias) { alias = old }
    }

    private var contextBinding: Binding<Int?> {
        Binding(get: { item.modelID.flatMap { model.draft.model($0)?.contextWindow } },
                set: { v in
                    if let id = item.modelID, let i = model.draft.models.firstIndex(where: { $0.id == id }) {
                        model.draft.models[i].contextWindow = (v ?? 0) > 0 ? v : nil
                    }
                })
    }
}

struct ProbeSummary: View {
    var probe: BackendProbe?
    var body: some View {
        if let probe {
            if probe.reachable {
                StatusLabel(level: .ok, text: Self.parts(probe).joined(separator: " · "))
            } else {
                StatusLabel(level: .error, text: probe.error ?? "Unreachable")
            }
        } else {
            Text("Not tested").foregroundStyle(.secondary)
        }
    }

    static func parts(_ probe: BackendProbe) -> [String] {
        let n = probe.models.count
        var parts = ["\(n) model\(n == 1 ? "" : "s")"]
        if probe.models.contains(where: { $0.logprobs == true }) { parts.append("logprobs") }
        if let ms = probe.latencyMS { parts.append("\(ms) ms") }
        return parts
    }
}

// MARK: Routing

private struct RoutingSection: View {
    @Bindable var model: AppModel
    @State private var advanced = false

    var body: some View {
        Section {
            ForEach(model.warnings, id: \.self) { StatusLabel(level: .warn, text: $0) }
            if model.draft.backends.isEmpty {
                Text("Add a provider first.").foregroundStyle(.secondary)
            } else {
                ModelPicker(model: model, slot: .main, title: "Sonnet", none: nil)
                    .help("Claude Code's default model. Also used for background tasks and Codex unless set below.")
                ModelPicker(model: model, slot: .opus, title: "Opus", none: "Same as Sonnet")
                    .help("Used when Claude Code asks for Opus (plan mode, /model). Can be on another provider.")
                DecisionRow(model: model)
                DisclosureGroup("More", isExpanded: $advanced) {
                    ModelPicker(model: model, slot: .small, title: "Haiku", none: "Same as Sonnet")
                        .help("Quick background work: titles, summaries.")
                    ModelPicker(model: model, slot: .vision, title: "Vision", none: "None")
                        .help("Describes images for a model without vision. Optional.")
                    ModelPicker(model: model, slot: .codex, title: "Codex", none: "Same as Sonnet")
                }
            }
        } header: {
            Text("Models")
        } footer: {
            Text("New sessions use the saved routing; running sessions keep theirs.").foregroundStyle(.secondary)
        }
    }
}

/// One picker over every model, grouped by provider. Picking a model that
/// isn't enabled yet enables it.
private struct ModelPicker: View {
    var model: AppModel
    var slot: RoleSlot
    var title: String
    /// The label for "no model" (nil = the slot needs one).
    var none: String?

    var body: some View {
        let current = model.draft.choice(for: slot)?.tag ?? ""
        Picker(title, selection: Binding(get: { current }, set: { model.setRole(slot, to: ModelChoice(tag: $0)) })) {
            if none != nil || current.isEmpty { Text(none ?? "Choose…").tag("") }
            ForEach(model.groups()) { g in
                Section(g.backendID) {
                    ForEach(g.items) { Text(Self.label($0)).tag($0.choice.tag) }
                }
            }
        }
    }

    static func label(_ item: ChoiceItem) -> String {
        var s = item.title
        let facts = RoutingChoices.facts(item)
        if !facts.isEmpty { s += "  ·  " + facts }
        if !item.enabled { s += "  (not in use)" }
        return s
    }
}

/// Auto mode's decision model and whether it can make single-token decisions.
private struct DecisionRow: View {
    @Bindable var model: AppModel
    private var choice: ModelChoice? { model.draft.choice(for: .decision) ?? model.draft.choice(for: .main) }

    var body: some View {
        ModelPicker(model: model, slot: .decision, title: "Auto mode", none: "Same as Sonnet")
            .help("The model that approves actions in auto mode. Needs logprobs (vLLM or SGLang); otherwise approvals are slower.")
        if let c = choice {
            let configured = model.draft.model(model.draft.modelID(for: .decision) ?? model.draft.roles.main)
            switch configured?.logprobs ?? model.probedModel(c)?.logprobs {
            case true?: EmptyView()
            case false?: StatusLabel(level: .warn, text: "\(c.upstream) has no logprobs, so approvals use slower generated answers.")
            case nil: EmptyView()
            }
        }
    }
}

// MARK: Documents

private struct DocumentsSection: View {
    @Bindable var model: AppModel
    @State private var url = ""
    @State private var result: CheckResult?
    @State private var testing = false

    var body: some View {
        Section("Documents") {
            LabeledContent("PaddleOCR") {
                HStack(spacing: 8) {
                    TextField("PaddleOCR", text: $url, prompt: Text("Optional"))
                        .labelsHidden()
                        .onChange(of: url) {
                            let t = url.trimmingCharacters(in: .whitespaces)
                            model.draft.documents.paddleOCR = t.isEmpty ? nil : ProviderRow.parse(t)
                            result = nil
                        }
                    Button(testing ? "Testing…" : "Test") { test() }.disabled(testing || model.draft.documents.paddleOCR == nil)
                }
            }
            .help("A PaddleOCR server's /layout-parsing URL. Used first when it's reachable.")
            if let result { StatusLabel(level: result.ok ? .ok : .error, text: result.detail) }
            Picker("Otherwise", selection: $model.draft.documents.local) {
                Text("Read on this Mac (with OCR)").tag(LocalExtraction.appleVision)
                Text("PDF text only").tag(LocalExtraction.textOnly)
                Text("Don't read documents").tag(LocalExtraction.off)
            }
        }
        .onAppear { url = model.draft.documents.paddleOCR?.absoluteString ?? "" }
        .onChange(of: model.draft.documents.paddleOCR) {
            if model.draft.documents.paddleOCR?.absoluteString != (url.isEmpty ? nil : url) { url = model.draft.documents.paddleOCR?.absoluteString ?? "" }
        }
    }

    private func test() {
        guard let u = model.draft.documents.paddleOCR else { return }
        testing = true
        Task { result = await EndpointCheck.ocr(u); testing = false }
    }
}

// MARK: Web search

private struct WebSearchSection: View {
    @Bindable var model: AppModel
    @State private var key = ""
    @State private var result: CheckResult?
    @State private var testing = false

    private var provider: Binding<SearchProvider?> {
        Binding(get: { model.draft.webSearch.provider }, set: { p in
            model.draft.webSearch.provider = p
            model.draft.webSearch.keyRef = p.map { "websearch-\($0.rawValue)" }
            key = ""; result = nil
        })
    }

    var body: some View {
        Section("Web Search") {
            Picker("Provider", selection: provider) {
                Text("Off").tag(SearchProvider?.none)
                Text("Tavily").tag(SearchProvider?.some(.tavily))
                Text("Brave").tag(SearchProvider?.some(.brave))
            }
            if model.draft.webSearch.provider != nil {
                LabeledContent("API key") {
                    HStack(spacing: 8) {
                        SecureField("API key", text: $key, prompt: Text(model.hasSearchKey() ? "Saved in Keychain" : "Required"))
                            .labelsHidden()
                            .onChange(of: key) { if !key.isEmpty { model.setSearchKey(key) } }
                        Button(testing ? "Testing…" : "Test") { test() }.disabled(testing || !model.hasSearchKey())
                    }
                }
                if let result { StatusLabel(level: result.ok ? .ok : .error, text: result.detail) }
            }
        }
    }

    private func test() {
        guard let p = model.draft.webSearch.provider, let ref = model.draft.webSearch.keyRef, let k = model.secrets.secret(ref) else { return }
        testing = true
        Task { result = await EndpointCheck.search(p, key: k); testing = false }
    }
}

// MARK: Launch

private struct LaunchSection: View {
    @Bindable var model: AppModel

    var body: some View {
        Section("Launch") {
            Picker("Terminal", selection: Binding(get: { model.launchSettings.terminal }, set: { model.launchSettings.terminal = $0; model.saveLaunchSettings() })) {
                ForEach(TerminalPreference.allCases, id: \.self) { Text($0.label).tag($0) }
            }
            Picker("Claude Code permissions", selection: Binding(get: { model.launchSettings.permissionMode }, set: { model.launchSettings.permissionMode = $0; model.saveLaunchSettings() })) {
                ForEach(PermissionMode.all, id: \.self) { Text($0 == "auto" ? "Auto" : $0).tag($0) }
            }
            TextField("Port", value: $model.draft.port, format: .number.grouping(.never))
                .help("The local port Claude Code and Codex connect to. Changing it restarts the engine.")
            Toggle("Open at login", isOn: Binding(get: { model.launchAtLogin }, set: { model.setLaunchAtLogin($0) }))
        }
    }
}

// MARK: Activity

private struct ActivitySection: View {
    var store: ActivityStore
    @State private var open = false

    var body: some View {
        Section {
            DisclosureGroup("Recent activity", isExpanded: $open) {
                if store.entries.isEmpty {
                    Text("Nothing yet.").foregroundStyle(.secondary)
                } else {
                    ForEach(store.entries.prefix(50)) { e in
                        HStack(alignment: .firstTextBaseline, spacing: 8) {
                            Text(e.date, format: .dateTime.hour().minute().second())
                                .monospacedDigit().foregroundStyle(.secondary)
                            Text(e.text).lineLimit(1).truncationMode(.middle)
                                .foregroundStyle(e.level == .error ? Theme.error : e.level == .warning ? Theme.warn : .primary)
                        }
                        .font(.callout)
                    }
                    Button("Clear") { store.clear() }.buttonStyle(.borderless)
                }
            }
        }
    }
}

// MARK: Previews

@MainActor private func previewModel() -> AppModel {
    let m = AppModel(environment: .isolated())
    var c = EngineConfig()
    c.backends = [Backend(id: "cn3", baseURL: URL(string: "http://192.168.13.33:8000/v1")!),
                  Backend(id: "eidonix", baseURL: URL(string: "https://llm.eidonix.com/v1")!)]
    c.models = [Model(id: "GLM-5.3", backendID: "eidonix", upstreamModel: "GLM-5.3-Flash", contextWindow: 1_048_576, logprobs: true),
                Model(id: "DeepSeek-V4.1", backendID: "cn3", upstreamModel: "DeepSeek-V4.1-Flash", contextWindow: 524_288, logprobs: false)]
    c.roles = Roles(main: "GLM-5.3", opus: "DeepSeek-V4.1")
    c.documents.paddleOCR = URL(string: "http://192.168.13.30:8002/layout-parsing")
    m.draft = c
    return m
}
#Preview("Settings · light") { SettingsView(model: previewModel()).preferredColorScheme(.light) }
#Preview("Settings · dark") { SettingsView(model: previewModel()).preferredColorScheme(.dark) }
