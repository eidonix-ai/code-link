import Foundation

/// The engine's working JSON: objects keep their key order and numbers keep
/// their original spelling, so a request that is parsed, edited and written
/// again stays byte-stable where it was not edited. That matters for prefix
/// caching: a backend renders tool schemas in the order it receives them.
///
/// `JSONValue` (EngineConfig, hooks) is the public, order-free form; convert
/// with `JSON(_:)` and `jsonValue`.
enum JSON: Sendable, Equatable {
    case null
    case bool(Bool)
    /// The number as written (`64`, `0.5`, `1e-3`).
    case number(String)
    case string(String)
    case array([JSON])
    case object(JSONObject)
}

/// An ordered JSON object. Setting an existing key keeps its position.
struct JSONObject: Sendable, Equatable, Sequence {
    private(set) var keys: [String] = []
    private(set) var values: [JSON] = []

    init() {}
    init(_ pairs: [(String, JSON)]) {
        for (k, v) in pairs { self[k] = v }
    }

    var count: Int { keys.count }
    var isEmpty: Bool { keys.isEmpty }

    subscript(key: String) -> JSON? {
        get {
            guard let i = keys.firstIndex(of: key) else { return nil }
            return values[i]
        }
        set {
            if let i = keys.firstIndex(of: key) {
                if let newValue { values[i] = newValue } else { keys.remove(at: i); values.remove(at: i) }
            } else if let newValue {
                keys.append(key); values.append(newValue)
            }
        }
    }

    func makeIterator() -> AnyIterator<(key: String, value: JSON)> {
        var i = 0
        return AnyIterator {
            guard i < keys.count else { return nil }
            defer { i += 1 }
            return (keys[i], values[i])
        }
    }

    /// Key order does not matter for equality.
    static func == (a: JSONObject, b: JSONObject) -> Bool {
        guard a.count == b.count else { return false }
        for (k, v) in a where b[k] != v { return false }
        return true
    }
}

// MARK: - Literals

extension JSON: ExpressibleByNilLiteral, ExpressibleByBooleanLiteral, ExpressibleByIntegerLiteral,
    ExpressibleByFloatLiteral, ExpressibleByStringLiteral, ExpressibleByArrayLiteral, ExpressibleByDictionaryLiteral {
    init(nilLiteral: ()) { self = .null }
    init(booleanLiteral value: Bool) { self = .bool(value) }
    init(integerLiteral value: Int) { self = .number(String(value)) }
    init(floatLiteral value: Double) { self = .number(JSON.format(value)) }
    init(stringLiteral value: String) { self = .string(value) }
    init(arrayLiteral elements: JSON...) { self = .array(elements) }
    init(dictionaryLiteral elements: (String, JSON)...) { self = .object(JSONObject(elements)) }
}

extension JSON {
    static func int(_ v: Int) -> JSON { .number(String(v)) }
    static func double(_ v: Double) -> JSON { .number(format(v)) }
    static func obj(_ pairs: [(String, JSON)]) -> JSON { .object(JSONObject(pairs)) }

    static func format(_ d: Double) -> String {
        guard d.isFinite else { return "0" }
        if d == d.rounded(), abs(d) < 1e15 { return String(Int64(d)) }
        return "\(d)"
    }
}

// MARK: - Access

extension JSON {
    subscript(key: String) -> JSON? {
        get { if case .object(let o) = self { return o[key] } else { return nil } }
        set {
            guard case .object(var o) = self else { return }
            o[key] = newValue
            self = .object(o)
        }
    }
    subscript(index: Int) -> JSON? {
        if case .array(let a) = self, a.indices.contains(index) { return a[index] }
        return nil
    }
    var string: String? { if case .string(let s) = self { return s } else { return nil } }
    var bool: Bool? { if case .bool(let b) = self { return b } else { return nil } }
    var double: Double? { if case .number(let n) = self { return Double(n) } else { return nil } }
    var int: Int? {
        guard case .number(let n) = self else { return nil }
        if let i = Int(n) { return i }
        if let d = Double(n), d.isFinite, abs(d) < 9e18 { return Int(d) }
        return nil
    }
    var array: [JSON]? { if case .array(let a) = self { return a } else { return nil } }
    var object: JSONObject? { if case .object(let o) = self { return o } else { return nil } }
    var isNull: Bool { if case .null = self { return true } else { return false } }
    /// The value of a block's `type` field.
    var type: String? { self["type"]?.string }
}

