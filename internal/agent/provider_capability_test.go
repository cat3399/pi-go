package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/tool"
)

type capabilityTool struct {
	overrideName string
	mode         agent.ToolExecutionMode
}

type sequentialRegistryTool struct{}

func (sequentialRegistryTool) Name() string { return "advertised" }
func (sequentialRegistryTool) ToolExecutionMode() tool.ExecutionMode {
	return tool.ExecutionSequential
}
func (sequentialRegistryTool) ExecuteJSON(context.Context, []byte) (tool.ToolResult, error) {
	return tool.ToolResult{Text: "unused"}, nil
}

func (*capabilityTool) Name() string         { return "capability-registry" }
func (*capabilityTool) Supports(string) bool { return true }
func (*capabilityTool) Execute(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return agent.ToolOutput{}, nil
}

func TestRegistryExecutorForwardsSequentialOverrideToProviderCapability(t *testing.T) {
	schema := []byte(`{"type":"object","additionalProperties":false,"properties":{},"required":[]}`)
	specification, err := tool.NewSpecification("advertised", "Sequential registry tool.", false, schema)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewRegistryWithSpecifications([]tool.Specification{specification}, sequentialRegistryTool{})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := agent.NewRegistryExecutor(registry)
	if err != nil {
		t.Fatal(err)
	}
	if mode, set := executor.ToolExecutionMode("advertised"); !set || mode != agent.ToolExecutionSequential {
		t.Fatalf("registry execution mode = %s/%t", mode, set)
	}
	definition, err := provider.NewToolDefinition("advertised", "Sequential registry tool.", false, schema)
	if err != nil {
		t.Fatal(err)
	}
	transcript := newSession(t)
	scripted := newScriptedProvider(t, mustTextTerminal(t, "done"))
	model, err := newTestModel("scripted", "scripted", "scripted-1")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.New(agent.Config{
		Provider:          scripted,
		Transcript:        transcript,
		Model:             model,
		Tool:              executor,
		Tools:             []provider.ToolDefinition{definition},
		Now:               func() time.Time { return agentTestEpoch },
		SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "run"); err != nil {
		t.Fatal(err)
	}
	if requests := scripted.Requests(); len(requests) != 1 || requests[0].ParallelToolCalls() {
		t.Fatalf("registry request capability = %#v", requests)
	}
}
func (*capabilityTool) ExecuteNamed(context.Context, string, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return agent.ToolOutput{}, nil
}
func (t *capabilityTool) ToolExecutionMode(name string) (agent.ToolExecutionMode, bool) {
	return t.mode, name == t.overrideName
}

func TestProviderParallelToolCapabilityMatchesEffectiveExecutionMode(t *testing.T) {
	definition, err := provider.NewToolDefinition(
		"advertised", "Exercise request capability admission.", false,
		[]byte(`{"type":"object","additionalProperties":false,"properties":{},"required":[]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		global   agent.ToolExecutionMode
		override string
		mode     agent.ToolExecutionMode
		want     bool
	}{
		{name: "parallel scheduler", want: true},
		{name: "global sequential", global: agent.ToolExecutionSequential, want: false},
		{name: "advertised sequential override", override: "advertised", mode: agent.ToolExecutionSequential, want: false},
		{name: "parallel override", override: "advertised", mode: agent.ToolExecutionParallel, want: true},
		{name: "unadvertised sequential override", override: "other", mode: agent.ToolExecutionSequential, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transcript := newSession(t)
			scripted := newScriptedProvider(t, mustTextTerminal(t, "done"))
			model, err := newTestModel("scripted", "scripted", "scripted-1")
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := agent.New(agent.Config{
				Provider:          scripted,
				Transcript:        transcript,
				Model:             model,
				Tool:              &capabilityTool{overrideName: test.override, mode: test.mode},
				Tools:             []provider.ToolDefinition{definition},
				ToolExecution:     test.global,
				Now:               func() time.Time { return agentTestEpoch },
				SettlementTimeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.Run(context.Background(), "run"); err != nil {
				t.Fatal(err)
			}
			requests := scripted.Requests()
			if len(requests) != 1 {
				t.Fatalf("provider requests = %d", len(requests))
			}
			if got := requests[0].ParallelToolCalls(); got != test.want {
				t.Fatalf("ParallelToolCalls() = %t, want %t", got, test.want)
			}
			if tools := requests[0].Tools(); len(tools) != 1 || tools[0].Name() != "advertised" {
				t.Fatalf("request tools = %#v", tools)
			}
		})
	}
}
