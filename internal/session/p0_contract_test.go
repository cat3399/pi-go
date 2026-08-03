package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

func TestP0ToolResultMetadataRoundTripsWithoutVendorShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	ids := []string{"one", "two"}
	next := 0
	s, err := Create(path, CreateOptions{ID: "p0", WorkingDir: dir, Now: func() time.Time { return time.UnixMilli(100) }, NewEntryID: func() (string, error) { value := ids[next]; next++; return value, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	text, err := llm.NewTextBlock("tool image attached")
	if err != nil {
		t.Fatal(err)
	}
	cost := llm.Cost{Input: .01, Output: .02, CacheRead: .03, CacheWrite: .04, Total: .10}
	usage, err := llm.NewUsage(llm.UsageSpec{Input: 11, Output: 7, CacheRead: 3, CacheWrite: 2, Cost: &cost})
	if err != nil {
		t.Fatal(err)
	}
	message, err := llm.NewToolResultMessageWithMetadata("call", "generic-tool", []llm.TextBlock{text}, false, time.UnixMilli(99), llm.ToolResultMetadata{Details: json.RawMessage(`{"opaque":{"stable":true}}`), Usage: &usage, AddedToolNames: []string{"late-tool", "other-tool"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(context.Background(), message, AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := reopened.BuildContext().Messages()[0].(llm.ToolResultMessage)
	if names := got.AddedToolNames(); len(names) != 2 || names[0] != "late-tool" || names[1] != "other-tool" {
		t.Fatalf("added tools=%#v", names)
	}
	gotUsage, ok := got.Usage()
	if !ok || gotUsage.Input() != 11 {
		t.Fatalf("usage=%#v,%t", gotUsage, ok)
	}
	gotCost, ok := gotUsage.Cost()
	if !ok || gotCost.Total != .10 {
		t.Fatalf("cost=%#v,%t", gotCost, ok)
	}
}

func TestP0SessionEntryUnionDecodesOriginalV3Shapes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "union.jsonl")
	data := `{"type":"session","version":3,"id":"s","timestamp":"2026-01-01T00:00:00.000Z","cwd":"` + dir + `"}
{"type":"thinking_level_change","id":"a","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","thinkingLevel":"high"}
{"type":"model_change","id":"b","parentId":"a","timestamp":"2026-01-01T00:00:02.000Z","provider":"generic","modelId":"model"}
{"type":"custom","id":"c","parentId":"b","timestamp":"2026-01-01T00:00:03.000Z","customType":"artifact","data":{"id":1}}
{"type":"custom_message","id":"d","parentId":"c","timestamp":"2026-01-01T00:00:04.000Z","customType":"note","content":[{"type":"text","text":"keep rich"}],"display":true,"details":{"ui":"only"}}
{"type":"branch_summary","id":"e","parentId":"d","timestamp":"2026-01-01T00:00:05.000Z","fromId":"b","summary":"branch"}
{"type":"label","id":"f","parentId":"e","timestamp":"2026-01-01T00:00:06.000Z","targetId":"d","label":"bookmark"}
{"type":"session_info","id":"g","parentId":"f","timestamp":"2026-01-01T00:00:07.000Z","name":"session name"}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entries := s.Entries()
	if len(entries) != 7 {
		t.Fatalf("entries=%d", len(entries))
	}
	if _, ok := entries[0].Payload().(ThinkingLevelChangePayload); !ok {
		t.Fatalf("thinking payload=%T", entries[0].Payload())
	}
	if _, ok := entries[1].Payload().(ModelChangePayload); !ok {
		t.Fatalf("model payload=%T", entries[1].Payload())
	}
	if _, ok := entries[2].Payload().(CustomPayload); !ok {
		t.Fatalf("custom payload=%T", entries[2].Payload())
	}
	if _, ok := entries[3].Payload().(CustomMessagePayload); !ok {
		t.Fatalf("custom message payload=%T", entries[3].Payload())
	}
	if _, ok := entries[4].Payload().(BranchSummaryPayload); !ok {
		t.Fatalf("branch payload=%T", entries[4].Payload())
	}
	if _, ok := entries[5].Payload().(LabelPayload); !ok {
		t.Fatalf("label payload=%T", entries[5].Payload())
	}
	if _, ok := entries[6].Payload().(SessionInfoPayload); !ok {
		t.Fatalf("info payload=%T", entries[6].Payload())
	}
	context := s.BuildContext()
	if len(context.AgentMessages()) != 2 || len(context.Messages()) != 2 {
		t.Fatalf("context agent/llm=%d/%d", len(context.AgentMessages()), len(context.Messages()))
	}
}

func TestP0SessionPayloadAppendAndReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "append.jsonl")
	ids := []string{"a", "b", "c", "d"}
	index := 0
	s, err := Create(path, CreateOptions{ID: "append", WorkingDir: dir, Now: func() time.Time { return time.UnixMilli(1) }, NewEntryID: func() (string, error) { value := ids[index]; index++; return value, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendPayload(context.Background(), ThinkingLevelChangePayload{ThinkingLevel: "high"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendPayload(context.Background(), ModelChangePayload{Provider: "generic", ModelID: "model"}); err != nil {
		t.Fatal(err)
	}
	custom, err := agentmsg.NewCustom(agentmsg.Custom{CustomType: "extension", Content: []llm.UserContentBlock{mustSessionText(t, "context")}, Display: true, Details: json.RawMessage(`{"kept":true}`), At: time.UnixMilli(3)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendPayload(context.Background(), CustomMessagePayload{Message: custom}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendPayload(context.Background(), SessionInfoPayload{Name: stringPtr("named")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	entries := reopened.Entries()
	if len(entries) != 4 {
		t.Fatalf("entries=%d", len(entries))
	}
	if payload, ok := entries[2].Payload().(CustomMessagePayload); !ok || payload.Message.CustomType != "extension" {
		t.Fatalf("payload=%#v", entries[2].Payload())
	}
}

func TestP0AgentMessageAndBranchSettingsSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-message.jsonl")
	ids := []string{"root", "model", "bash", "custom", "opaque", "branch"}
	next := 0
	s, err := Create(path, CreateOptions{ID: "agent-message", WorkingDir: dir, Now: func() time.Time { return time.UnixMilli(100) }, NewEntryID: func() (string, error) { id := ids[next]; next++; return id, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendPayload(context.Background(), ThinkingLevelChangePayload{ThinkingLevel: "high"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendPayload(context.Background(), ModelChangePayload{Provider: "generic", ModelID: "one"}); err != nil {
		t.Fatal(err)
	}
	code := 1
	bash, err := agentmsg.NewBashExecution(agentmsg.BashExecution{Command: "false", Output: "failed", ExitCode: &code, FullOutputPath: "/tmp/full", Truncated: true, At: time.UnixMilli(2)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendAgentMessage(context.Background(), bash, AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	custom, err := agentmsg.NewCustomText("extension", "string form", true, json.RawMessage(`{"present":true}`), time.UnixMilli(3))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendAgentMessage(context.Background(), custom, AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	opaque, err := agentmsg.NewOpaque(agentmsg.OpaqueMessage{Type: "extensionEvent", Data: json.RawMessage(`{"role":"extensionEvent","value":{"keep":true},"timestamp":4}`), At: time.UnixMilli(4)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendAgentMessage(context.Background(), opaque, AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SelectLeaf("model"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendPayload(context.Background(), ThinkingLevelChangePayload{ThinkingLevel: "low"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// Reopen selects the physical tail (the alternate branch).
	context := reopened.BuildContext()
	if level, ok := context.ThinkingLevel(); !ok || level != "low" {
		t.Fatalf("thinking = %q, %t", level, ok)
	}
	if selected, ok := context.Model(); !ok || selected.Provider != "generic" || selected.ModelID != "one" {
		t.Fatalf("model = %#v, %t", selected, ok)
	}
	if err := reopened.SelectLeaf("opaque"); err != nil {
		t.Fatal(err)
	}
	context = reopened.BuildContext()
	if level, _ := context.ThinkingLevel(); level != "high" {
		t.Fatalf("switched thinking = %q", level)
	}
	messages := context.AgentMessages()
	if len(messages) != 3 {
		t.Fatalf("agent messages = %#v", messages)
	}
	if got, ok := messages[0].(agentmsg.BashExecution); !ok || got.ExitCode == nil || *got.ExitCode != 1 || !got.Truncated || got.FullOutputPath != "/tmp/full" {
		t.Fatalf("bash = %#v", messages[0])
	}
	if got, ok := messages[1].(agentmsg.Custom); !ok || got.StringContent == nil || *got.StringContent != "string form" || string(got.Details) != `{"present":true}` {
		t.Fatalf("custom = %#v", messages[1])
	}
	if got, ok := messages[2].(agentmsg.OpaqueMessage); !ok || string(got.Data) != `{"role":"extensionEvent","value":{"keep":true},"timestamp":4}` {
		t.Fatalf("opaque = %#v", messages[2])
	}
	if projected := context.Messages(); len(projected) != 2 {
		t.Fatalf("LLM projection = %#v", projected)
	}
}

func TestP0ModelChangeRequiresModelID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing-model.jsonl")
	data := `{"type":"session","version":3,"id":"s","timestamp":"2026-01-01T00:00:00.000Z","cwd":"` + dir + `"}
{"type":"model_change","id":"a","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","provider":"generic"}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, OpenOptions{}); err == nil {
		t.Fatal("Open accepted model_change without modelId")
	}
}
func mustSessionText(t *testing.T, text string) llm.TextBlock {
	t.Helper()
	value, err := llm.NewTextBlock(text)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func stringPtr(value string) *string { return &value }
