import Foundation
import Darwin

enum IDs {
    private static let alphabet = Array("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
    /// `prefix` + 24 random alphanumerics.
    static func make(_ prefix: String) -> String {
        var g = SystemRandomNumberGenerator()
        return prefix + String((0..<24).map { _ in alphabet.randomElement(using: &g)! })
    }
}

enum NetworkAddress {
    /// Whether `host` is, or resolves to, a loopback, private, link-local or
    /// otherwise non-public address. Unresolvable hosts count as private.
    static func isPrivateHost(_ host: String) -> Bool {
        let h = host.trimmingCharacters(in: CharacterSet(charactersIn: "[]")).lowercased()
        if h == "localhost" || h.hasSuffix(".localhost") || h.hasSuffix(".local") { return true }
        var hints = addrinfo()
        hints.ai_family = AF_UNSPEC
        hints.ai_socktype = SOCK_STREAM
        var res: UnsafeMutablePointer<addrinfo>?
        guard getaddrinfo(h, nil, &hints, &res) == 0, let first = res else { return true }
        defer { freeaddrinfo(first) }
        var p: UnsafeMutablePointer<addrinfo>? = first
        while let ai = p {
            if let sa = ai.pointee.ai_addr {
                if sa.pointee.sa_family == sa_family_t(AF_INET) {
                    let a = sa.withMemoryRebound(to: sockaddr_in.self, capacity: 1) { UInt32(bigEndian: $0.pointee.sin_addr.s_addr) }
                    if isPrivateV4(a) { return true }
                } else if sa.pointee.sa_family == sa_family_t(AF_INET6) {
                    let bytes = sa.withMemoryRebound(to: sockaddr_in6.self, capacity: 1) { p6 in
                        withUnsafeBytes(of: p6.pointee.sin6_addr) { Array($0) }
                    }
                    if isPrivateV6(bytes) { return true }
                }
            }
            p = ai.pointee.ai_next
        }
        return false
    }

    static func isPrivateV4(_ a: UInt32) -> Bool {
        let b0 = a >> 24, b1 = (a >> 16) & 0xFF
        return b0 == 0 || b0 == 10 || b0 == 127 || (b0 == 169 && b1 == 254) || (b0 == 172 && (16...31).contains(b1))
            || (b0 == 192 && b1 == 168) || (b0 == 100 && (64...127).contains(b1)) || b0 >= 224
    }

    static func isPrivateV6(_ b: [UInt8]) -> Bool {
        guard b.count == 16 else { return true }
        if b[0..<15].allSatisfy({ $0 == 0 }) { return true }                 // :: and ::1
        if b[0] == 0xFE && (b[1] & 0xC0) == 0x80 { return true }             // fe80::/10
        if (b[0] & 0xFE) == 0xFC { return true }                             // fc00::/7
        if b[0] == 0xFF { return true }                                      // multicast
        if b[0..<10].allSatisfy({ $0 == 0 }) && b[10] == 0xFF && b[11] == 0xFF { // v4-mapped
            let a = UInt32(b[12]) << 24 | UInt32(b[13]) << 16 | UInt32(b[14]) << 8 | UInt32(b[15])
            return isPrivateV4(a)
        }
        return false
    }
}

extension String {
    /// At most `max` UTF-8 bytes, cut on a character boundary; whether it was cut.
    func clipped(toBytes max: Int) -> (String, Bool) {
        guard utf8.count > max else { return (self, false) }
        let u = Array(utf8)
        var end = max
        while end > 0, u[end] & 0xC0 == 0x80 { end -= 1 }
        return (String(decoding: u[..<end], as: UTF8.self), true)
    }
}
