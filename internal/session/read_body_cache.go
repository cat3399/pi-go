package session

import (
	"container/list"
	"sync"
)

const defaultReadBodyCacheBytes int64 = 4 << 20

// Only a small, budgeted working set of materialized bodies is retained. The
// entire allowance is charged to ReadIndex.EstimatedBytes up front, so the
// application's existing 128 MiB LRU still bounds all retained index/body data.
// Bodies too large for this allowance are read on demand and never cached.
type readBodyCache struct {
	mu      sync.Mutex
	entries map[int64]*list.Element
	lru     list.List
	bytes   int64
	limit   int64
}

type cachedReadBody struct {
	offset int64
	entry  Entry
	bytes  int64
}

func (c *readBodyCache) get(offset int64) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.entries[offset]; element != nil {
		c.lru.MoveToFront(element)
		return element.Value.(cachedReadBody).entry, true
	}
	return Entry{}, false
}

func (c *readBodyCache) put(offset int64, entry Entry) {
	// Include raw slice capacity (the decoder can overallocate), decoded
	// strings/blocks, map/list overhead and owned payload metadata. This is
	// deliberately more conservative than charging just the wire bytes.
	cost := int64(cap(entry.raw))*8 + 2048
	if c.limit <= 0 || cost > c.limit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.entries[offset]; element != nil {
		c.lru.MoveToFront(element)
		return
	}
	if c.entries == nil {
		c.entries = make(map[int64]*list.Element)
	}
	c.entries[offset] = c.lru.PushFront(cachedReadBody{offset: offset, entry: entry, bytes: cost})
	c.bytes += cost
	for c.bytes > c.limit || len(c.entries) > 256 {
		last := c.lru.Back()
		value := last.Value.(cachedReadBody)
		c.bytes -= value.bytes
		delete(c.entries, value.offset)
		c.lru.Remove(last)
	}
}
