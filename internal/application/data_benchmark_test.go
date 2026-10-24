package application_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
	"github.com/cat3399/pi-go/internal/surfacewire"
)

type dataBenchmarkShape struct {
	name                          string
	sessions, messages, textBytes int
	compacted, branched           bool
}

type dataBenchmarkCorpus struct {
	agentDir, cwd, firstID string
	bytes                  int64
	entries                int
}

// Keep the benchmark corpus tied to canonical behavior. These small instances
// exercise the same generator used by the large profiles, so future paging or
// cache benchmarks cannot quietly measure invalid or incomplete session data.
func TestDataReadCorpusSemantics(t *testing.T) {
	for _, shape := range []dataBenchmarkShape{
		{name: "linear", sessions: 3, messages: 240, textBytes: 64},
		{name: "compacted", sessions: 3, messages: 240, textBytes: 64, compacted: true},
		{name: "branched", sessions: 3, messages: 240, textBytes: 64, branched: true},
	} {
		t.Run(shape.name, func(t *testing.T) {
			corpus := makeDataBenchmarkCorpus(t, shape)
			api, managers := openDataBenchmarkService(t, corpus)
			catalog, err := api.ListSessions()
			if err != nil || len(catalog) != shape.sessions {
				t.Fatalf("catalog count = %d, %v", len(catalog), err)
			}
			for _, info := range catalog {
				if info.MessageCount != shape.messages || len(info.FirstMessage) != shape.textBytes {
					t.Fatalf("fixture summary lost messages: %#v", info)
				}
			}
			snapshot, err := api.SnapshotSession(corpus.firstID, "")
			if err != nil {
				t.Fatal(err)
			}
			wantContext := shape.messages
			if shape.compacted {
				wantContext = 201 // checkpoint, 100 kept entries, 100 later entries
			}
			if len(snapshot.Entries) != wantContext {
				t.Fatalf("context entries = %d, want %d", len(snapshot.Entries), wantContext)
			}
			if _, err := api.Dispatch(context.Background(), corpus.firstID, application.GetStateCommand{}); err != nil {
				t.Fatal(err)
			}
			manager := managers[corpus.firstID]
			if diagnostics := manager.LoadDiagnostics(); len(diagnostics) != 0 {
				t.Fatalf("fixture needs compatibility recovery: %#v", diagnostics)
			}
			entries := manager.Entries()
			wantEntries := shape.messages
			if shape.compacted || shape.branched {
				wantEntries++
			}
			if len(entries) != wantEntries {
				t.Fatalf("all entries = %d, want %d", len(entries), wantEntries)
			}
			for _, entry := range entries {
				if diagnostics := entry.Diagnostics(); len(diagnostics) != 0 {
					t.Fatalf("fixture entry %s is not fully projectable: %#v", entry.ID(), diagnostics)
				}
			}
			response, err := surfacewire.DispatchJSON(context.Background(), api, corpus.firstID, []byte(`{"type":"get_session_stats"}`), "interactive")
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(response.Data)
			if err != nil {
				t.Fatal(err)
			}
			var stats struct {
				TotalMessages, UserMessages, AssistantMessages, ToolCalls, ToolResults int
				Cost                                                                   float64
			}
			if err := json.Unmarshal(encoded, &stats); err != nil {
				t.Fatal(err)
			}
			if stats.TotalMessages != 240 || stats.UserMessages != 60 || stats.AssistantMessages != 120 || stats.ToolCalls != 60 || stats.ToolResults != 60 || math.Abs(stats.Cost-0.36) > 1e-12 {
				t.Fatalf("all-entry fixture accounting = %s", encoded)
			}
		})
	}
}

