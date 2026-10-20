import Foundation

/// What a chat function name presented to the model stands for.
struct ToolBinding: Sendable, Equatable {
    var namespace: String      // "" for a top-level tool
    var name: String           // the tool's own name inside its namespace
    var custom: Bool           // freeform tool (one `input` string) rather than JSON arguments
}

/// Maps between Responses tools and the chat functions the model sees, both
/// ways (docs/DESIGN.md §5).
///
/// Flattening rule (deterministic, collision-safe):
///   - a top-level tool keeps its name;
///   - a tool inside namespace NS is `NS__NAME`, characters outside
///     `[A-Za-z0-9_-]` replaced by `_`, at most 64 characters (a longer name
///     is cut and ends in `_` + an 8-hex hash of the full name);
///   - the implicit namespace `functions` is dropped when the bare name is
///     free, so Codex's auto-review sees `exec`, not `functions__exec`;
///   - a name already taken (a top-level tool always wins) gets `_2`, `_3`, …
struct ToolRegistry: Sendable {
    private(set) var byFlat: [String: ToolBinding] = [:]
    private var byKey: [String: String] = [:]
    private var byName: [String: String] = [:]

    private static func key(_ ns: String, _ name: String) -> String { ns + "\u{0}" + name }

    /// The chat function name for a Responses (namespace, name). A tool the
    /// request does not define (history naming an old one) gets the name the
    /// flattening rule gives it.
    func flatName(_ ns: String, _ name: String) -> String {
        if let flat = byKey[Self.key(ns, name)] { return flat }
        if ns.isEmpty, let flat = byName[name] { return flat }
        return ns.isEmpty ? name : Self.sanitize(ns + "__" + name)
    }

    func resolve(_ flat: String) -> ToolBinding? { byFlat[flat] }

    static func sanitize(_ s: String) -> String {
        var out = ""
        for c in s.unicodeScalars {
            switch c {
            case "a"..."z", "A"..."Z", "0"..."9", "_", "-": out.unicodeScalars.append(c)
            default: out.append("_")
            }
        }
        if out.utf8.count > 64 {
            out = String(out.prefix(55)) + "_" + String(format: "%08x", fnv32a(s))
        }
        return out
    }

    private static func fnv32a(_ s: String) -> UInt32 {
        var h: UInt32 = 2166136261
        for b in s.utf8 { h ^= UInt32(b); h = h &* 16777619 }
        return h
    }

    struct Translated: Sendable {
        var tools: [JSON] = []
        /// Tool types with no chat equivalent (`web_search`, …).
        var stripped: [JSON] = []
        var registry = ToolRegistry()
    }

    /// The schema every freeform tool is given.
    static let customParameters: JSON = [
        "type": "object",
        "properties": ["input": ["type": "string", "description": "The raw tool input, passed verbatim."]],
        "required": ["input"], "additionalProperties": false,
    ]

    /// Chat tools for Responses tools: functions map one to one, namespaces
    /// are flattened, custom (freeform) tools become a function with one
    /// string parameter `input`, anything else is stripped.
    static func translate(_ tools: [JSON]) -> Translated {
        struct Def { var ns: String; var tool: JSON }
        var defs: [Def] = []
        var out = Translated()
        for t in tools {
            switch t.type {
            case "namespace":
                for sub in t["tools"]?.array ?? [] where sub.type == "function" || sub.type == "custom" {
                    defs.append(Def(ns: t["name"]?.string ?? "", tool: sub))
                }
            case "function", "custom":
                defs.append(Def(ns: "", tool: t))
            default:
                out.stripped.append(t)
            }
        }
        // Top-level names are reserved first: they always keep their own.
        var used = Set<String>()
        for d in defs where d.ns.isEmpty { used.insert(d.tool["name"]?.string ?? "") }

        for d in defs {
            let name = d.tool["name"]?.string ?? ""
            let custom = d.tool.type == "custom"
            let k = key(d.ns, name)
            if out.registry.byKey[k] != nil { continue }
            var flat = name
            if !d.ns.isEmpty {
                flat = sanitize(d.ns + "__" + name)
                if d.ns == "functions" {
                    let bare = sanitize(name)
                    if !used.contains(bare) { flat = bare }
                }
                if used.contains(flat) {
                    let base = flat
                    var i = 2
                    while used.contains(flat) { flat = sanitize("\(base)_\(i)"); i += 1 }
                }
                used.insert(flat)
            }
            out.registry.byKey[k] = flat
            out.registry.byFlat[flat] = ToolBinding(namespace: d.ns, name: name, custom: custom)
            if out.registry.byName[name] == nil { out.registry.byName[name] = flat }

            var fn = JSONObject([("name", .string(flat))])
            if custom {
                fn["description"] = .string(customDescription(d.tool["description"]?.string ?? "", d.tool["format"]))
                fn["parameters"] = customParameters
            } else {
                if let desc = d.tool["description"]?.string, !desc.isEmpty { fn["description"] = .string(desc) }
                if let p = d.tool["parameters"], !p.isNull { fn["parameters"] = p }
                if let s = d.tool["strict"]?.bool { fn["strict"] = .bool(s) }
            }
            out.tools.append(["type": "function", "function": .object(fn)])
        }
        return out
    }

