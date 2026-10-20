// Package lru is a small thread-safe cache bounded by entry count and total
// cost (bytes), keyed by content hash. The feature modules use it so resent
// history isn't extracted or described again.
//
// Port of the Swift Features/Support/ContentCache.swift.
package lru

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// SHA256 is the hex SHA-256 of b (a cache key).
func SHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Cache is an LRU bounded by entry count and total cost.
type Cache[V any] struct {
	mu         sync.Mutex
	maxEntries int
	maxCost    int
	cost       int
	order      *list.List // front = most recently used
	items      map[string]*list.Element
}

type entry[V any] struct {
	key   string
	value V
	cost  int
}

func New[V any](maxEntries, maxCost int) *Cache[V] {
	return &Cache[V]{maxEntries: maxEntries, maxCost: maxCost, order: list.New(), items: map[string]*list.Element{}}
}

// Len is the number of entries.
func (c *Cache[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Get returns the value for key and marks it most recently used.
func (c *Cache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*entry[V]).value, true
	}
	var zero V
	return zero, false
}

// Put stores value; a value costing more than the whole budget is not
// stored. The least recently used entries go until both bounds hold.
func (c *Cache[V]) Put(key string, value V, cost int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cost > c.maxCost {
		return
	}
	if e, ok := c.items[key]; ok {
		c.cost -= e.Value.(*entry[V]).cost
		c.order.Remove(e)
	}
	c.items[key] = c.order.PushFront(&entry[V]{key: key, value: value, cost: cost})
	c.cost += cost
	for len(c.items) > c.maxEntries || c.cost > c.maxCost {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		e := c.order.Remove(oldest).(*entry[V])
		delete(c.items, e.key)
		c.cost -= e.cost
	}
}
