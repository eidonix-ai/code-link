import AppKit
import SwiftUI

/// The Settings window, managed directly. A menu-bar-only (LSUIElement) app
/// can't rely on SwiftUI's Settings scene: `showSettingsWindow:` no longer
/// opens it, and an accessory app's windows don't come to the front. While
/// the window is open the app is a regular app (front, in ⌘-Tab); closing
/// it returns the app to the menu bar.
@MainActor
final class SettingsWindow: NSObject, NSWindowDelegate {
    private var window: NSWindow?
    private let content: () -> AnyView

    init(content: @escaping () -> AnyView) { self.content = content }

    func show() {
        if window == nil {
            let w = NSWindow(contentViewController: NSHostingController(rootView: content()))
            w.title = "code-link Settings"
            w.styleMask = [.titled, .closable, .miniaturizable, .resizable]
            w.isReleasedWhenClosed = false
            w.setFrameAutosaveName("CodeLinkSettings")
            if w.frame.width < 400 { w.setContentSize(NSSize(width: 560, height: 680)) }
            w.delegate = self
            w.center()
            window = w
        }
        NSApp.setActivationPolicy(.regular)
        NSApp.activate()
        window?.makeKeyAndOrderFront(nil)
    }

    func windowWillClose(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
    }
}
