import SwiftUI
import AppKit
import CodeLinkEngine

struct MenuContent: View {
    @Bindable var model: AppModel

    var body: some View {
        let status = model.status
        Text(status.line)
        Divider()

        Button("Launch Claude Code…") { model.chooseFolderAndLaunch(.claudeCode) }
            .disabled(!model.config.isConfigured || model.isLaunching)
        Button("Launch Codex…") { model.chooseFolderAndLaunch(.codex) }
            .disabled(!model.config.isConfigured || model.isLaunching)

        let recents = model.launchSettings.recentFolders.existing()
        Menu("Recent Folders") {
            if recents.isEmpty {
                Text("No folders yet")
            } else {
                Section("Claude Code") {
                    ForEach(recents, id: \.self) { path in
                        Button(RecentFolders.title(for: path)) { Task { await model.launch(.claudeCode, folder: path) } }
                    }
                }
                Section("Codex") {
                    ForEach(recents, id: \.self) { path in
                        Button(RecentFolders.title(for: path)) { Task { await model.launch(.codex, folder: path) } }
                    }
                }
                Divider()
                Button("Clear Recent Folders") { model.clearRecentFolders() }
            }
        }
        .disabled(!model.config.isConfigured || model.isLaunching)

        Divider()

        let menu = MenuModels.make(config: model.config)
        if menu.groups.isEmpty {
            Text("No models enabled — open Settings")
        } else {
            Menu("Sonnet: \(menu.sonnetTitle)") { slotItems(.main, menu) }.disabled(model.hasUnsavedChanges)
            Menu("Opus: \(menu.opusTitle)") { slotItems(.opus, menu) }.disabled(model.hasUnsavedChanges)
            Text(model.hasUnsavedChanges ? "Save or revert Settings to re-route" : "New sessions use this; running ones keep theirs")
        }

        Button("Copy Claude Code Command") { model.copyCommand(.claudeCode) }.disabled(!model.config.isConfigured)
        Button("Copy Codex Command") { model.copyCommand(.codex) }.disabled(!model.config.isConfigured)

        Divider()

        if model.config.isConfigured {
            Button(model.isRunning ? "Stop Engine" : "Start Engine") { Task { await model.toggleEngine() } }
        }
        Button("Settings…") { model.openSettings() }.keyboardShortcut(",")
        Button("Quit code-link") { NSApplication.shared.terminate(nil) }.keyboardShortcut("q")
    }
}

extension MenuContent {
    /// Every enabled model grouped by provider, a checkmark on the current one.
    @ViewBuilder func slotItems(_ slot: RoleSlot, _ menu: MenuModels) -> some View {
        if slot == .opus {
            Button { Task { await model.switchRole(.opus, to: nil) } } label: {
                if menu.opusID == nil { Label("Same as Sonnet", systemImage: "checkmark") } else { Text("Same as Sonnet") }
            }
            Divider()
        }
        let current = slot == .main ? menu.sonnetID : menu.opusID
        ForEach(menu.groups) { g in
            Section(g.backendID) {
                ForEach(g.items) { item in
                    Button { Task { await model.switchRole(slot, to: item.modelID) } } label: {
                        if item.modelID == current { Label(item.title, systemImage: "checkmark") } else { Text(item.title) }
                    }
                }
            }
        }
    }
}

/// What the Sonnet and Opus submenus show.
struct MenuModels: Equatable {
    var groups: [ChoiceGroup]
    var sonnetID: String?
    var opusID: String?
    var sonnetTitle: String { sonnetID ?? "none" }
    var opusTitle: String { opusID ?? "same as Sonnet" }

    static func make(config: EngineConfig) -> MenuModels {
        MenuModels(groups: RoutingChoices.groups(config: config, enabledOnly: true),
                   sonnetID: config.roles.main.isEmpty ? nil : config.roles.main, opusID: config.roles.opus)
    }
}

/// The menu bar glyph: the link symbol, plus a status dot when there is
/// something to say (teal = running, amber = starting, red = trouble).
enum MenuBarIcon {
    @MainActor static func image(for kind: EngineStatus.Kind) -> NSImage {
        let dot: NSColor? = switch kind {
        case .running: NSColor(srgbRed: 0, green: 0.76, blue: 0.78, alpha: 1)
        case .starting: NSColor(srgbRed: 0.88, green: 0.65, blue: 0.23, alpha: 1)
        case .backendUnreachable, .failed: NSColor(srgbRed: 1, green: 0.35, blue: 0.32, alpha: 1)
        case .stopped, .notConfigured: nil
        }
        let base = NSImage(systemSymbolName: kind == .stopped || kind == .notConfigured ? "link.circle" : "link.circle.fill",
                           accessibilityDescription: "code-link")!
        guard let dot else { base.isTemplate = true; return base }
        // Template images ignore colour, so a dot needs a drawn image; the glyph follows the menu bar's appearance.
        let size = NSSize(width: 20, height: 18)
        let image = NSImage(size: size, flipped: false) { rect in
            let cfg = NSImage.SymbolConfiguration(pointSize: 14, weight: .regular).applying(.init(paletteColors: [.labelColor]))
            base.withSymbolConfiguration(cfg)?.draw(in: NSRect(x: 0, y: 1, width: 16, height: 16))
            let d: CGFloat = 7
            let dotRect = NSRect(x: rect.maxX - d, y: 0, width: d, height: d)
            NSColor.black.withAlphaComponent(0.0).setFill()
            dot.setFill()
            NSBezierPath(ovalIn: dotRect).fill()
            return true
        }
        image.isTemplate = false
        return image
    }
}
