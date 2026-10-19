import SwiftUI
import CodeLinkEngine

@main
struct CodeLinkApp: App {
    var body: some Scene {
        MenuBarExtra("code-link", systemImage: "link") {
            Text("code-link")
            Divider()
            Button("Quit") { NSApplication.shared.terminate(nil) }
        }
    }
}