// MARK: - JSONValue bridge

extension JSON {
    init(_ v: JSONValue) {
        switch v {
        case .null: self = .null
        case .bool(let b): self = .bool(b)
        case .number(let d): self = .number(JSON.format(d))
        case .string(let s): self = .string(s)
        case .array(let a): self = .array(a.map(JSON.init))
        case .object(let o): self = .object(JSONObject(o.sorted { $0.key < $1.key }.map { ($0.key, JSON($0.value)) }))
        }
    }
    var jsonValue: JSONValue {
        switch self {
        case .null: return .null
        case .bool(let b): return .bool(b)
        case .number(let n): return .number(Double(n) ?? 0)
        case .string(let s): return .string(s)
        case .array(let a): return .array(a.map(\.jsonValue))
        case .object(let o):
            var d: [String: JSONValue] = [:]
            for (k, v) in o { d[k] = v.jsonValue }
            return .object(d)
        }
    }
}

// MARK: - Parsing

struct JSONParseError: Error, CustomStringConvertible {
    var message: String
    var offset: Int
    var description: String { "invalid JSON at byte \(offset): \(message)" }
}

extension JSON {
    static func parse(_ data: Data) throws -> JSON {
        try data.withUnsafeBytes { raw in
            var p = JSONParser(bytes: raw)
            p.skipWhitespace()
            let v = try p.value(depth: 0)
            p.skipWhitespace()
            guard p.i == raw.count else { throw p.error("trailing characters") }
            return v
        }
    }
    static func parse(_ s: String) throws -> JSON { try parse(Data(s.utf8)) }
}

private struct JSONParser {
    let bytes: UnsafeRawBufferPointer
    var i = 0

    func error(_ m: String) -> JSONParseError { JSONParseError(message: m, offset: i) }

    mutating func skipWhitespace() {
        while i < bytes.count {
            switch bytes[i] {
            case 0x20, 0x0A, 0x0D, 0x09: i += 1
            default: return
            }
        }
    }

    mutating func value(depth: Int) throws -> JSON {
        guard depth < 512 else { throw error("nesting too deep") }
        guard i < bytes.count else { throw error("unexpected end") }
        switch bytes[i] {
        case UInt8(ascii: "{"):
            i += 1
            var o = JSONObject()
            skipWhitespace()
            if i < bytes.count, bytes[i] == UInt8(ascii: "}") { i += 1; return .object(o) }
            while true {
                skipWhitespace()
                guard i < bytes.count, bytes[i] == UInt8(ascii: "\"") else { throw error("expected key") }
                let k = try string()
                skipWhitespace()
                guard i < bytes.count, bytes[i] == UInt8(ascii: ":") else { throw error("expected ':'") }
                i += 1
                skipWhitespace()
                o[k] = try value(depth: depth + 1)
                skipWhitespace()
                guard i < bytes.count else { throw error("unexpected end in object") }
                if bytes[i] == UInt8(ascii: ",") { i += 1; continue }
                if bytes[i] == UInt8(ascii: "}") { i += 1; return .object(o) }
                throw error("expected ',' or '}'")
            }
        case UInt8(ascii: "["):
            i += 1
            var a: [JSON] = []
            skipWhitespace()
            if i < bytes.count, bytes[i] == UInt8(ascii: "]") { i += 1; return .array(a) }
            while true {
                skipWhitespace()
                a.append(try value(depth: depth + 1))
                skipWhitespace()
                guard i < bytes.count else { throw error("unexpected end in array") }
                if bytes[i] == UInt8(ascii: ",") { i += 1; continue }
                if bytes[i] == UInt8(ascii: "]") { i += 1; return .array(a) }
                throw error("expected ',' or ']'")
            }
        case UInt8(ascii: "\""):
            return .string(try string())
        case UInt8(ascii: "t"): try literal("true"); return .bool(true)
        case UInt8(ascii: "f"): try literal("false"); return .bool(false)
        case UInt8(ascii: "n"): try literal("null"); return .null
        default:
            return .number(try number())
        }
    }

