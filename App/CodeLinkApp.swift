import SwiftUI
import CodeLinkEngine

/// Opening code-link again while it runs (Finder, Spotlight, `open`) shows
/// Settings — the usual Mac behaviour for an app with no Dock icon.
final class AppDelegate: NSObject, NSApplicationDelegate {
    static let openSettings = Notification.Name("CodeLinkOpenSettings")
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        NotificationCenter.default.post(name: Self.openSettings, object: nil)
        return true
    }
}

@main
struct CodeLinkApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    @State private var model = AppModel()

    var body: some Scene {
        MenuBarExtra {
            MenuContent(model: model)
        } label: {
            Image(nsImage: MenuBarIcon.image(for: model.status.kind))
                .task { await model.bootstrap() }
                .onReceive(NotificationCenter.default.publisher(for: AppDelegate.openSettings)) { _ in model.openSettings() }
        }
        .menuBarExtraStyle(.menu)
        // Settings is an AppKit-managed window (SettingsWindow): SwiftUI's
        // Settings scene can't be opened reliably from a menu-bar-only app.
    }
}
