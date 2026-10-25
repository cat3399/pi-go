package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

func boundedIndexFixture(t *testing.T) (string, []Entry) {
	t.Helper()
	m, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	first := appendReadRun(t, m, "first", 2)
	long := appendReadRun(t, m, "long", 8)
	text, _ := llm.NewTextBlock(strings.Repeat("large tool output 中英\n", 80000))
	tool, _ := llm.NewToolResultMessage("large-call", "read", []llm.TextBlock{text}, false, time.UnixMilli(5000))
	if _, err := m.AppendLLMMessage(context.Background(), tool); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendCompaction(context.Background(), "compacted summary", long[0].ID(), 12345, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	label := "off-branch user label"
	if _, err := m.AppendLabelChange(context.Background(), first[0].ID(), &label); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendSessionInfo(context.Background(), "retained title"); err != nil {
		t.Fatal(err)
	}
	appendReadRun(t, m, "after compaction", 1)
	if err := m.Branch(first[0].ID()); err != nil {
		t.Fatal(err)
	}
	appendReadRun(t, m, "side branch", 1)
	entries := m.Entries()
	var data bytes.Buffer
	data.Write(m.Header().RawJSON())
	data.WriteString("\r\n\ninvalid-json-line\n")
	for i, entry := range entries {
		data.WriteString("  ")
		data.Write(entry.RawJSON())
		data.WriteString(" \t")
		if i != len(entries)-1 {
			data.WriteString("\r\n\n")
		}
	}
	path := filepath.Join(t.TempDir(), "large.jsonl")
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, entries
}

func TestBoundedReadIndexMatchesFullProjection(t *testing.T) {
	path, entries := boundedIndexFixture(t)
	thin, err := OpenReadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	full, err := openReadIndex(path, thin.Identity(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(thin.manager.store.loadDiagnostics, full.manager.store.loadDiagnostics) {
		t.Fatal("compatibility diagnostics changed")
	}
	for _, options := range []SnapshotOptions{{}, {IncludeContext: true}, {IncludeTree: true}, {IncludeContext: true, IncludeTree: true, LeafID: entries[len(entries)-6].ID()}} {
		a, ae := thin.ReadSnapshot(options)
		b, be := full.manager.ReadSnapshot(options)
		if ae != nil || be != nil || !reflect.DeepEqual(a, b) {
			t.Fatalf("snapshot mismatch (%+v): %v / %v", options, ae, be)
		}
	}
	leaf := entries[len(entries)-6].ID()
	for _, q := range []HistoryQuery{{Limit: 3}, {MaxBytes: 512}, {LeafID: &leaf, Limit: 5}, {AroundID: entries[0].ID(), Limit: 2}, {Direction: PageNewer, Limit: 4}} {
		for pages := 0; pages < 100; pages++ {
			a, ae := thin.ReadHistory(q)
			b, be := full.manager.ReadHistory(q)
			if ae != nil || be != nil || !reflect.DeepEqual(a, b) {
				t.Fatalf("history mismatch: %v / %v", ae, be)
			}
			if a.Older == nil {
				break
			}
			q = HistoryQuery{Cursor: a.Older, Limit: 3}
		}
	}
	for _, q := range []RunsQuery{{}, {MetadataOnly: true}, {AroundID: entries[len(entries)-2].ID(), Limit: 1}, {Direction: PageNewer, Limit: 1}} {
		a, ae := thin.ReadRuns(q)
		b, be := full.manager.ReadRuns(q)
		if ae != nil || be != nil || !reflect.DeepEqual(a, b) {
			t.Fatalf("runs mismatch: %v / %v", ae, be)
		}
		if len(a.Items) > 0 {
			hq := HistoryQuery{RunID: a.Items[0].Run.ID, Limit: 2}
			x, xe := thin.ReadHistory(hq)
			y, ye := full.manager.ReadHistory(hq)
			if xe != nil || ye != nil || !reflect.DeepEqual(x, y) {
				t.Fatalf("run details mismatch: %v / %v", xe, ye)
			}
		}
	}
	for _, q := range []NodeQuery{{Limit: 4}, {Tree: true}, {Role: "user"}, {Search: "REASONING"}, {Search: "output 中英"}, {LeafID: &leaf}} {
		a, ae := thin.ReadNodes(q)
		b, be := full.manager.ReadNodes(q)
		if ae != nil || be != nil || !reflect.DeepEqual(a, b) {
			t.Fatalf("nodes mismatch (%+v): %v / %v", q, ae, be)
		}
	}
	for _, entry := range entries {
		a, ae := thin.ReadEntry(entry.ID(), "")
		b, be := full.manager.ReadEntry(entry.ID(), "")
		if ae != nil || be != nil || !reflect.DeepEqual(a, b) {
			t.Fatalf("entry mismatch %s: %v / %v", entry.ID(), ae, be)
		}
		if a.Entry.readRecord != nil {
			t.Fatal("lazy body escaped public Entry boundary")
		}
	}
}

func TestBoundedReadIndexDoesNotRetainBodies(t *testing.T) {
	path, _ := boundedIndexFixture(t)
	index, err := OpenReadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if index.EstimatedBytes()-index.bodies.limit > info.Size()/4 {
		t.Fatalf("metadata index still scales with body size: %d / %d", index.EstimatedBytes()-index.bodies.limit, info.Size())
	}
	if index.bodies.bytes > index.bodies.limit {
		t.Fatal("materialized body working set exceeded its charged allowance")
	}
	for _, entry := range index.manager.store.entries {
		if len(entry.raw) != 0 || entry.message != nil || entry.readRecord == nil {
			t.Fatalf("retained body in %s", entry.ID())
		}
		if entry.payload != nil {
			if _, ok := entry.payload.(LabelPayload); !ok {
				t.Fatalf("retained payload %T", entry.payload)
			}
		}
		if entry.compaction != nil && entry.compaction.Summary != "" {
			t.Fatal("retained compaction body")
		}
	}
	// Metadata-only reads never materialize a body, including latest usage.
	if _, err := index.ReadRuns(RunsQuery{MetadataOnly: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := index.ReadSnapshot(SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestBoundedReadIndexRejectsChangedSourceForEveryRead(t *testing.T) {
	for _, change := range []string{"replace", "same-size-in-place", "truncate"} {
		t.Run(change, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.jsonl")
			raw := []byte(testHeader + "\n" + userEntryJSON("before", "one", "null", 1) + "\n")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			index, err := OpenReadIndex(path)
			if err != nil {
				t.Fatal(err)
			}
			before, err := index.ReadEntry("one", "")
			if err != nil {
				t.Fatal(err)
			}
			info, _ := os.Stat(path)
			replacement := bytes.Replace(raw, []byte("before"), []byte("after!"), 1)
			switch change {
			case "replace":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
			case "truncate":
				replacement = replacement[:len(replacement)/2]
			}
			if err := os.WriteFile(path, replacement, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			checks := []func() error{
				func() error { _, e := index.ReadHistory(HistoryQuery{}); return e }, func() error { _, e := index.ReadRuns(RunsQuery{}); return e },
				func() error { _, e := index.ReadNodes(NodeQuery{}); return e }, func() error { _, e := index.ReadSnapshot(SnapshotOptions{}); return e },
				func() error { _, e := index.ReadEntry("one", ""); return e },
			}
			for _, check := range checks {
				if err := check(); !errors.Is(err, ErrReadSourceChanged) {
					t.Fatalf("stale source accepted: %v", err)
				}
			}
			if !bytes.Contains(before.Entry.RawJSON(), []byte("before")) {
				t.Fatal("returned entry was not owned")
			}
		})
	}
}

func TestBoundedReadIndexConcurrentQueries(t *testing.T) {
	path, _ := boundedIndexFixture(t)
	index, err := OpenReadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 3; j++ {
				if _, err := index.ReadHistory(HistoryQuery{Limit: 5}); err != nil {
					t.Error(err)
				}
				if _, err := index.ReadRuns(RunsQuery{Limit: 2}); err != nil {
					t.Error(err)
				}
				if _, err := index.ReadNodes(NodeQuery{Limit: 5}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}

func TestBoundedReadIndexOffsetPreservesInvalidUTF8AndUnknownPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compat.jsonl")
	one := []byte(" \t" + userEntryJSON("before", "one", "null", 1) + " \r")
	one = bytes.Replace(one, []byte("before"), []byte{'a', 0xff, 'b'}, 1)
	two := []byte(` {"type":"future_kind","id":"two","parentId":"one","timestamp":"2026-01-01T00:00:00.000Z","payload":{"unknown":true}} `)
	raw := []byte(testHeader + "\nmalformed\n\n")
	raw = append(raw, one...)
	raw = append(raw, '\n')
	raw = append(raw, two...)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := OpenReadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range [][]byte{one, two} {
		entry, err := index.ReadEntry([]string{"one", "two"}[i], "")
		if err != nil || !bytes.Equal(entry.Entry.RawJSON(), want) {
			t.Fatalf("physical bytes lost: %v", err)
		}
	}
}

func BenchmarkBoundedReadIndexMetadata(b *testing.B) {
	path := filepath.Join(b.TempDir(), "source.jsonl")
	var raw bytes.Buffer
	raw.WriteString(testHeader + "\n")
	for i := 0; i < 1000; i++ {
		parent := "null"
		if i > 0 {
			parent = fmt.Sprintf("\"m%d\"", i-1)
		}
		raw.WriteString(userEntryJSON(strings.Repeat("x", 4096), fmt.Sprintf("m%d", i), parent, i+1) + "\n")
	}
	if err := os.WriteFile(path, raw.Bytes(), 0o600); err != nil {
		b.Fatal(err)
	}
	index, err := OpenReadIndex(path)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(index.EstimatedBytes()), "index-B")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := index.ReadSnapshot(SnapshotOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}
