import Foundation
import Security
import Synchronization
import CodeLinkEngine

/// Where secret values live. The app uses the Keychain; tests use memory.
protocol SecretBackend: Sendable {
    func get(_ account: String) -> String?
    func set(_ value: String, account: String) throws
    func delete(_ account: String)
}

struct KeychainError: LocalizedError {
    var status: OSStatus
    var errorDescription: String? {
        "Keychain error \(status): \((SecCopyErrorMessageString(status, nil) as String?) ?? "unknown")"
    }
}

/// Generic-password items under one service (`code-link`).
struct KeychainStore: SecretBackend {
    var service = "code-link"

    private func base(_ account: String) -> [String: Any] {
        [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: account]
    }

    func get(_ account: String) -> String? {
        var q = base(account)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        guard SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess, let data = out as? Data else { return nil }
        return String(data: data, encoding: .utf8)
    }

    func set(_ value: String, account: String) throws {
        let data = Data(value.utf8)
        let update = SecItemUpdate(base(account) as CFDictionary, [kSecValueData as String: data] as CFDictionary)
        if update == errSecSuccess { return }
        guard update == errSecItemNotFound else { throw KeychainError(status: update) }
        var add = base(account)
        add[kSecValueData as String] = data
        add[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlock
        let status = SecItemAdd(add as CFDictionary, nil)
        guard status == errSecSuccess else { throw KeychainError(status: status) }
    }

    func delete(_ account: String) { SecItemDelete(base(account) as CFDictionary) }
}

final class MemorySecretBackend: SecretBackend {
    private let items = Mutex<[String: String]>([:])
    init() {}
    func get(_ account: String) -> String? { items.withLock { $0[account] } }
    func set(_ value: String, account: String) throws { items.withLock { $0[account] = value } }
    func delete(_ account: String) { items.withLock { _ = $0.removeValue(forKey: account) } }
}

/// The engine's `SecretStore`: the backend plus values typed in Settings that
/// aren't saved yet (so Test works before Save). Saving commits them.
final class AppSecrets: SecretStore, Sendable {
    private let backend: any SecretBackend
    /// ref -> pending value; an empty value means "delete on save".
    private let pending = Mutex<[String: String]>([:])

    init(backend: any SecretBackend) { self.backend = backend }

    func secret(_ ref: String) -> String? {
        if let p = pending.withLock({ $0[ref] }) { return p.isEmpty ? nil : p }
        return backend.get(ref)
    }

    func setPending(_ value: String, ref: String) { pending.withLock { $0[ref] = value } }
    func pendingValue(_ ref: String) -> String? { pending.withLock { $0[ref] } }
    var hasPending: Bool { pending.withLock { !$0.isEmpty } }
    func discardPending() { pending.withLock { $0.removeAll() } }
    func hasSecret(_ ref: String) -> Bool { secret(ref) != nil }

    /// Writes pending values to the backend (deleting the empty ones).
    func commit() throws {
        let items = pending.withLock { $0 }
        for (ref, value) in items {
            if value.isEmpty { backend.delete(ref) } else { try backend.set(value, account: ref) }
        }
        pending.withLock { $0.removeAll() }
    }

    func delete(_ ref: String) { pending.withLock { _ = $0.removeValue(forKey: ref) }; backend.delete(ref) }
}

/// The engine access token: 32 random bytes, base64url, generated once. It
/// only opens the local engine and is handed to every session in its
/// environment, so it lives in a 0600 file next to the config rather than the
/// Keychain (which asks for a password whenever the app's signature changes).
enum AccessToken {
    static func make() -> String {
        var bytes = [UInt8](repeating: 0, count: 32)
        if SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes) != errSecSuccess {
            bytes = (0..<32).map { _ in UInt8.random(in: .min ... .max) }
        }
        return Data(bytes).base64EncodedString()
            .replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }

    static func loadOrCreate(at url: URL) -> String {
        if let data = try? Data(contentsOf: url) {
            let t = String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            if !t.isEmpty { return t }
        }
        let t = make()
        let fm = FileManager.default
        try? fm.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true,
                                attributes: [.posixPermissions: 0o700])
        if fm.createFile(atPath: url.path, contents: Data(t.utf8), attributes: [.posixPermissions: 0o600]) {
            try? fm.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
        }
        return t
    }
}
