package session

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestYoungReadIndexValidatesAgainAfterTimestampSettles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.jsonl")
	raw := []byte(testHeader + "\n" + userEntryJSON("before", "one", "null", 1) + "\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := OpenReadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if index.settled {
		t.Skip("filesystem timestamp already outside settling window")
	}
	replacement := bytes.Replace(raw, []byte("before"), []byte("after!"), 1)
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	// Model a clock tick shared by the initial index and the rewrite. Both
	// files have the same length/IDs, and the old index has its original digest.
	index.info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if valid, err := index.ValidateSource(); err != nil || valid {
		t.Fatalf("delayed query trusted a formerly-young timestamp: %t %v", valid, err)
	}
}
