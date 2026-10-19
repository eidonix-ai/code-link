import SwiftUI
import CodeLinkEngine

/// The one configuration page: providers, models, auto mode, documents,
/// web search, launch, and a quiet activity list.
struct SettingsView: View {
    @Bindable var model: AppModel
    /// Snapshots draw the whole page at its natural height with the disclosures open.
    var snapshot = false

    var body: some View {
        VStack(spacing: 0) {
            Header(model: model)
            if snapshot { page } else { ScrollView { page } }
            Footer(model: model)
        }
        .frame(width: 600, height: snapshot ? nil : 720)
        .tint(Theme.accent)
        .background(Color(nsColor: .windowBackgroundColor))
    }

    private var page: some View {
        VStack(alignment: .leading, spacing: 22) {
            ProvidersSection(model: model)
            ModelsSection(model: model, expanded: snapshot)
            AutoModeSection(model: model)
            DocumentsSection(model: model)
            WebSearchSection(model: model)
            LaunchSection(model: model)
            ActivitySection(store: model.activity, expanded: snapshot)
        }
        .padding(.horizontal, 20).padding(.vertical, 16)
    }
}

private struct Header: View {
    var model: AppModel
    var body: some View {
        let status = model.status
        VStack(spacing: 0) {
            HStack(spacing: 8) {
                Image(systemName: "link.circle.fill").font(.system(size: 18)).foregroundStyle(Theme.accent)
                Text("code-link").font(.system(size: 15, weight: .semibold))
                Spacer()
                Circle().fill(statusColor(status.kind)).frame(width: 8, height: 8)
                Text(status.line).font(.system(size: 12)).foregroundStyle(.secondary).lineLimit(1)
            }
            .padding(.horizontal, 20).padding(.vertical, 10)
            Rectangle().fill(Theme.accent).frame(height: 3)
        }
    }

    private func statusColor(_ kind: EngineStatus.Kind) -> Color {
        switch kind {
        case .running: Theme.accent
        case .starting: Theme.warn
        case .backendUnreachable, .failed: Theme.error
        case .stopped, .notConfigured: .secondary
        }
    }
}

private struct Footer: View {
    @Bindable var model: AppModel
    var body: some View {
        VStack(spacing: 0) {
            Divider()
            HStack(spacing: 10) {
                message
                Spacer()
                Button("Revert") { model.revert() }.disabled(!model.hasUnsavedChanges)
                Button("Save") { Task { await model.save() } }
                    .buttonStyle(.borderedProminent).keyboardShortcut("s", modifiers: .command)
                    .disabled(!model.hasUnsavedChanges)
            }
            .padding(.horizontal, 20).padding(.vertical, 10)
        }
    }

    @ViewBuilder private var message: some View {
        switch model.saveMessage {
        case .failed(let problems):
            StatusLabel(level: .error, text: problems.joined(separator: " "))
        case .saved where !model.hasUnsavedChanges:
            StatusLabel(level: .ok, text: "Saved and applied")
        default:
            if model.hasUnsavedChanges { StatusLabel(level: .warn, text: "Unsaved changes") } else { EmptyView() }
        }
    }
}

// MARK: Providers

private struct ProvidersSection: View {
    @Bindable var model: AppModel
    var body: some View {
        SettingsSection("Providers", subtitle: "where the models run") {
            ForEach($model.draft.backends) { $backend in
                ProviderRow(model: model, backend: $backend)
            }
            HStack {
                Button { model.addProvider() } label: { Label("Add provider", systemImage: "plus") }
                if model.draft.backends.isEmpty {
                    Text("Add the server your models run on (vLLM, SGLang, Flare, or an Anthropic endpoint).")
                        .font(.system(size: 11)).foregroundStyle(.secondary)
                }
            }
        }
    }
}

private struct ProviderRow: View {
    var model: AppModel
    @Binding var backend: Backend
    @State private var name = ""
    @State private var url = ""
    @State private var key = ""
    @FocusState private var nameFocused: Bool