// BenchmarkDataReads exercises the same Application and wire projections used
// by HTTP and native clients. Corpus construction and Runtime admission are not
// timed. Unopened means an uncached SessionManager, not a cold OS page cache.
func BenchmarkDataReads(b *testing.B) {
	shapes := []dataBenchmarkShape{
		{name: "catalog1000", sessions: 1000, messages: 20, textBytes: 256},
		{name: "catalog5000", sessions: 5000, messages: 12, textBytes: 256},
		{name: "history12000", sessions: 1, messages: 12000, textBytes: 1024},
		{name: "compacted12000", sessions: 1, messages: 12000, textBytes: 1024, compacted: true},
		{name: "branched12000", sessions: 1, messages: 12000, textBytes: 1024, branched: true},
	}
	for _, shape := range shapes {
		b.Run(shape.name, func(b *testing.B) {
			corpus := makeDataBenchmarkCorpus(b, shape)
			api, managers := openDataBenchmarkService(b, corpus)
			if _, err := api.Snapshot(); err != nil {
				b.Fatal(err)
			}
			b.Logf("fixture: sessions=%d entries=%d JSONL-bytes=%d text-bytes=%d compacted=%t branched=%t",
				shape.sessions, corpus.entries, corpus.bytes, shape.textBytes, shape.compacted, shape.branched)
			b.Run("CatalogWarm", func(b *testing.B) {
				benchmarkDataQuery(b, func() (any, error) { return surfacewire.Snapshot(api) })
			})
			b.Run("HistoryUnopened", func(b *testing.B) {
				benchmarkDataQuery(b, func() (any, error) {
					return surfacewire.SessionViewFor(api, corpus.firstID, "", false, true)
				})
			})
			if _, err := api.Dispatch(context.Background(), corpus.firstID, application.GetStateCommand{}); err != nil {
				b.Fatal(err)
			}
			b.Run("HistoryActive", func(b *testing.B) {
				benchmarkDataQuery(b, func() (any, error) {
					return surfacewire.SessionViewFor(api, corpus.firstID, "", false, true)
				})
			})
			b.Run("ContextActive", func(b *testing.B) {
				benchmarkDataQuery(b, func() (any, error) {
					return surfacewire.SessionContextFor(api, corpus.firstID, "", false, true)
				})
			})
			b.Run("StateActive", func(b *testing.B) {
				benchmarkDataQuery(b, func() (any, error) {
					return surfacewire.DispatchJSON(context.Background(), api, corpus.firstID, []byte(`{"type":"get_state"}`), "interactive")
				})
			})
			b.Run("StatsActive", func(b *testing.B) {
				benchmarkDataQuery(b, func() (any, error) {
					return surfacewire.DispatchJSON(context.Background(), api, corpus.firstID, []byte(`{"type":"get_session_stats"}`), "interactive")
				})
			})
			b.Run("CatalogAfterAppend", func(b *testing.B) {
				manager := managers[corpus.firstID]
				if manager == nil {
					b.Fatal("fixture Runtime was not admitted")
				}
				var wireBytes int
				b.ReportAllocs()
				b.ResetTimer()
				for index := 0; index < b.N; index++ {
					b.StopTimer()
					message, err := llm.NewUserTextMessage("incremental fixture update", time.UnixMilli(2_000_000_000_000+int64(index)))
					if err != nil {
						b.Fatal(err)
					}
					if _, err := manager.AppendLLMMessage(context.Background(), message); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
					value, err := surfacewire.Snapshot(api)
					if err != nil {
						b.Fatal(err)
					}
					encoded, err := json.Marshal(value)
					if err != nil {
						b.Fatal(err)
					}
					wireBytes = len(encoded)
				}
				b.StopTimer()
				b.ReportMetric(float64(wireBytes), "wire-B/op")
			})
		})
	}
}

func benchmarkDataQuery(b *testing.B, query func() (any, error)) {
	b.Helper()
	var wireBytes int
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		value, err := query()
		if err != nil {
			b.Fatal(err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			b.Fatal(err)
		}
		wireBytes = len(encoded)
	}
	b.StopTimer()
	b.ReportMetric(float64(wireBytes), "wire-B/op")
}

