package model

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func cached(providerID, id string) CachedCatalog {
	return CachedCatalog{Models: []Model{{Provider: providerID, ID: id, API: OpenAIResponsesAPI}}, CheckedAt: 1, ETag: "opaque"}
}

func TestStoreProviderScopedMergeAndUnknownPreservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models-store.json")
	writeFile(t, path, `{"future":{"opaque":true}}`)
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), "one", cached("one", "m1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), "two", cached("two", "m2")); err != nil {
		t.Fatal(err)
	}
	one, ok, err := s.Read(context.Background(), "one")
	if err != nil || !ok || one.Models[0].ID != "m1" {
		t.Fatalf("one=%#v %t %v", one, ok, err)
	}
	if err := s.Delete(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	_, ok, err = s.Read(context.Background(), "one")
	if err != nil || ok {
		t.Fatalf("deleted=%t %v", ok, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(b), `"future"`) {
		t.Fatalf("unknown cache entry lost: %s", b)
	}
}
func TestStoreConcurrentWriters(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "models-store.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Write(context.Background(), "provider", cached("provider", string(rune('a'+i)))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if _, ok, err := s.Read(context.Background(), "provider"); err != nil || !ok {
		t.Fatalf("read %t %v", ok, err)
	}
}
func contains(s, part string) bool {
	return len(part) == 0 || (len(s) >= len(part) && (func() bool {
		for i := 0; i+len(part) <= len(s); i++ {
			if s[i:i+len(part)] == part {
				return true
			}
		}
		return false
	})())
}
