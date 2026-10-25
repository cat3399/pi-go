package application

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/session"
)

func TestReadIndexCacheKeepsAlternatingBodyHeavySessions(t *testing.T) {
	cache := readIndexCache{entries: make(map[string]*cachedReadIndex), budget: 20 << 20}
	for sessionNumber := 0; sessionNumber < 3; sessionNumber++ {
		id := fmt.Sprintf("body-heavy-%d", sessionNumber)
		path := filepath.Join(t.TempDir(), id+".jsonl")
		var data bytes.Buffer
		fmt.Fprintf(&data, "{\"type\":\"session\",\"version\":3,\"id\":%q,\"timestamp\":\"2026-01-01T00:00:00.000Z\",\"cwd\":\"/tmp\"}\n", id)
		for entryNumber := 0; entryNumber < 200; entryNumber++ {
			parent, text := "null", "small first question"
			if entryNumber > 0 {
				parent = fmt.Sprintf("\"e%d\"", entryNumber-1)
				text = strings.Repeat("body", 4096)
			}
			fmt.Fprintf(&data, "{\"type\":\"message\",\"id\":\"e%d\",\"parentId\":%s,\"timestamp\":\"2026-01-01T00:00:00.000Z\",\"message\":{\"role\":\"user\",\"content\":%q,\"timestamp\":1767225600000}}\n", entryNumber, parent, text)
		}
		if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		index, err := session.OpenReadIndex(path)
		if err != nil {
			t.Fatal(err)
		}
		if index.EstimatedBytes() > cache.budget {
			t.Fatalf("body-heavy index cannot enter cache: %d", index.EstimatedBytes())
		}
		cache.entries[id] = &cachedReadIndex{path: path, identity: index.Identity(), index: index, used: uint64(sessionNumber)}
		cache.bytes += index.EstimatedBytes()
		cache.evictLocked()
	}
	for id, entry := range cache.entries {
		if entry.index == nil {
			t.Fatalf("alternating sessions evicted %s", id)
		}
		if _, err := entry.index.ReadHistory(session.HistoryQuery{Limit: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if cache.bytes > cache.budget {
		t.Fatal("cache exceeded its original byte budget")
	}
}
