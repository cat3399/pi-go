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

type sessionResourceFixture struct {
	builds     [][]string
	expansions map[string]string
}

func (r *sessionResourceFixture) BuildSystemPrompt(names []string) (string, agent.BuildSystemPromptOptions, error) {
	cloned := append([]string(nil), names...)
	r.builds = append(r.builds, cloned)
	return "active tools: " + strings.Join(names, ","), agent.BuildSystemPromptOptions{
		SelectedTools: cloned, CWD: "/workspace", ToolSnippets: map[string]string{"read": "Read files", "edit": "Edit files"},
	}, nil
}

func (r *sessionResourceFixture) ExpandPromptInput(text string) (string, error) {
	if expanded, exists := r.expansions[text]; exists {
		return expanded, nil
	}
	return text, nil
}

func (*sessionResourceFixture) Reload(context.Context) error { return nil }

type sessionCatalogExecutor struct{}

func (sessionCatalogExecutor) Name() string         { return "catalog" }
func (sessionCatalogExecutor) Supports(string) bool { return true }
func (sessionCatalogExecutor) Execute(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return agent.ToolOutput{Text: "unused"}, nil
}
func (sessionCatalogExecutor) ExecuteNamed(context.Context, string, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return agent.ToolOutput{Text: "unused"}, nil
}

func TestAgentSessionOwnsToolRegistryPromptRebuildAndPromptExpansion(t *testing.T) {
	definitions := make([]provider.ToolDefinition, 0, 3)
	for _, name := range []string{"read", "bash", "edit"} {
		definition, err := provider.NewToolDefinition(name, name, false, []byte(`{"type":"object"}`))
		if err != nil {
			t.Fatal(err)
		}
		definitions = append(definitions, definition)
	}
	resources := &sessionResourceFixture{expansions: map[string]string{
		"/skill:review now": "review instructions\n\nnow",
		"/review-rich":      "expanded rich prompt",
	}}
	implementation := newScriptedProvider(t, mustTextTerminal(t, "done"), mustTextTerminal(t, "rich done"))
	var hookPrompt string
	var hookOptions agent.BuildSystemPromptOptions
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		Tool: sessionCatalogExecutor{}, Tools: definitions[:2], AllTools: definitions,
		ActiveToolNames: []string{"read", "bash"}, Resources: resources,
		Hooks: agent.Hooks{BeforeAgentStart: func(_ context.Context, event agent.BeforeAgentStartEvent) (agent.BeforeAgentStartResult, error) {
			hookPrompt = event.Prompt
			hookOptions = event.SystemPromptOptions
			return agent.BeforeAgentStartResult{}, nil
		}},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := runtime.ActiveToolNames(); !sameStrings(got, []string{"read", "bash"}) {
		t.Fatalf("initial active tools = %v", got)
	}
	if got := runtime.AllTools(); len(got) != 3 || got[0].Name() != "read" || got[2].Name() != "edit" {
		t.Fatalf("all tools = %#v", got)
	}
	if runtime.SystemPrompt() != "active tools: read,bash" {
		t.Fatalf("initial system prompt = %q", runtime.SystemPrompt())
	}
	if err := runtime.SetActiveToolsByName([]string{"edit", "missing", "edit"}); err != nil {
		t.Fatal(err)
	}
	if got := runtime.ActiveToolNames(); !sameStrings(got, []string{"edit"}) {
		t.Fatalf("updated active tools = %v", got)
	}
	if runtime.SystemPrompt() != "active tools: edit" {
		t.Fatalf("updated system prompt = %q", runtime.SystemPrompt())
	}
	result, err := runtime.Run(context.Background(), "/skill:review now")
	if err != nil || !result.Succeeded() {
		t.Fatalf("Run() = (%#v, %v)", result, err)
	}
	if hookPrompt != "review instructions\n\nnow" || !sameStrings(hookOptions.SelectedTools, []string{"edit"}) {
		t.Fatalf("before_agent_start = prompt %q options %#v", hookPrompt, hookOptions)
	}
	requests := implementation.Requests()
	if len(requests) != 1 || requests[0].SystemPrompt() != "active tools: edit" || len(requests[0].Tools()) != 1 || requests[0].Tools()[0].Name() != "edit" {
		t.Fatalf("provider request = %#v", requests)
	}
	messages := requests[0].Messages()
	if len(messages) != 1 {
		t.Fatalf("request messages = %#v", messages)
	}
	user, ok := messages[0].(llm.UserContentMessage)
	if !ok || len(user.Content()) != 1 {
		t.Fatalf("user message = %#v", messages[0])
	}
	text, ok := user.Content()[0].(llm.TextBlock)
	if !ok || text.Text() != "review instructions\n\nnow" {
		t.Fatalf("expanded user content = %#v", user.Content())
	}
	richText, err := llm.NewTextBlock("/review-rich")
	if err != nil {
		t.Fatal(err)
	}
	result, err = runtime.RunContent(context.Background(), []llm.UserContentBlock{richText})
	if err != nil || !result.Succeeded() {
		t.Fatalf("RunContent() = (%#v, %v)", result, err)
	}
	requests = implementation.Requests()
	richMessages := requests[1].Messages()
	richUser, ok := richMessages[len(richMessages)-1].(llm.UserContentMessage)
	if !ok || richUser.Content()[0].(llm.TextBlock).Text() != "expanded rich prompt" || hookPrompt != "expanded rich prompt" {
		t.Fatalf("expanded rich input = message %#v, hook %q", richMessages[len(richMessages)-1], hookPrompt)
	}
	if err := runtime.SetActiveToolsByName([]string{}); err != nil {
		t.Fatal(err)
	}
	if got := runtime.ActiveToolNames(); len(got) != 0 || runtime.SystemPrompt() != "active tools: " {
		t.Fatalf("disabled tools = %v, prompt %q", got, runtime.SystemPrompt())
	}
	if len(resources.builds) != 3 || !sameStrings(resources.builds[0], []string{"read", "bash"}) ||
		!sameStrings(resources.builds[1], []string{"edit"}) || len(resources.builds[2]) != 0 {
		t.Fatalf("prompt rebuilds = %#v", resources.builds)
	}
}
