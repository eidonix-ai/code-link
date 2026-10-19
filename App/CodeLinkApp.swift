import SwiftUI
import CodeLinkEngine

@main
struct CodeLinkApp: App {
    @State private var model = AppModel()

    var body: some Scene {
        MenuBarExtra {
            MenuContent(model: model)
        } label: {
            Image(nsImage: MenuBarIcon.image(for: model.status.kind))
                .task { await model.bootstrap() }
        }
        .menuBarExtraStyle(.menu)

        Settings {
            SettingsView(model: model)
        }
        .windowResizability(.contentSize)
    }
}
