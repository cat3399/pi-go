package session

import (
	"bytes"
	"testing"
)

func TestReadBodyCacheIsBoundedAndSkipsOversizedEntries(t *testing.T) {
	cache := readBodyCache{limit: 4096}
	one := Entry{id: "one", raw: []byte("first raw body")}
	two := Entry{id: "two", raw: []byte("second raw body")}
	cache.put(1, one)
	cache.put(2, two)
	if _, ok := cache.get(1); ok {
		t.Fatal("LRU failed to evict the old body")
	}
	value, ok := cache.get(2)
	if !ok {
		t.Fatal("recent body not cached")
	}
	owned := value.RawJSON()
	owned[0] = 'X'
	again, _ := cache.get(2)
	if !bytes.Equal(again.RawJSON(), two.RawJSON()) {
		t.Fatal("public bytes mutated cached body")
	}
	cache.put(3, Entry{raw: make([]byte, 8192)})
	if _, ok := cache.get(3); ok {
		t.Fatal("oversized body entered working set")
	}
	if cache.bytes > cache.limit {
		t.Fatal("working set exceeded limit")
	}
}

func TestReadBodyCacheBoundsTinyRecordCount(t *testing.T) {
	cache := readBodyCache{limit: 16 << 20}
	for i := 0; i < 1000; i++ {
		cache.put(int64(i), Entry{raw: []byte("{}")})
	}
	if len(cache.entries) != 256 || cache.lru.Len() != 256 {
		t.Fatalf("unbounded tiny-body map: %d", len(cache.entries))
	}
}
