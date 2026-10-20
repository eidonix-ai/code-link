import SwiftUI
import AppKit

/// Eidonix teal as the control accent only (EIDONIX-UI-STYLE); everything else
/// is system colours. Light mode uses a deeper teal for contrast on white.
enum Theme {
    /// Brand teal #00C2C7 on dark, #009EA3 on light.
    static let accent = Color(nsColor: dynamic(light: 0x009EA3, dark: 0x00C2C7))
    static let ok = Color(nsColor: dynamic(light: 0x1E8E5A, dark: 0x34C77B))
    static let warn = Color(nsColor: dynamic(light: 0xB87700, dark: 0xE0A63A))
    static let error = Color(nsColor: dynamic(light: 0xE10600, dark: 0xFF5A52))

    static func dynamic(light: UInt32, dark: UInt32) -> NSColor {
        NSColor(name: nil) { appearance in
            appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua ? rgb(dark) : rgb(light)
        }
    }

    private static func rgb(_ v: UInt32) -> NSColor {
        NSColor(srgbRed: CGFloat((v >> 16) & 0xFF) / 255, green: CGFloat((v >> 8) & 0xFF) / 255, blue: CGFloat(v & 0xFF) / 255, alpha: 1)
    }
}

enum StatusLevel { case ok, warn, error, idle
    var color: Color { switch self { case .ok: Theme.ok; case .warn: Theme.warn; case .error: Theme.error; case .idle: .secondary } }
    var symbol: String { switch self { case .ok: "checkmark.circle.fill"; case .warn: "exclamationmark.triangle.fill"; case .error: "xmark.circle.fill"; case .idle: "circle.dotted" } }
}

/// A small status glyph with text.
struct StatusLabel: View {
    var level: StatusLevel
    var text: String
    var body: some View {
        Label { Text(text).foregroundStyle(.secondary) } icon: {
            Image(systemName: level.symbol).foregroundStyle(level.color)
        }
        .labelStyle(.titleAndIcon)
    }
}
