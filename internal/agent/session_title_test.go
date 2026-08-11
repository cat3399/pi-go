package agent_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestParseGeneratedSessionTitle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "plain", raw: "Fix session export", want: "Fix session export"},
		{name: "json fence", raw: "```json\n{\"title\":\"恢复项目技能管理\"}\n```", want: "恢复项目技能管理"},
		{name: "inline text fence", raw: "```text Concise title```", want: "Concise title"},
		{name: "localized label and quotes", raw: "标题：「排查模型选择失败。」", want: "排查模型选择失败"},
		{name: "first line only", raw: "Concise title\nHere is an explanation", want: "Concise title"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := agent.ParseGeneratedSessionTitle(test.raw)
			if err != nil {
				t.Fatalf("ParseGeneratedSessionTitle() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("ParseGeneratedSessionTitle() = %q, want %q", got, test.want)
			}
		})
	}

	long := strings.Repeat("会", 100)
	got, err := agent.ParseGeneratedSessionTitle(long)
	if err != nil {
		t.Fatal(err)
	}
	if runes := []rune(got); len(runes) != 80 {
		t.Fatalf("bounded title has %d runes, want 80", len(runes))
	}
	if _, err := agent.ParseGeneratedSessionTitle("```\n---\n```"); err == nil {
		t.Fatal("punctuation-only title was accepted")
	}
}

func TestGenerateSessionTitleKeepsAssistantContentAroundIncompleteToolCalls(t *testing.T) {
	manager := newSessionManager(t)
	user, err := llm.NewUserTextMessage("inspect the project", time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	text, err := llm.NewTextBlock("I found a likely cause")
	if err != nil {
		t.Fatal(err)
	}
	call, err := llm.NewToolCallBlock("unfinished-call", "read", []byte(`{"path":"file.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantToolUseMessage(
		[]llm.AssistantBlock{text, call}, llm.Usage{}, time.UnixMilli(2),
		llm.AssistantProvenance{Provider: "scripted", API: "scripted", Model: "test-model"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), assistant); err != nil {
		t.Fatal(err)
	}
	implementation := newScriptedProvider(t, mustTextTerminal(t, "Investigate project failure"))
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: manager, Model: sessionTestModel(t),
		ThinkingLevel: provider.ThinkingOff,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.GenerateSessionTitle(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := implementation.Requests()
	if len(requests) != 1 {
		t.Fatalf("title provider requests = %d", len(requests))
	}
	messages := requests[0].Messages()
	if len(messages) != 3 {
		t.Fatalf("title request messages = %d", len(messages))
	}
	terminal, ok := messages[1].(llm.AssistantTerminal)
	if !ok {
		t.Fatalf("sanitized assistant = %T", messages[1])
	}
	if len(terminal.Blocks()) != 1 {
		t.Fatalf("sanitized assistant conversation = %#v", terminal)
	}
	preserved, ok := terminal.Blocks()[0].(llm.TextBlock)
	if !ok || preserved.Text() != "I found a likely cause" {
		t.Fatalf("sanitized assistant blocks = %#v", terminal.Blocks())
	}
}

func TestGenerateSessionTitleDoesNotMutateConversation(t *testing.T) {
	implementation := newScriptedProvider(t,
		mustTextTerminal(t, "我会检查会话能力。"),
		mustTextTerminal(t, "标题：「完善会话管理能力。」"),
	)
	manager := newSessionManager(t)
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: manager, Model: sessionTestModel(t),
		ThinkingLevel: provider.ThinkingOff,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Run(context.Background(), "完善 Session 管理"); err != nil {
		t.Fatal(err)
	}
	before := manager.Entries()
	generated, err := coordinator.GenerateSessionTitle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if generated.Title != "完善会话管理能力" {
		t.Fatalf("title = %q", generated.Title)
	}
	after := manager.Entries()
	if len(after) != len(before) {
		t.Fatalf("title generation persisted transcript entries: before=%d after=%d", len(before), len(after))
	}
	for index := range before {
		if string(before[index].RawJSON()) != string(after[index].RawJSON()) {
			t.Fatalf("entry %d changed during title generation", index)
		}
	}
}
