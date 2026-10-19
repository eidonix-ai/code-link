import SwiftUI
import AppKit

/// Eidonix teal on system backgrounds (docs: EIDONIX-UI-STYLE). Light mode uses
/// a deeper teal where the colour is text or a thin line on white.
enum Theme {
    /// Brand teal #00C2C7 on dark, #009EA3 on light.
    static let accent = Color(nsColor: dynamic(light: 0x009EA3, dark: 0x00C2C7))
    static let ok = Color(nsColor: dynamic(light: 0x1E8E5A, dark: 0x34C77B))
    static let warn = Color(nsColor: dynamic(light: 0xB87700, dark: 0xE0A63A))
    static let error = Color(nsColor: dynamic(light: 0xE10600, dark: 0xFF5A52))
    static let hairline = Color.primary.opacity(0.12)
    static let well = Color.primary.opacity(0.045)

    static func dynamic(light: UInt32, dark: UInt32) -> NSColor {
        NSColor(name: nil) { appearance in
            appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua ? rgb(dark) : rgb(light)
        }
    }

    private static func rgb(_ v: UInt32) -> NSColor {
        NSColor(srgbRed: CGFloat((v >> 16) & 0xFF) / 255, green: CGFloat((v >> 8) & 0xFF) / 255, blue: CGFloat(v & 0xFF) / 255, alpha: 1)
    }
}

/// An uppercase section header with the brand-teal rule under it.
struct SettingsSection<Content: View>: View {
    var title: String
    var subtitle: String?
    @ViewBuilder var content: Content

    init(_ title: String, subtitle: String? = nil, @ViewBuilder content: () -> Content) {
        self.title = title; self.subtitle = subtitle; self.content = content()
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            VStack(alignment: .leading, spacing: 3) {
                HStack(alignment: .firstTextBaseline) {
                    Text(title.uppercased()).font(.system(size: 11, weight: .semibold)).tracking(0.8)
                    if let subtitle { Text(subtitle).font(.system(size: 11)).foregroundStyle(.secondary) }
                }
                Rectangle().fill(Theme.accent).frame(height: 2)
            }
            content
        }
    }
}

/// A label column + a control, in the dense settings style.
struct FieldRow<Content: View>: View {
    var label: String
    @ViewBuilder var content: Content
    init(_ label: String, @ViewBuilder content: () -> Content) { self.label = label; self.content = content() }
    var body: some View {
        HStack(alignment: .center, spacing: 8) {
            Text(label).font(.system(size: 12)).foregroundStyle(.secondary).frame(width: 96, alignment: .trailing)
            content
        }
    }
}

/// A quiet bordered well for one provider or tier.
struct Well<Content: View>: View {
    @ViewBuilder var content: Content
    var body: some View {
        VStack(alignment: .leading, spacing: 6) { content }
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Theme.well, in: RoundedRectangle(cornerRadius: 6))
            .overlay(RoundedRectangle(cornerRadius: 6).stroke(Theme.hairline))
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
        Label { Text(text).font(.system(size: 11)).foregroundStyle(.secondary) } icon: {
            Image(systemName: level.symbol).foregroundStyle(level.color).font(.system(size: 11))
        }
        .labelStyle(.titleAndIcon)
    }
}