    /// Explains the freeform contract to the model and, for a grammar tool,
    /// appends the grammar (the backend cannot enforce it, the model can follow it).
    private static func customDescription(_ desc: String, _ format: JSON?) -> String {
        var note = "This tool takes freeform text: pass it verbatim as the `input` string argument."
        if format?.type == "grammar", let def = format?["definition"]?.string?.trimmingCharacters(in: .whitespacesAndNewlines),
           !def.isEmpty {
            let (clipped, _) = def.clipped(toBytes: 4000)
            note += "\nThe input must match this \(format?["syntax"]?.string ?? "") grammar:\n\(clipped)"
        }
        return desc.isEmpty ? note : desc + "\n\n" + note
    }

    /// A Responses tool_choice as a chat one: strings pass through,
    /// `{type: function|custom, name}` gets the flattened name, allowed_tools
    /// collapses to its mode, anything else is `auto`.
    func toolChoice(_ tc: JSON) -> JSON {
        if let s = tc.string { return .string(s) }
        switch tc.type {
        case "function", "custom":
            let name = tc["name"]?.string ?? tc["function"]?["name"]?.string ?? ""
            if name.isEmpty { return "auto" }
            return ["type": "function", "function": ["name": .string(flatName(tc["namespace"]?.string ?? "", name))]]
        case "allowed_tools":
            if tc["mode"]?.string == "required" { return "required" }
        default:
            break
        }
        return "auto"
    }

    /// The freeform text of a custom tool call from the arguments the model
    /// wrote (`{"input": "…"}`). Anything of another shape passes as it is, so
    /// no model output is lost.
    static func customInput(_ args: String) -> String {
        if let j = try? JSON.parse(args) {
            if j.object != nil { return j["input"]?.string ?? args }
            if let s = j.string { return s }
        }
        return args
    }

    /// The Responses output item for a chat tool call: a `function_call`
    /// (with `namespace` when the tool lives in one) or, for a freeform tool,
    /// a `custom_tool_call`. `inProgress` items carry no arguments yet.
    func outputItem(itemID: String, callID: String, flatName: String, args: String, inProgress: Bool) -> JSON {
        let status: JSON = inProgress ? "in_progress" : "completed"
        let binding = resolve(flatName)
        if let b = binding, b.custom {
            var item: JSON = ["id": .string(itemID), "type": "custom_tool_call", "call_id": .string(callID),
                              "name": .string(b.name), "input": .string(inProgress ? "" : Self.customInput(args)), "status": status]
            if !b.namespace.isEmpty { item["namespace"] = .string(b.namespace) }
            return item
        }
        var item: JSON = ["id": .string(itemID), "type": "function_call", "call_id": .string(callID),
                          "arguments": .string(args), "status": status]
        if let b = binding, !b.namespace.isEmpty { item["namespace"] = .string(b.namespace) }
        item["name"] = .string(binding?.name ?? flatName)
        return item
    }
}