    mutating func literal(_ word: StaticString) throws {
        let n = word.utf8CodeUnitCount
        guard i + n <= bytes.count else { throw error("bad literal") }
        let w = UnsafeRawBufferPointer(start: word.utf8Start, count: n)
        for j in 0..<n where bytes[i + j] != w[j] { throw error("bad literal") }
        i += n
    }

    mutating func number() throws -> String {
        let start = i
        while i < bytes.count, Self.isNumberByte(bytes[i]) { i += 1 }
        guard i > start else { throw error("unexpected character") }
        let s = String(decoding: UnsafeRawBufferPointer(rebasing: bytes[start..<i]), as: UTF8.self)
        guard Double(s) != nil else { throw error("bad number") }
        return s
    }

    static func isNumberByte(_ b: UInt8) -> Bool {
        switch b {
        case UInt8(ascii: "0")...UInt8(ascii: "9"), UInt8(ascii: "-"), UInt8(ascii: "+"),
             UInt8(ascii: "."), UInt8(ascii: "e"), UInt8(ascii: "E"): return true
        default: return false
        }
    }

    mutating func string() throws -> String {
        i += 1 // opening quote
        let start = i
        // Fast path: no escapes.
        while i < bytes.count {
            let b = bytes[i]
            if b == UInt8(ascii: "\"") {
                let s = String(decoding: UnsafeRawBufferPointer(rebasing: bytes[start..<i]), as: UTF8.self)
                i += 1
                return s
            }
            if b == UInt8(ascii: "\\") { break }
            i += 1
        }
        var out = [UInt8](UnsafeRawBufferPointer(rebasing: bytes[start..<i]))
        while i < bytes.count {
            let b = bytes[i]
            if b == UInt8(ascii: "\"") {
                i += 1
                return String(decoding: out, as: UTF8.self)
            }
            if b != UInt8(ascii: "\\") { out.append(b); i += 1; continue }
            i += 1
            guard i < bytes.count else { break }
            let e = bytes[i]
            i += 1
            switch e {
            case UInt8(ascii: "\""): out.append(0x22)
            case UInt8(ascii: "\\"): out.append(0x5C)
            case UInt8(ascii: "/"): out.append(0x2F)
            case UInt8(ascii: "b"): out.append(0x08)
            case UInt8(ascii: "f"): out.append(0x0C)
            case UInt8(ascii: "n"): out.append(0x0A)
            case UInt8(ascii: "r"): out.append(0x0D)
            case UInt8(ascii: "t"): out.append(0x09)
            case UInt8(ascii: "u"):
                var scalar = try hex4()
                if (0xD800...0xDBFF).contains(scalar), i + 1 < bytes.count,
                   bytes[i] == UInt8(ascii: "\\"), bytes[i + 1] == UInt8(ascii: "u") {
                    let save = i
                    i += 2
                    let low = try hex4()
                    if (0xDC00...0xDFFF).contains(low) {
                        scalar = 0x10000 + ((scalar - 0xD800) << 10) + (low - 0xDC00)
                    } else {
                        i = save
                    }
                }
                let u = Unicode.Scalar(scalar) ?? "\u{FFFD}"
                out.append(contentsOf: Array(String(Character(u)).utf8))
            default:
                throw error("bad escape")
            }
        }
        throw error("unterminated string")
    }

