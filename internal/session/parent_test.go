package session

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRewriteParentSessionPreservesNonHeaderRecords(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "child.jsonl")
	parent := filepath.Join(directory, "parent.jsonl")
	prefix := []byte("damaged compatibility record\n")
	entry := []byte(`{"type":"message","id":"entry-1","parentId":null,"timestamp":"2026-08-11T00:00:01Z","message":{"role":"user","content":"keep me","timestamp":1786406401000}}` + "\n")
	header := []byte(`{"type":"session","version":3,"id":"child","timestamp":"2026-08-11T00:00:00Z","cwd":"` + directory + `","custom":{"retained":true}}` + "\n")
	if err := os.WriteFile(path, append(append(append([]byte{}, prefix...), header...), entry...), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RewriteParentSession(context.Background(), path, parent); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(updated, prefix) || !bytes.HasSuffix(updated, entry) {
		t.Fatalf("non-header records changed:\n%s", updated)
	}
	lines := bytes.Split(updated, []byte("\n"))
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(lines[1], &decoded); err != nil {
		t.Fatal(err)
	}
	var gotParent string
	if err := json.Unmarshal(decoded["parentSession"], &gotParent); err != nil || gotParent != parent {
		t.Fatalf("parentSession = %q, %v", gotParent, err)
	}
	if string(decoded["custom"]) != `{"retained":true}` {
		t.Fatalf("custom header data = %s", decoded["custom"])
	}

	if err := RewriteParentSession(context.Background(), path, ""); err != nil {
		t.Fatal(err)
	}
	updated, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines = bytes.Split(updated, []byte("\n"))
	decoded = nil
	if err := json.Unmarshal(lines[1], &decoded); err != nil {
		t.Fatal(err)
	}
	if _, present := decoded["parentSession"]; present {
		t.Fatal("parentSession was not removed")
	}
}
