import Foundation
import CryptoKit

/// Hex SHA-256 of some bytes (cache keys).
func featureSHA256(_ data: Data) -> String {
    SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
}

/// A small thread-safe LRU bounded by entry count and total cost (bytes).
final class ContentCache<Value: Sendable>: @unchecked Sendable {
    private struct Entry { var value: Value; var cost: Int }
    private let maxEntries: Int
    private let maxCost: Int
    private let lock = NSLock()
    private var entries: [String: Entry] = [:]
    private var order: [String] = []     // least recently used first
    private var totalCost = 0

    init(maxEntries: Int, maxCost: Int) { self.maxEntries = maxEntries; self.maxCost = maxCost }

    var count: Int { lock.lock(); defer { lock.unlock() }; return entries.count }

    func value(for key: String) -> Value? {
        lock.lock(); defer { lock.unlock() }
        guard let e = entries[key] else { return nil }
        touch(key)
        return e.value
    }

    func insert(_ value: Value, cost: Int, for key: String) {
        lock.lock(); defer { lock.unlock() }
        guard cost <= maxCost else { return }
        if let old = entries[key] { totalCost -= old.cost }
        entries[key] = Entry(value: value, cost: cost)
        totalCost += cost
        touch(key)
        while (entries.count > maxEntries || totalCost > maxCost), let oldest = order.first {
            order.removeFirst()
            if let e = entries.removeValue(forKey: oldest) { totalCost -= e.cost }
        }
    }

    private func touch(_ key: String) {
        if let i = order.firstIndex(of: key) { order.remove(at: i) }
        order.append(key)
    }
}