    mutating func hex4() throws -> UInt32 {
        guard i + 4 <= bytes.count else { throw error("bad \\u escape") }
        var v: UInt32 = 0
        for _ in 0..<4 {
            let c = bytes[i]
            let d: UInt32
            switch c {
            case UInt8(ascii: "0")...UInt8(ascii: "9"): d = UInt32(c - UInt8(ascii: "0"))
            case UInt8(ascii: "a")...UInt8(ascii: "f"): d = UInt32(c - UInt8(ascii: "a") + 10)
            case UInt8(ascii: "A")...UInt8(ascii: "F"): d = UInt32(c - UInt8(ascii: "A") + 10)
            default: throw error("bad \\u escape")
            }
            v = v << 4 | d
            i += 1
        }
        return v
    }
}

// MARK: - Serialization

extension JSON {
    /// Compact JSON. Non-ASCII is written as UTF-8 and `/` is not escaped.
    var data: Data { Data(bytes) }
    var bytes: [UInt8] {
        var out: [UInt8] = []
        out.reserveCapacity(256)
        write(into: &out)
        return out
    }
    var text: String { String(decoding: bytes, as: UTF8.self) }

    func write(into out: inout [UInt8]) {
        switch self {
        case .null: out.append(contentsOf: [0x6E, 0x75, 0x6C, 0x6C])
        case .bool(let b): out.append(contentsOf: b ? Array("true".utf8) : Array("false".utf8))
        case .number(let n): out.append(contentsOf: n.utf8)
        case .string(let s): JSON.writeString(s, into: &out)
        case .array(let a):
            out.append(UInt8(ascii: "["))
            for (i, v) in a.enumerated() {
                if i > 0 { out.append(UInt8(ascii: ",")) }
                v.write(into: &out)
            }
            out.append(UInt8(ascii: "]"))
        case .object(let o):
            out.append(UInt8(ascii: "{"))
            var first = true
            for (k, v) in o {
                if !first { out.append(UInt8(ascii: ",")) }
                first = false
                JSON.writeString(k, into: &out)
                out.append(UInt8(ascii: ":"))
                v.write(into: &out)
            }
            out.append(UInt8(ascii: "}"))
        }
    }

    private static let hexDigits = Array("0123456789abcdef".utf8)

    static func writeString(_ s: String, into out: inout [UInt8]) {
        out.append(0x22)
        var s = s
        s.withUTF8 { buf in
            var runStart = 0
            for (idx, b) in buf.enumerated() {
                let esc: [UInt8]?
                switch b {
                case 0x22: esc = [0x5C, 0x22]
                case 0x5C: esc = [0x5C, 0x5C]
                case 0x0A: esc = [0x5C, UInt8(ascii: "n")]
                case 0x0D: esc = [0x5C, UInt8(ascii: "r")]
                case 0x09: esc = [0x5C, UInt8(ascii: "t")]
                case 0x08: esc = [0x5C, UInt8(ascii: "b")]
                case 0x0C: esc = [0x5C, UInt8(ascii: "f")]
                case 0x00..<0x20:
                    esc = [0x5C, UInt8(ascii: "u"), 0x30, 0x30, hexDigits[Int(b >> 4)], hexDigits[Int(b & 0xF)]]
                default: esc = nil
                }
                if let esc {
                    if idx > runStart { out.append(contentsOf: UnsafeBufferPointer(rebasing: buf[runStart..<idx])) }
                    out.append(contentsOf: esc)
                    runStart = idx + 1
                }
            }
            if buf.count > runStart { out.append(contentsOf: UnsafeBufferPointer(rebasing: buf[runStart..<buf.count])) }
        }
        out.append(0x22)
    }
}

extension JSON: CustomStringConvertible {
    var description: String { text }
}
