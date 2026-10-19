import Foundation

// Small JSONValue accessors for the feature modules. Internal and
// feature-prefixed so they cannot collide with helpers other modules add.
extension JSONValue {
    var featureString: String? { if case .string(let s) = self { return s } else { return nil } }
    var featureNumber: Double? { if case .number(let n) = self { return n } else { return nil } }
    var featureBool: Bool? { if case .bool(let b) = self { return b } else { return nil } }
    var featureArray: [JSONValue]? { if case .array(let a) = self { return a } else { return nil } }
    var featureObject: [String: JSONValue]? { if case .object(let o) = self { return o } else { return nil } }

    /// `self["a"]`-style lookup on objects; nil for anything else.
    func feature(_ key: String) -> JSONValue? { featureObject?[key] }

    /// Walks object keys and array indices: `json.feature(path: "choices", 0, "message")`.
    func feature(path: Any...) -> JSONValue? {
        var cur: JSONValue = self
        for step in path {
            if let k = step as? String { guard let n = cur.feature(k) else { return nil }; cur = n }
            else if let i = step as? Int { guard let a = cur.featureArray, a.indices.contains(i) else { return nil }; cur = a[i] }
            else { return nil }
        }
        return cur
    }

    static func makeObject(_ pairs: KeyValuePairs<String, JSONValue>) -> JSONValue {
        var d: [String: JSONValue] = [:]
        for (k, v) in pairs { d[k] = v }
        return .object(d)
    }
}