func makeDataBenchmarkCorpus(b testing.TB, shape dataBenchmarkShape) dataBenchmarkCorpus {
	b.Helper()
	root := b.TempDir()
	corpus := dataBenchmarkCorpus{agentDir: filepath.Join(root, "agent"), cwd: filepath.Join(root, "workspace"), firstID: "perf-00000"}
	if err := os.MkdirAll(corpus.cwd, 0o700); err != nil {
		b.Fatal(err)
	}
	directory, err := session.SessionDirForAgentDir(corpus.cwd, corpus.agentDir)
	if err != nil {
		b.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		b.Fatal(err)
	}
	text := strings.Repeat("deterministic history payload ", shape.textBytes/30+1)[:shape.textBytes]
	for sessionIndex := 0; sessionIndex < shape.sessions; sessionIndex++ {
		id := fmt.Sprintf("perf-%05d", sessionIndex)
		path := filepath.Join(directory, id+".jsonl")
		file, err := os.Create(path)
		if err != nil {
			b.Fatal(err)
		}
		writer := bufio.NewWriterSize(file, 256*1024)
		encoder := json.NewEncoder(writer)
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(sessionIndex) * time.Hour)
		write := func(value any) {
			b.Helper()
			if err := encoder.Encode(value); err != nil {
				b.Fatal(err)
			}
		}
		write(map[string]any{"type": "session", "version": 3, "id": id, "timestamp": at.Format(time.RFC3339Nano), "cwd": corpus.cwd})
		parent := any(nil)
		for index := 0; index < shape.messages; index++ {
			entryID := fmt.Sprintf("e%08d", index)
			timestamp := at.Add(time.Duration(index) * time.Second)
			message := map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}, "timestamp": timestamp.UnixMilli()}
			callID := fmt.Sprintf("call-%d", index/4)
			switch index % 4 {
			case 1, 3:
				message["role"], message["api"], message["provider"], message["model"] = "assistant", "openai-completions", "fixture", "fixture-model"
				message["stopReason"] = "stop"
				message["usage"] = map[string]any{"input": 100, "output": 20, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 120,
					"cost": map[string]any{"input": 0.001, "output": 0.002, "cacheRead": 0, "cacheWrite": 0, "total": 0.003}}
				if index%4 == 1 {
					message["stopReason"] = "toolUse"
					message["content"] = []any{map[string]any{"type": "thinking", "thinking": "deterministic plan"},
						map[string]any{"type": "text", "text": text},
						map[string]any{"type": "toolCall", "id": callID, "name": "read", "arguments": map[string]any{"path": "fixture.txt"}}}
				}
			case 2:
				message["role"], message["toolCallId"], message["toolName"], message["isError"] = "toolResult", callID, "read", false
			}
			write(map[string]any{"type": "message", "id": entryID, "parentId": parent, "timestamp": timestamp.Format(time.RFC3339Nano), "message": message})
			corpus.entries++
			parent = entryID
			if shape.branched && index > 0 && index%128 == 0 {
				write(map[string]any{"type": "custom_message", "id": fmt.Sprintf("b%08d", index), "parentId": entryID,
					"timestamp": timestamp.Format(time.RFC3339Nano), "customType": "fixture-branch", "content": text, "display": true,
					"details": map[string]any{"fixture": true, "index": index}})
				corpus.entries++
			}
			if shape.compacted && index == shape.messages-101 {
				write(map[string]any{"type": "compaction", "id": "checkpoint", "parentId": parent, "timestamp": timestamp.Format(time.RFC3339Nano),
					"summary": "deterministic compaction checkpoint", "firstKeptEntryId": fmt.Sprintf("e%08d", index-99), "tokensBefore": 1_000_000})
				corpus.entries++
				parent = "checkpoint"
			}
		}
		if err := writer.Flush(); err != nil {
			b.Fatal(err)
		}
		if err := file.Close(); err != nil {
			b.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			b.Fatal(err)
		}
		corpus.bytes += info.Size()
	}
	return corpus
}

func openDataBenchmarkService(b testing.TB, corpus dataBenchmarkCorpus) (*application.Service, map[string]*session.SessionManager) {
	b.Helper()
	managers := make(map[string]*session.SessionManager)
	opener := func(ctx context.Context, config app.ProductionConfig, selection app.ProductionRuntimeOptions) (*agentruntime.Runtime, error) {
		manager, err := session.InMemorySessionManager(config.WorkingDir, session.NewSessionOptions{})
		if err != nil {
			return nil, err
		}
		if err := manager.SetSessionFile(selection.SessionPath); err != nil {
			_ = manager.Close()
			return nil, err
		}
		implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
		if err != nil {
			return nil, err
		}
		model, err := provider.NewModel(provider.ModelSpec{Provider: "fixture", API: "openai-completions", ID: "fixture-model", Name: "Fixture model",
			Input: []provider.InputKind{provider.InputText}, ContextWindow: 2_000_000, MaxTokens: 1000})
		if err != nil {
			return nil, err
		}
		factory := func(_ context.Context, options agentruntime.CreateOptions) (agentruntime.CreateResult, error) {
			coordinator, err := agent.NewSession(agent.SessionConfig{Provider: implementation, Model: model, SessionManager: options.SessionManager})
			if err != nil {
				return agentruntime.CreateResult{}, err
			}
			return agentruntime.CreateResult{Session: coordinator, Services: &agentruntime.Services{CWD: config.WorkingDir, AgentDir: config.AgentDir, Provider: implementation}}, nil
		}
		runtime, err := agentruntime.Create(ctx, factory, agentruntime.InitialOptions{CWD: config.WorkingDir, AgentDir: config.AgentDir, SessionManager: manager})
		if err != nil {
			return nil, err
		}
		managers[manager.SessionID()] = manager
		return runtime, nil
	}
	api, err := application.NewService(application.ServiceOptions{Production: app.ProductionConfig{WorkingDir: corpus.cwd, AgentDir: corpus.agentDir, Environment: []string{}},
		DisableReaper: true, OpenRuntime: opener})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := api.Close(context.Background()); err != nil {
			b.Error(err)
		}
	})
	return api, managers
}