    var body: some View {
        Well {
            HStack(spacing: 8) {
                TextField("Name", text: $name).focused($nameFocused).frame(width: 140)
                    .onSubmit(commitName)
                    .onChange(of: nameFocused) { if !nameFocused { commitName() } }
                Picker("Kind", selection: $backend.kind) {
                    Text("vLLM · SGLang · Flare").tag(BackendKind.openAICompatible)
                    Text("Anthropic").tag(BackendKind.anthropic)
                }
                .labelsHidden().frame(maxWidth: .infinity)
                Button(role: .destructive) { model.removeProvider(backend.id) } label: { Image(systemName: "trash") }
                    .buttonStyle(.borderless).help("Remove this provider")
            }
            TextField("Base URL, e.g. http://192.168.13.33:8000/v1", text: $url)
                .font(.system(size: 12, design: .monospaced))
                .onChange(of: url) { if let u = Self.parse(url) { backend.baseURL = u } }
                .overlay(alignment: .trailing) {
                    if Self.parse(url) == nil { Image(systemName: "exclamationmark.circle.fill").foregroundStyle(Theme.error).padding(.trailing, 6) }
                }
            HStack(spacing: 8) {
                SecureField(model.hasAPIKey(forProvider: backend.id) && key.isEmpty ? "API key saved in Keychain" : "API key (optional)", text: $key)
                    .onChange(of: key) { if !key.isEmpty { model.setAPIKey(key, forProvider: backend.id) } }
                if model.hasAPIKey(forProvider: backend.id) {
                    Button("Remove key") { key = ""; model.setAPIKey("", forProvider: backend.id) }.buttonStyle(.borderless)
                }
            }
            HStack(spacing: 8) {
                Button(model.probing.contains(backend.id) ? "Testing…" : "Test") { Task { await model.probe(backend.id) } }
                    .disabled(model.probing.contains(backend.id) || Self.parse(url) == nil)
                ProbeSummary(probe: model.probes[backend.id])
                Spacer()
            }
        }
        .onAppear { name = backend.id; url = backend.baseURL.absoluteString }
        .onChange(of: backend.id) { name = backend.id }
        .onChange(of: backend.baseURL) { if Self.parse(url) != backend.baseURL { url = backend.baseURL.absoluteString } }
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

struct ProbeSummary: View {
    var probe: BackendProbe?
    var body: some View {
        if let probe {
            if probe.reachable {
                StatusLabel(level: .ok, text: "Reachable · " + Self.parts(probe).joined(separator: " · "))
            } else {
                StatusLabel(level: .error, text: probe.error ?? "Unreachable")
            }
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

// MARK: Models

private struct ModelsSection: View {
    @Bindable var model: AppModel
    @State private var advanced: Bool
    init(model: AppModel, expanded: Bool = false) { self.model = model; _advanced = State(initialValue: expanded) }

    var body: some View {
        SettingsSection("Models", subtitle: "Claude Code's Opus and Sonnet slots") {
            TierEditor(model: model, slot: .main, title: "Sonnet", hint: "Claude Code's default model, background tasks and Codex unless overridden.")
            TierEditor(model: model, slot: .opus, title: "Opus", hint: "Used when Claude Code asks for Opus (plan mode, /model).")
            DisclosureGroup("Advanced", isExpanded: $advanced) {
                VStack(alignment: .leading, spacing: 8) {
                    RolePicker(model: model, slot: .small, label: "Small (Haiku)", none: "Same as Sonnet",
                               hint: "Quick background work: titles, summaries.")
                    RolePicker(model: model, slot: .vision, label: "Vision", none: "None",
                               hint: "Describes images when the model you use has no vision. Optional.")
                    RolePicker(model: model, slot: .codex, label: "Codex", none: "Same as Sonnet",
                               hint: "The model Codex uses.")
                }
                .padding(.top, 6)
            }
            .font(.system(size: 12))
        }
    }
}

private struct TierEditor: View {
    @Bindable var model: AppModel
    var slot: RoleSlot
    var title: String
    var hint: String
    @State private var chosenProvider: String?

    private var choice: ModelChoice? { model.draft.choice(for: slot) }
    private var providerID: String {
        choice?.backendID ?? chosenProvider ?? (slot == .main ? model.draft.backends.first?.id : nil) ?? ""
    }
    private var probedModels: [ProbedModel] { model.probes[providerID]?.models ?? [] }

    var body: some View {
        Well {
            HStack(spacing: 6) {
                Text(title).font(.system(size: 13, weight: .semibold))
                Text(hint).font(.system(size: 11)).foregroundStyle(.secondary).lineLimit(2)
            }
            FieldRow("Provider") {
                Picker("Provider", selection: Binding(get: { providerID }, set: setProvider)) {
                    if slot != .main { Text("Same as Sonnet").tag("") }
                    else if providerID.isEmpty { Text("Choose…").tag("") }
                    ForEach(model.draft.backends) { Text($0.id).tag($0.id) }
                }
                .labelsHidden()
            }
            if !providerID.isEmpty {
                FieldRow("Model") {
                    if probedModels.isEmpty {
                        TextField("Model name on this provider", text: Binding(
                            get: { choice?.upstream ?? "" },
                            set: { model.setRole(slot, to: $0.isEmpty ? nil : ModelChoice(backendID: providerID, upstream: $0)) }))
                    } else {
                        Picker("Model", selection: Binding(get: { choice?.upstream ?? "" },
                                                           set: { model.setRole(slot, to: $0.isEmpty ? nil : ModelChoice(backendID: providerID, upstream: $0)) })) {
                            if choice == nil { Text("Choose…").tag("") }
                            if let c = choice, !probedModels.contains(where: { $0.upstreamModel == c.upstream }) { Text(c.upstream).tag(c.upstream) }
                            ForEach(probedModels, id: \.upstreamModel) { Text(label($0)).tag($0.upstreamModel) }
                        }
                        .labelsHidden()
                    }
                    Button { Task { await model.probe(providerID) } } label: { Image(systemName: "arrow.clockwise") }
                        .buttonStyle(.borderless).help("Refresh the model list from the provider")
                        .disabled(model.probing.contains(providerID))
                }
                if let m = model.draft.model(model.draft.modelID(for: slot)) {
                    FieldRow("Context") {
                        TextField("auto", value: contextBinding(m.id), format: .number.grouping(.never))
                            .frame(width: 110).multilineTextAlignment(.trailing)
                        Text("tokens").font(.system(size: 11)).foregroundStyle(.secondary)
                        Spacer()
                        ModelFacts(model: model, choice: ModelChoice(backendID: m.backendID, upstream: m.upstreamModel), configured: m)
                    }
                }
            }
        }
        .task(id: providerID) { if !providerID.isEmpty { await model.probeIfNeeded(providerID) } }
    }

    private func label(_ m: ProbedModel) -> String {
        var s = m.upstreamModel
        if let c = m.contextWindow { s += "  ·  \(c / 1000)k" }
        if m.logprobs == true { s += "  ·  logprobs" }
        return s
    }

    private func setProvider(_ id: String) {
        guard id != providerID else { return }
        chosenProvider = id.isEmpty ? nil : id
        model.setRole(slot, to: nil)
    }

    private func contextBinding(_ id: String) -> Binding<Int?> {
        Binding(get: { model.draft.model(id)?.contextWindow },
                set: { v in if let i = model.draft.models.firstIndex(where: { $0.id == id }) { model.draft.models[i].contextWindow = (v ?? 0) > 0 ? v : nil } })
    }
}

/// logprobs / vision facts for a model, from the config or the last probe.
private struct ModelFacts: View {
    var model: AppModel
    var choice: ModelChoice
    var configured: Model?
    var body: some View {
        let logprobs = configured?.logprobs ?? model.probedModel(choice)?.logprobs
        HStack(spacing: 8) {
            if let logprobs { StatusLabel(level: logprobs ? .ok : .idle, text: logprobs ? "logprobs" : "no logprobs") }
            if configured?.vision == true { StatusLabel(level: .ok, text: "vision") }
        }
    }
}

private struct RolePicker: View {
    @Bindable var model: AppModel
    var slot: RoleSlot
    var label: String
    var none: String
    var hint: String

    var body: some View {
        FieldRow(label) {
            Picker(label, selection: Binding(
                get: { model.draft.choice(for: slot)?.tag ?? "" },
                set: { model.setRole(slot, to: ModelChoice(tag: $0)) })) {
                Text(none).tag("")
                ForEach(model.choices(), id: \.self) { c in Text("\(c.upstream)  ·  \(c.backendID)").tag(c.tag) }
            }
            .labelsHidden()
        }
        .help(hint)
        Text(hint).font(.system(size: 11)).foregroundStyle(.secondary).padding(.leading, 104)
    }
}

// MARK: Auto mode

private struct AutoModeSection: View {
    @Bindable var model: AppModel

    private var decisionChoice: ModelChoice? { model.draft.choice(for: .decision) ?? model.draft.choice(for: .main) }

    var body: some View {
        SettingsSection("Auto mode", subtitle: "the safety check behind “auto” permission mode") {
            RolePicker(model: model, slot: .decision, label: "Decision model", none: "Same as Sonnet",
                       hint: "Needs logprobs: use a vLLM or SGLang model. Flare has none yet.")
            HStack(spacing: 8) {
                Spacer().frame(width: 104)
                status
                Spacer()
                if let c = decisionChoice {
                    Button("Check") { Task { await model.probe(c.backendID) } }
                        .disabled(model.probing.contains(c.backendID))
                }
            }
        }
    }

    @ViewBuilder private var status: some View {
        if let c = decisionChoice {
            let configured = model.draft.model(model.draft.modelID(for: .decision) ?? model.draft.roles.main)
            switch configured?.logprobs ?? model.probedModel(c)?.logprobs {
            case true?: StatusLabel(level: .ok, text: "\(c.upstream) returns logprobs: fast single-token decisions")
            case false?: StatusLabel(level: .warn, text: "\(c.upstream) has no logprobs: decisions fall back to slower generated answers")
            case nil: StatusLabel(level: .idle, text: "Not checked yet")
            }
        } else {
            StatusLabel(level: .idle, text: "Choose a Sonnet model first")
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
        SettingsSection("Documents", subtitle: "PDFs and scans the model can read") {
            FieldRow("PaddleOCR") {
                TextField("http://host:8002/layout-parsing (optional)", text: $url)
                    .font(.system(size: 12, design: .monospaced))
                    .onChange(of: url) {
                        let t = url.trimmingCharacters(in: .whitespaces)
                        model.draft.documents.paddleOCR = t.isEmpty ? nil : ProviderRow.parse(t)
                        result = nil
                    }
                Button(testing ? "Testing…" : "Test") { test() }.disabled(testing || model.draft.documents.paddleOCR == nil)
            }
            if let result { StatusLabel(level: result.ok ? .ok : .error, text: result.detail).padding(.leading, 104) }
            FieldRow("Fallback") {
                Picker("Fallback", selection: $model.draft.documents.local) {
                    Text("Apple Vision").tag(LocalExtraction.appleVision)
                    Text("Text only").tag(LocalExtraction.textOnly)
                    Text("Off").tag(LocalExtraction.off)
                }
                .pickerStyle(.segmented).labelsHidden()
            }
            Text("Used when PaddleOCR isn't set or can't be reached. Apple Vision reads scanned pages on this Mac; Text only uses the PDF's own text.")
                .font(.system(size: 11)).foregroundStyle(.secondary).padding(.leading, 104)
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
        SettingsSection("Web search", subtitle: "for Claude Code's WebSearch and Codex") {
            FieldRow("Provider") {
                Picker("Provider", selection: provider) {
                    Text("Off").tag(SearchProvider?.none)
                    Text("Tavily").tag(SearchProvider?.some(.tavily))
                    Text("Brave").tag(SearchProvider?.some(.brave))
                }
                .pickerStyle(.segmented).labelsHidden()
            }
            if model.draft.webSearch.provider != nil {
                FieldRow("API key") {
                    SecureField(model.hasSearchKey() && key.isEmpty ? "Saved in Keychain" : "API key", text: $key)
                        .onChange(of: key) { if !key.isEmpty { model.setSearchKey(key) } }
                    Button(testing ? "Testing…" : "Test") { test() }.disabled(testing || !model.hasSearchKey())
                }
                if let result { StatusLabel(level: result.ok ? .ok : .error, text: result.detail).padding(.leading, 104) }
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
        SettingsSection("Launch", subtitle: "applies immediately") {
            FieldRow("Terminal") {
                Picker("Terminal", selection: Binding(get: { model.launchSettings.terminal }, set: { model.launchSettings.terminal = $0; model.saveLaunchSettings() })) {
                    ForEach(TerminalPreference.allCases, id: \.self) { Text($0.label).tag($0) }
                }
                .labelsHidden()
            }
            FieldRow("Claude Code") {
                Picker("Permission mode", selection: Binding(get: { model.launchSettings.permissionMode }, set: { model.launchSettings.permissionMode = $0; model.saveLaunchSettings() })) {
                    ForEach(PermissionMode.all, id: \.self) { Text($0 == "auto" ? "auto (recommended)" : $0).tag($0) }
                }
                .labelsHidden()
            }
            FieldRow("Local port") {
                TextField("18765", value: $model.draft.port, format: .number.grouping(.never))
                    .frame(width: 80).multilineTextAlignment(.trailing)
                Text("saved with Save; changing it restarts the engine").font(.system(size: 11)).foregroundStyle(.secondary)
            }
            FieldRow("") {
                Toggle("Open code-link at login", isOn: Binding(get: { model.launchAtLogin }, set: { model.setLaunchAtLogin($0) }))
            }
        }
    }
}

// MARK: Activity

private struct ActivitySection: View {
    var store: ActivityStore
    @State private var open: Bool
    init(store: ActivityStore, expanded: Bool = false) { self.store = store; _open = State(initialValue: expanded) }

    var body: some View {
        DisclosureGroup(isExpanded: $open) {
            VStack(alignment: .leading, spacing: 4) {
                if store.entries.isEmpty {
                    Text("Nothing yet. Requests, auto-mode decisions, searches and documents appear here.")
                        .font(.system(size: 11)).foregroundStyle(.secondary)
                } else {
                    ScrollView {
                        LazyVStack(alignment: .leading, spacing: 2) {
                            ForEach(store.entries.prefix(100)) { e in
                                HStack(alignment: .firstTextBaseline, spacing: 6) {
                                    Text(e.date, format: .dateTime.hour().minute().second())
                                        .font(.system(size: 10, design: .monospaced)).foregroundStyle(.secondary)
                                    Circle().fill(color(e.level)).frame(width: 5, height: 5)
                                    Text(e.text).font(.system(size: 11)).lineLimit(1).truncationMode(.middle)
                                }
                            }
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    .frame(height: 140)
                    HStack { Spacer(); Button("Clear") { store.clear() }.buttonStyle(.borderless).font(.system(size: 11)) }
                }
            }
            .padding(.top, 6)
        } label: {
            Text("RECENT ACTIVITY").font(.system(size: 11, weight: .semibold)).tracking(0.8)
        }
    }

    private func color(_ l: ActivityEntry.Level) -> Color { l == .error ? Theme.error : l == .warning ? Theme.warn : .secondary.opacity(0.6) }
}

// MARK: Previews

@MainActor private func previewModel() -> AppModel {
    let m = AppModel(environment: .isolated())
    var c = EngineConfig()
    c.backends = [Backend(id: "cn3-vllm", baseURL: URL(string: "http://192.168.13.33:8000/v1")!),
                  Backend(id: "anthropic", kind: .anthropic, baseURL: URL(string: "https://api.anthropic.com")!)]
    c.models = [Model(id: "GLM-5.3", backendID: "cn3-vllm", upstreamModel: "GLM-5.3", contextWindow: 1_048_576, logprobs: true),
                Model(id: "GLM-5.3-Flash", backendID: "cn3-vllm", upstreamModel: "GLM-5.3-Flash", contextWindow: 262_144, logprobs: true)]
    c.roles = Roles(main: "GLM-5.3-Flash", opus: "GLM-5.3")
    c.documents.paddleOCR = URL(string: "http://192.168.13.30:8002/layout-parsing")
    c.webSearch = WebSearchConfig(provider: .tavily, keyRef: "websearch-tavily")
    m.draft = c
    return m
}
#Preview("Settings · light") { SettingsView(model: previewModel()).preferredColorScheme(.light) }
#Preview("Settings · dark") { SettingsView(model: previewModel()).preferredColorScheme(.dark) }
