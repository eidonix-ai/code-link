import Foundation

/// POSIX shell quoting for the launch scripts and the copied commands.
enum ShellQuote {
    /// Always single-quoted: `it's` becomes `'it'"'"'s'`.
    static func quote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\"'\"'") + "'"
    }

    private static let safe = Set("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_@%+=:,./-")

    /// Left bare when every character is shell-safe, quoted otherwise.
    static func quoteIfNeeded(_ value: String) -> String {
        if !value.isEmpty, value.allSatisfy({ safe.contains($0) }) { return value }
        return quote(value)
    }

    /// A TOML basic string (for Codex `-c key="value"` overrides).
    static func tomlString(_ value: String) -> String {
        var out = "\""
        for ch in value.unicodeScalars {
            switch ch {
            case "\\": out += "\\\\"
            case "\"": out += "\\\""
            case "\n": out += "\\n"
            case "\t": out += "\\t"
            default: out.unicodeScalars.append(ch)
            }
        }
        return out + "\""
    }
}
